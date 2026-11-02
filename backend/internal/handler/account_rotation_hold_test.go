package handler

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type handlerRotationHoldCache struct {
	openAIHandlerRotationCache
	holdMu           sync.Mutex
	held             bool
	busyUntilHold    bool
	holdOnAttempt    int
	attempts         int
	slots            map[string]bool
	waiters          map[string]bool
	lastMax          int
	lastSticky       bool
	lastAccountID    int64
	rotationRenewals int
	blockOnAcquire   bool
}

var _ service.AccountRotationHoldCache = (*handlerRotationHoldCache)(nil)

func newHandlerRotationHoldFixture() (*OpenAIGatewayHandler, *handlerRotationHoldCache, *service.ConcurrencyService) {
	cache := &handlerRotationHoldCache{held: true, slots: make(map[string]bool), waiters: make(map[string]bool)}
	cache.phases = make(map[int64]string)
	concurrency := service.NewConcurrencyService(cache)
	concurrency.SetAccountRotation(openAIHandlerRotationSettings{enabled: false, mode: service.AccountRotationModeAggressive}, nil)
	h := &OpenAIGatewayHandler{
		gatewayService:    &service.OpenAIGatewayService{},
		concurrencyHelper: NewConcurrencyHelper(concurrency, SSEPingFormatComment, time.Second),
	}
	return h, cache, concurrency
}

func (c *handlerRotationHoldCache) CheckAccountRotation(ctx context.Context, _ int64, _ service.AccountRotationPolicy) (*service.AccountRotationState, error) {
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	return c.stateLocked(), ctx.Err()
}

func (c *handlerRotationHoldCache) stateLocked() *service.AccountRotationState {
	state := &service.AccountRotationState{Phase: "ready", WaitingCount: len(c.waiters)}
	if c.held {
		state.Phase, state.Hold, state.Reason = "resting", true, "manual"
	}
	return state
}

func (c *handlerRotationHoldCache) SetAccountRotationHold(context.Context, int64, int, service.AccountRotationPolicy) (*service.AccountRotationState, error) {
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	c.held = true
	return c.stateLocked(), nil
}

func (c *handlerRotationHoldCache) CancelAccountRotationHold(context.Context, int64, service.AccountRotationPolicy) (*service.AccountRotationState, error) {
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	c.held, c.busyUntilHold = false, false
	return c.stateLocked(), nil
}

func (c *handlerRotationHoldCache) AcquireAccountSlotWithRotationState(ctx context.Context, id int64, max int, requestID string, _ service.AccountRotationPolicy, sticky bool, _ string) (bool, *service.AccountRotationState, error) {
	c.holdMu.Lock()
	if c.blockOnAcquire {
		c.holdMu.Unlock()
		<-ctx.Done()
		return false, nil, ctx.Err()
	}
	defer c.holdMu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}
	c.attempts++
	c.lastMax, c.lastSticky, c.lastAccountID = max, sticky, id
	if c.holdOnAttempt > 0 && c.attempts == c.holdOnAttempt {
		c.held = true
	}
	state := c.stateLocked()
	if c.held || c.busyUntilHold {
		return false, state, nil
	}
	c.slots[requestID] = true
	return true, state, nil
}

func (c *handlerRotationHoldCache) RefreshAccountRotationWaiter(_ context.Context, _ int64, requestID string) error {
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	c.waiters[requestID] = true
	return nil
}

func (c *handlerRotationHoldCache) RemoveAccountRotationWaiter(_ context.Context, _ int64, requestID string) error {
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	delete(c.waiters, requestID)
	return nil
}

func (c *handlerRotationHoldCache) RefreshAccountRotationSlot(context.Context, int64, string) error {
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	c.rotationRenewals++
	return nil
}

func (c *handlerRotationHoldCache) ReleaseAccountSlot(_ context.Context, _ int64, requestID string) error {
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	delete(c.slots, requestID)
	return nil
}

func (c *handlerRotationHoldCache) assertCounts(t *testing.T, slots, waiters int) {
	t.Helper()
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	require.Len(t, c.slots, slots, "only forwarded requests may occupy account slots")
	require.Len(t, c.waiters, waiters, "HOLD waiters must have their own cancellable leases")
}

type handlerRotationHoldOutcome struct {
	release func()
	err     error
	result  openAISlotAcquireResult
}

func TestAccountRotationHoldHandlerQueueTimeoutDoesNotEndRest(t *testing.T) {
	for _, path := range []string{"already_held", "becomes_held_in_normal_queue"} {
		t.Run(path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, cache, concurrency := newHandlerRotationHoldFixture()
				c, recorder := newHelperTestContext(http.MethodPost, "/v1/responses")
				c.Request = c.Request.WithContext(concurrency.AccountRotationContext(c.Request.Context()))
				if path == "becomes_held_in_normal_queue" {
					cache.held, cache.busyUntilHold, cache.holdOnAttempt = false, true, 3
				}
				started := false
				done := make(chan handlerRotationHoldOutcome, 1)
				go func() {
					if path == "already_held" {
						release, err := h.concurrencyHelper.AcquireAccountSlotWithWaitTimeout(c, 7, 2, 30*time.Second, true, &started)
						done <- handlerRotationHoldOutcome{release: release, err: err}
						return
					}
					selection := openAIHandlerRotationWaitSelection(7)
					selection.WaitPlan.Timeout = 30 * time.Second
					release, result := h.acquireOpenAIAccountSlot(c, nil, "", selection, true, &started, zap.NewNop(), nil)
					done <- handlerRotationHoldOutcome{release: release, result: result}
				}()
				synctest.Wait()
				if path == "becomes_held_in_normal_queue" {
					time.Sleep(time.Second)
					synctest.Wait()
				}
				cache.assertCounts(t, 0, 1)
				time.Sleep(65 * time.Second)
				synctest.Wait()
				select {
				case outcome := <-done:
					t.Fatalf("ordinary queue timeout cut off HOLD: %+v", outcome)
				default:
				}
				cache.assertCounts(t, 0, 1)
				require.Empty(t, recorder.Body.String(), "no response bytes may precede HOLD admission")
				require.False(t, started)
				require.False(t, c.Writer.Written())
				_, err := concurrency.CancelAccountManualRest(c.Request.Context(), 7)
				require.NoError(t, err)
				time.Sleep(time.Second)
				outcome := <-done
				require.NoError(t, outcome.err)
				if path != "already_held" {
					require.Equal(t, openAISlotAcquireOK, outcome.result)
				}
				require.NotNil(t, outcome.release)
				cache.assertCounts(t, 1, 0)
				outcome.release()
				cache.assertCounts(t, 0, 0)
				require.EqualValues(t, 7, cache.lastAccountID)
				require.Zero(t, cache.accountAcquireCalls, "HOLD must never fall through to ordinary admission")
			})
		})
	}
}

func TestAccountRotationHoldHandlerCancellationClearsWaiterWithoutOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, cache, concurrency := newHandlerRotationHoldFixture()
		c, recorder := newHelperTestContext(http.MethodPost, "/v1/responses")
		ctx, cancel := context.WithCancel(concurrency.AccountRotationContext(c.Request.Context()))
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		started := false
		done := make(chan handlerRotationHoldOutcome, 1)
		go func() {
			release, err := h.concurrencyHelper.AcquireAccountSlotWithWaitTimeout(c, 7, 0, 30*time.Second, true, &started)
			done <- handlerRotationHoldOutcome{release: release, err: err}
		}()
		synctest.Wait()
		cache.assertCounts(t, 0, 1)
		time.Sleep(35 * time.Second)
		synctest.Wait()
		cache.assertCounts(t, 0, 1)
		cancel()
		outcome := <-done
		require.ErrorIs(t, outcome.err, context.Canceled)
		require.Nil(t, outcome.release)
		cache.assertCounts(t, 0, 0)
		require.Empty(t, recorder.Body.String())
		require.False(t, started)
	})
}

func TestAccountRotationHoldHandlerNormalRedisAcquireKeepsQueueDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, cache, _ := newHandlerRotationHoldFixture()
		cache.held, cache.blockOnAcquire = false, true
		c, recorder := newHelperTestContext(http.MethodPost, "/v1/responses")
		ctx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		started := false
		done := make(chan handlerRotationHoldOutcome, 1)
		go func() {
			release, err := h.concurrencyHelper.AcquireAccountSlotWithWaitTimeout(c, 7, 2, 2*time.Second, false, &started)
			done <- handlerRotationHoldOutcome{release: release, err: err}
		}()
		synctest.Wait()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		select {
		case result := <-done:
			require.ErrorIs(t, result.err, context.DeadlineExceeded)
			require.Nil(t, result.release)
		default:
			t.Fatal("ordinary Redis acquisition escaped the queue deadline without observing HOLD")
		}
		require.NoError(t, ctx.Err(), "only the normal queue budget should expire")
		cache.assertCounts(t, 0, 0)
		require.Empty(t, recorder.Body.String())
	})
}

func TestAccountRotationHoldAutoOffInflightReleaseBelongsToForwarder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, cache, concurrency := newHandlerRotationHoldFixture()
		cache.held = false
		c, _ := newHelperTestContext(http.MethodPost, "/v1/responses")
		ctx, cancel := context.WithCancel(concurrency.AccountRotationContext(c.Request.Context()))
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		started := false
		release, result := h.acquireOpenAIAccountSlot(c, nil, "", openAIHandlerRotationWaitSelection(7), false, &started, zap.NewNop(), nil)
		require.Equal(t, openAISlotAcquireOK, result)
		require.NotNil(t, release)
		defer release()
		cancel()
		time.Sleep(21 * time.Second)
		synctest.Wait()
		cache.assertCounts(t, 1, 0)
		require.Positive(t, cache.rotationRenewals, "client cancellation cannot hide an upstream request still draining")
		release()
		release()
		cache.assertCounts(t, 0, 0)
	})
}

func TestAccountRotationHoldWebSocketStickyTurnWaitsBeforePricing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, cache, concurrency := newHandlerRotationHoldFixture()
		cache.userSeq = []bool{true}
		c, recorder := newHelperTestContext(http.MethodGet, "/v1/responses")
		c.Request = c.Request.WithContext(concurrency.AccountRotationContext(c.Request.Context()))
		var pricing openAIWSTurnPricing
		type outcome struct {
			userRelease, accountRelease func()
			err                         error
		}
		done := make(chan outcome, 1)
		go func() {
			userRelease, accountRelease, err := h.admitOpenAIWSTurnForPricing(c.Request.Context(), c, 202, 3,
				&service.APIKey{ID: 77}, &service.Account{ID: 7, Concurrency: 2}, 2, 2, zap.NewNop(), &pricing)
			done <- outcome{userRelease, accountRelease, err}
		}()
		synctest.Wait()
		cache.assertCounts(t, 0, 1)
		require.True(t, pricing.currentOr(time.Time{}).IsZero(), "waiting time cannot freeze a billable turn")
		require.True(t, cache.lastSticky)
		require.Empty(t, recorder.Body.String())
		_, err := concurrency.CancelAccountManualRest(c.Request.Context(), 7)
		require.NoError(t, err)
		time.Sleep(time.Second)
		result := <-done
		require.NoError(t, result.err)
		require.NotNil(t, result.userRelease)
		require.NotNil(t, result.accountRelease)
		require.False(t, pricing.currentOr(time.Time{}).IsZero())
		result.accountRelease()
		result.userRelease()
		cache.assertCounts(t, 0, 0)
	})
}

func TestAccountRotationHoldGeminiMetadataWaitsWithoutGenerationSlotOrResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		openAI, cache, concurrency := newHandlerRotationHoldFixture()
		h := &GatewayHandler{concurrencyHelper: openAI.concurrencyHelper}
		c, recorder := newHelperTestContext(http.MethodGet, "/v1beta/models")
		ctx := service.WithAccountRotationSticky(c.Request.Context(), 7)
		c.Request = c.Request.WithContext(ctx)
		type outcome struct {
			release  func()
			admitted bool
		}
		done := make(chan outcome, 1)
		go func() {
			release, admitted := h.acquireGeminiRotationMetadataSlot(c, &service.Account{ID: 7, Concurrency: 1})
			done <- outcome{release, admitted}
		}()
		synctest.Wait()
		cache.assertCounts(t, 0, 1)
		require.Zero(t, cache.lastMax, "metadata bypasses generation limits but still obeys HOLD")
		require.True(t, cache.lastSticky)
		require.True(t, service.HasAccountRotationTracking(c.Request.Context()))
		require.Empty(t, recorder.Body.String())
		require.False(t, c.Writer.Written())
		_, err := concurrency.CancelAccountManualRest(c.Request.Context(), 7)
		require.NoError(t, err)
		time.Sleep(time.Second)
		result := <-done
		require.True(t, result.admitted)
		require.NotNil(t, result.release)
		cache.assertCounts(t, 1, 0)
		result.release()
		cache.assertCounts(t, 0, 0)
	})
}

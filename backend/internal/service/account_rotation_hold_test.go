package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// Clock transitions belong to the Redis tests. This fake controls admission
// decisions so these tests exercise the real service and scheduler wait loops.
type rotationHoldTestCache struct {
	schedulerTestConcurrencyCache
	mu           sync.Mutex
	states       map[int64]AccountRotationState
	slots        map[int64]map[string]bool
	waiters      map[int64]map[string]bool
	full         map[int64]bool
	acquireErr   map[int64]error
	waiterErr    error
	calls        []rotationHoldAcquireCall
	legacyCalls  int
	refreshCalls int
}

type rotationHoldAcquireCall struct {
	requestID string
	accountID int64
	max       int
	sticky    bool
	policy    AccountRotationPolicy
	tracked   bool
	trace     string
	deadline  time.Time
}

type rotationHoldTraceKey struct{}

var _ AccountRotationHoldCache = (*rotationHoldTestCache)(nil)

func newRotationHoldTestCache() *rotationHoldTestCache {
	return &rotationHoldTestCache{
		states: make(map[int64]AccountRotationState), slots: make(map[int64]map[string]bool),
		waiters: make(map[int64]map[string]bool), full: make(map[int64]bool), acquireErr: make(map[int64]error),
	}
}

func (c *rotationHoldTestCache) stateLocked(id int64) *AccountRotationState {
	state := c.states[id]
	if state.Phase == "" {
		state.Phase = "ready"
	}
	state.WaitingCount = len(c.waiters[id])
	return &state
}

func (c *rotationHoldTestCache) CheckAccountRotation(ctx context.Context, id int64, _ AccountRotationPolicy) (*AccountRotationState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stateLocked(id), ctx.Err()
}

func (c *rotationHoldTestCache) SetAccountRotationHold(_ context.Context, id int64, seconds int, _ AccountRotationPolicy) (*AccountRotationState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	phase := "resting"
	if len(c.slots[id]) > 0 {
		phase = "draining"
	}
	c.states[id] = AccountRotationState{Phase: phase, Hold: true, Reason: "manual", RestSeconds: seconds}
	return c.stateLocked(id), nil
}

func (c *rotationHoldTestCache) CancelAccountRotationHold(_ context.Context, id int64, _ AccountRotationPolicy) (*AccountRotationState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.states[id] = AccountRotationState{Phase: "ready"}
	return c.stateLocked(id), nil
}

func (c *rotationHoldTestCache) AcquireAccountSlotWithRotationState(ctx context.Context, id int64, max int, requestID string, policy AccountRotationPolicy, sticky bool, pool string) (bool, *AccountRotationState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}
	deadline, _ := ctx.Deadline()
	trace, _ := ctx.Value(rotationHoldTraceKey{}).(string)
	c.calls = append(c.calls, rotationHoldAcquireCall{
		requestID: requestID, accountID: id, max: max, sticky: sticky, policy: policy,
		tracked: HasAccountRotationTracking(ctx), trace: trace, deadline: deadline,
	})
	state := c.stateLocked(id)
	if err := c.acquireErr[id]; err != nil {
		return false, state, err
	}
	if state.Hold || (state.Recovering && state.RetryAfterMS > 0) || state.Phase == "resting" || (state.Phase == "draining" && (policy.Mode != AccountRotationModeSticky || !sticky)) || c.full[id] || (max > 0 && len(c.slots[id]) >= max) {
		return false, state, nil
	}
	if c.slots[id] == nil {
		c.slots[id] = make(map[string]bool)
	}
	c.slots[id][requestID] = true
	return true, state, nil
}

func (c *rotationHoldTestCache) AcquireAccountSlotWithRotation(ctx context.Context, id int64, max int, requestID string, policy AccountRotationPolicy, sticky bool, pool string) (bool, error) {
	c.mu.Lock()
	c.legacyCalls++
	c.mu.Unlock()
	acquired, _, err := c.AcquireAccountSlotWithRotationState(ctx, id, max, requestID, policy, sticky, pool)
	return acquired, err
}

func (c *rotationHoldTestCache) RefreshAccountRotationWaiter(_ context.Context, id int64, requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.waiterErr != nil {
		return c.waiterErr
	}
	if c.waiters[id] == nil {
		c.waiters[id] = make(map[string]bool)
	}
	c.waiters[id][requestID] = true
	return nil
}

func (c *rotationHoldTestCache) RemoveAccountRotationWaiter(_ context.Context, id int64, requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.waiters[id], requestID)
	return nil
}

func (c *rotationHoldTestCache) RefreshAccountRotationSlot(context.Context, int64, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshCalls++
	return nil
}

func (c *rotationHoldTestCache) ReleaseAccountSlot(_ context.Context, id int64, requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.slots[id], requestID)
	return nil
}

func (c *rotationHoldTestCache) setState(id int64, state AccountRotationState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.states[id] = state
}

func (c *rotationHoldTestCache) counts(id int64) (slots, waiters int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.slots[id]), len(c.waiters[id])
}

func (c *rotationHoldTestCache) assertCounts(t *testing.T, id int64, slots, waiters int) {
	t.Helper()
	actualSlots, actualWaiters := c.counts(id)
	require.Equal(t, slots, actualSlots, "real account slots")
	require.Equal(t, waiters, actualWaiters, "independent HOLD waiters")
}

func newRotationHoldConcurrency(enabled bool) (*ConcurrencyService, *rotationHoldTestCache) {
	cache := newRotationHoldTestCache()
	svc := NewConcurrencyService(cache)
	settings := *DefaultAccountRotationSettings()
	settings.Enabled = enabled
	svc.SetAccountRotation(openAIRotationSettingsStub{settings}, nil)
	return svc, cache
}

type rotationHoldAcquireOutcome struct {
	result *AcquireResult
	err    error
}

func requireRotationHoldPending[T any](t *testing.T, done <-chan T) {
	t.Helper()
	select {
	case result := <-done:
		t.Fatalf("HOLD completed before admission reopened: %+v", result)
	default:
	}
}

func TestAccountRotationHoldManualWithAutoOffTracksUnlimitedAndAuxiliary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, cache := newRotationHoldConcurrency(false)
		ctx := svc.AccountRotationContext(t.Context())
		require.True(t, HasAccountRotationTracking(ctx), "manual rest needs real tracking while automatic rotation is off")
		inflight, err := svc.AcquireAccountSlot(ctx, 7, 0)
		require.NoError(t, err)
		require.True(t, inflight.Acquired)
		defer inflight.ReleaseFunc()
		state, err := svc.StartAccountManualRest(ctx, 7, 1)
		require.NoError(t, err)
		require.True(t, state.Hold)
		require.Equal(t, "draining", state.Phase)
		require.Equal(t, 60, state.RestSeconds)

		type auxOutcome struct {
			release func()
			err     error
		}
		done := make(chan auxOutcome, 1)
		go func() {
			release, err := svc.AcquireAccountRotationOnly(ctx, &Account{ID: 7, Concurrency: 1})
			done <- auxOutcome{release, err}
		}()
		synctest.Wait()
		cache.assertCounts(t, 7, 1, 1)
		requireRotationHoldPending(t, done)
		inflight.ReleaseFunc()
		cache.setState(7, AccountRotationState{Phase: "resting", Hold: true, Reason: "manual", RestSeconds: 60})
		time.Sleep(61 * time.Second)
		synctest.Wait()
		cache.assertCounts(t, 7, 0, 1)
		requireRotationHoldPending(t, done)

		_, err = svc.CancelAccountManualRest(ctx, 7)
		require.NoError(t, err)
		time.Sleep(time.Second)
		outcome := <-done
		require.NoError(t, outcome.err)
		require.NotNil(t, outcome.release)
		cache.assertCounts(t, 7, 1, 0)
		outcome.release()
		cache.assertCounts(t, 7, 0, 0)
		for _, call := range cache.calls {
			require.Zero(t, call.policy.WorkSeconds)
			require.Zero(t, call.max, "auxiliary requests retain their unlimited generation admission")
		}
		require.Zero(t, cache.legacyCalls, "slot reservation and HOLD state must come from one atomic operation")
	})
}

func TestAccountRotationHoldStickyRemainsPinnedAfterRestEndsButCapacityIsFull(t *testing.T) {
	for _, reason := range []string{"manual", "early"} {
		t.Run(reason, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, cache := newRotationHoldConcurrency(true)
				cache.setState(7, AccountRotationState{Phase: "resting", Hold: true, Reason: reason})
				ctx := WithAccountRotationSticky(svc.AccountRotationContext(t.Context()), 7)
				done := make(chan rotationHoldAcquireOutcome, 1)
				go func() {
					result, err := svc.AcquireAccountSlot(ctx, 7, 2)
					done <- rotationHoldAcquireOutcome{result, err}
				}()
				synctest.Wait()
				cache.assertCounts(t, 7, 0, 1)
				cache.mu.Lock()
				cache.full[7] = true
				cache.mu.Unlock()
				cache.setState(7, AccountRotationState{Phase: "working"})
				time.Sleep(3 * time.Second)
				synctest.Wait()
				requireRotationHoldPending(t, done)
				cache.assertCounts(t, 7, 0, 1)
				cache.mu.Lock()
				cache.full[7] = false
				cache.mu.Unlock()
				time.Sleep(time.Second)
				outcome := <-done
				require.NoError(t, outcome.err)
				require.True(t, outcome.result.Acquired)
				cache.assertCounts(t, 7, 1, 0)
				outcome.result.ReleaseFunc()
				cache.assertCounts(t, 7, 0, 0)
				for _, call := range cache.calls {
					require.EqualValues(t, 7, call.accountID)
					require.True(t, call.sticky)
					require.Equal(t, outcome.result.RequestID, call.requestID)
				}
			})
		})
	}
}

func TestAccountRotationHoldCancellationAndAdmissionErrorsCleanWaiterWithoutSlot(t *testing.T) {
	for _, failure := range []string{"cancel", "acquire", "waiter"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, cache := newRotationHoldConcurrency(false)
				cache.setState(7, AccountRotationState{Phase: "draining", Hold: true, Reason: "manual"})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan rotationHoldAcquireOutcome, 1)
				go func() {
					result, err := svc.AcquireAccountSlot(ctx, 7, 2)
					done <- rotationHoldAcquireOutcome{result, err}
				}()
				synctest.Wait()
				cache.assertCounts(t, 7, 0, 1)
				expected := errors.New("HOLD cache unavailable")
				if failure == "cancel" {
					expected = context.Canceled
					cancel()
				} else {
					cache.mu.Lock()
					if failure == "acquire" {
						cache.acquireErr[7] = expected
					} else {
						cache.waiterErr = expected
					}
					cache.mu.Unlock()
					time.Sleep(time.Second)
				}
				outcome := <-done
				require.ErrorIs(t, outcome.err, expected)
				require.Nil(t, outcome.result)
				cache.assertCounts(t, 7, 0, 0)
			})
		})
	}
}

func TestAccountRotationHoldQueueContextKeepsProofAndRequestDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, cache := newRotationHoldConcurrency(false)
		cache.setState(7, AccountRotationState{Phase: "resting", Hold: true, Reason: "manual"})
		requestCtx, requestCancel := context.WithTimeout(t.Context(), time.Minute)
		defer requestCancel()
		queueCtx, queueCancel := context.WithTimeout(requestCtx, 2*time.Second)
		defer queueCancel()
		ctx := WithAccountRotationWaitContext(queueCtx, requestCtx)
		ctx = context.WithValue(ctx, rotationHoldTraceKey{}, "queue admission trace")
		ctx = WithAccountRotationSticky(svc.AccountRotationContext(ctx), 7)
		done := make(chan rotationHoldAcquireOutcome, 1)
		go func() {
			result, err := svc.AcquireAccountSlot(ctx, 7, 2)
			done <- rotationHoldAcquireOutcome{result, err}
		}()
		synctest.Wait()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		require.ErrorIs(t, queueCtx.Err(), context.DeadlineExceeded)
		require.NoError(t, requestCtx.Err())
		requireRotationHoldPending(t, done)
		cache.assertCounts(t, 7, 0, 1)
		require.GreaterOrEqual(t, len(cache.calls), 2)
		queueDeadline, _ := queueCtx.Deadline()
		requestDeadline, _ := requestCtx.Deadline()
		for i, call := range cache.calls {
			require.True(t, call.sticky, "queue-only sticky proof must survive switching the cancellation owner")
			require.True(t, call.tracked)
			require.Equal(t, "queue admission trace", call.trace)
			expected := requestDeadline
			if i == 0 {
				expected = queueDeadline
			}
			require.Equal(t, expected, call.deadline)
		}
		time.Sleep(time.Minute)
		outcome := <-done
		require.ErrorIs(t, outcome.err, context.DeadlineExceeded, "the request deadline remains authoritative after HOLD")
		require.Nil(t, outcome.result)
		cache.assertCounts(t, 7, 0, 0)
	})
}

func TestOpenAIAccountRotationHoldSchedulersWaitForSameAccount(t *testing.T) {
	for _, path := range []struct {
		name                    string
		advanced, batch, sticky bool
	}{
		{"advanced", true, true, false}, {"legacy_load", false, true, false}, {"legacy_priority", false, false, false},
		{"advanced_sticky", true, true, true}, {"legacy_load_sticky", false, true, true}, {"legacy_priority_sticky", false, false, true},
	} {
		t.Run(path.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, _ := newOpenAIRotationTestService(t, path.advanced, path.batch, false, AccountRotationModeAggressive)
				cache := newRotationHoldTestCache()
				svc.concurrencyService.cache = cache
				cache.setState(48101, AccountRotationState{Phase: "resting", Hold: true, Reason: "manual"})
				session := ""
				if path.sticky {
					session = "existing-session"
					require.NoError(t, svc.setStickySessionAccountID(t.Context(), nil, session, 48101, time.Hour))
				}
				type outcome struct {
					selection *AccountSelectionResult
					err       error
				}
				done := make(chan outcome, 1)
				go func() {
					selection, err := selectOpenAIRotationTestAccount(svc, t.Context(), session)
					done <- outcome{selection, err}
				}()
				synctest.Wait()
				cache.assertCounts(t, 48101, 0, 1)
				time.Sleep(46 * time.Second)
				synctest.Wait()
				requireRotationHoldPending(t, done)
				cache.assertCounts(t, 48102, 0, 0)
				cache.setState(48101, AccountRotationState{Phase: "ready"})
				time.Sleep(time.Second)
				result := <-done
				require.NoError(t, result.err)
				require.NotNil(t, result.selection)
				require.EqualValues(t, 48101, result.selection.Account.ID)
				require.True(t, result.selection.Acquired)
				require.Nil(t, result.selection.WaitPlan)
				require.True(t, HasAccountRotationTracking(ContextWithSelectionProfitGate(t.Context(), result.selection)))
				result.selection.ReleaseFunc()
				cache.assertCounts(t, 48101, 0, 0)
				for _, call := range cache.calls {
					require.EqualValues(t, 48101, call.accountID, "a HOLD cannot spill into the free fallback account")
					require.Equal(t, path.sticky, call.sticky)
				}
			})
		})
	}
}

func TestGatewayAccountRotationHoldAndGeminiPreserveSelectedAccount(t *testing.T) {
	for _, path := range []string{"gateway_legacy", "gateway_load", "gemini_auxiliary"} {
		t.Run(path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				base, _, accounts := newGatewayRotationFixture(AccountRotationModeAggressive, PlatformAnthropic)
				concurrency, cache := newRotationHoldConcurrency(false)
				base.concurrencyService = concurrency
				base.cfg = &config.Config{Gateway: config.GatewayConfig{Scheduling: config.GatewaySchedulingConfig{LoadBatchEnabled: path == "gateway_load"}}}
				cache.setState(1, AccountRotationState{Phase: "draining", Hold: true, Reason: "manual"})
				type outcome struct {
					accountID int64
					release   func()
					err       error
				}
				done := make(chan outcome, 1)
				go func() {
					if path == "gemini_auxiliary" {
						accounts.accounts[0].Platform, accounts.accounts[1].Platform = PlatformGemini, PlatformGemini
						gemini := &GeminiMessagesCompatService{accountRepo: accounts, concurrencyService: concurrency}
						account, err := gemini.SelectAccountForModel(t.Context(), nil, "", "")
						if err != nil {
							done <- outcome{err: err}
							return
						}
						release, err := concurrency.AcquireAccountRotationOnly(ContextWithAccountRotationSelection(t.Context(), account), account)
						done <- outcome{account.ID, release, err}
						return
					}
					selection, err := base.SelectAccountWithLoadAwareness(t.Context(), nil, "", "", nil, "", 0)
					if err != nil {
						done <- outcome{err: err}
						return
					}
					if !selection.Acquired {
						done <- outcome{err: fmt.Errorf("unexpected concurrency wait plan for held account %d", selection.Account.ID)}
						return
					}
					done <- outcome{selection.Account.ID, selection.ReleaseFunc, nil}
				}()
				synctest.Wait()
				cache.assertCounts(t, 1, 0, 1)
				requireRotationHoldPending(t, done)
				cache.assertCounts(t, 2, 0, 0)
				cache.setState(1, AccountRotationState{Phase: "ready"})
				time.Sleep(time.Second)
				result := <-done
				require.NoError(t, result.err)
				require.EqualValues(t, 1, result.accountID)
				require.NotNil(t, result.release)
				result.release()
				cache.assertCounts(t, 1, 0, 0)
			})
		})
	}
}

func TestAccountRotationHoldSchedulerAdmissionFailureCannotDispatchFallback(t *testing.T) {
	for _, path := range []string{"openai_advanced", "openai_legacy_load", "openai_legacy_priority", "openai_advanced_sticky", "openai_legacy_sticky", "openai_previous_response", "gateway_legacy", "gateway_load"} {
		for _, failure := range []string{"cancel", "cache"} {
			t.Run(path+"/"+failure, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					var selectAccount func(context.Context) (*AccountSelectionResult, error)
					var cache *rotationHoldTestCache
					var heldID, fallbackID int64
					if path == "gateway_legacy" || path == "gateway_load" {
						base, _, _ := newGatewayRotationFixture(AccountRotationModeAggressive, PlatformAnthropic)
						base.concurrencyService, cache = newRotationHoldConcurrency(false)
						base.cfg = &config.Config{Gateway: config.GatewayConfig{Scheduling: config.GatewaySchedulingConfig{LoadBatchEnabled: path == "gateway_load"}}}
						selectAccount = func(ctx context.Context) (*AccountSelectionResult, error) {
							return base.SelectAccountWithLoadAwareness(ctx, nil, "", "", nil, "", 0)
						}
						heldID, fallbackID = 1, 2
					} else {
						svc, _ := newOpenAIRotationTestService(t, path == "openai_advanced" || path == "openai_advanced_sticky", path != "openai_legacy_priority", false, AccountRotationModeAggressive)
						cache = newRotationHoldTestCache()
						svc.concurrencyService.cache = cache
						session := ""
						if path == "openai_advanced_sticky" || path == "openai_legacy_sticky" {
							session = "existing-session"
							require.NoError(t, svc.setStickySessionAccountID(t.Context(), nil, session, 48101, time.Hour))
						}
						selectAccount = func(ctx context.Context) (*AccountSelectionResult, error) {
							return selectOpenAIRotationTestAccount(svc, ctx, session)
						}
						if path == "openai_previous_response" {
							store := svc.getOpenAIWSStateStore()
							require.NoError(t, store.BindResponseAccount(t.Context(), 0, "resp_rotation_hold", 48101, time.Hour))
							selectAccount = func(ctx context.Context) (*AccountSelectionResult, error) {
								return svc.selectAccountByPreviousResponseIDForCapability(ctx, nil, "resp_rotation_hold", "gpt-5.1", nil, OpenAIEndpointCapabilityResponses, false)
							}
						}
						heldID, fallbackID = 48101, 48102
					}
					cache.setState(heldID, AccountRotationState{Phase: "resting", Hold: true, Reason: "manual"})
					type outcome struct {
						selection *AccountSelectionResult
						err       error
					}
					done := make(chan outcome, 1)
					go func() {
						selection, err := selectAccount(ctx)
						done <- outcome{selection, err}
					}()
					synctest.Wait()
					cache.assertCounts(t, heldID, 0, 1)
					expected := errors.New("HOLD admission unavailable")
					if failure == "cancel" {
						expected = context.Canceled
						cancel()
					} else {
						cache.mu.Lock()
						cache.acquireErr[heldID] = expected
						cache.mu.Unlock()
						time.Sleep(time.Second)
					}
					result := <-done
					if result.selection != nil && result.selection.ReleaseFunc != nil {
						defer result.selection.ReleaseFunc()
					}
					require.ErrorIs(t, result.err, expected, "a failed HOLD must stay closed rather than produce another account or a normal wait plan")
					require.Nil(t, result.selection)
					cache.assertCounts(t, heldID, 0, 0)
					cache.assertCounts(t, fallbackID, 0, 0)
				})
			})
		}
	}
}

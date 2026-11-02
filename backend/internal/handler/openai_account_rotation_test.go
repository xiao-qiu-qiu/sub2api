package handler

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type openAIHandlerRotationSettings struct {
	enabled bool
	mode    string
}

func (s openAIHandlerRotationSettings) GetAccountRotationSettings(context.Context) (*service.AccountRotationSettings, error) {
	settings := service.DefaultAccountRotationSettings()
	settings.Enabled, settings.Mode = s.enabled, s.mode
	return settings, nil
}

type openAIHandlerRotationCache struct {
	helperConcurrencyCacheStub
	phases                         map[int64]string
	rotationCalls                  int
	rotationAcquired               int
	stickyProof                    bool
	waitIncrements, waitDecrements int
	onRotationAcquire              func(int64, int) bool
	renewals                       atomic.Int32
}

func (c *openAIHandlerRotationCache) CheckAccountRotation(_ context.Context, id int64, _ service.AccountRotationPolicy) (*service.AccountRotationState, error) {
	return &service.AccountRotationState{Phase: c.phases[id]}, nil
}

func (c *openAIHandlerRotationCache) AcquireAccountSlotWithRotation(_ context.Context, id int64, _ int, _ string, policy service.AccountRotationPolicy, sticky bool, _ string) (bool, error) {
	c.rotationCalls++
	c.stickyProof = sticky
	if c.onRotationAcquire != nil && !c.onRotationAcquire(id, c.rotationCalls) {
		return false, nil
	}
	if c.phases[id] == "resting" || (c.phases[id] == "draining" && (policy.Mode != service.AccountRotationModeSticky || !sticky)) {
		return false, nil
	}
	c.rotationAcquired++
	return true, nil
}

func (c *openAIHandlerRotationCache) RefreshAccountRotationSlot(context.Context, int64, string) error {
	c.renewals.Add(1)
	return nil
}

func (c *openAIHandlerRotationCache) IncrementAccountWaitCount(context.Context, int64, int) (bool, error) {
	c.waitIncrements++
	return true, nil
}

func (c *openAIHandlerRotationCache) DecrementAccountWaitCount(context.Context, int64) error {
	c.waitDecrements++
	return nil
}

func newOpenAIHandlerRotationFixture(enabled bool, mode string) (*OpenAIGatewayHandler, *openAIHandlerRotationCache, *service.ConcurrencyService) {
	cache := &openAIHandlerRotationCache{phases: map[int64]string{}}
	concurrency := service.NewConcurrencyService(cache)
	concurrency.SetAccountRotation(openAIHandlerRotationSettings{enabled, mode}, nil)
	h := &OpenAIGatewayHandler{
		gatewayService:    &service.OpenAIGatewayService{},
		concurrencyHelper: NewConcurrencyHelper(concurrency, SSEPingFormatNone, time.Second),
	}
	return h, cache, concurrency
}

func openAIHandlerRotationWaitSelection(id int64) *service.AccountSelectionResult {
	return &service.AccountSelectionResult{
		Account:  &service.Account{ID: id, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 2},
		WaitPlan: &service.AccountWaitPlan{AccountID: id, MaxConcurrency: 2, MaxWaiting: 2, Timeout: 5 * time.Second},
	}
}

func TestOpenAIAccountRotationHandlerWaitExpiryReturnsReselectAndClearsQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, cache, _ := newOpenAIHandlerRotationFixture(true, service.AccountRotationModeAggressive)
		// The quick attempt and first wait attempt see a full working account.
		// Only the timer's retry crosses the work-window boundary.
		cache.onRotationAcquire = func(id int64, call int) bool {
			if id != 1 {
				return true
			}
			if call >= 3 {
				cache.phases[id] = "draining"
			}
			return false
		}
		c, recorder := newHelperTestContext(http.MethodPost, "/v1/responses")
		started := false
		release, result := h.acquireOpenAIAccountSlot(c, nil, "", openAIHandlerRotationWaitSelection(1), false, &started, zap.NewNop(), nil)
		require.Nil(t, release)
		require.Equal(t, openAISlotAcquireRotationBlocked, result)
		require.Equal(t, 3, cache.rotationCalls)
		require.Zero(t, cache.rotationAcquired)
		require.Equal(t, 1, cache.waitIncrements)
		require.Equal(t, 1, cache.waitDecrements, "abandoned account must leave no waiter behind")
		require.False(t, c.Writer.Written(), "caller still needs to exclude this account and reselect")
		require.Empty(t, recorder.Body.String())

		// Retry the admission with the replacement chosen by the scheduling loop.
		release, result = h.acquireOpenAIAccountSlot(c, nil, "", openAIHandlerRotationWaitSelection(2), false, &started, zap.NewNop(), nil)
		require.Equal(t, openAISlotAcquireOK, result)
		require.NotNil(t, release)
		release()
		require.Equal(t, 1, cache.rotationAcquired)
		require.Equal(t, 1, cache.accountReleaseCalls)
		require.Equal(t, 1, cache.waitIncrements, "replacement should acquire without reentering the closed queue")
	})
}

func TestOpenAIAccountRotationHandlerFastRestCannotUseStickyProof(t *testing.T) {
	h, cache, concurrency := newOpenAIHandlerRotationFixture(true, service.AccountRotationModeSticky)
	cache.phases[1] = "resting"
	c, recorder := newHelperTestContext(http.MethodPost, "/v1/responses")
	ctx := concurrency.AccountRotationContext(service.WithAccountRotationSticky(c.Request.Context(), 1))
	c.Request = c.Request.WithContext(ctx)
	started := false
	release, result := h.acquireOpenAIAccountSlot(c, nil, "old-session", openAIHandlerRotationWaitSelection(1), false, &started, zap.NewNop(), nil)
	require.Nil(t, release)
	require.Equal(t, openAISlotAcquireRotationBlocked, result)
	require.True(t, cache.stickyProof)
	require.Zero(t, cache.rotationAcquired)
	require.Zero(t, cache.waitIncrements, "rest must not become a concurrency wait")
	require.Empty(t, recorder.Body.String())
	require.False(t, c.Writer.Written())
}

func TestOpenAIAccountRotationWSTurnRejectsClosedAccountWith1013(t *testing.T) {
	for _, tc := range []struct {
		name, mode, phase string
		allowed           bool
	}{
		{"aggressive_drain", service.AccountRotationModeAggressive, "draining", false},
		{"aggressive_rest", service.AccountRotationModeAggressive, "resting", false},
		{"sticky_rest", service.AccountRotationModeSticky, "resting", false},
		{"sticky_existing_turn", service.AccountRotationModeSticky, "draining", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, cache, _ := newOpenAIHandlerRotationFixture(true, tc.mode)
			cache.phases[1] = tc.phase
			cache.userSeq = []bool{true}
			c, _ := newHelperTestContext(http.MethodGet, "/v1/responses")
			var pricing openAIWSTurnPricing
			userRelease, accountRelease, err := h.admitOpenAIWSTurnForPricing(c.Request.Context(), c, 202, 3,
				&service.APIKey{ID: 77}, &service.Account{ID: 1, Concurrency: 2}, 2, 2, zap.NewNop(), &pricing)
			if tc.allowed {
				require.NoError(t, err)
				require.NotNil(t, userRelease)
				require.NotNil(t, accountRelease)
				t.Cleanup(userRelease)
				t.Cleanup(accountRelease)
				require.Equal(t, 1, cache.rotationAcquired)
				require.False(t, pricing.currentOr(time.Time{}).IsZero())
			} else {
				var closeErr *service.OpenAIWSClientCloseError
				require.ErrorAs(t, err, &closeErr)
				require.Equal(t, coderws.StatusTryAgainLater, closeErr.StatusCode())
				require.True(t, errors.Is(err, service.ErrAccountRotationUnavailable))
				require.Nil(t, userRelease)
				require.Nil(t, accountRelease)
				require.Zero(t, cache.rotationAcquired, "rejected turn cannot reserve another account slot")
				require.Equal(t, 1, cache.userReleaseCalls)
				require.Equal(t, 1, cache.apiKeyReleaseCalls)
				require.True(t, pricing.currentOr(time.Time{}).IsZero(), "failed admission cannot freeze billable pricing")
			}
			require.True(t, cache.stickyProof, "later WS turns have an existing connection owner")
			require.Equal(t, 1, cache.rotationCalls)
			require.Zero(t, cache.accountAcquireCalls, "WS rotation cannot fall through to legacy slot reservation")
		})
	}
}

func TestOpenAIAccountRotationReleaseOnDoneWaitsForForwardingOwner(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "tracked", false: "off_keeps_cancel_cleanup"}[enabled], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, cache, concurrency := newOpenAIHandlerRotationFixture(enabled, service.AccountRotationModeAggressive)
				cache.accountSeq = []bool{true}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ctx = concurrency.AccountRotationContext(ctx)
				result, err := concurrency.AcquireAccountSlot(ctx, 1, 2)
				require.NoError(t, err)
				require.True(t, result.Acquired)
				release := wrapReleaseOnDone(ctx, result.ReleaseFunc)
				defer release()
				cancel()
				synctest.Wait()
				if enabled {
					require.Zero(t, cache.accountReleaseCalls, "cancel is not proof that upstream forwarding finished")
					time.Sleep(21 * time.Second)
					synctest.Wait()
					require.Positive(t, cache.renewals.Load(), "lease must keep renewing after client cancellation")
				} else {
					require.Equal(t, 1, cache.accountReleaseCalls)
					require.Zero(t, cache.rotationCalls)
				}
				release()
				release()
				require.Equal(t, 1, cache.accountReleaseCalls)
			})
		})
	}
}

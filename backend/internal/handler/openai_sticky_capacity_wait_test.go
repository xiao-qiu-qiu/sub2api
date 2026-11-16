//go:build unit

package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type stickyCapacityWaitCache struct {
	helperConcurrencyCacheStub
	readyAt                   time.Time
	queueFull                 bool
	probes                    []int64
	waitIncrements, waitExits int
}

func (c *stickyCapacityWaitCache) AcquireAccountSlot(_ context.Context, accountID int64, _ int, _ string) (bool, error) {
	c.probes = append(c.probes, accountID)
	return !c.readyAt.IsZero() && !time.Now().Before(c.readyAt), nil
}

func (c *stickyCapacityWaitCache) IncrementAccountWaitCount(context.Context, int64, int) (bool, error) {
	c.waitIncrements++
	return !c.queueFull, nil
}

func (c *stickyCapacityWaitCache) DecrementAccountWaitCount(context.Context, int64) error {
	c.waitExits++
	return nil
}

func TestOpenAIStickyCapacityWaitAdmissionKeepsOwner(t *testing.T) {
	for _, scenario := range []string{"slot within ten seconds", "timeout", "full queue", "client cancellation"} {
		t.Run(scenario, func(t *testing.T) {
			bindings := &stickyMigrationHTTPBindings{owner: 1, revision: "capacity", sessionKey: "openai:capacity"}
			h := newStickyMigrationHTTPHandler(t, bindings, nil)
			cache := &stickyCapacityWaitCache{}
			h.concurrencyHelper = NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second)
			synctest.Test(t, func(t *testing.T) {
				c, recorder := newHelperTestContext(http.MethodPost, "/v1/responses")
				if scenario == "slot within ten seconds" {
					cache.readyAt = time.Now().Add(5 * time.Second)
				}
				if scenario == "full queue" {
					cache.queueFull = true
				}
				if scenario == "client cancellation" {
					ctx, cancel := context.WithCancel(c.Request.Context())
					defer cancel()
					c.Request = c.Request.WithContext(ctx)
					time.AfterFunc(time.Second, cancel)
				}
				started := false
				selection := &service.AccountSelectionResult{
					Account:  &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1},
					WaitPlan: &service.AccountWaitPlan{AccountID: 1, MaxConcurrency: 1, Timeout: 10 * time.Second, MaxWaiting: 2},
				}
				at := time.Now()
				release, result := h.acquireResponsesAccountSlot(c, nil, "capacity", selection, false, &started, zap.NewNop())
				elapsed := time.Since(at)
				owner, err := bindings.GetSessionAccountID(t.Context(), 0, "")
				require.NoError(t, err)
				require.EqualValues(t, 1, owner, "queue admission must never replace the cache owner")
				require.Zero(t, bindings.deletes)
				require.Zero(t, bindings.casCalls)
				require.NotEmpty(t, cache.probes)
				for _, id := range cache.probes {
					require.EqualValues(t, 1, id, "all retries must stay on the existing account")
				}
				require.Equal(t, 1, cache.waitIncrements)
				if scenario == "slot within ten seconds" {
					require.Equal(t, openAISlotAcquireOK, result)
					require.NotNil(t, release)
					require.GreaterOrEqual(t, elapsed, 5*time.Second)
					require.Less(t, elapsed, 10*time.Second)
					require.False(t, c.Writer.Written())
					release()
					require.Equal(t, 1, cache.accountReleaseCalls)
				} else {
					require.Equal(t, openAISlotAcquireFailed, result)
					require.Nil(t, release)
					require.Zero(t, bindings.sets, "failed admission cannot bind another account")
					switch scenario {
					case "timeout":
						require.GreaterOrEqual(t, elapsed, 10*time.Second)
						require.Equal(t, http.StatusTooManyRequests, recorder.Code)
					case "full queue":
						require.Zero(t, elapsed, "the full queue is rejected without an unbounded extra waiter")
						require.Equal(t, http.StatusTooManyRequests, recorder.Code)
						require.Contains(t, recorder.Body.String(), gatewayQueueFullCode)
					case "client cancellation":
						require.Equal(t, time.Second, elapsed)
					}
				}
				if cache.queueFull {
					require.Zero(t, cache.waitExits)
				} else {
					require.Equal(t, 1, cache.waitExits, "every accepted waiter must leave the queue exactly once")
				}
			})
		})
	}
}

func TestAccountWaitPlanBoundedAdmissionCleansUp(t *testing.T) {
	for _, scenario := range []string{"fast slot", "wait then slot", "full queue", "timeout", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cache := &stickyCapacityWaitCache{}
				helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second)
				c, _ := newHelperTestContext(http.MethodPost, "/v1/chat/completions")
				plan := &service.AccountWaitPlan{AccountID: 1, MaxConcurrency: 1, Timeout: 10 * time.Second, MaxWaiting: 1}
				switch scenario {
				case "fast slot":
					cache.readyAt = time.Now()
				case "wait then slot":
					cache.readyAt = time.Now().Add(5 * time.Second)
				case "full queue":
					cache.queueFull = true
				case "cancel":
					ctx, cancel := context.WithCancel(c.Request.Context())
					defer cancel()
					c.Request = c.Request.WithContext(ctx)
					time.AfterFunc(time.Second, cancel)
				}
				started := false
				release, err := helper.AcquireAccountSlotWithWaitPlan(c, plan, false, &started)
				switch scenario {
				case "fast slot", "wait then slot":
					require.NoError(t, err)
					require.NotNil(t, release)
					release()
					require.Equal(t, 1, cache.accountReleaseCalls)
				case "full queue":
					var full *WaitQueueFullError
					require.ErrorAs(t, err, &full)
					require.Nil(t, release)
				case "timeout":
					var timeout *ConcurrencyError
					require.ErrorAs(t, err, &timeout)
					require.True(t, timeout.IsTimeout)
				case "cancel":
					require.True(t, errors.Is(err, context.Canceled))
				}
				if scenario == "fast slot" {
					require.Zero(t, cache.waitIncrements)
					require.Zero(t, cache.waitExits)
				} else {
					require.Equal(t, 1, cache.waitIncrements)
					if scenario == "full queue" {
						require.Zero(t, cache.waitExits)
					} else {
						require.Equal(t, 1, cache.waitExits)
					}
				}
			})
		})
	}
}

func TestAccountWaitPlanRotationExitCleansUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, cache, _ := newOpenAIHandlerRotationFixture(true, service.AccountRotationModeAggressive)
		cache.onRotationAcquire = func(_ int64, call int) bool {
			if call >= 3 {
				cache.phases[1] = "draining"
			}
			return false
		}
		c, _ := newHelperTestContext(http.MethodPost, "/v1/responses")
		started := false
		plan := &service.AccountWaitPlan{AccountID: 1, MaxConcurrency: 1, Timeout: 10 * time.Second, MaxWaiting: 1}
		release, err := h.concurrencyHelper.AcquireAccountSlotWithWaitPlan(c, plan, false, &started)
		require.ErrorIs(t, err, service.ErrAccountRotationUnavailable)
		require.Nil(t, release)
		require.Equal(t, 1, cache.waitIncrements)
		require.Equal(t, 1, cache.waitDecrements, "a rotation retry must not leak the abandoned account's queue entry")
	})
}

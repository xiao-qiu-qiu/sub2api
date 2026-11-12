//go:build unit

package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type accountCooldownHandlerGatewayStub struct {
	queue func(context.Context, int64, string) bool
	wait  func(context.Context) (bool, error)
}

func (g accountCooldownHandlerGatewayStub) QueueOpenAIAccountCooldown(ctx context.Context, id int64, model string) bool {
	return g.queue(ctx, id, model)
}

func (g accountCooldownHandlerGatewayStub) WaitForOpenAIAccountCooldown(ctx context.Context) (bool, error) {
	return g.wait(ctx)
}

// Mirror the combined user/key release contract used by acquireResponsesUserSlot.
func accountCooldownHandlerSlots(t *testing.T) (*openAIOAuthRecoveryUserSlot, *bool, *int) {
	t.Helper()
	held, admissions := false, 0
	slots := &openAIOAuthRecoveryUserSlot{acquireFunc: func() (func(), bool) {
		require.False(t, held, "admission must not overwrite a held user/key lease")
		held = true
		admissions++
		return func() { held = false }, true
	}}
	require.True(t, slots.acquire())
	t.Cleanup(slots.release)
	return slots, &held, &admissions
}

func TestOpenAIAccountCooldownHTTP_SelectionReleasesAndReadmits(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			c, rec := oauthRecoveryHandlerRequest(t, context.Background(), endpoint)
			slots, held, admissions := accountCooldownHandlerSlots(t)
			var waits int
			gateway := accountCooldownHandlerGatewayStub{wait: func(ctx context.Context) (bool, error) {
				waits++
				require.False(t, *held, "wait must own neither user nor key capacity")
				require.Equal(t, c.Request.Context(), ctx)
				return true, nil
			}}
			streamStarted := false
			handled, retry := (&OpenAIGatewayHandler{}).handleOpenAIAccountCooldownSelectionError(c, gateway, slots, fmt.Errorf("selection: %w", service.ErrOpenAIAccountCooldownAdmissionInterrupted), &streamStarted, endpoint == "messages")
			require.True(t, handled)
			require.True(t, retry)
			require.True(t, *held)
			require.Equal(t, 2, *admissions)
			require.Equal(t, 1, waits)
			require.Empty(t, rec.Body.String())
		})
	}
}

func TestOpenAIAccountCooldownHTTP_SelectionCancellationAndOtherErrors(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, rec := oauthRecoveryHandlerRequest(t, ctx, "responses")
			slots, held, admissions := accountCooldownHandlerSlots(t)
			gateway := accountCooldownHandlerGatewayStub{wait: func(ctx context.Context) (bool, error) {
				require.False(t, *held)
				cancel()
				return true, ctx.Err()
			}}
			err := service.ErrNoAvailableAccounts
			if canceled {
				err = service.ErrOpenAIAccountCooldownAdmissionInterrupted
			}
			streamStarted := false
			handled, retry := (&OpenAIGatewayHandler{}).handleOpenAIAccountCooldownSelectionError(c, gateway, slots, err, &streamStarted, false)
			require.Equal(t, canceled, handled)
			require.False(t, retry)
			require.Equal(t, !canceled, *held)
			require.Equal(t, 1, *admissions, "canceled waits never re-admit")
			require.Empty(t, rec.Body.String())
		})
	}
}

func TestOpenAIAccountCooldownHTTP_SelectionReturnsRealUserAndKeyLeases(t *testing.T) {
	helper, cache := newAPIKeyAdmissionHelper(t)
	ctx, cancel := service.WithAPIKeyAdmissionOwner(context.Background())
	defer cancel()
	c, _ := oauthRecoveryHandlerRequest(t, ctx, "responses")
	h := &OpenAIGatewayHandler{concurrencyHelper: helper}
	streamStarted := false
	slots := &openAIOAuthRecoveryUserSlot{acquireFunc: func() (func(), bool) {
		return h.acquireResponsesUserSlot(c, 100, 1, 99, 1, false, &streamStarted, zap.NewNop())
	}}
	require.True(t, slots.acquire())
	defer slots.release()
	gateway := accountCooldownHandlerGatewayStub{wait: func(waitCtx context.Context) (bool, error) {
		users, err := cache.GetUserConcurrency(waitCtx, 100)
		require.NoError(t, err)
		require.Zero(t, users)
		keys, err := cache.(service.APIKeyConcurrencyCache).GetAPIKeyConcurrencyBatch(waitCtx, []int64{99})
		require.NoError(t, err)
		require.Zero(t, keys[99])
		accounts, err := cache.GetAccountConcurrency(waitCtx, 1)
		require.NoError(t, err)
		require.Zero(t, accounts, "selection waits never acquire account capacity")
		return true, nil
	}}
	handled, retry := h.handleOpenAIAccountCooldownSelectionError(c, gateway, slots, service.ErrOpenAIAccountCooldownAdmissionInterrupted, &streamStarted, false)
	require.True(t, handled)
	require.True(t, retry)
	users, err := cache.GetUserConcurrency(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 1, users)
	keys, err := cache.(service.APIKeyConcurrencyCache).GetAPIKeyConcurrencyBatch(ctx, []int64{99})
	require.NoError(t, err)
	require.Equal(t, 1, keys[99])
}

func TestOpenAIAccountCooldownHTTP_ReplayOnlyPreOutputBPS429(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		reason     service.GatewayFailureReason
		result     *service.OpenAIForwardResult
		replaySafe bool
		queued     bool
		wantQueue  bool
		wantRetry  bool
	}{
		{"bps429", 429, service.ExcelBPSRateLimitedReason, nil, true, true, true, true},
		{"ordinary429", 429, "", nil, true, true, false, false},
		{"other_status", 503, service.ExcelBPSRateLimitedReason, nil, true, true, false, false},
		{"observed_result", 429, service.ExcelBPSRateLimitedReason, &service.OpenAIForwardResult{}, true, true, false, false},
		{"semantic_output", 429, service.ExcelBPSRateLimitedReason, nil, false, true, false, false},
		{"budget_exhausted", 429, service.ExcelBPSRateLimitedReason, nil, true, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := oauthRecoveryHandlerRequest(t, context.Background(), "responses")
			slots, held, admissions := accountCooldownHandlerSlots(t)
			queues, waits := 0, 0
			gateway := accountCooldownHandlerGatewayStub{
				queue: func(_ context.Context, id int64, model string) bool {
					queues++
					require.EqualValues(t, 7, id)
					require.Equal(t, "mapped-model", model)
					return tc.queued
				},
				wait: func(context.Context) (bool, error) {
					waits++
					require.False(t, *held)
					return true, nil // also the service contract at the 30s deadline
				},
			}
			streamStarted := false
			handled, retry := (&OpenAIGatewayHandler{}).retryOpenAIAccountCooldownHTTP(c, gateway, slots, &service.Account{ID: 7}, "mapped-model", tc.result, &service.UpstreamFailoverError{StatusCode: tc.status, Reason: tc.reason}, tc.replaySafe, &streamStarted, false)
			require.Equal(t, tc.wantRetry, handled)
			require.Equal(t, tc.wantRetry, retry)
			require.Equal(t, tc.wantQueue, queues == 1)
			require.Equal(t, tc.wantRetry, waits == 1)
			require.True(t, *held)
			wantAdmissions := 1
			if tc.wantRetry {
				wantAdmissions++
			}
			require.Equal(t, wantAdmissions, *admissions)
		})
	}
}

func TestOpenAIAccountCooldownHTTP_StreamWaitPropagatesHeartbeatAndErrorFormat(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			c, rec := oauthRecoveryHandlerRequest(t, context.Background(), endpoint)
			setOpsRequestContext(c, "gpt-5.1", true)
			slots, held, _ := accountCooldownHandlerSlots(t)
			h := &OpenAIGatewayHandler{concurrencyHelper: NewConcurrencyHelper(nil, SSEPingFormatComment, time.Millisecond)}
			gateway := accountCooldownHandlerGatewayStub{wait: func(ctx context.Context) (bool, error) {
				if *held {
					return false, errors.New("generation slots still held")
				}
				timer := time.NewTimer(15 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return true, ctx.Err()
				case <-timer.C:
					return true, nil
				}
			}}
			streamStarted := false
			handled, retry := h.handleOpenAIAccountCooldownSelectionError(c, gateway, slots, service.ErrOpenAIAccountCooldownAdmissionInterrupted, &streamStarted, endpoint == "messages")
			require.True(t, handled)
			require.True(t, retry)
			require.True(t, streamStarted, "the caller must see the heartbeat's committed SSE state")
			require.True(t, gatewayStreamHasOnlyHeartbeats(c))
			require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
			h.handleOpenAIAccountCooldownHTTPError(c, errors.New("wait unavailable"), streamStarted, endpoint == "messages")
			require.Equal(t, http.StatusOK, rec.Code)
			if endpoint == "responses" {
				require.Contains(t, rec.Body.String(), "event: response.failed")
			} else {
				require.Contains(t, rec.Body.String(), "event: error")
			}
			require.Contains(t, rec.Body.String(), "cooldown wait failed")
			require.NotContains(t, rec.Body.String(), "upstream_auth_recovery")
		})
	}
}

func TestOpenAIAccountCooldownHTTP_StreamCancellationJoinsWaitWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _ := oauthRecoveryHandlerRequest(t, ctx, "responses")
	setOpsRequestContext(c, "gpt-5.1", true)
	h := &OpenAIGatewayHandler{concurrencyHelper: NewConcurrencyHelper(nil, SSEPingFormatComment, time.Millisecond)}
	var stopped atomic.Bool
	gateway := accountCooldownHandlerGatewayStub{wait: func(ctx context.Context) (bool, error) {
		defer stopped.Store(true)
		cancel()
		<-ctx.Done()
		return true, ctx.Err()
	}}
	streamStarted := false
	_, err := h.waitForOpenAIAccountCooldownHTTP(c, gateway, &streamStarted)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, stopped.Load(), "the wait worker must settle before the handler exits")
}

type accountCooldownBudgetCaptureContext struct {
	context.Context
	key any
}

func (c *accountCooldownBudgetCaptureContext) Value(key any) any {
	c.key = key
	return c.Context.Value(key)
}

func TestOpenAIAccountCooldownBudgets_HTTPAndWSTurn(t *testing.T) {
	// Capture the opaque context key through the public constructor's lookup.
	// This checks budget ownership without exposing service internals.
	probe := &accountCooldownBudgetCaptureContext{Context: context.Background()}
	service.WithOpenAIAccountCooldownBudget(probe)
	require.NotNil(t, probe.key)
	c, _ := oauthRecoveryHandlerRequest(t, context.Background(), "responses")
	h := &OpenAIGatewayHandler{gatewayService: &service.OpenAIGatewayService{}}
	require.True(t, h.waitForOpenAIOAuthRecoveryBeforeAdmission(c, nil, "gpt-5.1", false, false))
	require.NotNil(t, c.Request.Context().Value(probe.key), "HTTP installs the cooldown budget before concurrency admission")
	turns := newOpenAIOAuthWSTurnRecoveryContext(context.Background())
	first := turns.Value(probe.key)
	require.NotNil(t, first)
	child := context.WithValue(turns, struct{ name string }{"child"}, true)
	turns.beginTurn()
	second := turns.Value(probe.key)
	require.NotNil(t, second)
	require.NotSame(t, first, second, "new response.create owns a fresh 30s allowance")
	require.Same(t, second, child.Value(probe.key), "existing transport contexts must see the current turn's budget")
}

func TestOpenAIAccountCooldownWS_WaitPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _ := oauthRecoveryHandlerRequest(t, ctx, "responses")
	gateway := accountCooldownHandlerGatewayStub{wait: func(waitCtx context.Context) (bool, error) {
		require.Same(t, ctx, waitCtx)
		cancel()
		return true, waitCtx.Err()
	}}
	waited, err := waitForOpenAIAccountCooldownWS(ctx, c, gateway)
	require.True(t, waited)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, strings.Contains(err.Error(), "authentication"))
}

func TestOpenAIAccountCooldownWS_FailoverReleasesSlotsAndChecksReplaySafety(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		reason                service.GatewayFailureReason
		status                int
		safe, queued, handled bool
	}{
		{"bps429", service.ExcelBPSRateLimitedReason, 429, true, true, true},
		{"ordinary429", "", 429, true, true, false},
		{"other_status", service.ExcelBPSRateLimitedReason, 503, true, true, false},
		{"semantic_output", service.ExcelBPSRateLimitedReason, 429, false, true, false},
		{"exhausted_budget", service.ExcelBPSRateLimitedReason, 429, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := oauthRecoveryHandlerRequest(t, context.Background(), "responses")
			held, waited := true, false
			gateway := accountCooldownHandlerGatewayStub{
				queue: func(_ context.Context, id int64, model string) bool {
					require.EqualValues(t, 7, id)
					require.Equal(t, "outbound-model", model)
					return tc.queued
				},
				wait: func(context.Context) (bool, error) {
					require.False(t, held, "account, user and key leases must be released before parking")
					waited = true
					return true, nil
				},
			}
			handled, err := waitForOpenAIAccountCooldownWSFailover(c.Request.Context(), c, gateway, func() { held = false }, 7, "outbound-model", &service.UpstreamFailoverError{StatusCode: tc.status, Reason: tc.reason}, tc.safe)
			require.NoError(t, err)
			require.Equal(t, tc.handled, handled)
			require.Equal(t, tc.handled, waited)
			require.Equal(t, !tc.handled, held)
		})
	}
}

//go:build unit

package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type anthropic413ReviewSettings struct{ service.SettingRepository }

func (*anthropic413ReviewSettings) GetValue(_ context.Context, key string) (string, error) {
	if key == service.SettingKeyRectifierSettings {
		return `{"enabled":true,"thinking_budget_enabled":true,"thinking_signature_enabled":false,"apikey_signature_enabled":false}`, nil
	}
	return "", service.ErrSettingNotFound
}

func (*anthropic413ReviewSettings) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

type anthropic413ReviewRun struct {
	response *httptest.ResponseRecorder
	accounts []int64
	bodies   [][]byte
	events   []*service.OpsUpstreamErrorEvent
	entries  []*service.OpsInsertErrorLogInput
	observed []service.AccountConcurrencyResult
	before   map[int64]service.AutoConfigConcurrencyState
	after    map[int64]service.AutoConfigConcurrencyState
}

func runAnthropic413ReviewRequest(t *testing.T, committed bool, firstStatus int) *anthropic413ReviewRun {
	t.Helper()
	gin.SetMode(gin.TestMode)
	run := &anthropic413ReviewRun{
		response: httptest.NewRecorder(),
		before:   map[int64]service.AutoConfigConcurrencyState{},
		after:    map[int64]service.AutoConfigConcurrencyState{},
	}
	var bodyMu sync.Mutex
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
			return
		}
		bodyMu.Lock()
		run.bodies = append(run.bodies, body)
		attempt := len(run.bodies)
		bodyMu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		if attempt == 1 && firstStatus != 0 {
			writer.WriteHeader(firstStatus)
			if firstStatus == http.StatusBadRequest {
				_, _ = io.WriteString(writer, `{"type":"error","error":{"type":"invalid_request_error","message":"thinking.budget_tokens must be >= 1024"}}`)
			} else {
				_, _ = io.WriteString(writer, `{"type":"error","error":{"type":"api_error","message":"synthetic earlier service failure"}}`)
			}
			return
		}
		writer.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(writer, `{"type":"error","error":{"type":"request_too_large","message":"fixture-must-not-leak"}}`)
	}))
	t.Cleanup(provider.Close)
	target, err := url.Parse(provider.URL)
	require.NoError(t, err)
	upstream := &anthropic413HTTPUpstream{client: provider.Client(), target: target}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Ops.Enabled = true
	groupID := int64(4130)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	policy := service.DefaultOAuthAutoConfig()
	policy.Revision = "synthetic-413-review"
	policy.UpgradeEnabled = true
	policy.UpgradeGroupIDs = []int64{groupID}
	policy.SuccessesPerStep, policy.MaxConcurrency, policy.UpgradeStep, policy.CooldownSeconds = 10, 20, 1, 120
	var accounts []*service.Account
	for _, accountID := range []int64{4131, 4132} {
		accounts = append(accounts, &service.Account{
			ID: accountID, Name: "synthetic-review", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 10,
			Credentials:   map[string]any{"api_key": "fixture-key"},
			AccountGroups: []service.AccountGroup{{AccountID: accountID, GroupID: groupID}},
		})
		state := service.AutoConfigConcurrencyState{
			Revision: policy.Revision, Concurrency: 10, Successes: 3, Required: 10, Maximum: 20, Step: 1,
			PausedUntil: time.Now().Add(-time.Hour),
		}
		run.before[accountID], run.after[accountID] = state, state
	}
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: accounts}, nil, nil, nil, nil)
	settings := service.NewSettingService(&anthropic413ReviewSettings{}, cfg)
	rateLimits := service.NewRateLimitService(&anthropic413AccountRepo{}, nil, cfg, nil, nil)
	gateway := service.NewGatewayService(
		nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, cfg,
		snapshot, nil, nil, rateLimits, nil, nil, upstream,
		nil, nil, nil, nil, nil, settings, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	handler := &GatewayHandler{
		gatewayService: gateway, billingCacheService: billing, cfg: cfg, maxAccountSwitches: 1,
		concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0),
	}
	if firstStatus == http.StatusBadRequest {
		rule := anthropic413PassthroughRule(http.StatusBadRequest, "synthetic repair rule")
		rule.ErrorCodes = []int{http.StatusBadRequest}
		handler.errorPassthroughService = service.NewErrorPassthroughService(&anthropic413PassthroughRepo{rules: []*model.ErrorPassthroughRule{rule}}, nil)
	}
	setupOpsErrorLogTestQueue(t, 4)
	ops := service.NewOpsService(nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
	require.True(t, ops.IsMonitoringEnabled(context.Background()))
	ops.SetAutoConfigObserver(func(result service.AccountConcurrencyResult) {
		run.observed = append(run.observed, result)
		state := run.after[result.AccountID]
		next, _ := service.AdvanceConcurrency(state, state.Concurrency, policy, result, time.Now())
		run.after[result.AccountID] = next
	})
	router := gin.New()
	router.Use(OpsErrorLoggerMiddleware(ops))
	router.POST("/v1/messages", func(requestContext *gin.Context) {
		apiKey := &service.APIKey{ID: 4133, UserID: 4134, GroupID: &groupID, Group: group, Status: service.StatusActive,
			User: &service.User{ID: 4134, Balance: 100, Concurrency: 10}}
		requestContext.Set(string(middleware.ContextKeyAPIKey), apiKey)
		requestContext.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
		if committed {
			requestContext.Header("Content-Type", "text/event-stream")
			_, err := io.WriteString(requestContext.Writer, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
			require.NoError(t, err)
			requestContext.Writer.Flush()
		}
		handler.Messages(requestContext)
		if value, ok := requestContext.Get(service.OpsUpstreamErrorsKey); ok {
			run.events = append(run.events, value.([]*service.OpsUpstreamErrorEvent)...)
		}
	})
	body := fmt.Sprintf(`{"model":"claude-sonnet-4-5","max_tokens":16,"thinking":{"type":"enabled","budget_tokens":1},"stream":%t,"messages":[{"role":"user","content":"synthetic-413-review"}]}`, committed)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	router.ServeHTTP(run.response, req)
	bodyMu.Lock()
	run.bodies = append([][]byte(nil), run.bodies...)
	bodyMu.Unlock()
	upstream.mu.Lock()
	run.accounts = append(run.accounts, upstream.accounts...)
	upstream.mu.Unlock()
	for len(opsErrorLogQueue) > 0 {
		run.entries = append(run.entries, (<-opsErrorLogQueue).entry)
	}
	if committed {
		require.Equal(t, http.StatusOK, run.response.Code)
		require.Equal(t, 1, strings.Count(run.response.Body.String(), "event: error\n"))
		require.Contains(t, run.response.Body.String(), `"type":"request_too_large"`)
	} else {
		require.Equal(t, http.StatusRequestEntityTooLarge, run.response.Code)
		require.JSONEq(t, fmt.Sprintf(`{"type":"error","error":{"type":"request_too_large","message":%q}}`, anthropic413ClientMessage), run.response.Body.String())
	}
	require.NotContains(t, run.response.Body.String(), "fixture-must-not-leak")
	return run
}

func TestAnthropic413ReviewAutoConfigNoCooldown(t *testing.T) {
	for _, boundary := range []string{"json", "sse"} {
		t.Run(boundary, func(t *testing.T) {
			run := runAnthropic413ReviewRequest(t, boundary == "sse", 0)
			require.Len(t, run.accounts, 1)
			require.Len(t, run.entries, 1)
			require.Equal(t, http.StatusRequestEntityTooLarge, run.entries[0].StatusCode)
			require.Empty(t, run.observed, "final 413 must not reach concurrency cooldown")
			require.Equal(t, run.before, run.after)
		})
	}
}

func TestAnthropic413ReviewPreservesEarlierFailures(t *testing.T) {
	for _, boundary := range []string{"json", "sse"} {
		t.Run(boundary, func(t *testing.T) {
			run := runAnthropic413ReviewRequest(t, boundary == "sse", http.StatusServiceUnavailable)
			require.Len(t, run.accounts, 2)
			require.NotEqual(t, run.accounts[0], run.accounts[1])
			require.Len(t, run.observed, 1)
			require.Equal(t, run.accounts[0], run.observed[0].AccountID)
			require.False(t, run.observed[0].Success)
			require.NotNil(t, run.after[run.accounts[0]].LastFailureAt)
			require.True(t, run.after[run.accounts[0]].PausedUntil.After(run.before[run.accounts[0]].PausedUntil))
			require.Equal(t, run.before[run.accounts[1]], run.after[run.accounts[1]])
			require.Len(t, run.events, 1)
			require.Equal(t, http.StatusServiceUnavailable, run.events[0].UpstreamStatusCode)
		})
	}
}

func TestAnthropic413ReviewBudgetRepairMonitoring(t *testing.T) {
	for _, boundary := range []string{"json", "sse"} {
		t.Run(boundary, func(t *testing.T) {
			run := runAnthropic413ReviewRequest(t, boundary == "sse", http.StatusBadRequest)
			require.Len(t, run.accounts, 2)
			require.Equal(t, run.accounts[0], run.accounts[1])
			require.Len(t, run.bodies, 2)
			require.Equal(t, int64(1), gjson.GetBytes(run.bodies[0], "thinking.budget_tokens").Int())
			require.Equal(t, int64(service.BudgetRectifyBudgetTokens), gjson.GetBytes(run.bodies[1], "thinking.budget_tokens").Int())
			require.Len(t, run.events, 1)
			require.Equal(t, "budget_constraint_error", run.events[0].Kind)
			require.Equal(t, http.StatusBadRequest, run.events[0].UpstreamStatusCode)
			require.True(t, run.events[0].SkipMonitoring)
			require.Len(t, run.observed, 1)
			require.False(t, run.observed[0].Success)
			require.Len(t, run.entries, 1, "earlier 400 skip_monitoring must not hide final 413")
			entry := run.entries[0]
			require.Equal(t, http.StatusRequestEntityTooLarge, entry.StatusCode)
			require.Equal(t, "request_too_large", entry.ErrorType)
			require.Equal(t, "request", entry.ErrorPhase)
			require.Equal(t, "client", entry.ErrorOwner)
			require.True(t, entry.IsBusinessLimited)
			require.Nil(t, entry.AccountID)
		})
	}
}

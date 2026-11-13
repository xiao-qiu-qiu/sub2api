//go:build unit

package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// The client history carries a completed tool round trip; a switched account
// must receive exactly the same converted history.
const excelBPSFailoverRequestBody = `{"model":"gpt-6-astra","stream":%t,"input":[` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"list the files"}]},` +
	`{"type":"function_call","call_id":"call_ls","name":"shell","arguments":"{\"cmd\":\"ls\"}"},` +
	`{"type":"function_call_output","call_id":"call_ls","output":"a.txt"}],` +
	`"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}]}`

// excelBPSFailoverUpstream answers in call order and records the account of
// every BPS attempt.
type excelBPSFailoverUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
	bodies     [][]byte
	urls       []string
	answer     func(call int) *http.Response
	onDo       func(call int)
}

func (u *excelBPSFailoverUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	u.mu.Lock()
	call := len(u.accountIDs)
	u.accountIDs = append(u.accountIDs, accountID)
	u.bodies = append(u.bodies, body)
	u.urls = append(u.urls, req.URL.String())
	u.mu.Unlock()
	if u.onDo != nil {
		u.onDo(call)
	}
	return u.answer(call), nil
}

func (u *excelBPSFailoverUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

func excelBPS429(retryAfter string) *http.Response {
	header := http.Header{"Content-Type": {"application/json"}}
	if retryAfter != "" {
		header.Set("Retry-After", retryAfter)
	}
	return &http.Response{StatusCode: http.StatusTooManyRequests, Header: header,
		Body: io.NopCloser(strings.NewReader(`{"error":{"code":"basispoints_rate_limited","message":"PRIVATE_UPSTREAM workspace detail"}}`))}
}

func excelBPSPreGeneration429() *http.Response {
	const wire = "event: response.created\ndata: " +
		`{"type":"response.created","response":{"id":"resp_rejected_attempt","status":"in_progress","output":[]}}` + "\n\n" +
		"event: response.in_progress\ndata: " +
		`{"type":"response.in_progress","response":{"id":"resp_rejected_attempt","status":"in_progress","output":[]}}` + "\n\n" +
		"event: error\ndata: " +
		`{"type":"error","code":"rate_limit_exceeded","message":"PRIVATE_UPSTREAM rate limit"}` + "\n\n"
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{
		"Content-Type": {"text/event-stream"},
		"Retry-After":  {"60"},
	}, Body: io.NopCloser(strings.NewReader(wire))}
}

func excelBPSPaymentRequired(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusPaymentRequired, Header: http.Header{
		"Content-Type": {"application/json"},
	}, Body: io.NopCloser(strings.NewReader(body))}
}

const excelBPSDeactivatedWorkspaceBody = `{"error":{"code":"deactivated_workspace",` +
	`"message":"PRIVATE_UPSTREAM workspace detail access_token=token-1 refresh_token=private-refresh-token chatgpt-1 list the files"}}`

func excelBPSCompleted() *http.Response {
	wire := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bps_failover\",\"status\":\"completed\",\"model\":\"gpt-6-astra\"," +
		"\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
}

// excelBPSFailoverAccountRepo also answers the no-account diagnosis query.
type excelBPSFailoverAccountRepo struct {
	openAIImagesFailoverAccountRepo
}

func (r excelBPSFailoverAccountRepo) SetError(context.Context, int64, string) error {
	return nil
}

type excelBPSFailoverStateAccountRepo struct {
	excelBPSFailoverAccountRepo
	mu              sync.Mutex
	errorAccountIDs []int64
	errorMessages   map[int64]string
	setErrorErr     error
}

func (r *excelBPSFailoverStateAccountRepo) SetError(_ context.Context, id int64, message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errorAccountIDs = append(r.errorAccountIDs, id)
	if r.setErrorErr != nil {
		return r.setErrorErr
	}
	if r.errorMessages == nil {
		r.errorMessages = make(map[int64]string)
	}
	r.errorMessages[id] = message
	return nil
}

func (r *excelBPSFailoverStateAccountRepo) GetByID(ctx context.Context, id int64) (*service.Account, error) {
	account, err := r.excelBPSFailoverAccountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Reflect SetError in admission lookups as the real repository does.
	if message, ok := r.errorMessages[id]; ok {
		account.Status = service.StatusError
		account.ErrorMessage = message
	}
	return account, nil
}

func (r *excelBPSFailoverStateAccountRepo) GetOpenAITurnAdmission(ctx context.Context, id int64) (*service.Account, *service.Account, error) {
	account, err := r.GetByID(ctx, id)
	return account, nil, err
}

func (r *excelBPSFailoverStateAccountRepo) schedulableAccounts(platform string) []service.Account {
	r.mu.Lock()
	defer r.mu.Unlock()
	var accounts []service.Account
	for _, account := range r.accountsForPlatform(platform) {
		if _, unavailable := r.errorMessages[account.ID]; !unavailable {
			accounts = append(accounts, account)
		}
	}
	return accounts
}

func (r *excelBPSFailoverStateAccountRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, _ int64, platform string) ([]service.Account, error) {
	return r.schedulableAccounts(platform), nil
}

func (r *excelBPSFailoverStateAccountRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	return r.schedulableAccounts(platform), nil
}

func (r *excelBPSFailoverStateAccountRepo) ListSchedulableUngroupedByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	return r.schedulableAccounts(platform), nil
}

func (r *excelBPSFailoverStateAccountRepo) errorCalls() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.errorAccountIDs...)
}

func (r excelBPSFailoverAccountRepo) ListModelAvailabilityCandidates(_ context.Context, _ *int64, platforms []string, _ bool) ([]service.Account, error) {
	var out []service.Account
	for _, platform := range platforms {
		out = append(out, r.accountsForPlatform(platform)...)
	}
	return out, nil
}

// loadBatch selects the production default (load-aware) scheduling path;
// otherwise the simple priority path is used.
func newExcelBPSFailoverTestHandler(t *testing.T, upstream service.HTTPUpstream, loadBatch bool, bindings ...service.GatewayCache) *OpenAIGatewayHandler {
	t.Helper()
	handler, _ := newExcelBPSFailoverTestHandlerWithRepo(t, upstream, loadBatch, bindings...)
	return handler
}

func newExcelBPSFailoverTestHandlerWithRepo(t *testing.T, upstream service.HTTPUpstream, loadBatch bool, bindings ...service.GatewayCache) (*OpenAIGatewayHandler, *excelBPSFailoverStateAccountRepo) {
	t.Helper()
	var accounts []service.Account
	for _, id := range []int64{1, 2} {
		accounts = append(accounts, service.Account{
			ID: id, Name: fmt.Sprintf("bps-account-%d", id), Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{3131},
			Priority:    int(id - 1),
			Credentials: map[string]any{"access_token": fmt.Sprintf("token-%d", id), "chatgpt_account_id": fmt.Sprintf("chatgpt-%d", id)},
			Extra:       map[string]any{"openai_excel_bps": true},
		})
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	concurrencyService := service.NewConcurrencyService(nil)
	var schedulingConcurrency *service.ConcurrencyService
	if loadBatch {
		cfg.Gateway.Scheduling.LoadBatchEnabled = true
		schedulingConcurrency = concurrencyService
	}
	var binding service.GatewayCache
	if len(bindings) > 0 {
		binding = bindings[0]
	}
	accountRepo := &excelBPSFailoverStateAccountRepo{
		excelBPSFailoverAccountRepo: excelBPSFailoverAccountRepo{openAIImagesFailoverAccountRepo{accounts: accounts}},
	}
	rateLimitService := service.NewRateLimitService(accountRepo, nil, cfg, nil, nil)
	gatewayService := service.NewOpenAIGatewayService(
		accountRepo,
		nil, nil, nil, nil, nil, nil, binding,
		cfg,
		nil, schedulingConcurrency, service.NewBillingService(cfg, nil), rateLimitService, nil,
		upstream,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	handler := NewOpenAIGatewayHandler(
		gatewayService,
		concurrencyService,
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, nil, nil, nil,
		cfg,
	)
	handler.maxAccountSwitches = 10
	return handler, accountRepo
}

func newExcelBPSFailoverTestContext(ctx context.Context, stream bool) (*gin.Context, *httptest.ResponseRecorder) {
	groupID := int64(3131)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(fmt.Sprintf(excelBPSFailoverRequestBody, stream))))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:               99,
		GroupID:          &groupID,
		Group:            &service.Group{ID: groupID, Platform: service.PlatformOpenAI},
		User:             &service.User{ID: 100},
		AllowLongContext: true,
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})
	return c, rec
}

func TestOpenAIResponsesExcelBPSDeactivatedWorkspaceFailsOverAndBlocksAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		loadBatch, stream bool
	}{{false, false}, {false, true}, {true, false}, {true, true}} {
		t.Run(fmt.Sprintf("load_batch=%t/stream=%t", tc.loadBatch, tc.stream), func(t *testing.T) {
			upstream := &excelBPSFailoverUpstream{answer: func(call int) *http.Response {
				if call == 0 {
					return excelBPSPaymentRequired(excelBPSDeactivatedWorkspaceBody)
				}
				return excelBPSCompleted()
			}}
			handler, repo := newExcelBPSFailoverTestHandlerWithRepo(t, upstream, tc.loadBatch, &oauthRecoveryHandlerBindings{})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, rec := newExcelBPSFailoverTestContext(ctx, tc.stream)
			c.Request.Header.Set("session_id", "bps-deactivated-workspace")

			handler.Responses(c)

			calls := upstream.calls()
			require.Len(t, calls, 2, "an initial access-state rejection must use the healthy backup")
			require.NotEqual(t, calls[0], calls[1])
			require.Equal(t, []int64{calls[0]}, repo.errorCalls(), "only the deactivated account must be marked unavailable")
			blocked, err := repo.GetByID(ctx, calls[0])
			require.NoError(t, err)
			require.Equal(t, service.StatusError, blocked.Status)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "done")
			for _, private := range []string{"PRIVATE_UPSTREAM", "deactivated_workspace", "access_token", "refresh_token", "token-1", "token-2", "private-refresh-token", "chatgpt-1"} {
				require.NotContains(t, rec.Body.String(), private)
			}
			for _, private := range []string{"PRIVATE_UPSTREAM", "access_token", "refresh_token", "token-1", "private-refresh-token", "chatgpt-1", "list the files"} {
				require.NotContains(t, blocked.ErrorMessage, private)
			}
			for _, url := range upstream.urls {
				require.Equal(t, basispoints.ResponsesURL, url, "failover must stay on the BPS route")
			}
			history := gjson.GetBytes(upstream.bodies[0], "input").Raw
			require.NotEmpty(t, history)
			for _, item := range []string{"list the files", "call_ls", "shell", "a.txt"} {
				require.Contains(t, history, item)
			}
			require.JSONEq(t, history, gjson.GetBytes(upstream.bodies[1], "input").Raw,
				"messages, tool calls and tool outputs must survive the account switch")
			rawEvents, ok := c.Get(service.OpsUpstreamErrorsKey)
			require.True(t, ok)
			events, ok := rawEvents.([]*service.OpsUpstreamErrorEvent)
			require.True(t, ok)
			require.Len(t, events, 1)
			require.Equal(t, "failover", events[0].Kind)
			require.Equal(t, calls[0], events[0].AccountID)
			require.Equal(t, http.StatusPaymentRequired, events[0].UpstreamStatusCode)

			// A fresh session cannot pass merely because the first request rebound
			// its sticky session to the backup. The first account remains unavailable.
			for _, sessionID := range []string{"bps-deactivated-workspace", "bps-fresh-session"} {
				c, rec = newExcelBPSFailoverTestContext(ctx, tc.stream)
				c.Request.Header.Set("session_id", sessionID)
				handler.Responses(c)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "done")
			}
			require.Equal(t, []int64{calls[0], calls[1], calls[1], calls[1]}, upstream.calls(),
				"neither sticky nor fresh requests may reuse the deactivated account")
			require.NoError(t, ctx.Err())
		})
	}
}

func TestOpenAIResponsesExcelBPSDeactivatedWorkspaceSetErrorFailureKeepsAccountBlocked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		loadBatch, stream bool
	}{{false, false}, {false, true}, {true, false}, {true, true}} {
		t.Run(fmt.Sprintf("load_batch=%t/stream=%t", tc.loadBatch, tc.stream), func(t *testing.T) {
			upstream := &excelBPSFailoverUpstream{answer: func(call int) *http.Response {
				if call == 1 {
					return excelBPSPaymentRequired(excelBPSDeactivatedWorkspaceBody)
				}
				return excelBPSCompleted()
			}}
			handler, repo := newExcelBPSFailoverTestHandlerWithRepo(t, upstream, tc.loadBatch, &oauthRecoveryHandlerBindings{})
			repo.setErrorErr = errors.New("SetError persistence unavailable")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			// Bind an older session to the account before it becomes unavailable,
			// so retrying that session exercises stale sticky selection as well.
			c, rec := newExcelBPSFailoverTestContext(ctx, tc.stream)
			c.Request.Header.Set("session_id", "bps-before-persistence-failure")
			handler.Responses(c)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Len(t, upstream.calls(), 1)

			c, rec = newExcelBPSFailoverTestContext(ctx, tc.stream)
			c.Request.Header.Set("session_id", "bps-persistence-failure")
			handler.Responses(c)

			calls := upstream.calls()
			require.Len(t, calls, 3, "the initial rejection must fail over even when SetError fails")
			require.Equal(t, calls[0], calls[1], "the older session must be bound to the account that later fails")
			require.NotEqual(t, calls[1], calls[2])
			require.Equal(t, []int64{calls[1]}, repo.errorCalls())
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "done")
			require.NotContains(t, rec.Body.String(), "PRIVATE_UPSTREAM")
			require.NotContains(t, rec.Body.String(), repo.setErrorErr.Error())

			// No persistent error or cooldown may hide the scheduling regression:
			// only the runtime quarantine can exclude this still-active snapshot.
			stale, err := repo.GetByID(ctx, calls[1])
			require.NoError(t, err)
			require.Equal(t, service.StatusActive, stale.Status)
			require.True(t, stale.IsSchedulable())
			require.Empty(t, stale.ErrorMessage)
			require.Nil(t, stale.TempUnschedulableUntil)
			require.Nil(t, stale.RateLimitResetAt)
			require.Nil(t, stale.OverloadUntil)
			require.Len(t, repo.schedulableAccounts(service.PlatformOpenAI), 2)

			for _, sessionID := range []string{"bps-before-persistence-failure", "bps-persistence-failure", "bps-after-persistence-failure"} {
				c, rec = newExcelBPSFailoverTestContext(ctx, tc.stream)
				c.Request.Header.Set("session_id", sessionID)
				handler.Responses(c)
				require.Equal(t, http.StatusOK, rec.Code, "session=%s: %s", sessionID, rec.Body.String())
				require.Contains(t, rec.Body.String(), "done")
			}
			require.Equal(t, []int64{calls[1], calls[1], calls[2], calls[2], calls[2], calls[2]}, upstream.calls(),
				"older sticky, rebound sticky and fresh sessions must all avoid the account after a failed SetError write")
			require.Equal(t, []int64{calls[1]}, repo.errorCalls(), "the blocked account must never reach upstream again")
			require.NoError(t, ctx.Err())
		})
	}
}

func TestOpenAIResponsesExcelBPSDeactivatedWorkspaceOnEveryAccountReturnsSafe502(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		loadBatch, stream bool
	}{{false, false}, {false, true}, {true, false}, {true, true}} {
		t.Run(fmt.Sprintf("load_batch=%t/stream=%t", tc.loadBatch, tc.stream), func(t *testing.T) {
			upstream := &excelBPSFailoverUpstream{answer: func(int) *http.Response {
				return excelBPSPaymentRequired(excelBPSDeactivatedWorkspaceBody)
			}}
			handler, repo := newExcelBPSFailoverTestHandlerWithRepo(t, upstream, tc.loadBatch)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, rec := newExcelBPSFailoverTestContext(ctx, tc.stream)

			handler.Responses(c)

			calls := upstream.calls()
			require.Len(t, calls, 2, "each rejected account may be tried only once even with a larger switch budget")
			require.NotEqual(t, calls[0], calls[1])
			require.Equal(t, calls, repo.errorCalls(), "both deactivated accounts must be isolated")
			require.NoError(t, ctx.Err(), "pool exhaustion must finish without waiting for client cancellation")
			require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
			require.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String())
			require.Equal(t, "Upstream access is temporarily unavailable, please retry later",
				gjson.Get(rec.Body.String(), "error.message").String())
			for _, private := range []string{"PRIVATE_UPSTREAM", "deactivated_workspace", "workspace detail", "list the files", "access_token", "refresh_token", "token-1", "token-2", "private-refresh-token", "chatgpt-1"} {
				require.NotContains(t, rec.Body.String(), private)
			}
			require.NotContains(t, rec.Body.String(), "event:", "a stream request rejected before output must still receive an HTTP error")
		})
	}
}

func TestOpenAIResponsesExcelBPSPaymentRequiredDoesNotFailOverOrBlockAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		loadBatch, stream bool
	}{{false, false}, {false, true}, {true, false}, {true, true}} {
		t.Run(fmt.Sprintf("load_batch=%t/stream=%t", tc.loadBatch, tc.stream), func(t *testing.T) {
			upstream := &excelBPSFailoverUpstream{answer: func(call int) *http.Response {
				if call == 0 {
					return excelBPSPaymentRequired(`{"error":{"message":"Payment Required"}}`)
				}
				return excelBPSCompleted()
			}}
			handler, repo := newExcelBPSFailoverTestHandlerWithRepo(t, upstream, tc.loadBatch, &oauthRecoveryHandlerBindings{})
			c, rec := newExcelBPSFailoverTestContext(context.Background(), tc.stream)
			c.Request.Header.Set("session_id", "bps-payment-required")

			handler.Responses(c)

			calls := upstream.calls()
			require.Len(t, calls, 1, "a plain 402 is not evidence that the workspace is deactivated")
			require.Empty(t, repo.errorCalls(), "a generic payment error must not persist an account error")
			require.Equal(t, http.StatusPaymentRequired, rec.Code, rec.Body.String())
			require.Equal(t, "basispoints_upstream_error", gjson.Get(rec.Body.String(), "error.code").String())
			require.Equal(t, "Excel BPS rejected this request; account scheduling was not changed",
				gjson.Get(rec.Body.String(), "error.message").String())
			for _, sessionID := range []string{"bps-payment-required", "bps-payment-fresh-session"} {
				c, rec = newExcelBPSFailoverTestContext(context.Background(), tc.stream)
				c.Request.Header.Set("session_id", sessionID)
				handler.Responses(c)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "done")
			}
			require.Equal(t, []int64{calls[0], calls[0], calls[0]}, upstream.calls(),
				"a generic payment error must not isolate the account or prevent new sessions from selecting it")
			require.Empty(t, repo.errorCalls())
		})
	}
}

func TestOpenAIResponsesExcelBPSPreGeneration429FailsOverImmediately(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, loadBatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("load_batch=%t", loadBatch), func(t *testing.T) {
			upstream := &excelBPSFailoverUpstream{answer: func(call int) *http.Response {
				if call == 0 {
					return excelBPSPreGeneration429()
				}
				return excelBPSCompleted()
			}}
			handler := newExcelBPSFailoverTestHandler(t, upstream, loadBatch, &oauthRecoveryHandlerBindings{})
			// A cooldown wait would outlive this request and prevent completion.
			// This checks the branch taken, without an elapsed-time assertion.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, rec := newExcelBPSFailoverTestContext(ctx, true)
			c.Request.Header.Set("session_id", "bps-stream-rejection-failover")

			handler.Responses(c)

			require.NoError(t, ctx.Err(), "the backup must finish before a cooldown wait can expire")
			require.Equal(t, []int64{1, 2}, upstream.calls(), "the rejected account must be excluded immediately")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
			require.Contains(t, rec.Body.String(), "resp_bps_failover")
			require.Contains(t, rec.Body.String(), "done")
			require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.completed\n"))
			require.NotContains(t, rec.Body.String(), "resp_rejected_attempt")
			require.NotContains(t, rec.Body.String(), "rate_limit_exceeded")
			require.NotContains(t, rec.Body.String(), "PRIVATE_UPSTREAM")
			for _, url := range upstream.urls {
				require.Equal(t, basispoints.ResponsesURL, url)
			}
			require.NotEmpty(t, gjson.GetBytes(upstream.bodies[0], "input").Raw)
			require.Equal(t, gjson.GetBytes(upstream.bodies[0], "input").Raw, gjson.GetBytes(upstream.bodies[1], "input").Raw,
				"the replacement must receive the same message and tool history")
		})
	}
}

func TestOpenAIExcelBPSPreGeneration429AfterHeartbeatFailsOver(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		endpoint, terminal string
	}{
		{"responses", "event: response.completed\n"},
		{"chat/completions", "data: [DONE]"},
	} {
		for _, loadBatch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/load_batch=%t", tc.endpoint, loadBatch), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				c, rec := newExcelBPSFailoverTestContext(ctx, true)
				c.Request.URL.Path = "/v1/" + tc.endpoint
				c.Request.Header.Set("session_id", "bps-heartbeat-rejection-failover")
				if tc.endpoint != "responses" {
					const body = `{"model":"gpt-6-astra","stream":true,"messages":[{"role":"user","content":"preserve this input"}],"max_tokens":20}`
					c.Request.Body = io.NopCloser(strings.NewReader(body))
					c.Request.ContentLength = int64(len(body))
				}
				heartbeatWritten := false
				pingHelper := NewConcurrencyHelper(nil, SSEPingFormatComment, 15*time.Second)
				upstream := &excelBPSFailoverUpstream{
					onDo: func(call int) {
						if call == 0 {
							// Emit after the handler's pre-forward snapshot. Count the
							// gateway heartbeat only, exactly as its real writer does.
							require.NoError(t, pingHelper.writeWaitPing(c, &heartbeatWritten))
							require.True(t, gatewayStreamHasOnlyHeartbeats(c))
						}
					},
					answer: func(call int) *http.Response {
						if call == 0 {
							return excelBPSPreGeneration429()
						}
						success := excelBPSCompleted()
						success.Body = io.NopCloser(io.MultiReader(
							strings.NewReader("event: response.output_text.delta\ndata: "+`{"type":"response.output_text.delta","delta":"done"}`+"\n\n"),
							success.Body,
						))
						return success
					},
				}
				handler := newExcelBPSFailoverTestHandler(t, upstream, loadBatch, &oauthRecoveryHandlerBindings{})

				runOAuthRecoveryHandler(handler, c, tc.endpoint)

				require.NoError(t, ctx.Err(), "an already flushed comment must not trigger a cooldown wait")
				require.True(t, heartbeatWritten)
				require.True(t, rec.Flushed)
				require.Equal(t, []int64{1, 2}, upstream.calls(), rec.Body.String())
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.True(t, strings.HasPrefix(rec.Body.String(), string(SSEPingFormatComment)))
				require.Contains(t, rec.Body.String(), "done")
				require.Contains(t, rec.Body.String(), tc.terminal)
				require.NotContains(t, rec.Body.String(), "resp_rejected_attempt")
				require.NotContains(t, rec.Body.String(), "rate_limit_exceeded")
				require.NotContains(t, rec.Body.String(), "PRIVATE_UPSTREAM")
				for _, url := range upstream.urls {
					require.Equal(t, basispoints.ResponsesURL, url)
				}
			})
		}
	}
}

// A bound request waits for the short BPS cooldown and retries its original
// account. A healthy backup must not be borrowed during that wait.
func TestOpenAIResponsesExcelBPS429WaitsForRecoveryBeforeRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		loadBatch, stream bool
	}{{false, false}, {false, true}, {true, false}, {true, true}} {
		stream := tc.stream
		t.Run(fmt.Sprintf("load_batch=%t/stream=%t", tc.loadBatch, stream), func(t *testing.T) {
			upstream := &excelBPSFailoverUpstream{answer: func(call int) *http.Response {
				if call == 0 {
					return excelBPS429("1")
				}
				return excelBPSCompleted()
			}}
			handler := newExcelBPSFailoverTestHandler(t, upstream, tc.loadBatch, &oauthRecoveryHandlerBindings{})

			c, rec := newExcelBPSFailoverTestContext(context.Background(), stream)
			c.Request.Header.Set("session_id", "bps-cooldown-recovery")
			started := time.Now()
			handler.Responses(c)
			require.GreaterOrEqual(t, time.Since(started), time.Second, "retry must wait for the original cooldown")

			calls := upstream.calls()
			require.Len(t, calls, 2)
			throttled := calls[0]
			require.Equal(t, throttled, calls[1], "the original sticky account must be retried after recovery")
			require.Equal(t, http.StatusOK, rec.Code)
			require.Contains(t, rec.Body.String(), "done")
			require.NotContains(t, rec.Body.String(), "PRIVATE_UPSTREAM")
			require.NotContains(t, rec.Body.String(), "basispoints_rate_limited")
			for _, url := range upstream.urls {
				require.Equal(t, basispoints.ResponsesURL, url)
			}
			require.NotEmpty(t, gjson.GetBytes(upstream.bodies[0], "input").Raw)
			require.Equal(t, gjson.GetBytes(upstream.bodies[0], "input").Raw, gjson.GetBytes(upstream.bodies[1], "input").Raw,
				"messages, tool calls and tool outputs must survive the account switch unchanged")
			rawEvents, ok := c.Get(service.OpsUpstreamErrorsKey)
			require.True(t, ok)
			events, ok := rawEvents.([]*service.OpsUpstreamErrorEvent)
			require.True(t, ok)
			require.Len(t, events, 1)
			require.Equal(t, "failover", events[0].Kind)
			require.Equal(t, throttled, events[0].AccountID)
			require.Equal(t, http.StatusTooManyRequests, events[0].UpstreamStatusCode)

			c, rec = newExcelBPSFailoverTestContext(context.Background(), stream)
			c.Request.Header.Set("session_id", "bps-cooldown-recovery")
			handler.Responses(c)

			require.Equal(t, []int64{throttled, throttled, throttled}, upstream.calls(), "a recovered sticky account remains the owner of new requests")
			require.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

func TestOpenAIResponsesExcelBPS429OnEveryAccountReturnsRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Exercise the real 30s budget once. The remaining table checks existing
	// failover/error formatting with that exhausted budget, avoiding a fresh
	// 30s sleep for every Retry-After variant.
	upstream := &excelBPSFailoverUpstream{answer: func(int) *http.Response { return excelBPS429("60") }}
	handler := newExcelBPSFailoverTestHandler(t, upstream, false)
	c, rec := newExcelBPSFailoverTestContext(context.Background(), false)
	started := time.Now()
	handler.Responses(c)
	require.GreaterOrEqual(t, time.Since(started), service.OpenAIAccountCooldownWait)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	require.Len(t, upstream.calls(), 2, "the backup is available only after the wait budget")
	probe := &accountCooldownBudgetCaptureContext{Context: context.Background()}
	service.WithOpenAIAccountCooldownBudget(probe)
	budgetCtx := context.WithValue(context.Background(), probe.key, c.Request.Context().Value(probe.key))
	for _, tc := range []struct {
		name, retryAfter, wantRetryAfter string
		stream, loadBatch                bool
	}{
		{name: "json", retryAfter: "30", wantRetryAfter: "30"},
		{name: "stream request", retryAfter: "30", wantRetryAfter: "30", stream: true},
		{name: "load-aware scheduling", retryAfter: "30", wantRetryAfter: "30", loadBatch: true},
		{name: "load-aware stream request", retryAfter: "30", wantRetryAfter: "30", stream: true, loadBatch: true},
		{name: "invalid retry after", retryAfter: "soon"},
		{name: "retry after beyond a week", retryAfter: "999999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &excelBPSFailoverUpstream{answer: func(int) *http.Response { return excelBPS429(tc.retryAfter) }}
			handler := newExcelBPSFailoverTestHandler(t, upstream, tc.loadBatch)

			c, rec := newExcelBPSFailoverTestContext(budgetCtx, tc.stream)
			handler.Responses(c)

			calls := upstream.calls()
			require.Len(t, calls, 2)
			require.NotEqual(t, calls[0], calls[1])
			require.Equal(t, http.StatusTooManyRequests, rec.Code, "exhausting upstream 429s must preserve the rate limit")
			require.Equal(t, "rate_limit_error", gjson.Get(rec.Body.String(), "error.type").String())
			require.Equal(t, "basispoints_rate_limited", gjson.Get(rec.Body.String(), "error.code").String())
			require.NotContains(t, rec.Body.String(), "PRIVATE_UPSTREAM")
			require.NotContains(t, rec.Body.String(), "workspace")
			require.Equal(t, tc.wantRetryAfter, rec.Header().Get("Retry-After"))

			// After its wait allowance is exhausted, a cooling pool is answered
			// without an additional upstream call or another cooldown wait.
			c, rec = newExcelBPSFailoverTestContext(budgetCtx, tc.stream)
			handler.Responses(c)

			require.Len(t, upstream.calls(), 2)
			require.Equal(t, http.StatusTooManyRequests, rec.Code, "BPS cooldowns must remain a rate limit on subsequent requests")
			require.Equal(t, "rate_limit_error", gjson.Get(rec.Body.String(), "error.type").String())
			require.Contains(t, rec.Body.String(), "rate-limited")
		})
	}
}

func TestOpenAIResponsesExcelBPSSelectionFailureClassification(t *testing.T) {
	fallback := noAccountErrorClassification{
		Status: http.StatusServiceUnavailable, ErrType: "api_error", Message: "Service temporarily unavailable",
		Diagnosis: service.ModelAvailabilityDiagnosis{
			Complete: true, HasAccountsInPool: true, HasModelSupport: true,
			ConfiguredAccounts: 2, ModelSupportedAccounts: 2,
		},
	}
	modelNotFound := noAccountErrorClassification{
		Status: http.StatusNotFound, ErrType: "model_not_found", Code: "model_not_found", ModelNotFound: true,
	}
	modelCapacity := fallback
	modelCapacity.Code = "model_capacity_exhausted"
	bpsEmpty := fmt.Errorf("%w (pool=2, filtered: excel_bps_rate_limited=2)", service.ErrNoAvailableAccounts)
	for _, tc := range []struct {
		name     string
		err      error
		fallback noAccountErrorClassification
		status   int
		code     string
	}{
		{name: "BPS cooldown overrides generic empty selection", err: bpsEmpty, fallback: fallback, status: 429},
		{name: "ordinary empty selection", err: service.ErrNoAvailableAccounts, fallback: fallback, status: 503, code: "no_available_accounts"},
		{name: "persisted quota diagnosis", err: bpsEmpty, fallback: modelCapacity, status: 503, code: "model_capacity_exhausted"},
		{name: "unsupported model", err: bpsEmpty, fallback: modelNotFound, status: 404, code: "model_not_found"},
		{name: "repository failure", err: errors.New("database unavailable"), fallback: fallback, status: 503},
		{name: "zero BPS cooldowns", err: fmt.Errorf("%w (excel_bps_rate_limited=0)", service.ErrNoAvailableAccounts), fallback: fallback, status: 503, code: "no_available_accounts"},
		{name: "other rate limit", err: fmt.Errorf("%w (model_rate_limited=2)", service.ErrNoAvailableAccounts), fallback: fallback, status: 503, code: "no_available_accounts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyOpenAIResponsesSelectionFailure(tc.err, tc.fallback)
			require.Equal(t, tc.status, got.Status)
			require.Equal(t, tc.code, got.Code)
			require.Equal(t, tc.fallback.Diagnosis, got.Diagnosis)
			if tc.status == http.StatusTooManyRequests {
				require.Equal(t, "rate_limit_error", got.ErrType)
				require.Contains(t, got.Message, "rate-limited")
			} else {
				require.Equal(t, tc.fallback.ErrType, got.ErrType)
				require.Equal(t, tc.fallback.Message, got.Message)
			}
		})
	}
}

func TestOpenAIResponsesExcelBPS429StopsWhenClientCancels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := &excelBPSFailoverUpstream{
		answer: func(int) *http.Response { return excelBPS429("30") },
		onDo:   func(int) { cancel() },
	}
	handler := newExcelBPSFailoverTestHandler(t, upstream, false)
	c, rec := newExcelBPSFailoverTestContext(ctx, false)

	handler.Responses(c)

	require.Len(t, upstream.calls(), 1, "a gone client must not be replayed on another account")
	require.Equal(t, statusClientClosedRequest, c.Writer.Status())
	require.Zero(t, rec.Body.Len())
}

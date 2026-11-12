package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type manualTestAccountRepo struct {
	service.AccountRepository
	account *service.Account
}

func (r *manualTestAccountRepo) GetByID(context.Context, int64) (*service.Account, error) {
	return r.account, nil
}

func (r *manualTestAccountRepo) GetOpenAITurnAdmission(context.Context, int64) (*service.Account, *service.Account, error) {
	return r.account, nil, nil
}

type manualTestBPSUpstream struct {
	calls int
	nonce string
}

func (u *manualTestBPSUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if req.URL.Host != "bps.openai.com" {
		return nil, fmt.Errorf("unexpected upstream: %s", req.URL.Host)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.calls++
	if u.nonce == "" {
		u.nonce = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`).FindString(string(body))
	}
	answer := "OK"
	if u.nonce != "" {
		answer = u.nonce
	}
	output := []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": answer}}}}
	if u.nonce != "" && u.calls == 2 {
		envelope, _ := json.Marshal(map[string]any{"name": "sub2api_probe_echo", "arguments": map[string]any{"nonce": u.nonce}})
		output = []any{map[string]any{"type": "function_call", "id": "fc_manual", "call_id": "call_manual",
			"name": "run_officejs", "status": "completed", "arguments": map[string]any{"code": string(envelope), "summary": "Echo nonce"}}}
	}
	event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
		"id": fmt.Sprintf("resp_manual_%d", u.calls), "status": "completed", "model": "gpt-6-astra", "output": output,
	}})
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader("event: response.completed\ndata: " + string(event) + "\n\n"))}, nil
}

func (u *manualTestBPSUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func TestAccountHandlerManualTestsBypassLocalAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"connection", "pelican", service.AccountTestModeBPSTools} {
		t.Run(mode, func(t *testing.T) {
			future := time.Now().Add(time.Hour)
			account := &service.Account{ID: 300, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Status: service.StatusActive, Schedulable: false, Concurrency: 1, RateLimitResetAt: &future,
				TempUnschedulableUntil: &future, OverloadUntil: &future,
				Credentials: map[string]any{"access_token": "fixture-token", "chatgpt_account_id": "fixture-account"},
				Extra:       map[string]any{"openai_excel_bps": true, "openai_passthrough": true}}
			repo := &manualTestAccountRepo{account: account}
			upstream := &manualTestBPSUpstream{}
			cfg := &config.Config{}
			gateway := service.NewOpenAIGatewayService(
				repo, nil, nil, nil, nil, nil, nil, nil, cfg,
				nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil,
			)
			t.Cleanup(gateway.StopOpenAICodexTicketHarvester)
			_, err := gateway.AdmitOpenAITurn(context.Background(), nil, account, "gpt-6-astra")
			require.ErrorContains(t, err, "account_ineligible", "business requests must still be rejected")
			testService := service.NewAccountTestService(repo, nil, nil, nil, nil, upstream, cfg, nil)
			testService.SetOpenAIGatewayService(gateway)
			handler := &AccountHandler{accountTestService: testService}
			router := gin.New()
			path := "/api/v1/admin/accounts/300/test"
			if mode == "pelican" {
				path = "/api/v1/admin/accounts/300/pelican-test"
				router.POST("/api/v1/admin/accounts/:id/pelican-test", handler.PelicanTest)
			} else {
				router.POST("/api/v1/admin/accounts/:id/test", handler.Test)
			}
			payload, err := json.Marshal(map[string]any{"model_id": "gpt-6-astra", "prompt": "Reply OK", "mode": mode})
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(payload)))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)
			require.Equal(t, http.StatusOK, recorder.Code)
			require.Contains(t, recorder.Body.String(), `"type":"test_complete"`)
			wantCalls := 1
			if mode == service.AccountTestModeBPSTools {
				wantCalls = 3
			}
			require.Equal(t, wantCalls, upstream.calls)
			require.False(t, account.Schedulable, "testing must not change the stored scheduling switch")
			require.Equal(t, future, *account.RateLimitResetAt)
		})
	}
}

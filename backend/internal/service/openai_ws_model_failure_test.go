//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const wsModelFailureError = `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"model_not_found","message":"The 'gpt-5.1' model is not supported when using Codex with a ChatGPT account."}}`
const wsModelFailureTerminal = `{"type":"response.failed","response":{"id":"resp_model_rejected","status":"failed","error":{"type":"invalid_request_error","code":"model_not_found","message":"The 'gpt-5.1' model is not supported when using Codex with a ChatGPT account."},"usage":{"input_tokens":3,"output_tokens":0}}}`

type wsModelFailureRepository struct {
	modelNotFoundAccountRepoStub
	stateContextErr error
}

type wsModelFailureTurnConn struct {
	*openAIWSCaptureConn
	requests chan struct{}
}

func (connection *wsModelFailureTurnConn) WriteJSON(ctx context.Context, value any) error {
	if err := connection.openAIWSCaptureConn.WriteJSON(ctx, value); err != nil {
		return err
	}
	select {
	case connection.requests <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (connection *wsModelFailureTurnConn) ReadMessage(ctx context.Context) ([]byte, error) {
	select {
	case <-connection.requests:
		return connection.openAIWSCaptureConn.ReadMessage(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (connection *wsModelFailureTurnConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	payload, err := connection.ReadMessage(ctx)
	return coderws.MessageText, payload, err
}

func (connection *wsModelFailureTurnConn) WriteFrame(ctx context.Context, _ coderws.MessageType, payload []byte) error {
	return connection.WriteJSON(ctx, json.RawMessage(payload))
}

func (repository *wsModelFailureRepository) SetModelRateLimit(ctx context.Context, accountID int64, model string, until time.Time, reason ...string) error {
	repository.stateContextErr = ctx.Err()
	return repository.modelNotFoundAccountRepoStub.SetModelRateLimit(ctx, accountID, model, until, reason...)
}

func assertWSModelCooldown(t *testing.T, repository *modelNotFoundAccountRepoStub, account *Account, model string) {
	t.Helper()
	require.Len(t, repository.modelRateLimitCalls, 1)
	call := repository.modelRateLimitCalls[0]
	require.Equal(t, account.ID, call.accountID)
	require.Equal(t, model, call.scope)
	require.Equal(t, upstreamCodexPlanGatedModelReason, call.reason)
	require.WithinDuration(t, time.Now().Add(30*time.Minute), call.resetAt, 5*time.Second)
	require.Zero(t, repository.tempCalls)
	snapshot := *account
	setAccountModelRateLimitSnapshot(&snapshot, model, call.resetAt, call.reason, time.Now())
	require.Positive(t, snapshot.getRateLimitRemainingForKey(model))
	if snapshot.GetMappedModel(model) == model {
		require.False(t, snapshot.IsSchedulableForModelWithContext(context.Background(), model))
	}
	require.True(t, snapshot.IsSchedulableForModelWithContext(context.Background(), "gpt-5.6"))
}

func TestWSModelFailureClassification(t *testing.T) {
	account := wsSSETestAccount()
	for _, test := range []struct {
		payload string
		status  int
	}{
		{wsModelFailureError, 400},
		{wsModelFailureTerminal, 400},
		{`{"type":"error","error":{"status_code":404,"code":"model_not_found","message":"model not found"}}`, 404},
		{`{"type":"error","error":{"code":"model_not_found","message":"model not found"}}`, 404},
		{`{"type":"error","status":400,"error":{"code":"invalid_request_error","message":"bad input"}}`, 0},
		{`{"type":"error","status":401,"error":{"code":"token_revoked","message":"Unauthorized"}}`, 0},
		{`{"type":"error","status":429,"error":{"code":"rate_limit_exceeded"}}`, 0},
		{`{"type":"error","status":400,"error":{"code":"invalid_encrypted_content"}}`, 0},
		{`{"type":"response.completed","response":{"error":{"code":"model_not_found"}}}`, 0},
		{strings.Replace(wsModelFailureError, `"status":400`, `"status":200`, 1), 0},
	} {
		require.Equal(t, test.status, openAIWSModelFailureStatus(account, []byte(test.payload)), test.payload)
	}
	account.Type = AccountTypeAPIKey
	require.Zero(t, openAIWSModelFailureStatus(account, []byte(wsModelFailureError)))
}

func TestWSModelFailurePolicyAndCancellation(t *testing.T) {
	for _, payload := range []string{wsModelFailureError, wsModelFailureTerminal} {
		repository := &wsModelFailureRepository{}
		gateway := &OpenAIGatewayService{rateLimitService: &RateLimitService{accountRepo: repository}}
		account := wsSSETestAccount()
		account.Credentials["model_mapping"] = map[string]any{"public": "gpt-5.1", "gpt-5.1": "gpt-5.6"}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if gjson.Get(payload, "type").String() == "error" {
			gateway.handleOpenAIWSErrorEventTransientFailure(ctx, account, "gpt-5.1", nil, []byte(payload))
		} else {
			require.True(t, gateway.handleOpenAIWSFailureAccountSideEffects(ctx, account, "gpt-5.1", nil, []byte(payload)))
		}
		require.NoError(t, repository.stateContextErr)
		assertWSModelCooldown(t, &repository.modelNotFoundAccountRepoStub, account, "gpt-5.1")
		require.False(t, gateway.isOpenAIAccountRuntimeBlocked(account))
	}
	for _, poolMode := range []bool{false, true} {
		repository := &modelNotFoundAccountRepoStub{}
		gateway := &OpenAIGatewayService{rateLimitService: &RateLimitService{accountRepo: repository}}
		account := wsSSETestAccount()
		account.Type = AccountTypeAPIKey
		account.Credentials["pool_mode"] = poolMode
		if !poolMode {
			account.Credentials["custom_error_codes_enabled"] = true
			account.Credentials["custom_error_codes"] = []any{float64(429)}
		}
		gateway.handleOpenAIWSModelFailure(context.Background(), account, "gpt-5.1", []byte(`{"type":"error","status":404,"error":{"code":"model_not_found","message":"model not found"}}`))
		require.Empty(t, repository.modelRateLimitCalls)
		require.Zero(t, repository.tempCalls)
	}
}

func TestWSModelFailureDoesNotPenalizeAccountHealth(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	limiter, redis := qualitySignalTestLimiter(t)
	limiter.accountRepo = &modelNotFoundAccountRepoStub{}
	gateway := &OpenAIGatewayService{rateLimitService: limiter}
	gateway.settingService = NewSettingService(&openAIAdvancedSchedulerSettingRepoStub{values: map[string]string{openAIAdvancedSchedulerSettingKey: "true"}}, nil)
	limiter.settingService = gateway.settingService
	gateway.initOpenAIAccountScheduler()
	account := wsSSETestAccount()
	gateway.openaiAccountStats.reportAt(account.ID, true, nil, time.Now())
	before, _, _ := gateway.openaiAccountStats.snapshot(account.ID)
	for _, payload := range []string{wsModelFailureError, wsModelFailureTerminal} {
		gateway.handleOpenAIWSFailureAccountSideEffects(context.Background(), account, "gpt-5.1", nil, []byte(payload))
	}
	require.False(t, redis.Exists(quality5xxPendingKey))
	wrapped := NewOpenAIWSClientCloseError(coderws.StatusTryAgainLater, "reconnect", ErrOpenAIWSModelUnavailable)
	for range 5 {
		require.False(t, gateway.ReportOpenAIAccountScheduleResult(account, "gpt-5.1", false, nil, wrapped))
		require.False(t, gateway.ReportOpenAIAccountScheduleResult(account, "gpt-5.1", true, nil, wrapped))
		require.False(t, gateway.ObserveOpenAIAccountHealthFailure(context.Background(), account, wrapped))
	}
	after, _, _ := gateway.openaiAccountStats.snapshot(account.ID)
	require.Equal(t, before, after)
	gateway.ReportOpenAIAccountScheduleResult(account, "gpt-5.6", false, nil, errors.New("fixture network failure"))
	control, _, _ := gateway.openaiAccountStats.snapshot(account.ID)
	require.Greater(t, control, after)
}

func TestWSModelFailureHTTPDoesNotReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, prewarm := range []bool{false, true} {
		for _, payload := range []string{wsModelFailureError, wsModelFailureTerminal} {
			t.Run(fmt.Sprintf("prewarm=%t/%s", prewarm, gjson.Get(payload, "type").String()), func(t *testing.T) {
				connection := &openAIWSCaptureConn{events: [][]byte{[]byte(payload)}}
				dialer := &wsSSETestDialer{conn: connection}
				httpPeer := &httpUpstreamRecorder{}
				gateway := wsSSETestService(t, dialer, httpPeer)
				gateway.cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = prewarm
				repository := &modelNotFoundAccountRepoStub{}
				gateway.rateLimitService = &RateLimitService{accountRepo: repository}
				account := wsSSETestAccount()
				writer := httptest.NewRecorder()
				ginContext, _ := gin.CreateTestContext(writer)
				ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				SetOpenAIClientTransport(ginContext, OpenAIClientTransportHTTP)
				_, err := gateway.Forward(context.Background(), ginContext, account, []byte(wsSSETestRequest))
				require.ErrorIs(t, err, ErrOpenAIWSModelUnavailable)
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover))
				require.Contains(t, writer.Body.String(), payload)
				require.Equal(t, "text/event-stream", writer.Header().Get("Content-Type"))
				require.EqualValues(t, 1, dialer.calls.Load())
				require.Nil(t, httpPeer.lastReq)
				connection.mu.Lock()
				writes := append([]map[string]any(nil), connection.writes...)
				closed := connection.closed
				connection.mu.Unlock()
				require.Len(t, writes, 1)
				require.True(t, closed)
				assertWSModelCooldown(t, repository, account, writes[0]["model"].(string))
				if prewarm {
					require.Equal(t, false, writes[0]["generate"])
				}
			})
		}
	}
}

func TestWSModelFailureHTTPRawFailureAndUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		connection := &openAIWSCaptureConn{events: [][]byte{[]byte(wsModelFailureTerminal)}}
		gateway := wsSSETestService(t, &wsSSETestDialer{conn: connection}, &httpUpstreamRecorder{})
		repository := &modelNotFoundAccountRepoStub{}
		gateway.rateLimitService = &RateLimitService{accountRepo: repository}
		account := wsSSETestAccount()
		writer := httptest.NewRecorder()
		ginContext, _ := gin.CreateTestContext(writer)
		ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		body := map[string]any{"model": "gpt-5.4", "stream": stream, "input": "hello"}
		result, err := gateway.forwardOpenAIWSV2(context.Background(), ginContext, account, body, "", "", "fixture-token",
			NewOpenAIWSProtocolResolver(gateway.cfg).Resolve(account), true, stream, "gpt-5.4", "gpt-5.4", time.Now(), 1, "", new(bool))
		require.ErrorIs(t, err, ErrOpenAIWSModelUnavailable)
		require.Contains(t, writer.Body.String(), wsModelFailureTerminal)
		require.NotNil(t, result)
		require.Equal(t, 3, result.Usage.InputTokens)
		require.True(t, result.ObservedUsageOnError)
		require.False(t, result.SucceededForScheduling())
		require.False(t, result.SucceededForStickyMigration())
		if !stream {
			require.Equal(t, http.StatusBadRequest, writer.Code)
		}
		assertWSModelCooldown(t, repository, account, "gpt-5.4")
	}
}

func TestWSModelFailureNativeDoesNotReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		for _, failure := range []string{wsModelFailureError, wsModelFailureTerminal} {
			for _, laterTurn := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/later=%t", mode, gjson.Get(failure, "type").String(), laterTurn), func(t *testing.T) {
					upstream := &wsModelFailureTurnConn{openAIWSCaptureConn: &openAIWSCaptureConn{}, requests: make(chan struct{}, 8)}
					if laterTurn {
						upstream.events = append(upstream.events, []byte(`{"type":"response.completed","response":{"id":"resp_model_first","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`))
					}
					upstream.events = append(upstream.events, []byte(failure))
					dialer := &wsSSETestDialer{conn: upstream}
					gateway := wsSSETestService(t, dialer, &httpUpstreamRecorder{})
					repository := &modelNotFoundAccountRepoStub{}
					gateway.rateLimitService = &RateLimitService{accountRepo: repository}
					gateway.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
					account := wsSSETestAccount()
					account.Extra["openai_oauth_responses_websockets_v2_mode"] = mode
					if mode == OpenAIWSIngressModePassthrough {
						gateway.openaiWSPassthroughDialer = dialer
					}
					done := make(chan error, 1)
					type turnOutcome struct {
						result *OpenAIForwardResult
						err    error
					}
					outcomes := make(chan turnOutcome, 4)
					server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						connection, err := coderws.Accept(writer, request, nil)
						if err != nil {
							done <- err
							return
						}
						defer connection.CloseNow()
						ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
						ginContext.Request = request.Clone(request.Context())
						ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
						defer cancel()
						_, first, err := connection.Read(ctx)
						if err != nil {
							done <- err
							return
						}
						hooks := &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, err error) {
							outcomes <- turnOutcome{result: result, err: err}
						}}
						forwardErr := gateway.ProxyResponsesWebSocketFromClient(ctx, ginContext, connection, account, "fixture-token", first, hooks)
						var closeErr *OpenAIWSClientCloseError
						if errors.As(forwardErr, &closeErr) {
							_ = connection.Close(closeErr.StatusCode(), closeErr.Reason())
						}
						done <- forwardErr
					}))
					defer server.Close()
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
					require.NoError(t, err)
					defer client.CloseNow()
					firstRequest := []byte(`{"type":"response.create","model":"gpt-5.1","input":[{"role":"user","content":"hello"}]}`)
					require.NoError(t, client.Write(ctx, coderws.MessageText, firstRequest))
					if laterTurn {
						_, completed, readErr := client.Read(ctx)
						require.NoError(t, readErr)
						require.Equal(t, "response.completed", gjson.GetBytes(completed, "type").String())
						require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":[{"role":"user","content":"next turn"}]}`)))
					}
					_, response, err := client.Read(ctx)
					require.NoError(t, err)
					require.JSONEq(t, failure, string(response))
					_, _, err = client.Read(ctx)
					require.Equal(t, coderws.StatusTryAgainLater, coderws.CloseStatus(err))
					select {
					case forwardErr := <-done:
						require.ErrorIs(t, forwardErr, ErrOpenAIWSModelUnavailable)
						var failover *UpstreamFailoverError
						require.False(t, errors.As(forwardErr, &failover))
					case <-ctx.Done():
						t.Fatal("model failure did not terminate the native connection")
					}
					upstream.mu.Lock()
					writes := append([]map[string]any(nil), upstream.writes...)
					closed := upstream.closed
					upstream.mu.Unlock()
					wantWrites := 1
					if laterTurn {
						wantWrites = 2
					}
					require.Len(t, writes, wantWrites)
					require.True(t, closed)
					assertWSModelCooldown(t, repository, account, writes[len(writes)-1]["model"].(string))
					require.Len(t, outcomes, wantWrites)
					if laterTurn {
						completed := <-outcomes
						require.NoError(t, completed.err)
					}
					failed := <-outcomes
					require.ErrorIs(t, failed.err, ErrOpenAIWSModelUnavailable)
					if gjson.Get(failure, "type").String() == "response.failed" {
						require.NotNil(t, failed.result)
						require.Equal(t, 3, failed.result.Usage.InputTokens)
						require.True(t, failed.result.ObservedUsageOnError)
					}
				})
			}
		}
	}
}

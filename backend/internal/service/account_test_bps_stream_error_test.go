package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func bpsStreamErrorAccountTestService(account *Account, upstream *httpUpstreamRecorder) *AccountTestService {
	return &AccountTestService{
		accountRepo:          &stateProbeAccountRepo{account: account},
		openaiGatewayService: openAIClientToolsTestService(upstream),
	}
}

func assertBPSStreamErrorTestEvents(t *testing.T, wire, want string) {
	t.Helper()
	errors := 0
	for _, line := range strings.Split(wire, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event TestEvent
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
		require.NotEqual(t, "test_complete", event.Type, "a failed upstream must never complete the account test")
		require.False(t, event.Success)
		if event.Type == "error" {
			errors++
			require.Equal(t, want, event.Error)
		}
	}
	require.Equal(t, 1, errors, "the manual test must emit exactly one error event")
	for _, field := range []string{"PRIVATE_ERROR_CODE", "PRIVATE_ERROR_TYPE", "PRIVATE_PARAMETER"} {
		require.NotContains(t, wire, field)
	}
}

func TestAccountTestBPSStreamError(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
	}{
		{"nested rate limit", bpsFailureWire(t, "error", "rate_limit_exceeded", "", 0)},
		{"flat rate limit", "event: error\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"PRIVATE_UPSTREAM_BODY\",\"param\":\"PRIVATE_PARAMETER\"}\n\n"},
		{"failed permission", bpsFailureWire(t, "response.failed", "permission_denied", "", 0)},
		{"unknown identifiers", bpsFailureWire(t, "error", "PRIVATE_ERROR_CODE", "PRIVATE_ERROR_TYPE", 0)},
		{"upstream cancelled", bpsFailureWire(t, "response.cancelled", "", "", 0)},
		{"upstream cancelled with rate limit", bpsFailureWire(t, "response.cancelled", "rate_limit_exceeded", "", 0)},
	} {
		for _, background := range []bool{false, true} {
			mode := "manual"
			if background {
				mode = "background"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				account := excelAccount()
				upstream := &httpUpstreamRecorder{resp: responsesAccountTestHTTP(tc.wire)}
				svc := bpsStreamErrorAccountTestService(account, upstream)
				want := "PRIVATE_UPSTREAM_BODY"
				if background {
					result, err := svc.RunTestBackground(t.Context(), account.ID, "gpt-6-astra")
					require.NoError(t, err, "execution returns a failed result rather than an infrastructure error")
					require.NotNil(t, result)
					require.Equal(t, "failed", result.Status)
					require.Equal(t, want, result.ErrorMessage)
					require.Empty(t, result.ResponseText)
					require.NotContains(t, result.ErrorMessage, "PRIVATE_ERROR_CODE")
				} else {
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
					err := svc.TestAccountConnection(c, account.ID, "gpt-6-astra", "Reply OK", AccountTestModeDefault)
					require.EqualError(t, err, want)
					assertBPSStreamErrorTestEvents(t, rec.Body.String(), want)
				}
				require.Len(t, upstream.requests, 1, "an HTTP 200 failure terminal must not replay or fall back")
				require.Equal(t, "bps.openai.com", upstream.lastReq.URL.Host)
				require.Equal(t, "/basispoints/api/responses", upstream.lastReq.URL.Path)
			})
		}
	}
}

type bpsOriginalMessageCase struct {
	name, body, want string
	status           int
}

func bpsOriginalMessageCases(t *testing.T) []bpsOriginalMessageCase {
	t.Helper()
	message := "  PRIVATE 原始诊断：encrypted content could not be verified\n" + strings.Repeat("请检查账号的工作区权限。", 240) + "\n保留末尾空白。  "
	jsonMessage := `  {"message":"上游诊断 JSON 字符串","detail":"encrypted content"}  `
	var cases []bpsOriginalMessageCase
	for _, kind := range []string{"error", "response.failed", "response.cancelled"} {
		for _, flat := range []bool{false, true} {
			if flat && kind != "error" {
				continue
			}
			name := kind + "/nested"
			if flat {
				name = kind + "/flat"
			}
			for _, value := range []struct {
				name    string
				message any
				want    string
			}{
				{"original", message, message},
				{"JSON message", jsonMessage, jsonMessage},
				{"empty", "", "Excel BPS upstream failure: invalid_value; request was not replayed"},
				{"missing", nil, "Excel BPS upstream failure: invalid_value; request was not replayed"},
				{"null", nil, "Excel BPS upstream failure: invalid_value; request was not replayed"},
				{"non-string", map[string]any{"input": "not an error message"}, "Excel BPS upstream failure: invalid_value; request was not replayed"},
			} {
				detail := map[string]any{"code": "invalid_value", "type": "PRIVATE_ERROR_TYPE", "param": "PRIVATE_PARAMETER", "message": value.message}
				if value.name == "missing" {
					delete(detail, "message")
				}
				payload := map[string]any{"type": kind, "error": detail}
				if kind != "error" {
					payload = map[string]any{"type": kind, "response": map[string]any{"status": strings.TrimPrefix(kind, "response."), "output": []any{}, "error": detail}}
				} else if flat {
					payload = detail
					payload["type"] = "error"
				}
				raw, err := json.Marshal(payload)
				require.NoError(t, err)
				cases = append(cases, bpsOriginalMessageCase{name + "/" + value.name, "event: " + kind + "\ndata: " + string(raw) + "\n\n", value.want, http.StatusOK})
			}
		}
	}
	for _, status := range []int{400, 401, 403, 429, 500} {
		raw, err := json.Marshal(map[string]any{"error": map[string]any{"message": message}})
		require.NoError(t, err)
		cases = append(cases, bpsOriginalMessageCase{http.StatusText(status), string(raw), message, status})
	}
	for _, field := range []string{"response.error.message", "message", "detail", "error"} {
		var payload any = map[string]any{field: message}
		if field == "response.error.message" {
			payload = map[string]any{"response": map[string]any{"error": map[string]any{"message": message}}, "error": map[string]any{"message": "wrong error"}, "message": "wrong root"}
		}
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		cases = append(cases, bpsOriginalMessageCase{"HTTP/" + field, string(raw), message, http.StatusBadGateway})
	}
	jsonBody, err := json.Marshal(message)
	require.NoError(t, err)
	cases = append(cases,
		bpsOriginalMessageCase{"HTTP/non-JSON body", message, message, http.StatusBadGateway},
		bpsOriginalMessageCase{"HTTP/JSON string body", string(jsonBody), message, http.StatusBadGateway},
		bpsOriginalMessageCase{"HTTP/JSON message", `{"error":{"message":` + string(mustBPSMessageJSON(t, jsonMessage)) + `}}`, jsonMessage, http.StatusBadGateway},
		bpsOriginalMessageCase{"HTTP/missing message", `{"error":{"code":"invalid_value"},"input":{"message":"not an error message"}}`, "Excel BPS rejected this request; account scheduling was not changed", http.StatusBadRequest},
		bpsOriginalMessageCase{"HTTP/empty message", `{"error":{"message":""}}`, "Excel BPS rejected this request; account scheduling was not changed", http.StatusBadRequest},
		bpsOriginalMessageCase{"HTTP/non-string message", `{"error":{"message":42}}`, "Excel BPS rejected this request; account scheduling was not changed", http.StatusBadRequest},
		bpsOriginalMessageCase{"HTTP/429 empty message", `{"error":{"message":""}}`, excelBPSRateLimitedClientMessage, http.StatusTooManyRequests},
	)
	secretMessage := "  PRIVATE 原因仍可见\n" +
		"test-token refresh-credential id-credential api-credential session-credential cookie-credential\n" +
		"Bearer bearer-credential https://privateuser:privatepass@example.invalid/?api_key=query-credential\n" +
		"file-privateAttachment /api/bps-images/privateCapability\n" +
		`{"access_token":"json-credential","diagnosis":"encrypted content diagnostic"}  `
	wantSecretMessage := "  PRIVATE 原因仍可见\n" +
		"[redacted] [redacted] [redacted] [redacted] [redacted] [redacted]\n" +
		"Bearer [redacted] https://[redacted]@example.invalid/?api_key=***\n" +
		"file-[redacted] /api/bps-images/[redacted]\n" +
		`{"access_token":"***","diagnosis":"encrypted content diagnostic"}  `
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusTooManyRequests} {
		payload := map[string]any{"error": map[string]any{"message": secretMessage}}
		if status == http.StatusOK {
			payload["type"] = "error"
		}
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		body := string(raw)
		if status == http.StatusOK {
			body = "event: error\ndata: " + body + "\n\n"
		}
		cases = append(cases, bpsOriginalMessageCase{"credentials/" + http.StatusText(status), body, wantSecretMessage, status})
	}
	return cases
}

func bpsOriginalMessageAccount() *Account {
	account := excelAccount()
	for key, value := range map[string]string{"refresh_token": "refresh-credential", "id_token": "id-credential", "api_key": "api-credential", "session_key": "session-credential", "cookie": "cookie-credential"} {
		account.Credentials[key] = value
	}
	return account
}

func mustBPSMessageJSON(t *testing.T, message string) []byte {
	t.Helper()
	raw, err := json.Marshal(message)
	require.NoError(t, err)
	return raw
}

func TestAccountTestBPSOriginalMessage(t *testing.T) {
	for _, transport := range []string{"sse", "ws"} {
		for _, tc := range bpsOriginalMessageCases(t) {
			for _, background := range []bool{false, true} {
				mode := "manual"
				if background {
					mode = "background"
				}
				t.Run(transport+"/"+tc.name+"/"+mode, func(t *testing.T) {
					account := bpsOriginalMessageAccount()
					account.Extra[ExcelBPSTransportKey] = transport
					upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"text/event-stream"}, "Retry-After": {"1"}}, Body: io.NopCloser(strings.NewReader(tc.body))}}
					svc := bpsStreamErrorAccountTestService(account, upstream)
					ws := &excelBPSWSTestUpstream{httpUpstreamRecorder: upstream}
					if transport == "ws" {
						svc.openaiGatewayService.httpUpstream = ws
					}
					if background {
						result, err := svc.RunTestBackground(t.Context(), account.ID, "gpt-6-astra")
						require.NoError(t, err)
						require.Equal(t, "failed", result.Status)
						require.Equal(t, tc.want, result.ErrorMessage)
						require.Empty(t, result.ResponseText)
					} else {
						rec := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(rec)
						c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/300/test", nil).WithContext(WithManualAccountTest(t.Context()))
						err := svc.TestAccountConnection(c, account.ID, "gpt-6-astra", "Reply OK", AccountTestModeDefault)
						require.EqualError(t, err, tc.want)
						assertBPSStreamErrorTestEvents(t, rec.Body.String(), tc.want)
					}
					require.Len(t, upstream.requests, 1, "a connectivity test must not retry, switch accounts or fall back to native Codex")
					require.Equal(t, "/basispoints/api/responses", upstream.lastReq.URL.Path)
					if transport == "ws" {
						require.Equal(t, 1, ws.wsCalls)
						require.Zero(t, ws.httpCalls)
					}
				})
			}
		}
	}
}

// Cancel only once the real gateway starts reading an accepted HTTP 200 stream.
// This distinguishes local cancellation from an upstream response.cancelled event.
type bpsStreamErrorCanceledBody struct{ cancel context.CancelFunc }

func (b *bpsStreamErrorCanceledBody) Read([]byte) (int, error) {
	b.cancel()
	return 0, context.Canceled
}

func (*bpsStreamErrorCanceledBody) Close() error { return nil }

func TestAccountTestBPSStreamErrorLocalCancellation(t *testing.T) {
	for _, background := range []bool{false, true} {
		mode := "manual"
		if background {
			mode = "background"
		}
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			account := excelAccount()
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       &bpsStreamErrorCanceledBody{cancel: cancel},
			}}
			svc := bpsStreamErrorAccountTestService(account, upstream)
			if background {
				result, err := svc.RunTestBackground(ctx, account.ID, "gpt-6-astra")
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, "failed", result.Status)
				require.Equal(t, context.Canceled.Error(), result.ErrorMessage)
				require.Empty(t, result.ResponseText)
			} else {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(ctx)
				err := svc.TestAccountConnection(c, account.ID, "gpt-6-astra", "Reply OK", AccountTestModeDefault)
				require.EqualError(t, err, context.Canceled.Error())
				// A disconnected consumer need not receive another SSE frame.
				_, message := parseTestSSEOutput(rec.Body.String())
				if message != "" {
					require.Equal(t, context.Canceled.Error(), message)
				}
				require.NotContains(t, rec.Body.String(), `"type":"test_complete"`)
				require.NotContains(t, rec.Body.String(), "basispoints_upstream_cancelled")
				require.NotContains(t, rec.Body.String(), "Client disconnected:")
			}
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, "bps.openai.com", upstream.lastReq.URL.Host)
		})
	}
}

func TestAccountTestBPSStreamErrorRefusalPriority(t *testing.T) {
	// Both refusal evidence and a classified terminal are present. The refusal
	// must remain the result rather than the later permission error.
	wire := responsesTestWire(`{"type":"response.refusal.delta","delta":"PRIVATE_REFUSAL"}`) +
		bpsFailureWire(t, "response.failed", "permission_denied", "", 0)
	account := excelAccount()
	upstream := &httpUpstreamRecorder{resp: responsesAccountTestHTTP(wire)}
	svc := bpsStreamErrorAccountTestService(account, upstream)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
	err := svc.TestAccountConnection(c, account.ID, "gpt-6-astra", "Reply OK", AccountTestModeDefault)
	require.EqualError(t, err, errResponsesTestRefused.Error())
	assertBPSStreamErrorTestEvents(t, rec.Body.String(), errResponsesTestRefused.Error())
	require.NotContains(t, rec.Body.String(), "permission_denied")
	require.Len(t, upstream.requests, 1)
}

func TestAccountTestBPSStreamErrorNativeModel(t *testing.T) {
	// An account-wide BPS toggle can still send a model through native Codex.
	// Exercise the shared gateway helper with an actual non-BPS dispatch.
	account := excelAccount()
	account.Extra["openai_excel_bps_models"] = []string{"gpt-6-astra"}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("")),
	}}
	svc := bpsStreamErrorAccountTestService(account, upstream)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
	err := svc.testResponsesGatewayAccountConnection(c, account, "gpt-5.6-sol", "Reply OK")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "Excel BPS")
	assertBPSStreamErrorTestEvents(t, rec.Body.String(), err.Error())
	require.Len(t, upstream.requests, 1)
	require.Equal(t, chatgptCodexURL, upstream.lastReq.URL.String())
}

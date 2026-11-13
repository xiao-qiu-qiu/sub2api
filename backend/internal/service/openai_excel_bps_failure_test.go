package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/qualityqueue"
	"github.com/Wei-Shaw/sub2api/internal/pkg/qualityqueue/queuetest"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func bpsFailureWire(t *testing.T, kind, code, errorType string, status int) string {
	t.Helper()
	detail := map[string]any{"code": code, "type": errorType, "message": "PRIVATE_UPSTREAM_BODY", "param": "PRIVATE_PARAMETER"}
	event := map[string]any{"type": kind}
	if status != 0 {
		event["status"] = status
	}
	if kind == "error" {
		event["error"] = detail
	} else {
		event["response"] = map[string]any{
			"id": "resp_failure", "status": strings.TrimPrefix(kind, "response."), "error": detail,
			"output": []any{}, "usage": map[string]any{"input_tokens": 7, "output_tokens": 2},
		}
	}
	raw, err := json.Marshal(event)
	require.NoError(t, err)
	return "event: " + kind + "\ndata: " + string(raw) + "\n\n"
}

func TestExcelBPSFailureAfterCompactKeepalive(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: bpsCompletionResponse(http.StatusOK,
		bpsFailureWire(t, "response.failed", "rate_limit_exceeded", "", 0))}
	svc := openAIClientToolsTestService(upstream)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	MarkOpenAICompactClientStream(c)
	stop := StartOpenAICompactSSEKeepalive(c, time.Hour)
	defer stop()
	value, ok := c.Get(openAICompactSSEKeepaliveKey)
	require.True(t, ok)
	keepalive, ok := value.(*openAICompactSSEKeepalive)
	require.True(t, ok)
	require.True(t, keepalive.beat())
	body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": "continue"})
	require.NoError(t, err)
	_, err = svc.Forward(context.Background(), c, excelAccount(), body)
	require.Error(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed\n"))
	require.NotContains(t, rec.Body.String(), "PRIVATE_")
	require.False(t, json.Valid(rec.Body.Bytes()), "already committed keepalive requires an SSE terminal")
	require.Contains(t, rec.Body.String(), "rate_limit_exceeded")
	observed, ok := GetOpsStreamError(c)
	require.True(t, ok)
	require.Equal(t, http.StatusTooManyRequests, observed.IntendedStatus)
	require.Len(t, upstream.requests, 1)
}

func TestExcelBPSFailureQualityObservationDoesNotCoolAccount(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: bpsCompletionResponse(http.StatusOK,
		bpsFailureWire(t, "response.failed", "rate_limit_exceeded", "", 0))}
	svc := openAIClientToolsTestService(upstream)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx := withPelicanTestOptions(context.Background(), pelicanTestOptions{observeOnly: true})
	body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": "observe"})
	require.NoError(t, err)
	account := excelAccount()
	_, err = svc.Forward(ctx, c, account, body)
	require.Error(t, err)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.False(t, svc.isExcelBPSCoolingDown(account, "gpt-6-astra"))
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Len(t, upstream.requests, 1)
}

func TestExcelBPSFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name, event, code, errorType string
		explicit, status             int
		wantCode, wantType           string
	}{
		{"invalid input", "response.failed", "invalid_value", "", 0, 400, "invalid_value", "invalid_request_error"},
		{"message too big", "response.failed", "message_too_big", "", 0, 413, "message_too_big", "invalid_request_error"},
		{"WS request too big", "response.failed", "basispoints_ws_message_too_big", "", 0, 413, "basispoints_ws_message_too_big", "invalid_request_error"},
		{"WS response too big", "response.failed", "basispoints_ws_response_too_big", "", 0, 502, "basispoints_ws_response_too_big", "server_error"},
		{"authentication", "response.failed", "invalid_api_key", "", 0, 401, "invalid_api_key", "authentication_error"},
		{"permission", "response.failed", "permission_denied", "", 0, 403, "permission_denied", "permission_error"},
		{"rate limit", "error", "rate_limit_exceeded", "", 0, 429, "rate_limit_exceeded", "rate_limit_error"},
		{"type fallback", "response.failed", "", "overloaded_error", 0, 503, "basispoints_upstream_error", "server_error"},
		{"explicit status", "response.failed", "invalid_value", "invalid_request_error", 429, 429, "invalid_value", "rate_limit_error"},
		{"cancelled", "response.cancelled", "", "", 0, 502, "basispoints_upstream_cancelled", "server_error"},
		{"cancelled rate limit", "response.cancelled", "rate_limit_exceeded", "", 0, 429, "rate_limit_exceeded", "rate_limit_error"},
		{"unknown", "error", "PRIVATE_ERROR_CODE", "PRIVATE_ERROR_TYPE", 0, 502, "basispoints_upstream_error", "server_error"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				wire := bpsFailureWire(t, tc.event, tc.code, tc.errorType, tc.explicit)
				if stream || tc.status == http.StatusTooManyRequests {
					prefix, err := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": "already delivered"})
					require.NoError(t, err)
					wire = "data: " + string(prefix) + "\n\n" + wire
				}
				upstream := &httpUpstreamRecorder{resp: bpsCompletionResponse(http.StatusOK, wire)}
				svc := openAIClientToolsTestService(upstream)
				account := excelAccount()
				account.Extra["openai_excel_bps_auto_disable_on_403"] = true
				body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "stream": stream, "input": "hello"})
				require.NoError(t, err)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				result, err := svc.Forward(context.Background(), c, account, body)
				require.Error(t, err)
				require.NotNil(t, result)
				require.Equal(t, tc.event, result.UpstreamTerminalEvent)
				require.Len(t, upstream.requests, 1, "never replay a request after an upstream terminal")
				require.True(t, IsResponseCommitted(c))
				var failover *UpstreamFailoverError
				require.NotErrorAs(t, err, &failover)
				require.NotContains(t, rec.Body.String(), "PRIVATE_")
				require.NotContains(t, err.Error(), "PRIVATE_")
				require.NotContains(t, rec.Body.String(), "basispoints_stream_incomplete")
				require.NotContains(t, rec.Body.String(), "response.completed")
				if tc.event != "error" {
					require.EqualValues(t, 7, result.Usage.InputTokens)
					require.EqualValues(t, 2, result.Usage.OutputTokens)
				}
				if stream {
					require.Equal(t, http.StatusOK, rec.Code)
					require.Contains(t, rec.Body.String(), "already delivered")
					found := 0
					for _, line := range strings.Split(rec.Body.String(), "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						raw := strings.TrimPrefix(line, "data: ")
						if gjson.Get(raw, "type").String() != tc.event {
							continue
						}
						found++
						if tc.event == "error" {
							require.False(t, gjson.Get(raw, "error").Exists())
							require.Equal(t, tc.wantCode, gjson.Get(raw, "code").String())
							require.NotEmpty(t, gjson.Get(raw, "message").String())
							require.Equal(t, "null", gjson.Get(raw, "param").Raw)
							require.EqualValues(t, 1, gjson.Get(raw, "sequence_number").Int())
							require.EqualValues(t, tc.status, gjson.Get(raw, "status").Int())
						} else {
							require.Equal(t, tc.wantCode, gjson.Get(raw, "response.error.code").String())
							require.Equal(t, tc.wantType, gjson.Get(raw, "response.error.type").String())
							require.EqualValues(t, tc.status, gjson.Get(raw, "response.error.status").Int())
						}
					}
					require.Equal(t, 1, found)
				} else {
					require.Equal(t, tc.status, rec.Code)
					require.Equal(t, tc.wantCode, gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
					require.Equal(t, tc.wantType, gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
				}
				observed, ok := GetOpsStreamError(c)
				require.True(t, ok)
				require.Equal(t, tc.status, observed.IntendedStatus)
				require.Equal(t, tc.wantCode, observed.Code)
				require.Equal(t, http.StatusOK, c.GetInt(OpsUpstreamStatusCodeKey), "mapped status is not an upstream HTTP status")
				require.Equal(t, tc.status == 429, svc.isExcelBPSCoolingDown(account, "gpt-6-astra"))
				require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
				require.True(t, account.IsSchedulable())
				require.True(t, account.IsExcelBPSEnabled(), "an in-band 403 must not disable the account")
			})
		}
	}
}

func TestExcelBPSRequestRejectedClassification(t *testing.T) {
	for _, tc := range []struct {
		name, payload, code string
		status              int
	}{
		{"flat policy", `{"type":"error","code":"request_rejected","message":"This request was blocked by our usage policy."}`, "basispoints_usage_policy_rejected", 403},
		{"nested policy", `{"type":"error","error":{"code":"request_rejected","message":"This request was blocked by our usage policy."}}`, "basispoints_usage_policy_rejected", 403},
		{"failed policy", `{"type":"response.failed","response":{"status":"failed","output":[],"error":{"code":"request_rejected","message":"This request was blocked by our usage policy."}}}`, "basispoints_usage_policy_rejected", 403},
		{"flat body", `{"type":"error","code":"request_rejected","message":"Invalid request body."}`, "basispoints_invalid_request_body", 400},
		{"nested body", `{"type":"error","error":{"code":"request_rejected","message":"Invalid request body."}}`, "basispoints_invalid_request_body", 400},
		{"failed body", `{"type":"response.failed","response":{"status":"failed","output":[],"error":{"code":"request_rejected","message":"Invalid request body."}}}`, "basispoints_invalid_request_body", 400},
		{"unknown rejection", `{"type":"error","code":"request_rejected","message":"PRIVATE_UNKNOWN_REJECTION"}`, "request_rejected", 403},
		{"policy suffix", `{"type":"error","code":"request_rejected","message":"This request was blocked by our usage policy. PRIVATE_SUFFIX"}`, "request_rejected", 403},
		{"body whitespace", `{"type":"error","code":"request_rejected","message":"Invalid request body. "}`, "request_rejected", 403},
		{"body case", `{"type":"error","code":"request_rejected","message":"invalid request body."}`, "request_rejected", 403},
		{"different code", `{"type":"error","code":"PRIVATE_OTHER_REJECTION","message":"This request was blocked by our usage policy."}`, "basispoints_upstream_error", 502},
		{"different code case", `{"type":"error","code":"REQUEST_REJECTED","message":"Invalid request body."}`, "basispoints_upstream_error", 502},
		{"message outside error", `{"type":"error","message":"This request was blocked by our usage policy.","error":{"code":"request_rejected","message":"PRIVATE_UNKNOWN_REJECTION"}}`, "request_rejected", 403},
		{"echoed input", `{"type":"error","error":{"code":"request_rejected","message":"PRIVATE_UNKNOWN_REJECTION"},"input":{"code":"request_rejected","message":"Invalid request body."}}`, "request_rejected", 403},
		{"selected error only", `{"type":"response.failed","response":{"status":"failed","output":[],"error":{"code":"request_rejected","message":"PRIVATE_UNKNOWN_REJECTION"}},"error":{"code":"request_rejected","message":"This request was blocked by our usage policy."}}`, "request_rejected", 403},
		{"explicit status keeps precedence", `{"type":"error","status":400,"code":"request_rejected","message":"This request was blocked by our usage policy."}`, "basispoints_usage_policy_rejected", 400},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				r := miniredis.RunT(t)
				client := queuetest.NewClient(r.Addr())
				t.Cleanup(func() { _ = client.Close() })
				queue := qualityqueue.NewRedis(client)
				t.Cleanup(func() { _ = queue.Close() })
				kind := gjson.Get(tc.payload, "type").String()
				upstream := &httpUpstreamRecorder{resp: bpsCompletionResponse(http.StatusOK, "event: "+kind+"\ndata: "+tc.payload+"\n\n")}
				svc := openAIClientToolsTestService(upstream)
				svc.rateLimitService = &RateLimitService{qualityTrigger: &quality5xxTrigger{queue: queue}}
				account := excelAccount()
				account.Extra["openai_excel_bps_auto_disable_on_403"] = true
				body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": "hello", "stream": stream})
				require.NoError(t, err)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				result, err := svc.Forward(context.Background(), c, account, body)
				require.Error(t, err)
				require.NotNil(t, result)
				require.Equal(t, kind, result.UpstreamTerminalEvent)
				require.Len(t, upstream.requests, 1, "rejection classification must not replay")
				var failover *UpstreamFailoverError
				require.NotErrorAs(t, err, &failover)
				require.True(t, IsResponseCommitted(c))
				require.NotContains(t, rec.Body.String(), "PRIVATE_")
				observed, ok := GetOpsStreamError(c)
				require.True(t, ok)
				require.Equal(t, tc.code, observed.Code)
				require.Equal(t, tc.status, observed.IntendedStatus)
				require.Equal(t, http.StatusOK, c.GetInt(OpsUpstreamStatusCodeKey))
				if stream {
					require.Equal(t, http.StatusOK, rec.Code)
					require.Contains(t, rec.Body.String(), tc.code)
				} else {
					require.Equal(t, tc.status, rec.Code)
					require.Equal(t, tc.code, gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
				}
				wantSignals := int64(0)
				if tc.status >= 500 {
					wantSignals = 1
				}
				require.Equal(t, wantSignals, client.ZCard(context.Background(), quality5xxPendingKey).Val(), "only unknown 502 failures may enqueue 5xx quality probes")
				require.False(t, svc.isExcelBPSCoolingDown(account, "gpt-6-astra"))
				require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
				require.True(t, account.IsSchedulable())
				require.True(t, account.IsExcelBPSEnabled())
			})
		}
	}
}

func TestOpenAIStreamFailureStatusPreservesStructured4xx(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		status        int
	}{
		{"root status", `{"type":"error","status":400}`, 400},
		{"root status code", `{"type":"error","status_code":413}`, 413},
		{"error status", `{"type":"error","error":{"status":422}}`, 422},
		{"error status code", `{"type":"error","error":{"status_code":404}}`, 404},
		{"response error status", `{"type":"response.failed","response":{"error":{"status":409}}}`, 409},
		{"response error status code", `{"type":"response.failed","response":{"error":{"status_code":402}}}`, 402},
		{"last 4xx", `{"type":"error","status":499}`, 499},
		{"invalid request type", `{"type":"error","error":{"type":"invalid_request_error"}}`, 400},
		{"nested invalid request type", `{"type":"response.failed","response":{"error":{"type":"invalid_request_error"}}}`, 400},
		{"message only stays generic", `{"type":"error","error":{"message":"Invalid request body."}}`, 502},
		{"echoed status ignored", `{"type":"error","input":{"error":{"status":400}}}`, 502},
		{"string 400 ignored", `{"type":"error","status":"400"}`, 502},
		{"fraction ignored", `{"type":"error","status":400.5}`, 502},
		{"success status ignored", `{"type":"error","status":200}`, 502},
		{"generic 5xx unchanged", `{"type":"error","status":500}`, 502},
		{"legacy string 401", `{"type":"error","error":{"status":"401"}}`, 401},
		{"legacy overload", `{"type":"error","error":{"status":529}}`, 529},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(tc.payload)
			require.Equal(t, tc.status, openAIStreamFailureStatus(payload, extractOpenAISSEErrorMessage(payload)))
		})
	}
}

func TestExcelBPSResponsesErrorFieldsAndExtractors(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("nested=%t/stream=%t", nested, stream), func(t *testing.T) {
				detail := map[string]any{"code": "invalid_value", "message": "PRIVATE_UPSTREAM_BODY", "param": "input[2].id"}
				event := map[string]any{"type": "error", "error": detail}
				if !nested {
					event = detail
					event["type"] = "error"
				}
				raw, err := json.Marshal(event)
				require.NoError(t, err)
				upstream := &httpUpstreamRecorder{resp: bpsCompletionResponse(http.StatusOK,
					"event: error\ndata: "+string(raw)+"\n\n")}
				svc := openAIClientToolsTestService(upstream)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": "hello", "stream": stream})
				require.NoError(t, err)
				result, err := svc.Forward(context.Background(), c, excelAccount(), body)
				require.Error(t, err)
				require.NotNil(t, result)
				require.Equal(t, "error", result.UpstreamTerminalEvent)
				require.Len(t, upstream.requests, 1)
				require.NotContains(t, rec.Body.String(), "PRIVATE")
				require.NotContains(t, rec.Body.String(), "response.completed")
				raw = rec.Body.Bytes()
				if !stream {
					require.Equal(t, http.StatusBadRequest, rec.Code)
					require.Equal(t, "invalid_value", gjson.GetBytes(raw, "error.code").String())
					require.Equal(t, "input[2].id", gjson.GetBytes(raw, "error.param").String())
					return
				}
				require.Equal(t, http.StatusOK, rec.Code)
				require.Equal(t, 1, strings.Count(string(raw), "event: error\n"))
				raw = []byte(strings.TrimSpace(strings.TrimPrefix(string(raw), "event: error\ndata: ")))
				require.True(t, json.Valid(raw))
				require.False(t, gjson.GetBytes(raw, "error").Exists())
				require.Equal(t, "error", gjson.GetBytes(raw, "type").String())
				require.Equal(t, "input[2].id", gjson.GetBytes(raw, "param").String())
				require.Equal(t, "0", gjson.GetBytes(raw, "sequence_number").Raw)
				require.False(t, IsSuccessfulStreamTerminal(raw))
				require.Equal(t, "invalid_value", extractUpstreamErrorCode(raw))
				message := extractOpenAISSEErrorMessage(raw)
				require.NotEmpty(t, message)
				failedSSE := buildOpenAIResponseFailedSSE("resp_error", "gpt-6-astra", 1, raw, "")
				failedJSON := strings.TrimSpace(strings.TrimPrefix(failedSSE, "event: response.failed\ndata: "))
				for _, failed := range []string{failedJSON, string(buildOpenAIWSHTTPBridgeFailedEvent("resp_error", "gpt-6-astra", 1, raw, ""))} {
					require.Equal(t, "failed", gjson.Get(failed, "response.status").String())
					require.Equal(t, "invalid_value", gjson.Get(failed, "response.error.code").String())
					require.Equal(t, message, gjson.Get(failed, "response.error.message").String())
					require.Equal(t, "input[2].id", gjson.Get(failed, "response.error.param").String())
				}
			})
		}
	}
}

func TestExcelBPSPreGeneration429FailsOverImmediately(t *testing.T) {
	rateLimit := "data: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\"}\n\n"
	controls := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_rejected\",\"status\":\"in_progress\",\"output\":[]}}\n\ndata: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_rejected\",\"output\":[]}}\n\n"
	for _, tc := range []struct {
		name, prefix, failure string
		manual, retry         bool
		heartbeat             string
	}{
		{name: "first error", failure: rateLimit, retry: true},
		{name: "event between tool JSON lines", prefix: "data: {\"type\":\"response.output_item.added\",\nevent: response.output_item.added\ndata: \"item\":{\"type\":\"function_call\",\"id\":\"tool_1\",\"call_id\":\"call_1\",\"name\":\"run\",\"arguments\":\"\"},\"output_index\":0}\n\n", failure: rateLimit},

		{name: "queued heartbeat", failure: rateLimit, retry: true, heartbeat: "gateway"},
		{name: "earlier BPS heartbeat", failure: rateLimit, retry: true, heartbeat: "bps"},
		{name: "unmarked prior bytes", failure: rateLimit, heartbeat: "unmarked"},
		{name: "initial controls", prefix: controls, failure: rateLimit, retry: true},
		{name: "comments and done", prefix: ": heartbeat\n\n" + controls, failure: rateLimit + "data: [DONE]\n\n", retry: true},
		{name: "type only limit", prefix: controls, failure: "data: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\"}}\n\n", retry: true},
		{name: "reasoning item blocks replay", prefix: "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"reasoning_1\",\"type\":\"reasoning\",\"summary\":[]},\"output_index\":0}\n\n", failure: rateLimit},
		{name: "withheld tool blocks replay", prefix: "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"tool_1\",\"type\":\"function_call\",\"name\":\"unknown_tool\",\"call_id\":\"call_1\",\"arguments\":\"\"},\"output_index\":0}\n\n", failure: rateLimit},
		{name: "text blocks replay", prefix: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"accepted output\"}\n\n", failure: rateLimit},
		{name: "terminal usage blocks replay", prefix: controls, failure: "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"output\":[],\"usage\":{\"output_tokens\":1},\"error\":{\"code\":\"rate_limit_exceeded\"}}}\n\n"},
		{name: "unknown event blocks replay", prefix: "data: {\"type\":\"response.unknown_progress\"}\n\n", failure: rateLimit},
		{name: "manual test reports its account", prefix: controls, failure: rateLimit, manual: true},
		{name: "cancellation is not rejection", prefix: controls, failure: "data: {\"type\":\"response.cancelled\",\"response\":{\"status\":\"cancelled\",\"output\":[],\"error\":{\"code\":\"rate_limit_exceeded\"}}}\n\n"},
	} {
		for _, stream := range []bool{false, true} {
			if tc.heartbeat != "" && !stream {
				continue
			}
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				upstream := &httpUpstreamRecorder{resp: bpsCompletionResponse(http.StatusOK, tc.prefix+tc.failure)}
				svc := openAIClientToolsTestService(upstream)
				account := excelAccount()
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				ctx := context.Background()
				if tc.manual {
					ctx = WithManualAccountTest(ctx)
				}
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
				if tc.heartbeat != "" {
					n, _ := c.Writer.WriteString(": queued\n\n")
					if tc.heartbeat == "gateway" {
						c.Set("gateway_stream_heartbeat_bytes", n)
					} else if tc.heartbeat == "bps" {
						recordOpenAIStreamKeepaliveBytes(c, n)
					}
					c.Writer.Flush()
				}
				body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": "hello", "stream": stream})
				require.NoError(t, err)
				result, err := svc.Forward(ctx, c, account, body)
				require.Error(t, err)
				require.Len(t, upstream.requests, 1)
				var failover *UpstreamFailoverError
				if tc.retry {
					require.ErrorAs(t, err, &failover)
					require.Nil(t, result)
					require.Equal(t, http.StatusTooManyRequests, failover.StatusCode)
					require.Equal(t, ExcelBPSRateLimitedReason, failover.Reason)
					require.True(t, failover.SkipAccountCooldownWait)
					require.True(t, failover.SafeToFailoverAfterWrite)
					require.False(t, failover.RetryableOnSameAccount)
					if tc.heartbeat != "" {
						require.Equal(t, ": queued\n\n", rec.Body.String())
					} else {
						require.Empty(t, rec.Body.String(), "failed response IDs and control frames must not leak")
					}
					require.False(t, IsResponseCommitted(c))
					require.True(t, svc.isExcelBPSCoolingDown(account, "gpt-6-astra"))
				} else {
					require.NotErrorAs(t, err, &failover)
					require.NotNil(t, result)
				}
			})
		}
	}
}

type bpsRequestRejectionRecorder struct {
	AccountRepository
	account *Account
	reasons []string
}

func (r *bpsRequestRejectionRecorder) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}
func (r *bpsRequestRejectionRecorder) MarkBPSRequestRejected(_ context.Context, observed *Account, reason string) (bool, error) {
	r.reasons = append(r.reasons, reason)
	return true, nil
}

func TestExcelBPSRequestRejectedQuarantinePaths(t *testing.T) {
	for _, tc := range []struct {
		name, payload, reason string
		status                int
	}{
		{"event named rejection", "{\"code\":\"request_rejected\",\"message\":\"Invalid request body.\"}", "invalid_request", 200},
		{"multiline rejection", "{\n\"code\":\"request_rejected\",\n\"message\":\"PRIVATE_TEXT\"\n}", "upstream_rejected", 200},
		{"flat stream policy", "{\"type\":\"error\",\"code\":\"request_rejected\",\"message\":\"This request was blocked by our usage policy.\"}", "usage_policy", 200},
		{"nested stream body", "{\"type\":\"error\",\"error\":{\"code\":\"request_rejected\",\"message\":\"Invalid request body.\"}}", "invalid_request", 200},
		{"failed unknown rejection", "{\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"output\":[],\"error\":{\"code\":\"request_rejected\",\"message\":\"PRIVATE_TEXT\"}}}", "upstream_rejected", 200},
		{"http forbidden", "{\"error\":{\"code\":\"request_rejected\",\"message\":\"This request was blocked by our usage policy.\"}}", "usage_policy", 403},
		{"http bad request", "{\"error\":{\"code\":\"request_rejected\",\"message\":\"Invalid request body.\"}}", "invalid_request", 400},
		{"other code ignored", "{\"type\":\"error\",\"error\":{\"code\":\"permission_denied\",\"message\":\"request_rejected\"}}", "", 200},
		{"echoed code ignored", "{\"type\":\"error\",\"error\":{\"code\":\"other_error\"},\"input\":{\"code\":\"request_rejected\"}}", "", 200},
	} {
		for _, mode := range []string{"business", "manual", "observation"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				wire := tc.payload
				if tc.status == 200 {
					wire = "event: error\ndata: " + strings.ReplaceAll(wire, "\n", "\ndata: ") + "\n\n"
				}
				upstream := &httpUpstreamRecorder{resp: bpsCompletionResponse(tc.status, wire)}
				svc := openAIClientToolsTestService(upstream)
				account := excelAccount()
				account.ID = 90601
				account.Extra["openai_excel_bps_auto_disable_on_403"] = true
				repo := &bpsRequestRejectionRecorder{account: account}
				svc.accountRepo = repo
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				ctx := context.Background()
				switch mode {
				case "manual":
					ctx = WithManualAccountTest(ctx)
				case "observation":
					ctx = withPelicanTestOptions(ctx, pelicanTestOptions{observeOnly: true})
				}
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
				body := []byte("{\"model\":\"gpt-6-astra\",\"input\":\"hello\",\"stream\":true}")
				_, err := svc.Forward(ctx, c, account, body)
				require.Error(t, err)
				require.Len(t, upstream.requests, 1, "manual and observation requests must still reach the selected account")
				if mode == "business" && tc.reason != "" {
					require.Equal(t, []string{tc.reason}, repo.reasons)
				} else {
					require.Empty(t, repo.reasons)
				}
				require.True(t, account.IsExcelBPSEnabled(), "the rejection marker must not silently change BPS configuration")
				require.NotContains(t, rec.Body.String(), "PRIVATE_TEXT")
			})
		}
	}
}

func TestExcelBPSUsageEvidenceBlocksFailover(t *testing.T) {
	for _, usage := range []string{
		"{\"cache_creation_input_tokens\":12}",
		"{\"cache_read_input_tokens\":12}",
		"{\"input_tokens_details\":{\"cached_tokens\":12}}",
		"{\"output_tokens_details\":{\"image_tokens\":12}}",
		"{\"future_meter\":12}",
		"{\"future_meter\":\"unknown\"}",
	} {
		t.Run(usage, func(t *testing.T) {
			wire := "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"output\":[],\"usage\":" + usage + ",\"error\":{\"code\":\"rate_limit_exceeded\"}}}\n\n"
			upstream := &httpUpstreamRecorder{resp: bpsCompletionResponse(http.StatusOK, wire)}
			svc := openAIClientToolsTestService(upstream)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			result, err := svc.Forward(context.Background(), c, excelAccount(), []byte("{\"model\":\"gpt-6-astra\",\"input\":\"hello\",\"stream\":true}"))
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			require.NotNil(t, result)
			require.Len(t, upstream.requests, 1)
		})
	}
}

func TestExcelBPSRawRejectionRequiresCompleteSSEFrame(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		want       bool
	}{
		{"event after data", "data: {\"code\":\"request_rejected\",\"message\":\"Invalid request body.\"}\nevent: error\n\n", true},
		{"EOF commits frame", "data: {\"code\":\"request_rejected\"}\nevent: error", true},
		{"event between JSON lines", "data: {\"code\":\"request_rejected\",\nevent: error\ndata: \"message\":\"Invalid request body.\"}\n\n", true},
		{"invalid concatenated JSON", "event: error\ndata: {\"code\":\"request_rejected\"}\ndata: invalid\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: bpsCompletionResponse(200, tc.wire)}
			svc := openAIClientToolsTestService(upstream)
			account := excelAccount()
			account.ID = 90602
			repo := &bpsRequestRejectionRecorder{account: account}
			svc.accountRepo = repo
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			_, err := svc.Forward(context.Background(), c, account, []byte("{\"model\":\"gpt-6-astra\",\"input\":\"hello\",\"stream\":true}"))
			require.Error(t, err)
			if tc.want {
				require.Len(t, repo.reasons, 1)
			} else {
				require.Empty(t, repo.reasons)
			}
		})
	}
}

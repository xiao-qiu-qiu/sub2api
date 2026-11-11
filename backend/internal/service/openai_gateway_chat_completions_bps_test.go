package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Exercise the public Chat entry point with the same upstream doubles used by
// the Responses/BPS tests, including the actual transport dispatch boundary.
func TestForwardAsChatCompletions_BPSResponseConversion(t *testing.T) {
	for _, transport := range []string{"sse", "ws"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", transport, stream), func(t *testing.T) {
				upstream := &excelBPSWSTestUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{
					resp: bpsCompletionResponse(http.StatusOK, chatBPSTextWire("gpt-6-astra")),
				}}
				svc := openAIClientToolsTestService(upstream.httpUpstreamRecorder)
				svc.httpUpstream = upstream
				account := excelAccount()
				account.Extra[ExcelBPSTransportKey] = transport
				account.Extra["openai_excel_bps_models"] = []string{"gpt-6-astra"}
				body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","messages":[{"role":"system","content":"Answer briefly."},{"role":"user","content":"hello BPS"}],"stream":%t}`, stream))
				originalBody := append([]byte(nil), body...)
				c, rec := chatBPSContext(body)
				c.Request.Header.Set("x-codex-turn-state", "synthetic-codex-state")

				result, err := svc.ForwardAsChatCompletions(t.Context(), c, account, body, "chat-cache", "")

				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.requests, 1)
				require.Equal(t, basispoints.ResponsesURL, upstream.lastReq.URL.String())
				require.Equal(t, http.MethodPost, upstream.lastReq.Method)
				require.Equal(t, "Bearer test-token", upstream.lastReq.Header.Get("Authorization"))
				require.Equal(t, "test-account", upstream.lastReq.Header.Get("Chatgpt-Account-Id"))
				require.Equal(t, "text/event-stream", upstream.lastReq.Header.Get("Accept"))
				require.Empty(t, upstream.lastReq.Header.Get("x-codex-turn-state"))
				require.True(t, HTTPUpstreamRedirectsDisabled(upstream.lastReq.Context()))
				if transport == "ws" {
					require.Equal(t, 1, upstream.wsCalls)
					require.Zero(t, upstream.httpCalls)
					require.Equal(t, HTTPUpstreamProfileExcelBPSWebSocket, HTTPUpstreamProfileFromContext(upstream.lastReq.Context()))
				} else {
					require.Equal(t, 1, upstream.httpCalls)
					require.Zero(t, upstream.wsCalls)
					require.Equal(t, HTTPUpstreamProfileExcelBPS, HTTPUpstreamProfileFromContext(upstream.lastReq.Context()))
				}
				require.Equal(t, transport, rec.Header().Get("X-Sub2API-BPS-Transport"))
				require.Equal(t, "gpt-6-astra", gjson.GetBytes(upstream.lastBody, "model").String())
				require.Contains(t, string(upstream.lastBody), "hello BPS")
				require.Contains(t, string(upstream.lastBody), "Answer briefly.")
				require.False(t, gjson.GetBytes(upstream.lastBody, "messages").Exists())
				require.Equal(t, originalBody, body)
				require.Equal(t, "gpt-6-astra", result.Model)
				require.Equal(t, "gpt-6-astra", result.BillingModel)
				require.Equal(t, "gpt-6-astra", result.UpstreamModel)
				require.Equal(t, "/basispoints/api/responses", result.UpstreamEndpoint)
				require.Equal(t, "/basispoints/api/responses", GetActualOpenAIUpstreamEndpoint(c))
				require.Equal(t, stream, result.Stream)
				require.Equal(t, 13, result.Usage.InputTokens)
				require.Equal(t, 7, result.Usage.OutputTokens)
				require.Equal(t, 5, result.Usage.CacheReadInputTokens)
				require.Equal(t, http.StatusOK, rec.Code)
				if stream {
					require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
					chunks := chatBPSChunks(t, rec.Body.String())
					var text strings.Builder
					var finishes int
					var usage gjson.Result
					for _, chunk := range chunks {
						require.Equal(t, "chat.completion.chunk", chunk.Get("object").String())
						_, _ = text.WriteString(chunk.Get("choices.0.delta.content").String())
						if chunk.Get("choices.0.finish_reason").String() == "stop" {
							finishes++
						}
						if chunk.Get("usage").IsObject() {
							usage = chunk.Get("usage")
						}
					}
					require.Equal(t, "Hello from BPS", text.String())
					require.Equal(t, 1, finishes)
					chatBPSAssertUsage(t, usage)
				} else {
					require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
					require.True(t, json.Valid(rec.Body.Bytes()), rec.Body.String())
					response := gjson.ParseBytes(rec.Body.Bytes())
					require.Equal(t, "chat.completion", response.Get("object").String())
					require.Equal(t, "gpt-6-astra", response.Get("model").String())
					require.Equal(t, "assistant", response.Get("choices.0.message.role").String())
					require.Equal(t, "Hello from BPS", response.Get("choices.0.message.content").String())
					require.Equal(t, "stop", response.Get("choices.0.finish_reason").String())
					chatBPSAssertUsage(t, response.Get("usage"))
				}
			})
		}
	}
}

func TestForwardAsChatCompletions_BPSMapsAliasOnlyOnce(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: bpsCompletionResponse(http.StatusOK, chatBPSTextWire("gpt-6-astra"))}
	svc := openAIClientToolsTestService(upstream)
	account := excelAccount()
	account.Credentials["model_mapping"] = map[string]any{
		"public-bps":  "gpt-6-astra",
		"gpt-6-astra": "gpt-6-sol",
	}
	account.Extra["openai_excel_bps_models"] = []string{"gpt-6-astra"}
	body := []byte(`{"model":"public-bps","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, rec := chatBPSContext(body)

	result, err := svc.ForwardAsChatCompletions(t.Context(), c, account, body, "", "")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, basispoints.ResponsesURL, upstream.lastReq.URL.String())
	require.Equal(t, "gpt-6-astra", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "public-bps", result.Model)
	require.Equal(t, "gpt-6-astra", result.BillingModel)
	require.Equal(t, "gpt-6-astra", result.UpstreamModel)
	require.Equal(t, "public-bps", gjson.GetBytes(rec.Body.Bytes(), "model").String())
}

func TestForwardAsChatCompletions_BPSToolCallConversion(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			// Reuse the native run_officejs fixture so the real BPS tool bridge
			// restores the client function before the Chat conversion runs.
			wire := excelBPSRepairWire(t, "chat_tool", "Run lookup", `{"name":"lookup","arguments":{"city":"Tokyo"}}`)
			upstream := &httpUpstreamRecorder{resp: bpsCompletionResponse(http.StatusOK, wire)}
			svc := openAIClientToolsTestService(upstream)
			body := []byte(fmt.Sprintf(`{"model":"gpt-6.2-astra","messages":[{"role":"user","content":"Look up Tokyo"}],"stream":%t,"tools":[{"type":"function","function":{"name":"lookup","description":"Look up a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]}`, stream))
			c, rec := chatBPSContext(body)

			result, err := svc.ForwardAsChatCompletions(t.Context(), c, excelAccount(), body, "", "")

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, basispoints.ResponsesURL, upstream.lastReq.URL.String())
			require.Equal(t, "gpt-6.2-astra", gjson.GetBytes(upstream.lastBody, "model").String())
			require.False(t, gjson.GetBytes(upstream.lastBody, "tools").Exists())
			require.Contains(t, string(upstream.lastBody), "lookup")
			require.NotContains(t, rec.Body.String(), "run_officejs")
			require.Equal(t, 10, result.Usage.InputTokens)
			require.Equal(t, 2, result.Usage.OutputTokens)
			if stream {
				var name, arguments strings.Builder
				var finishes int
				for _, chunk := range chatBPSChunks(t, rec.Body.String()) {
					for _, call := range chunk.Get("choices.0.delta.tool_calls").Array() {
						require.Zero(t, call.Get("index").Int())
						if call.Get("id").Exists() {
							require.Equal(t, "call_chat_tool", call.Get("id").String())
						}
						_, _ = name.WriteString(call.Get("function.name").String())
						_, _ = arguments.WriteString(call.Get("function.arguments").String())
					}
					if chunk.Get("choices.0.finish_reason").String() == "tool_calls" {
						finishes++
					}
				}
				require.Equal(t, "lookup", name.String())
				require.JSONEq(t, `{"city":"Tokyo"}`, arguments.String())
				require.Equal(t, 1, finishes)
			} else {
				require.True(t, json.Valid(rec.Body.Bytes()), rec.Body.String())
				response := gjson.ParseBytes(rec.Body.Bytes())
				require.Equal(t, "tool_calls", response.Get("choices.0.finish_reason").String())
				calls := response.Get("choices.0.message.tool_calls").Array()
				require.Len(t, calls, 1)
				require.Equal(t, "call_chat_tool", calls[0].Get("id").String())
				require.Equal(t, "function", calls[0].Get("type").String())
				require.Equal(t, "lookup", calls[0].Get("function.name").String())
				require.JSONEq(t, `{"city":"Tokyo"}`, calls[0].Get("function.arguments").String())
			}
		})
	}
}

func TestForwardAsChatCompletions_BPSDisabledOrModelMissUsesNative(t *testing.T) {
	for _, scenario := range []string{"disabled", "model_miss"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", scenario, stream), func(t *testing.T) {
				upstream := &excelBPSWSTestUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{
					resp: bpsCompletionResponse(http.StatusOK, chatBPSTextWire("gpt-6-astra")),
				}}
				svc := openAIClientToolsTestService(upstream.httpUpstreamRecorder)
				svc.httpUpstream = upstream
				account := excelAccount()
				account.Extra[ExcelBPSTransportKey] = "ws"
				if scenario == "disabled" {
					account.Extra["openai_excel_bps"] = false
				} else {
					account.Extra["openai_excel_bps_models"] = []string{"gpt-6-sol"}
				}
				body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hello"}],"stream":%t}`, stream))
				c, rec := chatBPSContext(body)

				result, err := svc.ForwardAsChatCompletions(t.Context(), c, account, body, "", "")

				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.requests, 1)
				require.Equal(t, "chatgpt.com", upstream.lastReq.URL.Host)
				require.Equal(t, "/backend-api/codex/responses", upstream.lastReq.URL.Path)
				require.Equal(t, 1, upstream.httpCalls)
				require.Zero(t, upstream.wsCalls, "BPS WS setting must not change the native route")
				require.Empty(t, rec.Header().Get("X-Sub2API-BPS-Transport"))
				require.Contains(t, rec.Body.String(), "Hello from BPS")
				if stream {
					chatBPSChunks(t, rec.Body.String())
				} else {
					require.True(t, json.Valid(rec.Body.Bytes()), rec.Body.String())
					require.Equal(t, "chat.completion", gjson.GetBytes(rec.Body.Bytes(), "object").String())
				}
			})
		}
	}
}

func TestForwardAsChatCompletions_BPSErrorsDoNotReplayOrAppendResponse(t *testing.T) {
	for _, scenario := range []string{"response_failed", "http_rejection"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", scenario, stream), func(t *testing.T) {
				wire := bpsFailureWire(t, "response.failed", "invalid_value", "invalid_request_error", 0)
				status := http.StatusOK
				if scenario == "http_rejection" {
					status = http.StatusBadRequest
					wire = `{"error":{"code":"invalid_value","message":"invalid request"}}`
				} else if stream {
					wire = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial answer\"}\n\n" + wire
				}
				upstream := &httpUpstreamRecorder{resp: bpsCompletionResponse(status, wire)}
				svc := openAIClientToolsTestService(upstream)
				body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hello"}],"stream":%t}`, stream))
				c, rec := chatBPSContext(body)

				_, err := svc.ForwardAsChatCompletions(t.Context(), c, excelAccount(), body, "", "")

				require.Error(t, err)
				require.Len(t, upstream.requests, 1, "accepted failures and request errors are not replayed")
				require.Equal(t, basispoints.ResponsesURL, upstream.lastReq.URL.String())
				require.True(t, IsResponseCommitted(c), "handler must not append a second error")
				var failover *UpstreamFailoverError
				require.NotErrorAs(t, err, &failover)
				require.NotContains(t, rec.Body.String(), "response.failed")
				require.NotContains(t, rec.Body.String(), "response.completed")
				if scenario == "response_failed" && stream {
					require.Equal(t, http.StatusOK, rec.Code)
					var errors int
					var text strings.Builder
					for _, chunk := range chatBPSChunks(t, rec.Body.String()) {
						_, _ = text.WriteString(chunk.Get("choices.0.delta.content").String())
						if chunk.Get("error").Exists() {
							errors++
							require.Equal(t, "invalid_value", chunk.Get("error.code").String())
							require.False(t, chunk.Get("choices").Exists())
						}
						require.Empty(t, chunk.Get("choices.0.finish_reason").String(), "failure must not finish as success")
					}
					require.Equal(t, "partial answer", text.String())
					require.Equal(t, 1, errors)
				} else {
					require.Equal(t, http.StatusBadRequest, rec.Code)
					require.True(t, json.Valid(rec.Body.Bytes()), "expected one JSON error: %s", rec.Body.String())
					require.True(t, gjson.GetBytes(rec.Body.Bytes(), "error").IsObject())
					require.False(t, gjson.GetBytes(rec.Body.Bytes(), "choices").Exists())
					require.NotContains(t, rec.Body.String(), "[DONE]")
				}
			})
		}
	}
}

func TestForwardAsChatCompletions_BPSNormalIncomplete(t *testing.T) {
	for _, tc := range []struct {
		reason string
		finish string
	}{
		{"max_output_tokens", "length"},
		{"content_filter", "content_filter"},
	} {
		t.Run(tc.reason+"/bridge", func(t *testing.T) {
			// Inspect the real BPS bridge before asserting the Chat contract.
			// normalizeFailureEvent must not erase a normal incomplete reason.
			_, bridge, err := basispoints.Prepare([]byte(`{"model":"gpt-6-astra","input":"hello","stream":true}`), "", nil)
			require.NoError(t, err)
			converted := bridge.StreamWithRepairs(t.Context(), io.NopCloser(strings.NewReader(chatBPSIncompleteWire(tc.reason))), nil, nil)
			defer func() { _ = converted.Close() }()
			wire, err := io.ReadAll(converted)
			require.NoError(t, err)
			var terminal gjson.Result
			for _, line := range strings.Split(string(wire), "\n") {
				if strings.HasPrefix(line, "data: ") {
					payload := gjson.Parse(strings.TrimPrefix(line, "data: "))
					if payload.Get("type").String() == "response.incomplete" {
						terminal = payload
					}
				}
			}
			require.True(t, terminal.Exists(), "bridge lost incomplete terminal: %s", wire)
			require.Equal(t, "incomplete", terminal.Get("response.status").String())
			require.Equal(t, tc.reason, terminal.Get("response.incomplete_details.reason").String())
			require.False(t, terminal.Get("response.error").IsObject(), "normal incomplete became a failure: %s", wire)
			require.Nil(t, basispoints.ParseUpstreamFailure([]byte(terminal.Raw)))
			require.EqualValues(t, 13, terminal.Get("response.usage.input_tokens").Int())
			require.EqualValues(t, 7, terminal.Get("response.usage.output_tokens").Int())
		})
		for _, transport := range []string{"sse", "ws"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", tc.reason, transport, stream), func(t *testing.T) {
					upstream := &excelBPSWSTestUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{
						resp: bpsCompletionResponse(http.StatusOK, chatBPSIncompleteWire(tc.reason)),
					}}
					svc := openAIClientToolsTestService(upstream.httpUpstreamRecorder)
					svc.httpUpstream = upstream
					account := excelAccount()
					account.Extra[ExcelBPSTransportKey] = transport
					body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hello"}],"stream":%t}`, stream))
					c, rec := chatBPSContext(body)

					result, err := svc.ForwardAsChatCompletions(t.Context(), c, account, body, "", "")

					require.Len(t, upstream.requests, 1, "normal incomplete must not be replayed")
					require.Equal(t, basispoints.ResponsesURL, upstream.lastReq.URL.String())
					require.NotNil(t, result)
					require.Equal(t, 13, result.Usage.InputTokens)
					require.Equal(t, 7, result.Usage.OutputTokens)
					require.NoError(t, err, "normal incomplete must retain its Chat finish reason; wire=%s", rec.Body.String())
					require.Equal(t, http.StatusOK, rec.Code)
					if stream {
						var text strings.Builder
						var finishes int
						var usage gjson.Result
						for _, chunk := range chatBPSChunks(t, rec.Body.String()) {
							require.False(t, chunk.Get("error").Exists(), chunk.Raw)
							_, _ = text.WriteString(chunk.Get("choices.0.delta.content").String())
							if chunk.Get("choices.0.finish_reason").String() == tc.finish {
								finishes++
							}
							if chunk.Get("usage").IsObject() {
								usage = chunk.Get("usage")
							}
						}
						require.Equal(t, "Partial BPS answer", text.String())
						require.Equal(t, 1, finishes)
						chatBPSAssertUsage(t, usage)
					} else {
						require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
						require.True(t, json.Valid(rec.Body.Bytes()), rec.Body.String())
						response := gjson.ParseBytes(rec.Body.Bytes())
						require.False(t, response.Get("error").Exists(), response.Raw)
						require.Equal(t, "chat.completion", response.Get("object").String())
						require.Equal(t, "Partial BPS answer", response.Get("choices.0.message.content").String())
						require.Equal(t, tc.finish, response.Get("choices.0.finish_reason").String())
						chatBPSAssertUsage(t, response.Get("usage"))
					}
				})
			}
		}
	}
}

func TestForwardAsChatCompletions_BPSServiceTierDoesNotSurcharge(t *testing.T) {
	for _, tier := range []string{"priority", "ultrafast"} {
		for _, transport := range []string{"sse", "ws"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", tier, transport, stream), func(t *testing.T) {
					upstream := &excelBPSWSTestUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{
						resp: bpsCompletionResponse(http.StatusOK, chatBPSTextWire("gpt-6-astra")),
					}}
					svc := openAIClientToolsTestService(upstream.httpUpstreamRecorder)
					svc.httpUpstream = upstream
					account := excelAccount()
					account.Extra[ExcelBPSTransportKey] = transport
					body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hello"}],"service_tier":%q,"stream":%t}`, tier, stream))
					c, rec := chatBPSContext(body)

					result, err := svc.ForwardAsChatCompletions(t.Context(), c, account, body, "", "")

					require.NoError(t, err)
					require.NotNil(t, result)
					require.Len(t, upstream.requests, 1)
					require.Equal(t, basispoints.ResponsesURL, upstream.lastReq.URL.String())
					require.False(t, gjson.GetBytes(upstream.lastBody, "service_tier").Exists(), "BPS does not send the requested tier")
					require.Equal(t, http.StatusOK, rec.Code)
					// RecordUsage settles this result before choosing its tier multiplier.
					resolution := ApplyOpenAIServiceTierBillingResolution(account, result)
					require.Equal(t, 1.0, serviceTierCostMultiplier(resolution.Billing),
						"BPS omitted service_tier=%q, so it must not surcharge; requested=%q observed=%q billing=%q",
						tier, resolution.Requested, resolution.Observed, resolution.Billing)
				})
			}
		}
	}
}

func chatBPSIncompleteWire(reason string) string {
	return fmt.Sprintf(`data: {"type":"response.created","response":{"id":"resp_chat_bps_incomplete","model":"gpt-6-astra","status":"in_progress","output":[]}}

data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Partial BPS answer"}

data: {"type":"response.incomplete","response":{"id":"resp_chat_bps_incomplete","model":"gpt-6-astra","status":"incomplete","error":null,"incomplete_details":{"reason":%q},"output":[{"type":"message","id":"msg_chat_bps_incomplete","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"Partial BPS answer"}]}],"usage":{"input_tokens":13,"output_tokens":7,"total_tokens":20,"input_tokens_details":{"cached_tokens":5}}}}

data: [DONE]

`, reason)
}

func chatBPSContext(body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec
}

func chatBPSTextWire(model string) string {
	return fmt.Sprintf(`data: {"type":"response.created","response":{"id":"resp_chat_bps","model":%q,"status":"in_progress","output":[]}}

data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hello from BPS"}

data: {"type":"response.completed","response":{"id":"resp_chat_bps","model":%q,"status":"completed","output":[{"type":"message","id":"msg_chat_bps","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello from BPS"}]}],"usage":{"input_tokens":13,"output_tokens":7,"total_tokens":20,"input_tokens_details":{"cached_tokens":5}}}}

data: [DONE]

`, model, model)
}

// Parse the wire independently of the production Responses-to-Chat converter.
// Every data frame must be JSON, with exactly one final Chat DONE sentinel.
func chatBPSChunks(t *testing.T, wire string) []gjson.Result {
	t.Helper()
	var chunks []gjson.Result
	var done bool
	for _, line := range strings.Split(wire, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		require.False(t, done, "frame after DONE: %s", line)
		require.True(t, strings.HasPrefix(line, "data: "), "non-Chat SSE frame: %s", line)
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			done = true
			continue
		}
		require.True(t, json.Valid([]byte(payload)), payload)
		chunk := gjson.Parse(payload)
		require.False(t, chunk.Get("type").Exists(), "Responses event leaked to Chat: %s", payload)
		chunks = append(chunks, chunk)
	}
	require.True(t, done, "missing DONE: %s", wire)
	return chunks
}

func chatBPSAssertUsage(t *testing.T, usage gjson.Result) {
	t.Helper()
	require.EqualValues(t, 13, usage.Get("prompt_tokens").Int())
	require.EqualValues(t, 7, usage.Get("completion_tokens").Int())
	require.EqualValues(t, 20, usage.Get("total_tokens").Int())
	require.EqualValues(t, 5, usage.Get("prompt_tokens_details.cached_tokens").Int())
}

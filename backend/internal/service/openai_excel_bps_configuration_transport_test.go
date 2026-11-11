package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestExcelBPSConfigurationUpdateTransports(t *testing.T) {
	for _, transport := range []string{"sse", "ws"} {
		for _, stream := range []bool{false, true} {
			for _, scenario := range []string{"effort", "schema", "clear_format", "compact"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", transport, stream, scenario), func(t *testing.T) {
					format := map[string]any{"type": "json_schema", "name": "answer", "schema": map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "integer"}}, "required": []string{"answer"}, "additionalProperties": false}}
					update := map[string]any{"type": "configuration_update", "reasoning": map[string]any{"effort": "max"}}
					source := map[string]any{"model": "gpt-6-astra", "reasoning": map[string]any{"effort": "low"}, "stream": stream}
					answer, path := "continued", "/v1/responses"
					switch scenario {
					case "schema", "compact":
						update["text"] = map[string]any{"format": format}
						answer = `{"answer":42}`
					case "clear_format":
						source["text"] = map[string]any{"format": format}
						update["text"] = map[string]any{"format": map[string]any{"type": "text"}}
					}
					source["input"] = []any{
						map[string]any{"role": "user", "content": "old question"},
						map[string]any{"type": "reasoning", "encrypted_content": "synthetic-reasoning", "summary": []any{}},
						map[string]any{"type": "compaction", "encrypted_content": "synthetic-history-state"},
						update, map[string]any{"role": "user", "content": "next question"},
					}
					output := []any{map[string]any{"type": "message", "id": "msg_config", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": answer}}}}
					if scenario == "compact" {
						path = "/v1/responses/compact"
						output = []any{map[string]any{"type": "compaction", "id": "cmp_config", "encrypted_content": "synthetic-new-state"}}
					}
					event, err := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_config", "status": "completed", "model": "gpt-6-astra", "output": output, "usage": map[string]any{"input_tokens": 12, "output_tokens": 3}}})
					require.NoError(t, err)
					recorder := &httpUpstreamRecorder{resp: bpsCompletionResponse(200, "data: "+string(event)+"\n\n")}
					u := &excelBPSWSTestUpstream{httpUpstreamRecorder: recorder}
					svc := openAIClientToolsTestService(recorder)
					svc.httpUpstream = u
					account := excelAccount()
					account.Extra[ExcelBPSTransportKey] = transport
					body, err := json.Marshal(source)
					require.NoError(t, err)
					response := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(response)
					c.Request = httptest.NewRequest("POST", path, bytes.NewReader(body))
					result, err := svc.Forward(t.Context(), c, account, body)
					require.NoError(t, err)
					require.Equal(t, 200, response.Code)
					require.Equal(t, "xhigh", gjson.GetBytes(u.lastBody, "reasoning_effort").String())
					require.Equal(t, "xhigh", optionalStringValue(result.ReasoningEffort))
					require.Equal(t, "max", optionalStringValue(result.RequestedReasoningEffort))
					require.NotContains(t, string(u.lastBody), "configuration_update")
					require.Contains(t, string(u.lastBody), "synthetic-reasoning")
					require.Contains(t, string(u.lastBody), "synthetic-history-state")
					require.Equal(t, transport, response.Header().Get("X-Sub2API-BPS-Transport"))
					if transport == "ws" {
						require.Equal(t, 1, u.wsCalls)
						require.Zero(t, u.httpCalls)
					} else {
						require.Equal(t, 1, u.httpCalls)
						require.Zero(t, u.wsCalls)
					}
					if scenario == "compact" {
						require.Contains(t, string(u.lastBody), "compaction_trigger")
						require.Contains(t, response.Body.String(), "synthetic-new-state")
					} else {
						require.NotContains(t, response.Body.String(), "response.failed")
						if scenario == "schema" {
							require.Contains(t, string(u.lastBody), "exactly one JSON value")
						} else {
							require.Contains(t, response.Body.String(), answer)
							require.NotContains(t, string(u.lastBody), "exactly one JSON value")
						}
					}
				})
			}
		}
	}
}

func TestExcelBPSConfigurationInvalidUpdateStopsBeforeUpstream(t *testing.T) {
	for _, transport := range []string{"sse", "ws"} {
		t.Run(transport, func(t *testing.T) {
			u := &excelBPSWSTestUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{}}
			svc := openAIClientToolsTestService(u.httpUpstreamRecorder)
			svc.httpUpstream = u
			account := excelAccount()
			account.Extra[ExcelBPSTransportKey] = transport
			body := []byte(`{"model":"gpt-6-astra","input":[{"type":"configuration_update","reasoning":{"effort":7}},{"role":"user","content":"hello"}]}`)
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
			result, err := svc.Forward(t.Context(), c, account, body)
			require.Error(t, err)
			require.Nil(t, result)
			require.Equal(t, 400, response.Code)
			require.Equal(t, "basispoints_request_invalid", gjson.Get(response.Body.String(), "error.code").String())
			require.Contains(t, gjson.Get(response.Body.String(), "error.message").String(), "input[0].configuration_update")
			require.Zero(t, u.wsCalls)
			require.Zero(t, u.httpCalls)
		})
	}
}

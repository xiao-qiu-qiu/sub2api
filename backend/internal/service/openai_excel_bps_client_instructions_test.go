//go:build unit

package service

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestExcelBPSClientInstructionsFollowSavedSettings(t *testing.T) {
	for _, transport := range []string{"sse", "ws"} {
		for _, withTools := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/tools=%t", transport, withTools), func(t *testing.T) {
				account := excelAccount()
				account.Extra[ExcelBPSTransportKey] = transport
				upstream := &excelBPSWSTestUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{}}
				gateway := openAIClientToolsTestService(upstream.httpUpstreamRecorder)
				gateway.httpUpstream = upstream
				gateway.settingService = &SettingService{settingRepo: newStubSettingRepo()}
				tools := ""
				if withTools {
					tools = `,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]`
				}
				body := []byte(`{"model":"gpt-6-astra","stream":true,"instructions":"Original client instruction","input":"Reply exactly OK","excel_bps_client_instructions":"untrusted request override"` + tools + `}`)
				for _, custom := range []string{"", "Use client context.\n\nKeep answers concise.", "Use the updated client context.", ""} {
					require.NoError(t, gateway.settingService.persistSystemSettingsUpdates(context.Background(), map[string]string{SettingKeyExcelBPSClientInstructions: custom}))
					upstream.resp = bpsInstructionsTestResponse()
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
					_, err := gateway.Forward(context.Background(), c, account, body)
					require.NoError(t, err)
					input := gjson.GetBytes(upstream.lastBody, "input").Array()
					require.Len(t, input, 3)
					require.Equal(t, "Original client instruction", input[0].Get("content.0.text").String())
					expected := custom
					if expected == "" {
						expected = basispoints.DefaultClientInstructions
					}
					protocol := input[1].Get("content.0.text").String()
					require.Equal(t, "developer", input[1].Get("role").String())
					require.Contains(t, protocol, expected+"\n\n")
					require.NotContains(t, protocol, "untrusted request override")
					if custom != "" {
						require.NotContains(t, protocol, basispoints.DefaultClientInstructions)
					}
					if withTools {
						require.Contains(t, protocol, "Client tool catalog:")
						require.Contains(t, protocol, "lookup")
						require.Contains(t, protocol, "run_officejs")
					} else {
						require.Contains(t, protocol, "No client tools are available")
					}
					require.Equal(t, "Reply exactly OK", input[2].Get("content.0.text").String())
				}
				if transport == "ws" {
					require.Equal(t, 4, upstream.wsCalls)
					require.Zero(t, upstream.httpCalls)
				} else {
					require.Equal(t, 4, upstream.httpCalls)
					require.Zero(t, upstream.wsCalls)
				}
			})
		}
	}
}

//go:build unit

package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const bpsTextTestProtocol = basispoints.DefaultClientInstructions + "\n\nNo client tools are available for this request. Return assistant text and do not call tools."

func bpsInstructionsTestResponse() *http.Response {
	wire := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n" +
		`data: {"type":"response.completed","response":{"id":"resp_bps_instructions","status":"completed","model":"gpt-6-astra","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]}}` + "\n\n"
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
}

func assertBPSTestInstructions(t *testing.T, body []byte, prompt string, wantProtocol bool) {
	t.Helper()
	input := gjson.GetBytes(body, "input").Array()
	if wantProtocol {
		require.Len(t, input, 2)
		require.Equal(t, "developer", input[0].Get("role").String())
		require.Equal(t, bpsTextTestProtocol, input[0].Get("content.0.text").String())
	} else {
		require.Len(t, input, 1, "disabled tests must send only the user test text")
	}
	require.JSONEq(t, fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}`, prompt), input[len(input)-1].Raw)
	require.False(t, gjson.GetBytes(body, "instructions").Exists())
	require.False(t, gjson.GetBytes(body, "inject_default_instructions").Exists())
	require.False(t, gjson.GetBytes(body, "skip_protocol_instructions").Exists())
}

func TestAccountConnectionBPSProtocolInstructions(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mode         string
		options      []AccountTestOptions
		wantProtocol bool
	}{
		{"omitted preserves background probes", AccountTestModeDefault, nil, true},
		{"explicit false", AccountTestModeDefault, []AccountTestOptions{{InjectDefaultInstructions: false}}, false},
		{"explicit true", AccountTestModeDefault, []AccountTestOptions{{InjectDefaultInstructions: true}}, true},
		{"compact omitted", AccountTestModeCompact, nil, true},
		{"compact explicit false", AccountTestModeCompact, []AccountTestOptions{{InjectDefaultInstructions: false}}, true},
		{"compact explicit true", AccountTestModeCompact, []AccountTestOptions{{InjectDefaultInstructions: true}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := excelAccount()
			upstream := &httpUpstreamRecorder{resp: bpsInstructionsTestResponse()}
			svc := &AccountTestService{
				accountRepo:          &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}},
				openaiGatewayService: openAIClientToolsTestService(upstream),
			}
			c, recorder := newTestContext()
			const prompt = "Reply exactly OK"

			require.NoError(t, svc.TestAccountConnection(c, account.ID, "gpt-6-astra", prompt, tc.mode, tc.options...))
			require.Len(t, upstream.requests, 1)
			require.Equal(t, "bps.openai.com", upstream.lastReq.URL.Host)
			assertBPSTestInstructions(t, upstream.lastBody, prompt, tc.wantProtocol)
			require.Nil(t, c.Request.Context().Value(bpsTestSkipProtocolInstructionsKey{}), "the override must remain scoped to the forwarded test")
			require.Contains(t, recorder.Body.String(), `"type":"test_complete"`)
		})
	}
}

func TestAccountConnectionBPSModelScopedInstructionSwitch(t *testing.T) {
	for _, tc := range []struct {
		name          string
		model         string
		mapping       map[string]any
		bpsModels     []string
		upstreamModel string
		wantBPS       bool
	}{
		{"native model", "gpt-5.6-sol", nil, []string{"gpt-6-astra"}, "gpt-5.6-sol", false},
		{"native alias", "native-alias", map[string]any{"native-alias": "gpt-5.6-sol"}, []string{"native-alias", "gpt-6-astra"}, "gpt-5.6-sol", false},
		{"native default", "", nil, []string{"gpt-6-astra"}, openai.DefaultTestModel, false},
		{"native mapped default", "", map[string]any{openai.DefaultTestModel: "gpt-5.6-sol"}, []string{"gpt-6-astra"}, "gpt-5.6-sol", false},
		{"bps alias", "bps-alias", map[string]any{"bps-alias": "gpt-6-astra"}, []string{"gpt-6-astra"}, "gpt-6-astra", true},
		{"bps default", "", nil, []string{openai.DefaultTestModel}, openai.DefaultTestModel, true},
		{"bps mapped default", "", map[string]any{openai.DefaultTestModel: "gpt-6-astra"}, []string{"gpt-6-astra"}, "gpt-6-astra", true},
	} {
		for _, inject := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/inject=%t", tc.name, inject), func(t *testing.T) {
				account := excelAccount()
				account.Extra["openai_excel_bps_models"] = tc.bpsModels
				if tc.mapping != nil {
					account.Credentials["model_mapping"] = tc.mapping
				}
				nativeUpstream := &httpUpstreamRecorder{resp: bpsInstructionsTestResponse()}
				bpsUpstream := &httpUpstreamRecorder{resp: bpsInstructionsTestResponse()}
				gateway := openAIClientToolsTestService(bpsUpstream)
				svc := &AccountTestService{
					accountRepo:          &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}},
					httpUpstream:         nativeUpstream,
					openaiGatewayService: gateway,
					cfg:                  gateway.cfg,
				}
				c, recorder := newTestContext()
				const prompt = "Reply exactly OK"

				require.NoError(t, svc.TestAccountConnection(c, account.ID, tc.model, prompt, AccountTestModeDefault, AccountTestOptions{InjectDefaultInstructions: inject}))
				if tc.wantBPS {
					require.Empty(t, nativeUpstream.requests)
					require.Len(t, bpsUpstream.requests, 1)
					require.Equal(t, "bps.openai.com", bpsUpstream.lastReq.URL.Host)
					require.Equal(t, tc.upstreamModel, gjson.GetBytes(bpsUpstream.lastBody, "model").String())
					assertBPSTestInstructions(t, bpsUpstream.lastBody, prompt, inject)
				} else {
					require.Empty(t, bpsUpstream.requests, "native models must use the ordinary Codex test path")
					require.Len(t, nativeUpstream.requests, 1)
					require.Equal(t, chatgptCodexAPIURL, nativeUpstream.lastReq.URL.String())
					require.Equal(t, tc.upstreamModel, gjson.GetBytes(nativeUpstream.lastBody, "model").String())
					require.Len(t, gjson.GetBytes(nativeUpstream.lastBody, "input").Array(), 1)
					require.Equal(t, prompt, gjson.GetBytes(nativeUpstream.lastBody, "input.0.content.0.text").String())
					wantInstructions := ""
					if inject {
						wantInstructions = openai.AccountTestInstructions
					}
					require.Equal(t, wantInstructions, gjson.GetBytes(nativeUpstream.lastBody, "instructions").String())
				}
				require.Contains(t, recorder.Body.String(), `"type":"test_complete"`)
			})
		}
	}
}

func TestExcelBPSBusinessRequestKeepsProtocolInstructions(t *testing.T) {
	for _, extra := range []string{"", `,"inject_default_instructions":false,"skip_protocol_instructions":true`} {
		t.Run(fmt.Sprintf("client flags=%t", extra != ""), func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: bpsInstructionsTestResponse()}
			gateway := openAIClientToolsTestService(upstream)
			body := []byte(`{"model":"gpt-6-astra","stream":true,"input":"Reply exactly OK"` + extra + `}`)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))

			_, err := gateway.Forward(context.Background(), c, excelAccount(), body)
			require.NoError(t, err)
			require.Len(t, upstream.requests, 1)
			assertBPSTestInstructions(t, upstream.lastBody, "Reply exactly OK", true)
		})
	}
}

func TestAccountConnectionBPSToolsIgnoresInstructionSwitch(t *testing.T) {
	for _, inject := range []bool{false, true} {
		t.Run(fmt.Sprintf("inject=%t", inject), func(t *testing.T) {
			account := excelAccount()
			upstream := &bpsProbeUpstream{}
			svc := bpsProbeTestService(upstream)
			svc.accountRepo = &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
			c, recorder := newTestContext()

			require.NoError(t, svc.TestAccountConnection(c, account.ID, "gpt-6-astra", "", AccountTestModeBPSTools, AccountTestOptions{InjectDefaultInstructions: inject}))
			require.Len(t, upstream.bodies, 3)
			for i, body := range upstream.bodies {
				require.Equal(t, "developer", gjson.GetBytes(body, "input.0.role").String())
				protocol := gjson.GetBytes(body, "input.0.content.0.text").String()
				if i == 0 {
					require.Equal(t, bpsTextTestProtocol, protocol)
				} else {
					require.Contains(t, protocol, "Client tool catalog:")
					require.Contains(t, protocol, bpsAccountProbeTool)
				}
			}
			require.Contains(t, recorder.Body.String(), `"type":"test_complete"`)
		})
	}
}

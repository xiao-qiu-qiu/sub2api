//go:build unit

package service

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAccountTestService_TextPromptReachesUpstream(t *testing.T) {
	const prompt = "ciallo! 请原样回复 \"连接测试\"\n第二行"
	for _, tc := range []struct {
		name, platform, accountType, model, promptPath string
		response                                       func() *http.Response
		options                                        []AccountTestOptions
		wantInstructions                               string
	}{
		{"claude oauth", PlatformAnthropic, AccountTypeOAuth, "claude-sonnet-4-6", "messages.0.content.0.text", adaptiveCNAnthropicTestResponse, nil, ""},
		{"claude api key", PlatformAnthropic, AccountTypeAPIKey, "claude-sonnet-4-6", "messages.0.content.0.text", adaptiveCNAnthropicTestResponse, nil, ""},
		{"antigravity api key", PlatformAntigravity, AccountTypeAPIKey, "claude-sonnet-4-6", "messages.0.content.0.text", adaptiveCNAnthropicTestResponse, nil, ""},
		{"bedrock api key", PlatformAnthropic, AccountTypeBedrock, "claude-sonnet-4-6", "messages.0.content.0.text", func() *http.Response {
			return newJSONResponse(http.StatusOK, `{"content":[{"text":"ok"}]}`)
		}, nil, ""},
		{"openai oauth", PlatformOpenAI, AccountTypeOAuth, "gpt-6-astra", "input.0.content.0.text", adaptiveCNResponsesTestResponse, nil, ""},
		{"openai api key", PlatformOpenAI, AccountTypeAPIKey, "gpt-6-astra", "input.0.content.0.text", adaptiveCNResponsesTestResponse, nil, ""},
		{"openai oauth instructions disabled", PlatformOpenAI, AccountTypeOAuth, "gpt-6-astra", "input.0.content.0.text", adaptiveCNResponsesTestResponse, []AccountTestOptions{{InjectDefaultInstructions: false}}, ""},
		{"openai api key instructions disabled", PlatformOpenAI, AccountTypeAPIKey, "gpt-6-astra", "input.0.content.0.text", adaptiveCNResponsesTestResponse, []AccountTestOptions{{InjectDefaultInstructions: false}}, ""},
		{"openai oauth instructions enabled", PlatformOpenAI, AccountTypeOAuth, "gpt-6-astra", "input.0.content.0.text", adaptiveCNResponsesTestResponse, []AccountTestOptions{{InjectDefaultInstructions: true}}, openai.AccountTestInstructions},
		{"openai api key instructions enabled", PlatformOpenAI, AccountTypeAPIKey, "gpt-6-astra", "input.0.content.0.text", adaptiveCNResponsesTestResponse, []AccountTestOptions{{InjectDefaultInstructions: true}}, openai.AccountTestInstructions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{
				ID: 901, Platform: tc.platform, Type: tc.accountType,
				Status: StatusActive, Concurrency: 1,
				Credentials: map[string]any{
					"api_key": "test-key", "access_token": "test-token",
					"base_url": "https://upstream.example", "auth_mode": "apikey",
				},
			}
			svc, upstream := adaptiveCNAccountTestService(account, tc.response())
			c, recorder := newTestContext()

			require.NoError(t, svc.TestAccountConnection(c, account.ID, tc.model, "  "+prompt+"  ", AccountTestModeDefault, tc.options...))
			require.Len(t, upstream.requests, 1)
			require.Equal(t, prompt, gjson.GetBytes(upstream.lastBody, tc.promptPath).String())
			if tc.platform == PlatformOpenAI {
				instructions := gjson.GetBytes(upstream.lastBody, "instructions")
				require.True(t, instructions.Exists(), "OAuth-compatible instructions field must be present")
				require.Equal(t, gjson.String, instructions.Type)
				require.Equal(t, tc.wantInstructions, instructions.String())
			}
			require.Contains(t, recorder.Body.String(), `"type":"test_complete"`)
		})
	}
}

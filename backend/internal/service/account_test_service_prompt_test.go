//go:build unit

package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAccountTestService_TextPromptReachesUpstream(t *testing.T) {
	const prompt = "ciallo! 请原样回复 \"连接测试\"\n第二行"
	for _, tc := range []struct {
		name, platform, accountType, model, promptPath string
		response                                       func() *http.Response
	}{
		{"claude oauth", PlatformAnthropic, AccountTypeOAuth, "claude-sonnet-4-6", "messages.0.content.0.text", adaptiveCNAnthropicTestResponse},
		{"claude api key", PlatformAnthropic, AccountTypeAPIKey, "claude-sonnet-4-6", "messages.0.content.0.text", adaptiveCNAnthropicTestResponse},
		{"antigravity api key", PlatformAntigravity, AccountTypeAPIKey, "claude-sonnet-4-6", "messages.0.content.0.text", adaptiveCNAnthropicTestResponse},
		{"bedrock api key", PlatformAnthropic, AccountTypeBedrock, "claude-sonnet-4-6", "messages.0.content.0.text", func() *http.Response {
			return newJSONResponse(http.StatusOK, `{"content":[{"text":"ok"}]}`)
		}},
		{"openai oauth", PlatformOpenAI, AccountTypeOAuth, "gpt-6-astra", "input.0.content.0.text", adaptiveCNResponsesTestResponse},
		{"openai api key", PlatformOpenAI, AccountTypeAPIKey, "gpt-6-astra", "input.0.content.0.text", adaptiveCNResponsesTestResponse},
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

			require.NoError(t, svc.TestAccountConnection(c, account.ID, tc.model, "  "+prompt+"  ", AccountTestModeDefault))
			require.Len(t, upstream.requests, 1)
			require.Equal(t, prompt, gjson.GetBytes(upstream.lastBody, tc.promptPath).String())
			require.Contains(t, recorder.Body.String(), `"type":"test_complete"`)
		})
	}
}

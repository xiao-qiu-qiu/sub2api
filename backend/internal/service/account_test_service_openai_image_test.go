package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountTestService_OpenAIImageOAuthHandlesOutputItemDoneFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)

	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
			},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"ig_123\",\"type\":\"image_generation_call\",\"result\":\"aGVsbG8=\",\"revised_prompt\":\"draw a cat\",\"output_format\":\"png\"}}\n\n" +
					"data: {\"type\":\"response.completed\",\"response\":{\"created_at\":1710000006,\"tool_usage\":{\"image_gen\":{\"images\":1}},\"output\":[]}}\n\n" +
					"data: [DONE]\n\n",
			)),
		},
	}
	svc := &AccountTestService{httpUpstream: upstream}
	account := &Account{
		ID:       53,
		Name:     "openai-oauth",
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "token-123",
		},
	}

	err := svc.testOpenAIImageOAuth(c, context.Background(), account, "gpt-image-1", "draw a cat")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(upstream.lastReq.Context()))
	require.Contains(t, rec.Body.String(), "Calling Codex /responses image tool")
	require.Contains(t, rec.Body.String(), "data:image/png;base64,aGVsbG8=")
	require.Contains(t, rec.Body.String(), "\"success\":true")
}

func TestAccountTestService_OpenAIImageAPIKeyUsesConfiguredV1BaseURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)

	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
			},
			Body: io.NopCloser(strings.NewReader(`{"data":[{"b64_json":"aGVsbG8=","revised_prompt":"draw a cat"}]}`)),
		},
	}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{},
	}
	account := &Account{
		ID:       54,
		Name:     "openai-apikey",
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "test-api-key",
			"base_url": "https://image-upstream.example/v1",
		},
	}

	err := svc.testOpenAIImageAPIKey(c, context.Background(), account, "gpt-image-2", "draw a cat")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(upstream.lastReq.Context()))
	require.Equal(t, "https://image-upstream.example/v1/images/generations", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer test-api-key", upstream.lastReq.Header.Get("Authorization"))
	require.Contains(t, rec.Body.String(), "data:image/png;base64,aGVsbG8=")
	require.Contains(t, rec.Body.String(), "\"success\":true")
}

func TestAccountTestService_OpenAIImagePreviewOutputFormat(t *testing.T) {
	// A valid 1x1 GIF: the preview must preserve these bytes even when metadata disagrees.
	const gifBase64 = "R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"
	gin.SetMode(gin.TestMode)

	for _, route := range []struct {
		name         string
		accountType  string
		model        string
		wantPath     string
		responses    bool
		itemFallback bool
	}{
		{name: "api_key", accountType: AccountTypeAPIKey, model: "gpt-image-2", wantPath: "/v1/images/generations"},
		{name: "oauth_direct", accountType: AccountTypeOAuth, model: "gpt-image-2.6-flare", wantPath: "/backend-api/codex/images/generations"},
		{name: "oauth_responses_completed", accountType: AccountTypeOAuth, model: "gpt-image-1", wantPath: "/backend-api/codex/responses", responses: true},
		{name: "oauth_responses_item_fallback", accountType: AccountTypeOAuth, model: "gpt-image-1", wantPath: "/backend-api/codex/responses", responses: true, itemFallback: true},
	} {
		t.Run(route.name, func(t *testing.T) {
			for _, test := range []struct {
				name       string
				encoded    string
				itemFormat string
				topFormat  string
				wantMIME   string
				directOnly bool
			}{
				{name: "gif_missing_format", encoded: gifBase64, wantMIME: "image/gif"},
				{name: "gif_wrong_item_format", encoded: gifBase64, itemFormat: "png", wantMIME: "image/gif"},
				{name: "gif_wrong_top_format", encoded: gifBase64, topFormat: "jpeg", wantMIME: "image/gif"},
				{name: "gif_wrong_item_and_top_format", encoded: gifBase64, itemFormat: "webp", topFormat: "png", wantMIME: "image/gif"},
				{name: "unknown_bytes_use_item_format", encoded: "aGVsbG8=", itemFormat: "webp", topFormat: "jpeg", wantMIME: "image/webp", directOnly: true},
				{name: "unknown_bytes_use_top_format", encoded: "aGVsbG8=", topFormat: "jpeg", wantMIME: "image/jpeg", directOnly: true},
			} {
				if route.responses && test.directOnly {
					continue
				}
				t.Run(test.name, func(t *testing.T) {
					marshal := func(value any) string {
						t.Helper()
						body, err := json.Marshal(value)
						require.NoError(t, err)
						return string(body)
					}
					item := map[string]any{"revised_prompt": "draw a cat"}
					if test.itemFormat != "" {
						item["output_format"] = test.itemFormat
					}
					var body, contentType string
					if route.responses {
						item["id"] = "ig_preview"
						item["type"] = "image_generation_call"
						item["result"] = test.encoded
						response := map[string]any{
							"created_at": 1710000006,
							"output":     []any{item},
						}
						if test.topFormat != "" {
							response["tools"] = []any{map[string]any{"type": "image_generation", "output_format": test.topFormat}}
						}
						if route.itemFallback {
							response["output"] = []any{}
							body = "data: " + marshal(map[string]any{"type": "response.output_item.done", "item": item}) + "\n\n"
						}
						body += "data: " + marshal(map[string]any{"type": "response.completed", "response": response}) + "\n\ndata: [DONE]\n\n"
						contentType = "text/event-stream"
					} else {
						item["b64_json"] = test.encoded
						response := map[string]any{"data": []any{item}}
						if test.topFormat != "" {
							response["output_format"] = test.topFormat
						}
						body = marshal(response)
						contentType = "application/json"
					}

					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)
					upstream := &httpUpstreamRecorder{resp: &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{contentType}},
						Body:       io.NopCloser(strings.NewReader(body)),
					}}
					svc := &AccountTestService{httpUpstream: upstream, cfg: &config.Config{}}
					account := &Account{
						ID: 54, Name: "openai-image-preview", Platform: PlatformOpenAI, Type: route.accountType,
						Credentials: map[string]any{
							"api_key": "test-api-key", "access_token": "test-token", "base_url": "https://image-upstream.example/v1",
						},
					}
					var err error
					if route.accountType == AccountTypeAPIKey {
						err = svc.testOpenAIImageAPIKey(c, context.Background(), account, route.model, "draw a cat")
					} else {
						err = svc.testOpenAIImageOAuth(c, context.Background(), account, route.model, "draw a cat")
					}
					require.NoError(t, err)
					require.NotNil(t, upstream.lastReq)
					require.Equal(t, route.wantPath, upstream.lastReq.URL.Path)
					require.Len(t, upstream.requests, 1)

					var images, contents, completions []TestEvent
					for _, line := range strings.Split(rec.Body.String(), "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						var event TestEvent
						require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
						require.NotEqual(t, "error", event.Type, event.Error)
						switch event.Type {
						case "image":
							images = append(images, event)
						case "content":
							contents = append(contents, event)
						case "test_complete":
							completions = append(completions, event)
						}
					}
					require.Len(t, images, 1)
					require.Equal(t, test.wantMIME, images[0].MimeType)
					require.Equal(t, "data:"+test.wantMIME+";base64,"+test.encoded, images[0].ImageURL)
					require.Contains(t, contents, TestEvent{Type: "content", Text: "draw a cat"})
					require.Len(t, completions, 1)
					require.True(t, completions[0].Success)
				})
			}
		})
	}
}

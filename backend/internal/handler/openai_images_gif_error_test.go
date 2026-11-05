//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type openAIImagesGIFErrorUpstream struct {
	service.HTTPUpstream
	statusCode     int
	body           string
	accountIDs     []int64
	paths          []string
	beforeResponse func()
}

func (u *openAIImagesGIFErrorUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.accountIDs = append(u.accountIDs, accountID)
	u.paths = append(u.paths, req.URL.Path)
	if u.beforeResponse != nil {
		u.beforeResponse()
	}
	return &http.Response{
		StatusCode: u.statusCode,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(u.body)),
	}, nil
}

func newOpenAIImagesGIFErrorHandler(t *testing.T, accountType string, upstream *openAIImagesGIFErrorUpstream) (*OpenAIGatewayHandler, *gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	credentials := map[string]any{"api_key": "sk-test", "base_url": "https://api.openai.com/v1"}
	if accountType == service.AccountTypeOAuth {
		credentials = map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"}
	}
	accounts := []service.Account{
		{
			ID: 1, Name: "gif-account-1", Platform: service.PlatformOpenAI, Type: accountType,
			Status: service.StatusActive, Schedulable: true, Priority: 0, Credentials: credentials,
		},
		{
			ID: 2, Name: "gif-account-2", Platform: service.PlatformOpenAI, Type: accountType,
			Status: service.StatusActive, Schedulable: true, Priority: 1, Credentials: credentials,
		},
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gateway := service.NewOpenAIGatewayService(
		openAIImagesFailoverAccountRepo{accounts: accounts},
		nil, nil, nil, nil, nil, nil, nil, cfg,
		nil, nil, nil, nil, nil, upstream,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	h := NewOpenAIGatewayHandler(
		gateway, service.NewConcurrencyService(nil), billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, nil, nil, nil, cfg,
	)
	h.maxAccountSwitches = 10

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"gpt-image-2.6-flare","prompt":"a looping dot","output_format":"gif"}`,
	))
	c.Request.Header.Set("Content-Type", "application/json")
	groupID := int64(3130)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID: 99, UserID: 100, GroupID: &groupID,
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, AllowImageGeneration: true},
		User:  &service.User{ID: 100},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})
	return h, c, rec
}

func openAIImagesGIFErrorPNGResponse(t *testing.T) string {
	t.Helper()
	var body bytes.Buffer
	require.NoError(t, png.Encode(&body, image.NewRGBA(image.Rect(0, 0, 32, 16))))
	// A claimed GIF format must not hide actual PNG bytes.
	return fmt.Sprintf(`{"output_format":"gif","data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(body.Bytes()))
}

func requireOpenAIImagesGIFSingleRequest(t *testing.T, upstream *openAIImagesGIFErrorUpstream, accountType string) {
	t.Helper()
	require.Equal(t, []int64{1}, upstream.accountIDs, "GIF errors must not replay or switch accounts")
	wantPath := "/v1/images/generations"
	if accountType == service.AccountTypeOAuth {
		wantPath = "/backend-api/codex/images/generations"
	}
	require.Equal(t, []string{wantPath}, upstream.paths, "GIF errors must not fall back to Responses")
}

func TestOpenAIGatewayHandlerImages_GIFErrorsAreWrittenOnceWithoutReplay(t *testing.T) {
	pngResponse := openAIImagesGIFErrorPNGResponse(t)
	for _, accountType := range []string{service.AccountTypeAPIKey, service.AccountTypeOAuth} {
		for _, tc := range []struct {
			name        string
			statusCode  int
			body        string
			wantStatus  int
			wantType    string
			wantCode    string
			wantMessage string
		}{
			{
				name: "actual_png", statusCode: http.StatusOK, body: pngResponse,
				wantStatus: http.StatusBadGateway, wantType: "upstream_error", wantCode: "invalid_gif_output",
				wantMessage: "Images API returned non-GIF bytes for output_format=gif",
			},
			{
				name: "gif_not_enabled", statusCode: http.StatusBadRequest,
				body:       `{"error":{"type":"invalid_request_error","code":"gif_not_enabled","message":"GIF output is not enabled for this request."}}`,
				wantStatus: http.StatusBadRequest, wantType: "invalid_request_error", wantCode: "gif_not_enabled",
				wantMessage: "GIF output is not enabled for this request.",
			},
		} {
			t.Run(accountType+"/"+tc.name, func(t *testing.T) {
				upstream := &openAIImagesGIFErrorUpstream{statusCode: tc.statusCode, body: tc.body}
				h, c, rec := newOpenAIImagesGIFErrorHandler(t, accountType, upstream)
				h.cfg.Gateway.ImageNonstreamKeepaliveInterval = 1
				upstream.beforeResponse = func() {
					require.False(t, service.OpenAIImagesJSONKeepalivePresent(c), "GIF must retain a real final HTTP status even when keepalive is configured")
				}
				h.Images(c)

				requireOpenAIImagesGIFSingleRequest(t, upstream, accountType)
				require.True(t, c.Writer.Written())
				require.Equal(t, tc.wantStatus, rec.Code)
				require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
				require.True(t, json.Valid(rec.Body.Bytes()), "response must contain exactly one JSON document: %s", rec.Body.String())
				require.Equal(t, tc.wantType, gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
				require.Equal(t, tc.wantCode, gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
				require.Equal(t, tc.wantMessage, gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
				require.Equal(t, 1, strings.Count(rec.Body.String(), tc.wantMessage), "an already-written 400 must not be appended again")
			})
		}
	}
}

func TestAsyncImageHandler_GIFInvalidOutputPreservesGatewayError(t *testing.T) {
	for _, accountType := range []string{service.AccountTypeAPIKey, service.AccountTypeOAuth} {
		t.Run(accountType, func(t *testing.T) {
			upstream := &openAIImagesGIFErrorUpstream{statusCode: http.StatusOK, body: openAIImagesGIFErrorPNGResponse(t)}
			h, c, _ := newOpenAIImagesGIFErrorHandler(t, accountType, upstream)
			body, err := io.ReadAll(c.Request.Body)
			require.NoError(t, err)
			c.Request.URL.Path += "/async"
			taskCtx, recorder, cancel := newAsyncImageContext(c, body, time.Minute)
			defer cancel()
			store := &asyncImageMemoryStore{tasks: make(map[string]*service.ImageTaskRecord)}
			tasks := service.NewImageTaskServiceWithUploader(store, nil, time.Hour, time.Minute)
			owner := service.ImageTaskOwner{UserID: 100, APIKeyID: 99}
			task, err := tasks.Create(context.Background(), owner)
			require.NoError(t, err)

			// Execute the real async worker deterministically, including its gateway dispatch and error persistence.
			async := NewAsyncImageHandler(tasks, h)
			async.run(task.ID, service.PlatformOpenAI, taskCtx, recorder, cancel)
			got, err := tasks.Get(context.Background(), owner, task.ID)
			require.NoError(t, err)
			require.Equal(t, service.ImageTaskStatusFailed, got.Status)
			require.Equal(t, http.StatusBadGateway, got.HTTPStatus)
			require.Empty(t, got.Result)
			require.Equal(t, "upstream_error", gjson.GetBytes(got.Error, "type").String())
			require.Equal(t, "invalid_gif_output", gjson.GetBytes(got.Error, "code").String())
			require.Equal(t, "Images API returned non-GIF bytes for output_format=gif", gjson.GetBytes(got.Error, "message").String())
			requireOpenAIImagesGIFSingleRequest(t, upstream, accountType)
		})
	}
}

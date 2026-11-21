package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestRequestCaptureSettings(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
	for _, body := range []map[string]any{
		{"request_capture_quota_mib": 0}, {"request_capture_quota_mib": -1}, {"request_capture_quota_mib": 1.5},
		{"request_capture_retention_days": 0}, {"request_capture_retention_days": 31}, {"request_capture_quota_mib": int64(1<<63 - 1)},
	} {
		r := doUpdateSettings(t, h, body, nil)
		require.Equal(t, 400, r.Code, r.Body.String())
	}
	r := doUpdateSettings(t, h, map[string]any{"request_capture_enabled": true, "request_capture_quota_mib": 42, "request_capture_retention_days": 30}, nil)
	require.Equal(t, 200, r.Code, r.Body.String())
	require.True(t, gjson.Get(r.Body.String(), "data.request_capture_enabled").Bool())
	require.Equal(t, "42", repo.values[service.SettingKeyRequestCaptureQuotaMiB])
	r = doUpdateSettings(t, h, map[string]any{"site_name": "keep capture"}, nil)
	require.Equal(t, 200, r.Code, r.Body.String())
	require.EqualValues(t, 30, gjson.Get(r.Body.String(), "data.request_capture_retention_days").Int())
	require.True(t, gjson.Get(r.Body.String(), "data.request_capture_enabled").Bool())
	public, err := h.settingService.GetPublicSettings(context.Background())
	require.NoError(t, err)
	raw, err := json.Marshal(public)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "request_capture")
}

func TestRequestCaptureDistillationSettingsRoundTrip(t *testing.T) {
	const (
		include       = service.SettingKeyRequestCaptureIncludeDistillation
		minOutput     = service.SettingKeyRequestCaptureDistillationMinOutputTokens
		maxCache      = service.SettingKeyRequestCaptureDistillationMaxCacheReadTokens
		nonStreamOnly = service.SettingKeyRequestCaptureDistillationNonStreamOnly
	)
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
	getSettings := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
		h.GetSettings(ctx)
		return recorder
	}
	assertSettings := func(recorder *httptest.ResponseRecorder, want map[string]string) {
		t.Helper()
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		for key, value := range want {
			require.Equal(t, value, gjson.GetBytes(recorder.Body.Bytes(), "data."+key).Raw, key)
		}
	}

	defaults := map[string]string{include: "false", minOutput: "3000", maxCache: "0", nonStreamOnly: "true"}
	assertSettings(getSettings(), defaults)
	assertSettings(doUpdateSettings(t, h, map[string]any{"site_name": "legacy client"}, nil), defaults)
	for key := range defaults {
		require.NotContains(t, repo.values, key)
	}

	before, err := h.settingService.GetAllSettings(context.Background())
	require.NoError(t, err)
	initial := map[string]any{include: true, minOutput: 4500, maxCache: int64(9007199254740991), nonStreamOnly: false}
	configured := map[string]string{include: "true", minOutput: "4500", maxCache: "9007199254740991", nonStreamOnly: "false"}
	assertSettings(doUpdateSettings(t, h, initial, nil), configured)
	assertSettings(getSettings(), configured)
	for key, value := range configured {
		require.Equal(t, value, repo.values[key], key)
	}
	after, err := h.settingService.GetAllSettings(context.Background())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{include, minOutput, maxCache, nonStreamOnly}, diffSettings(before, after, nil, nil, UpdateSettingsRequest{}))

	assertSettings(doUpdateSettings(t, h, map[string]any{"site_name": "keep filters"}, nil), configured)
	for key := range configured {
		require.NotContains(t, repo.lastUpdates, key)
	}

	cleared := map[string]string{include: "false", minOutput: "0", maxCache: "0", nonStreamOnly: "false"}
	assertSettings(doUpdateSettings(t, h, map[string]any{include: false, minOutput: 0, maxCache: 0}, nil), cleared)
	assertSettings(getSettings(), cleared)
	for key, value := range cleared {
		require.Equal(t, value, repo.values[key], key)
	}
}

func TestRequestCaptureDistillationSettingsRejectInvalidValues(t *testing.T) {
	const (
		minOutput = service.SettingKeyRequestCaptureDistillationMinOutputTokens
		maxCache  = service.SettingKeyRequestCaptureDistillationMaxCacheReadTokens
		include   = service.SettingKeyRequestCaptureIncludeDistillation
	)
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
	for _, key := range []string{minOutput, maxCache} {
		for _, invalid := range []any{-1, 1.5, int64(9007199254740992), json.Number("9223372036854775808"), "3000", nil} {
			recorder := doUpdateSettings(t, h, map[string]any{key: invalid, "site_name": "must not persist"}, nil)
			require.Equal(t, http.StatusBadRequest, recorder.Code, "%s=%v: %s", key, invalid, recorder.Body.String())
		}
	}
	for _, invalid := range []any{"false", nil} {
		recorder := doUpdateSettings(t, h, map[string]any{include: invalid, "site_name": "must not persist"}, nil)
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	}
	require.Empty(t, repo.values)
}

func TestRequestCaptureGateAndValidation(t *testing.T) {
	h := NewRequestCaptureHandler(nil, nil, nil, nil)
	router := gin.New()
	called := false
	router.GET("/capture", h.Gate, func(c *gin.Context) { called = true; c.Status(200) })
	r := httptest.NewRecorder()
	router.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/capture", nil))
	require.Equal(t, 404, r.Code)
	require.False(t, called)
	for _, body := range []string{`{"target_id":0,"duration_minutes":1}`, `{"target_type":"user","target_id":1,"duration_minutes":1441}`, `{"target_type":"all","target_id":1,"duration_minutes":10}`, `{"target_type":"user","target_id":1,"duration_minutes":1.5}`} {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest(http.MethodPost, "/capture", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.Create(c)
		require.Equal(t, 400, r.Code, r.Body.String())
	}
}

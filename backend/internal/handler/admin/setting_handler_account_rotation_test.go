package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAccountRotationAdminRejectsInvalidSettingsWithoutWriting(t *testing.T) {
	valid := map[string]any{
		"enabled": false, "mode": "aggressive", "work_minutes": 300,
		"rest_min_minutes": 10, "rest_max_minutes": 30, "stagger_percent": 10,
	}
	cases := map[string]any{
		"null object": nil, "empty object": map[string]any{}, "array": []any{},
		"string": "aggressive", "number": 1,
	}
	for _, field := range []string{"enabled", "mode", "work_minutes", "rest_min_minutes", "rest_max_minutes", "stagger_percent"} {
		missing, null := make(map[string]any), make(map[string]any)
		for key, value := range valid {
			missing[key], null[key] = value, value
		}
		delete(missing, field)
		null[field] = nil
		cases[field+" missing"], cases[field+" null"] = missing, null
	}
	for field, values := range map[string][]any{
		"enabled":          {"true", 1},
		"mode":             {"", "AGGRESSIVE", " sticky ", "invalid", 1},
		"work_minutes":     {0, -1, 1441, 1.5, "300"},
		"rest_min_minutes": {0, -1, 121, 31, 1.5},
		"rest_max_minutes": {0, -1, 9, 121, 1.5},
		"stagger_percent":  {-1, 31, 1.5},
		"unknown":          {true},
	} {
		for index, value := range values {
			object := make(map[string]any)
			for key, original := range valid {
				object[key] = original
			}
			object[field] = value
			name, err := json.Marshal([]any{field, index, value})
			require.NoError(t, err)
			cases[string(name)] = object
		}
	}
	for name, object := range cases {
		t.Run(name, func(t *testing.T) {
			h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
			rec := doUpdateSettings(t, h, map[string]any{"account_rotation_settings": object}, nil)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Empty(t, repo.lastUpdates)
		})
	}
	// A numeric overflow must fail during JSON binding before any write.
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings", strings.NewReader(`{"account_rotation_settings":{"work_minutes":999999999999999999999999}}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UpdateSettings(c)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Empty(t, repo.lastUpdates)
}

func TestAccountRotationAdminRoundTripBoundariesAndOmission(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
	initial, err := h.settingService.GetAccountRotationSettings(t.Context())
	require.NoError(t, err)
	require.False(t, initial.Enabled)
	for _, settings := range []*service.AccountRotationSettings{
		{Enabled: true, Mode: "aggressive", WorkMinutes: 1, RestMinMinutes: 1, RestMaxMinutes: 1, StaggerPercent: 0},
		{Enabled: true, Mode: "sticky", WorkMinutes: 1440, RestMinMinutes: 120, RestMaxMinutes: 120, StaggerPercent: 30},
		service.DefaultAccountRotationSettings(),
	} {
		rec := doUpdateSettings(t, h, map[string]any{"account_rotation_settings": settings}, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var actual service.AccountRotationSettings
		require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(rec.Body.Bytes(), "data.account_rotation_settings").Raw), &actual))
		require.NotEmpty(t, actual.Revision)
		expected := *settings
		expected.Revision = actual.Revision
		require.Equal(t, expected, actual)
		cached, err := h.settingService.GetAccountRotationSettings(t.Context())
		require.NoError(t, err)
		require.Equal(t, &actual, cached)
	}
	stored := repo.values[service.SettingKeyAccountRotationSettings]
	rec := doUpdateSettings(t, h, map[string]any{"site_name": "Rotation settings test"}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, stored, repo.values[service.SettingKeyAccountRotationSettings])
	require.NotContains(t, repo.lastUpdates, service.SettingKeyAccountRotationSettings)
	get := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(get)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	h.GetSettings(c)
	require.Equal(t, http.StatusOK, get.Code, get.Body.String())
	require.JSONEq(t, stored, gjson.GetBytes(get.Body.Bytes(), "data.account_rotation_settings").Raw)
}

func TestAccountRotationAdminIgnoresClientRevision(t *testing.T) {
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{})
	settings := service.DefaultAccountRotationSettings()
	settings.Enabled, settings.Revision = true, "forged-revision"
	rec := doUpdateSettings(t, h, map[string]any{"account_rotation_settings": settings}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	revision := gjson.GetBytes(rec.Body.Bytes(), "data.account_rotation_settings.revision").String()
	require.NotEmpty(t, revision)
	require.NotEqual(t, "forged-revision", revision)
	settings.Revision = "second-forged-revision"
	rec = doUpdateSettings(t, h, map[string]any{"account_rotation_settings": settings}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, revision, gjson.GetBytes(rec.Body.Bytes(), "data.account_rotation_settings.revision").String())
}

func TestAccountRotationAdminRejectsRegionalConcurrencyEnable(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
	h.settingService = service.NewSettingService(repo, &config.Config{
		Runtime: config.RuntimeConfig{RegionalConcurrencyFile: "regional-concurrency.json"},
	})
	settings := service.DefaultAccountRotationSettings()
	settings.Enabled = true
	rec := doUpdateSettings(t, h, map[string]any{"account_rotation_settings": settings}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "runtime.regional_concurrency_file")
	require.Contains(t, rec.Body.String(), "regional concurrency mode is not supported")
	require.Empty(t, repo.lastUpdates)
	settings.Enabled = false
	rec = doUpdateSettings(t, h, map[string]any{"account_rotation_settings": settings}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

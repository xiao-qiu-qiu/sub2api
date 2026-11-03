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
		"recovery_min_interval_ms": 500, "recovery_max_interval_ms": 2000,
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
		"enabled":                          {"true", 1},
		"scheduled_hold_enabled":           {nil, "true", "false", 0, 1, []any{}, map[string]any{}},
		"early_rest_enabled":               {nil, "true", 1, []any{}, map[string]any{}},
		"early_rest_window_minutes":        {nil, 0, -1, 1441, 1.5, json.Number("1.0"), json.Number("1e1"), "20", true, []any{}, map[string]any{}, json.Number("999999999999999999999999")},
		"early_rest_concurrency_threshold": {nil, 0, -1, 10001, 1.5, json.Number("1.0"), "2", true, []any{}, map[string]any{}, json.Number("999999999999999999999999")},
		"early_rest_seconds":               {nil, 0, -1, 7201, 1.5, json.Number("1.0"), "120", true, []any{}, map[string]any{}, json.Number("999999999999999999999999")},
		"mode":                             {"", "AGGRESSIVE", " sticky ", "invalid", 1},
		"work_minutes":                     {0, -1, 1441, 1.5, "300"},
		"rest_min_minutes":                 {0, -1, 121, 31, 1.5},
		"rest_max_minutes":                 {0, -1, 9, 121, 1.5},
		"stagger_percent":                  {-1, 31, 1.5},
		"recovery_min_interval_ms":         {nil, 0, -1, 60001, 2001, 1.5, "500", true, []any{}, map[string]any{}, json.Number("999999999999999999999999")},
		"recovery_max_interval_ms":         {nil, 0, -1, 60001, 499, 1.5, "2000", true, []any{}, map[string]any{}, json.Number("999999999999999999999999")},
		"unknown":                          {true},
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
	require.True(t, initial.ScheduledHoldEnabled)
	require.Equal(t, 20, initial.EarlyRestWindowMinutes)
	require.Equal(t, 2, initial.EarlyRestConcurrencyThreshold)
	require.Equal(t, 120, initial.EarlyRestSeconds)
	for _, settings := range []*service.AccountRotationSettings{
		{Enabled: true, ScheduledHoldEnabled: false, EarlyRestWindowMinutes: 1, EarlyRestConcurrencyThreshold: 1, EarlyRestSeconds: 1, Mode: "aggressive", WorkMinutes: 1, RestMinMinutes: 1, RestMaxMinutes: 1, StaggerPercent: 0, RecoveryMinIntervalMS: 1, RecoveryMaxIntervalMS: 1},
		{Enabled: true, ScheduledHoldEnabled: true, EarlyRestEnabled: true, EarlyRestWindowMinutes: 1440, EarlyRestConcurrencyThreshold: 10000, EarlyRestSeconds: 7200, Mode: "sticky", WorkMinutes: 1440, RestMinMinutes: 120, RestMaxMinutes: 120, StaggerPercent: 30, RecoveryMinIntervalMS: 60000, RecoveryMaxIntervalMS: 60000},
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

func TestAccountRotationAdminLegacyRecoveryDefaultsPreservePolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fields   map[string]any
		min, max int
	}{
		{"both missing", nil, 500, 2000},
		{"max missing", map[string]any{"recovery_min_interval_ms": 100}, 100, 2000},
		{"min missing", map[string]any{"recovery_max_interval_ms": 60000}, 500, 60000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
			legacy := map[string]any{
				"enabled": false, "early_rest_enabled": true, "mode": "sticky", "work_minutes": 75,
				"rest_min_minutes": 2, "rest_max_minutes": 9, "stagger_percent": 23,
			}
			for field, value := range tc.fields {
				legacy[field] = value
			}
			rec := doUpdateSettings(t, h, map[string]any{"account_rotation_settings": legacy}, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var actual service.AccountRotationSettings
			require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(rec.Body.Bytes(), "data.account_rotation_settings").Raw), &actual))
			require.Equal(t, service.AccountRotationSettings{
				Enabled: false, EarlyRestEnabled: true, Mode: "sticky", WorkMinutes: 75,
				ScheduledHoldEnabled: true, EarlyRestWindowMinutes: 20,
				EarlyRestConcurrencyThreshold: 2, EarlyRestSeconds: 120,
				RestMinMinutes: 2, RestMaxMinutes: 9, StaggerPercent: 23, Revision: actual.Revision,
				RecoveryMinIntervalMS: tc.min, RecoveryMaxIntervalMS: tc.max,
			}, actual)
			require.Equal(t, int64(tc.min), gjson.Get(repo.values[service.SettingKeyAccountRotationSettings], "recovery_min_interval_ms").Int())
			require.Equal(t, int64(tc.max), gjson.Get(repo.values[service.SettingKeyAccountRotationSettings], "recovery_max_interval_ms").Int())
		})
	}
}

func TestAccountRotationAdminLegacyClientDefaultsEarlyRestToFalse(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
	settings := service.DefaultAccountRotationSettings()
	settings.Enabled, settings.EarlyRestEnabled = true, true
	first := doUpdateSettings(t, h, map[string]any{"account_rotation_settings": settings}, nil)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	firstRevision := gjson.GetBytes(first.Body.Bytes(), "data.account_rotation_settings.revision").String()
	legacy := map[string]any{
		"enabled": true, "mode": "aggressive", "work_minutes": 300,
		"rest_min_minutes": 10, "rest_max_minutes": 30, "stagger_percent": 10,
	}
	second := doUpdateSettings(t, h, map[string]any{"account_rotation_settings": legacy}, nil)
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	require.False(t, gjson.GetBytes(second.Body.Bytes(), "data.account_rotation_settings.early_rest_enabled").Bool())
	require.NotEqual(t, firstRevision, gjson.GetBytes(second.Body.Bytes(), "data.account_rotation_settings.revision").String())
	require.False(t, gjson.Get(repo.values[service.SettingKeyAccountRotationSettings], "early_rest_enabled").Bool())
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

func TestAccountRotationAdminScheduledHoldOmissionAndExplicitFalse(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
	legacy := map[string]any{
		"enabled": true, "mode": "sticky", "work_minutes": 75,
		"rest_min_minutes": 2, "rest_max_minutes": 9, "stagger_percent": 23,
	}
	for _, tc := range []struct {
		name string
		hold *bool
	}{
		{"legacy defaults", nil},
		{"explicit false", new(false)},
		{"legacy resets to default", nil},
		{"explicit true", new(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delete(legacy, "scheduled_hold_enabled")
			expected := true
			if tc.hold != nil {
				legacy["scheduled_hold_enabled"], expected = *tc.hold, *tc.hold
			}
			rec := doUpdateSettings(t, h, map[string]any{"account_rotation_settings": legacy}, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, expected, gjson.GetBytes(rec.Body.Bytes(), "data.account_rotation_settings.scheduled_hold_enabled").Bool())
			require.True(t, gjson.Get(repo.values[service.SettingKeyAccountRotationSettings], "scheduled_hold_enabled").Exists())
			require.Equal(t, expected, gjson.Get(repo.values[service.SettingKeyAccountRotationSettings], "scheduled_hold_enabled").Bool())
			for field, value := range map[string]int64{"early_rest_window_minutes": 20, "early_rest_concurrency_threshold": 2, "early_rest_seconds": 120} {
				require.Equal(t, value, gjson.GetBytes(rec.Body.Bytes(), "data.account_rotation_settings."+field).Int())
				require.Equal(t, value, gjson.Get(repo.values[service.SettingKeyAccountRotationSettings], field).Int())
			}
		})
	}
}

func TestAccountRotationAdminNewFieldsChangeRevision(t *testing.T) {
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{})
	settings := service.DefaultAccountRotationSettings()
	save := func() string {
		t.Helper()
		rec := doUpdateSettings(t, h, map[string]any{"account_rotation_settings": settings}, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return gjson.GetBytes(rec.Body.Bytes(), "data.account_rotation_settings.revision").String()
	}
	previous := save()
	for _, change := range []func(){
		func() { settings.ScheduledHoldEnabled = false },
		func() { settings.EarlyRestWindowMinutes++ },
		func() { settings.EarlyRestConcurrencyThreshold++ },
		func() { settings.EarlyRestSeconds++ },
	} {
		change()
		next := save()
		require.NotEmpty(t, next)
		require.NotEqual(t, previous, next)
		require.Equal(t, next, save(), "unchanged content must preserve revision")
		previous = next
	}
}

func TestDecodeAccountRotationSettingsRejectsTrailingJSON(t *testing.T) {
	valid, err := json.Marshal(service.DefaultAccountRotationSettings())
	require.NoError(t, err)
	for _, trailing := range []string{" {}", " null", " invalid"} {
		t.Run(trailing, func(t *testing.T) {
			_, err := decodeAccountRotationSettings(json.RawMessage(string(valid) + trailing))
			require.Error(t, err)
		})
	}
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

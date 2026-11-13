package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func getExcelBPSClientInstructionsSettings(t *testing.T, h *SettingHandler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	h.GetSettings(c)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return rec
}

func TestSettingsExcelBPSLargeRequestHTTPFallbackRoundTripAndOmission(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{
		service.SettingKeySiteName:                   "keep site",
		service.SettingKeyExcelBPSEnabled:            "false",
		service.SettingKeyExcelBPSClientInstructions: "keep prompt",
	})
	key := service.SettingKeyExcelBPSLargeRequestHTTPFallbackEnabled
	rec := getExcelBPSClientInstructionsSettings(t, h)
	require.Equal(t, gjson.True, gjson.Get(rec.Body.String(), "data."+key).Type)
	require.NotContains(t, repo.values, key, "GET must not persist defaults")
	rec = doUpdateSettings(t, h, map[string]any{"site_name": "keep site"}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, gjson.True, gjson.Get(rec.Body.String(), "data."+key).Type)
	require.NotContains(t, repo.values, key, "omitted new setting must not be written")

	for _, enabled := range []bool{false, true, false} {
		rec = doUpdateSettings(t, h, map[string]any{key: enabled}, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, enabled, gjson.Get(rec.Body.String(), "data."+key).Bool())
		require.True(t, gjson.Get(rec.Body.String(), "data."+key).Exists())
		require.Equal(t, enabled, repo.values[key] == "true")
		if !enabled {
			require.Equal(t, "false", repo.values[key])
		}
		require.Equal(t, enabled, h.settingService.IsExcelBPSLargeRequestHTTPFallbackEnabled(context.Background()))
		for _, payload := range []map[string]any{{"site_name": "keep site"}, {key: nil}} {
			rec = doUpdateSettings(t, h, payload, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.NotContains(t, repo.lastUpdates, key)
			require.Equal(t, enabled, gjson.Get(rec.Body.String(), "data."+key).Bool())
			require.Equal(t, enabled, h.settingService.IsExcelBPSLargeRequestHTTPFallbackEnabled(context.Background()))
		}
		rec = getExcelBPSClientInstructionsSettings(t, h)
		require.Equal(t, enabled, gjson.Get(rec.Body.String(), "data."+key).Bool())
		require.True(t, gjson.Get(rec.Body.String(), "data."+key).Exists())
		require.Equal(t, "keep site", repo.values[service.SettingKeySiteName])
		require.Equal(t, "false", repo.values[service.SettingKeyExcelBPSEnabled])
		require.Equal(t, "keep prompt", repo.values[service.SettingKeyExcelBPSClientInstructions])
	}
	before := maps.Clone(repo.values)
	rec = doUpdateSettings(t, h, map[string]any{key: "false", "site_name": "must not save"}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, before, repo.values)
}

func TestSettingsExcelBPSLargeRequestHTTPFallbackAuditAndAdminOnly(t *testing.T) {
	key := service.SettingKeyExcelBPSLargeRequestHTTPFallbackEnabled
	before := &service.SystemSettings{ExcelBPSLargeRequestHTTPFallbackEnabled: true}
	after := &service.SystemSettings{ExcelBPSLargeRequestHTTPFallbackEnabled: false}
	require.Contains(t, diffSettings(before, after, nil, nil, UpdateSettingsRequest{}), key)
	require.NotContains(t, diffSettings(after, after, nil, nil, UpdateSettingsRequest{}), key)
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{key: "false"})
	public, err := h.settingService.GetPublicSettings(context.Background())
	require.NoError(t, err)
	encoded, err := json.Marshal(public)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), key)
	for _, schema := range []any{service.PublicSettings{}, service.PublicSettingsInjectionPayload{}, dto.PublicSettings{}} {
		typ := reflect.TypeOf(schema)
		for i := range typ.NumField() {
			field := typ.Field(i)
			require.NotEqual(t, "ExcelBPSLargeRequestHTTPFallbackEnabled", field.Name, typ.Name())
			require.NotEqual(t, key, field.Tag.Get("json"), typ.Name())
		}
	}
}

func TestSettingsExcelBPSClientInstructionsDefaultCustomAndOmission(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
	key := service.SettingKeyExcelBPSClientInstructions
	defaultKey := "excel_bps_default_client_instructions"
	rec := getExcelBPSClientInstructionsSettings(t, h)
	require.Equal(t, basispoints.DefaultClientInstructions, gjson.Get(rec.Body.String(), "data."+key).String())
	require.Equal(t, basispoints.DefaultClientInstructions, gjson.Get(rec.Body.String(), "data."+defaultKey).String())
	require.NotContains(t, repo.values, key, "GET must not persist the default")
	require.Equal(t, basispoints.DefaultClientInstructions, h.settingService.GetExcelBPSClientInstructions(context.Background()))
	custom := "secret-client-context\r\n\r\n  第二段\n\tkeep indentation"
	rec = doUpdateSettings(t, h, map[string]any{key: "\n　" + custom + "\t ", defaultKey: "cannot change the default"}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, custom, repo.values[key])
	require.Equal(t, custom, gjson.Get(rec.Body.String(), "data."+key).String())
	require.Equal(t, basispoints.DefaultClientInstructions, gjson.Get(rec.Body.String(), "data."+defaultKey).String())
	require.Equal(t, custom, h.settingService.GetExcelBPSClientInstructions(context.Background()))
	require.NotContains(t, repo.values, defaultKey)
	for _, payload := range []map[string]any{{"site_name": "unrelated save"}, {key: nil}, {defaultKey: "ignored"}} {
		rec = doUpdateSettings(t, h, payload, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, custom, repo.values[key])
		require.NotContains(t, repo.lastUpdates, key, "omitted/null prompt must not rewrite an old snapshot")
		require.Equal(t, custom, h.settingService.GetExcelBPSClientInstructions(context.Background()))
	}
	rec = getExcelBPSClientInstructionsSettings(t, h)
	require.Equal(t, custom, gjson.Get(rec.Body.String(), "data."+key).String())
}

func TestSettingsExcelBPSClientInstructionsReset(t *testing.T) {
	for _, reset := range []string{"", " \t\n　", basispoints.DefaultClientInstructions, "\t" + basispoints.DefaultClientInstructions + "\n"} {
		h, repo := newStepUpSwitchTestHandler(t, map[string]string{service.SettingKeyExcelBPSClientInstructions: "custom override"})
		require.Equal(t, "custom override", h.settingService.GetExcelBPSClientInstructions(context.Background()))
		rec := doUpdateSettings(t, h, map[string]any{service.SettingKeyExcelBPSClientInstructions: reset}, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Empty(t, repo.values[service.SettingKeyExcelBPSClientInstructions])
		require.Equal(t, basispoints.DefaultClientInstructions, gjson.Get(rec.Body.String(), "data.excel_bps_client_instructions").String())
		require.Equal(t, basispoints.DefaultClientInstructions, h.settingService.GetExcelBPSClientInstructions(context.Background()))
		rec = getExcelBPSClientInstructionsSettings(t, h)
		require.Equal(t, basispoints.DefaultClientInstructions, gjson.Get(rec.Body.String(), "data.excel_bps_client_instructions").String())
	}
}

func TestSettingsExcelBPSClientInstructionsUnicodeValidationBeforeWrite(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
	key := service.SettingKeyExcelBPSClientInstructions
	maximum := strings.Repeat("界🙂", service.ExcelBPSClientInstructionsMaxLength/2)
	rec := doUpdateSettings(t, h, map[string]any{key: " \n" + maximum + "\t"}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, maximum, repo.values[key])
	before := maps.Clone(repo.values)
	for _, invalid := range []any{maximum + "字", strings.Repeat("x", service.ExcelBPSClientInstructionsMaxLength+1), 123, []string{"invalid"}} {
		repo.lastUpdates = nil
		rec = doUpdateSettings(t, h, map[string]any{key: invalid, "site_name": "must not save"}, nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Equal(t, before, repo.values)
		require.Nil(t, repo.lastUpdates, "invalid prompt must reject the entire update before writing")
		require.Equal(t, maximum, h.settingService.GetExcelBPSClientInstructions(context.Background()))
	}
}

func TestSettingsExcelBPSClientInstructionsAreAdminOnly(t *testing.T) {
	custom := "private-client-instructions-never-public"
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{service.SettingKeyExcelBPSClientInstructions: custom})
	public, err := h.settingService.GetPublicSettings(context.Background())
	require.NoError(t, err)
	encoded, err := json.Marshal(public)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), custom)
	require.NotContains(t, string(encoded), "client_instructions")
	for _, schema := range []any{service.PublicSettings{}, service.PublicSettingsInjectionPayload{}, dto.PublicSettings{}} {
		typ := reflect.TypeOf(schema)
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			require.NotContains(t, field.Name, "ClientInstructions", typ.Name())
			require.NotContains(t, field.Tag.Get("json"), "client_instructions", typ.Name())
		}
	}
}

func TestSettingsExcelBPSClientInstructionsAuditDoesNotLogPrompt(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{})
	private := "unique-private-prompt-marker\nprivate second paragraph"
	rec := doUpdateSettings(t, h, map[string]any{service.SettingKeyExcelBPSClientInstructions: private}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, logs.String(), service.SettingKeyExcelBPSClientInstructions)
	require.NotContains(t, logs.String(), "unique-private-prompt-marker")
	require.NotContains(t, logs.String(), "private second paragraph")
}

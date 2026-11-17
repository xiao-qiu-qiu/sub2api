//go:build unit

package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountOpsPushPlusHandlerSavesAndRetainsRedactedToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := &opsHandlerWritableSettings{raw: `{"cooldown_minutes":60}`}
	svc := service.NewAccountOpsService(settings, opsHandlerQueue{}, nil)
	svc.SetNotificationDependencies(nil, opsHandlerEncryptor{}, true, "UTC")
	handler := NewAccountOpsHandler(svc, nil)
	router := gin.New()
	router.PUT("/webhooks/:id", handler.SaveWebhook)
	router.GET("/config", handler.GetConfig)
	send := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		return recorder
	}
	result := send(http.MethodPut, "/webhooks/push", `{"provider":"pushplus","enabled":true,"name":"值班微信","token":"synthetic-private-token","recipient":"must-not-save@example.test"}`)
	require.Equal(t, http.StatusOK, result.Code)
	require.Contains(t, settings.raw, `"token_cipher":"synthetic-cipher"`)
	require.NotContains(t, settings.raw, "synthetic-private-token")
	require.NotContains(t, settings.raw, "must-not-save")
	require.Contains(t, result.Body.String(), `"provider":"pushplus"`)
	require.Contains(t, result.Body.String(), `"token_configured":true`)
	require.NotContains(t, result.Body.String(), `"token":`)
	require.NotContains(t, result.Body.String(), "synthetic-private-token")
	require.NotContains(t, result.Body.String(), "cipher")
	result = send(http.MethodGet, "/config", "")
	require.Equal(t, http.StatusOK, result.Code)
	require.Contains(t, result.Body.String(), `"token_configured":true`)
	require.NotContains(t, result.Body.String(), "cipher")

	// Configured flags and unrelated input are excluded by the scoped DTO.
	svc.SetNotificationDependencies(nil, nil, false, "UTC")
	result = send(http.MethodPut, "/webhooks/push", `{"enabled":true,"name":"新名称","token_configured":false,"url_configured":true,"secret_configured":true}`)
	require.Equal(t, http.StatusOK, result.Code)
	require.Contains(t, result.Body.String(), `"name":"新名称"`)
	require.Contains(t, result.Body.String(), `"token_configured":true`)
	require.Contains(t, settings.raw, `"token_cipher":"synthetic-cipher"`)
	result = send(http.MethodPut, "/webhooks/unsaved", `{"provider":"pushplus","enabled":true,"token_configured":true}`)
	require.Equal(t, http.StatusBadRequest, result.Code)
	require.NotContains(t, settings.raw, `"id":"unsaved"`)
}

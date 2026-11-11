package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type creationPresetHandlerSettings struct {
	service.SettingRepository
	values map[string]string
	err    error
}

func (r *creationPresetHandlerSettings) GetValue(_ context.Context, key string) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	raw, ok := r.values[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return raw, nil
}
func (r *creationPresetHandlerSettings) Set(_ context.Context, key, raw string) error {
	if r.err != nil {
		return r.err
	}
	r.values[key] = raw
	return nil
}

const creationPresetTestPath = "/api/v1/admin/account-ops/creation-preset"
const creationPresetTestJSON = `{"version":1,"platform":"openai","type":"oauth","concurrency":7,"priority":-50,"load_factor":null,"rate_multiplier":0,"group_rate_multiplier":0,"group_ids":[],"proxy_id":null,"extra":{"cost_multiplier":0,"openai_excel_bps_transport":"ws","openai_oauth_responses_websockets_v2_mode":"http_bridge"},"model_mapping":{"source":"target"}}`

func creationPresetTestRouter(repo *creationPresetHandlerSettings, scope context.Context) *gin.Engine {
	h := NewAccountOpsHandler(service.NewAccountOpsService(repo, nil, nil), nil)
	r := gin.New()
	if scope != nil {
		r.Use(func(c *gin.Context) { c.Request = c.Request.WithContext(scope) })
	}
	r.Use((&AccountHandler{}).AuthorizeObserver)
	r.GET(creationPresetTestPath, h.GetCreationPreset)
	r.PUT(creationPresetTestPath, h.SaveCreationPreset)
	return r
}
func creationPresetHTTP(r *gin.Engine, method, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, creationPresetTestPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestAccountCreationPresetHTTPRoundTripAndNoPreset(t *testing.T) {
	repo := &creationPresetHandlerSettings{values: map[string]string{service.SettingKeyOAuthAutoConfig: "global-policy"}}
	r := creationPresetTestRouter(repo, nil)
	w := creationPresetHTTP(r, http.MethodGet, "")
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"data":null`)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	w = creationPresetHTTP(r, http.MethodPut, creationPresetTestJSON)
	require.Equal(t, 200, w.Code, w.Body.String())
	var saved struct {
		Data service.AccountCreationPreset `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &saved))
	require.Equal(t, -50, saved.Data.Priority)
	require.Nil(t, saved.Data.LoadFactor)
	require.Zero(t, saved.Data.RateMultiplier)
	require.Zero(t, saved.Data.GroupRateMultiplier)
	require.True(t, saved.Data.AutoPauseOnExpired)
	w = creationPresetHTTP(creationPresetTestRouter(repo, nil), http.MethodGet, "")
	require.Equal(t, 200, w.Code)
	var loaded struct {
		Data service.AccountCreationPreset `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &loaded))
	require.Equal(t, saved.Data, loaded.Data)
	require.Equal(t, "global-policy", repo.values[service.SettingKeyOAuthAutoConfig])
}

func TestAccountCreationPresetHTTPRejectsInvalidOrSensitiveBody(t *testing.T) {
	repo := &creationPresetHandlerSettings{values: map[string]string{}}
	r := creationPresetTestRouter(repo, nil)
	require.Equal(t, 200, creationPresetHTTP(r, http.MethodPut, creationPresetTestJSON).Code)
	before := repo.values[service.SettingKeyAccountCreationPreset]
	for _, body := range []string{"{}", "null", "{", strings.Replace(creationPresetTestJSON, `"extra":{`, `"extra":{"access_token":"secret",`, 1), strings.Replace(creationPresetTestJSON, `"extra":{`, `"extra":{"openai_excel_bps_authorization_pending":false,`, 1), creationPresetTestJSON + "{}", strings.Repeat("x", service.MaxAccountCreationPresetBytes+1)} {
		w := creationPresetHTTP(r, http.MethodPut, body)
		require.Equal(t, 400, w.Code)
		require.NotContains(t, w.Body.String(), "secret")
		require.Equal(t, before, repo.values[service.SettingKeyAccountCreationPreset])
	}
}

func TestAccountCreationPresetHTTPObserverAndScopedKeyIsolation(t *testing.T) {
	for _, ctx := range []context.Context{service.WithObserverScope(t.Context(), []int64{1}), adminaccess.WithPrincipal(t.Context(), &adminaccess.Principal{})} {
		repo := &creationPresetHandlerSettings{values: map[string]string{service.SettingKeyAccountCreationPreset: creationPresetTestJSON}}
		r := creationPresetTestRouter(repo, ctx)
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			w := creationPresetHTTP(r, method, creationPresetTestJSON)
			require.Equal(t, 403, w.Code)
			require.NotContains(t, w.Body.String(), "model_mapping")
			require.Equal(t, creationPresetTestJSON, repo.values[service.SettingKeyAccountCreationPreset])
		}
	}
}

func TestAccountCreationPresetHTTPStorageFailureDoesNotLeak(t *testing.T) {
	repo := &creationPresetHandlerSettings{values: map[string]string{}, err: errors.New("storage secret")}
	r := creationPresetTestRouter(repo, nil)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		w := creationPresetHTTP(r, method, creationPresetTestJSON)
		require.Equal(t, 503, w.Code)
		require.NotContains(t, w.Body.String(), "secret")
	}
}

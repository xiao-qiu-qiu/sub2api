package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type accountRotationAdminCache struct {
	service.ConcurrencyCache
	service.AccountRotationCache
	states             map[int64]*service.AccountRotationState
	err                error
	concurrencyBatches [][]int64
	rotationBatches    [][]int64
}

func (c *accountRotationAdminCache) GetAccountConcurrencyBatch(_ context.Context, ids []int64) (map[int64]int, error) {
	c.concurrencyBatches = append(c.concurrencyBatches, append([]int64(nil), ids...))
	counts := make(map[int64]int, len(ids))
	for _, id := range ids {
		counts[id] = 2
	}
	return counts, nil
}

func (c *accountRotationAdminCache) GetAccountsLoadBatch(_ context.Context, accounts []service.AccountWithConcurrency) (map[int64]*service.AccountLoadInfo, error) {
	ids := make([]int64, len(accounts))
	loads := make(map[int64]*service.AccountLoadInfo, len(accounts))
	for i, account := range accounts {
		ids[i] = account.ID
		loads[account.ID] = &service.AccountLoadInfo{AccountID: account.ID, CurrentConcurrency: 2}
	}
	c.concurrencyBatches = append(c.concurrencyBatches, ids)
	return loads, nil
}

func (c *accountRotationAdminCache) CheckAccountRotationBatch(_ context.Context, ids []int64, _ service.AccountRotationPolicy) (map[int64]*service.AccountRotationState, error) {
	c.rotationBatches = append(c.rotationBatches, append([]int64(nil), ids...))
	return c.states, c.err
}

func setupAccountRotationAdminRouter(t *testing.T, enabled bool) (*gin.Engine, *accountRotationAdminCache, *service.Account) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	settings := service.DefaultAccountRotationSettings()
	settings.Enabled = enabled
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	settingsService := service.NewSettingService(&settingHandlerRepoStub{values: map[string]string{
		service.SettingKeyAccountRotationSettings: string(raw),
	}}, nil)
	cache := &accountRotationAdminCache{states: map[int64]*service.AccountRotationState{
		101: {Phase: "working", StartedAt: 100, DrainAt: 18100},
		102: {Phase: "resting", RestUntil: 19000, RestSeconds: 1200},
	}}
	concurrency := service.NewConcurrencyService(cache)
	concurrency.SetAccountRotation(settingsService, nil)
	adminService := newStubAdminService()
	adminService.accounts = []service.Account{
		{ID: 101, Name: "first", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Schedulable: true, Concurrency: 4,
			Credentials: map[string]any{"email": "account@example.com", "access_token": "rotation-access-secret", "refresh_token": "rotation-refresh-secret"}},
		{ID: 102, Name: "second", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 4,
			Credentials: map[string]any{"api_key": "rotation-api-key-secret"}},
	}
	adminService.getAccountResult = &adminService.accounts[0]
	h := NewAccountHandler(adminService, nil, nil, nil, nil, nil, nil, nil, nil, concurrency, nil, nil, nil, nil)
	router := gin.New()
	router.GET("/accounts", h.List)
	router.GET("/accounts/:id", h.GetByID)
	return router, cache, &adminService.accounts[0]
}

func TestAccountRotationAdminRuntimeEnvelopesBatchAndRedactCredentials(t *testing.T) {
	for _, path := range []string{"/accounts", "/accounts?lite=1", "/accounts/101"} {
		t.Run(path, func(t *testing.T) {
			router, cache, account := setupAccountRotationAdminRouter(t, true)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			expectedIDs := []int64{101, 102}
			items := gjson.GetBytes(rec.Body.Bytes(), "data.items").Array()
			if path == "/accounts/101" {
				expectedIDs = []int64{101}
				items = []gjson.Result{gjson.GetBytes(rec.Body.Bytes(), "data")}
			}
			require.Len(t, items, len(expectedIDs))
			require.Equal(t, [][]int64{expectedIDs}, cache.concurrencyBatches)
			require.Equal(t, [][]int64{expectedIDs}, cache.rotationBatches, "one rotation batch per response")
			require.Equal(t, "working", items[0].Get("account_rotation.phase").String())
			require.EqualValues(t, 2, items[0].Get("current_concurrency").Int())
			require.EqualValues(t, 18100, items[0].Get("account_rotation.drain_at").Int())
			if len(items) == 2 {
				require.Equal(t, "resting", items[1].Get("account_rotation.phase").String())
			}
			for _, secret := range []string{"rotation-access-secret", "rotation-refresh-secret", "rotation-api-key-secret"} {
				require.NotContains(t, rec.Body.String(), secret)
			}
			// Runtime state belongs to admin response envelopes, not shared DTOs.
			plain, err := json.Marshal(dto.AccountFromService(account))
			require.NoError(t, err)
			require.False(t, gjson.GetBytes(plain, "account_rotation").Exists())
		})
	}
}

func TestAccountRotationAdminRuntimeOmittedWhenDisabledOrLookupFails(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, path := range []string{"/accounts", "/accounts?lite=1", "/accounts/101"} {
			t.Run(path+map[bool]string{false: "/disabled", true: "/lookup-error"}[enabled], func(t *testing.T) {
				router, cache, _ := setupAccountRotationAdminRouter(t, enabled)
				cache.err = errors.New("rotation state unavailable")
				// Even a partial map accompanied by an error must not look ready.
				cache.states[101] = &service.AccountRotationState{Phase: "ready"}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.NotContains(t, rec.Body.String(), `"account_rotation"`)
				if !enabled {
					require.Empty(t, cache.rotationBatches)
				}
			})
		}
	}
}

func TestAccountRotationAdminRuntimeChangesListETag(t *testing.T) {
	router, cache, _ := setupAccountRotationAdminRouter(t, true)
	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/accounts?lite=1", nil))
	require.Equal(t, http.StatusOK, first.Code)
	require.NotEmpty(t, first.Header().Get("ETag"))
	cache.states[101].Phase = "draining"
	req := httptest.NewRequest(http.MethodGet, "/accounts?lite=1", nil)
	req.Header.Set("If-None-Match", first.Header().Get("ETag"))
	changed := httptest.NewRecorder()
	router.ServeHTTP(changed, req)
	require.Equal(t, http.StatusOK, changed.Code)
	require.NotEqual(t, first.Header().Get("ETag"), changed.Header().Get("ETag"))
	require.Equal(t, "draining", gjson.GetBytes(changed.Body.Bytes(), "data.items.0.account_rotation.phase").String())
}

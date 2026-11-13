package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type accountRuntimeLoadCache struct {
	service.ConcurrencyCache
	loads     map[int64]*service.AccountLoadInfo
	counts    map[int64]int
	batches   [][]service.AccountWithConcurrency
	loadError error
}

func (s *accountRuntimeLoadCache) GetAccountsLoadBatch(_ context.Context, accounts []service.AccountWithConcurrency) (map[int64]*service.AccountLoadInfo, error) {
	s.batches = append(s.batches, append([]service.AccountWithConcurrency(nil), accounts...))
	return s.loads, s.loadError
}

func (s *accountRuntimeLoadCache) GetAccountConcurrencyBatch(_ context.Context, _ []int64) (map[int64]int, error) {
	return s.counts, nil
}

func accountRuntimeLoadRouter(cache *accountRuntimeLoadCache) (*gin.Engine, *AccountHandler) {
	gin.SetMode(gin.TestMode)
	svc := newStubAdminService()
	svc.accounts = []service.Account{{ID: 101, Concurrency: 2}, {ID: 102, Concurrency: 4}}
	svc.getAccountResult = &svc.accounts[0]
	concurrency := service.NewConcurrencyService(cache)
	concurrency.SetAccountLoadBatchCacheTTL(0)
	handler := &AccountHandler{adminService: svc, concurrencyService: concurrency}
	router := gin.New()
	router.GET("/accounts", handler.List)
	router.GET("/accounts/:id", handler.GetByID)
	return router, handler
}

func accountRuntimeLoadItems(rec *httptest.ResponseRecorder, detail bool) []gjson.Result {
	if detail {
		return []gjson.Result{gjson.GetBytes(rec.Body.Bytes(), "data")}
	}
	return gjson.GetBytes(rec.Body.Bytes(), "data.items").Array()
}

func TestAccountCurrentWaitingRuntimeEnvelopesUseOneBatch(t *testing.T) {
	for _, path := range []string{"/accounts", "/accounts?lite=1", "/accounts/101"} {
		t.Run(path, func(t *testing.T) {
			cache := &accountRuntimeLoadCache{loads: map[int64]*service.AccountLoadInfo{
				101: {AccountID: 101, CurrentConcurrency: 2, WaitingCount: 3},
				102: {AccountID: 102, CurrentConcurrency: 4},
			}}
			router, _ := accountRuntimeLoadRouter(cache)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			detail := path == "/accounts/101"
			items := accountRuntimeLoadItems(rec, detail)
			expectedBatch := []service.AccountWithConcurrency{{ID: 101, MaxConcurrency: 2}}
			if !detail {
				expectedBatch = append(expectedBatch, service.AccountWithConcurrency{ID: 102, MaxConcurrency: 4})
			}
			require.Equal(t, [][]service.AccountWithConcurrency{expectedBatch}, cache.batches)
			require.Len(t, items, len(expectedBatch))
			require.EqualValues(t, 2, items[0].Get("current_concurrency").Int())
			require.EqualValues(t, 3, items[0].Get("current_waiting").Int())
			if !detail {
				require.EqualValues(t, 4, items[1].Get("current_concurrency").Int())
				require.True(t, items[1].Get("current_waiting").Exists(), "zero queue count must remain explicit")
				require.Zero(t, items[1].Get("current_waiting").Int())
			}
		})
	}
}

func TestAccountCurrentWaitingChangesListETag(t *testing.T) {
	for _, path := range []string{"/accounts", "/accounts?lite=1"} {
		t.Run(path, func(t *testing.T) {
			cache := &accountRuntimeLoadCache{loads: map[int64]*service.AccountLoadInfo{101: {CurrentConcurrency: 2, WaitingCount: 3}}}
			router, _ := accountRuntimeLoadRouter(cache)
			first := httptest.NewRecorder()
			router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equal(t, http.StatusOK, first.Code)
			etag := first.Header().Get("ETag")
			require.NotEmpty(t, etag)
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("If-None-Match", etag)
			same := httptest.NewRecorder()
			router.ServeHTTP(same, req)
			require.Equal(t, http.StatusNotModified, same.Code)

			cache.loads[101].WaitingCount = 4
			changed := httptest.NewRecorder()
			router.ServeHTTP(changed, req)
			require.Equal(t, http.StatusOK, changed.Code)
			require.NotEqual(t, etag, changed.Header().Get("ETag"))
			require.EqualValues(t, 4, gjson.GetBytes(changed.Body.Bytes(), "data.items.0.current_waiting").Int())
		})
	}
}

func TestAccountCurrentWaitingLookupFailureAndUnavailableDefaultToZero(t *testing.T) {
	for _, path := range []string{"/accounts", "/accounts?lite=1", "/accounts/101"} {
		for _, mode := range []string{"error", "no-service", "no-cache"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				cache := &accountRuntimeLoadCache{
					loads:     map[int64]*service.AccountLoadInfo{101: {CurrentConcurrency: 2, WaitingCount: 9}},
					loadError: errors.New("load unavailable"),
				}
				router, handler := accountRuntimeLoadRouter(cache)
				if mode == "no-service" {
					handler.concurrencyService = nil
				} else if mode == "no-cache" {
					handler.concurrencyService = service.NewConcurrencyService(nil)
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				for _, item := range accountRuntimeLoadItems(rec, path == "/accounts/101") {
					require.True(t, item.Get("current_waiting").Exists())
					require.Zero(t, item.Get("current_waiting").Int())
					require.Zero(t, item.Get("current_concurrency").Int())
				}
			})
		}
	}
}

func TestAccountCurrentWaitingPreservesConcurrencySortSnapshot(t *testing.T) {
	for _, lite := range []string{"0", "1"} {
		t.Run(lite, func(t *testing.T) {
			cache := &accountRuntimeLoadCache{
				counts: map[int64]int{101: 3, 102: 2},
				loads: map[int64]*service.AccountLoadInfo{
					101: {CurrentConcurrency: 1, WaitingCount: 4},
					102: {CurrentConcurrency: 2, WaitingCount: -1},
				},
			}
			router, _ := accountRuntimeLoadRouter(cache)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/accounts?sort_by=current_concurrency&sort_order=desc&lite="+lite, nil))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			items := accountRuntimeLoadItems(rec, false)
			require.Len(t, items, 2)
			require.EqualValues(t, 101, items[0].Get("id").Int())
			require.EqualValues(t, 3, items[0].Get("current_concurrency").Int(), "the queue lookup must not overwrite the sorting snapshot")
			require.EqualValues(t, 4, items[0].Get("current_waiting").Int())
			require.Zero(t, items[1].Get("current_waiting").Int(), "queue count cannot be negative")
			require.Len(t, cache.batches, 1)
		})
	}
}

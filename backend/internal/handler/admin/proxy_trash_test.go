//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type proxyTrashServiceStub struct {
	service.AdminService
	search                 string
	page, size             int
	restoreID, permanentID int64
	err                    error
}

func (s *proxyTrashServiceStub) ListProxyTrash(_ context.Context, page, size int, search string) ([]service.ProxyTrashItem, int64, error) {
	s.page, s.size, s.search = page, size, search
	return []service.ProxyTrashItem{{ID: 9, Name: "proxy", Protocol: "http", Host: "exit.example", Port: 8080, Status: "inactive", DeletedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}}, 1, s.err
}
func (s *proxyTrashServiceStub) RestoreProxy(_ context.Context, id int64) error {
	s.restoreID = id
	return s.err
}
func (s *proxyTrashServiceStub) PermanentlyDeleteProxy(_ context.Context, id int64) error {
	s.permanentID = id
	return s.err
}

func TestProxyTrashHandlerPaginationAndCredentialFreeFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &proxyTrashServiceStub{}
	router := gin.New()
	router.GET("/proxies/trash", NewProxyHandler(svc).ListTrash)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/proxies/trash?page=2&page_size=10&search=%20exit%20", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 2, svc.page)
	require.Equal(t, 10, svc.size)
	require.Equal(t, "exit", svc.search)
	var body struct {
		Data struct {
			Items              []map[string]any `json:"items"`
			Page, Total, Pages int
			Size               int `json:"page_size"`
		}
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, 2, body.Data.Page)
	require.Equal(t, 10, body.Data.Size)
	require.Equal(t, 1, body.Data.Total)
	require.Len(t, body.Data.Items, 1)
	item := body.Data.Items[0]
	require.Len(t, item, 7)
	for _, key := range []string{"id", "name", "protocol", "host", "port", "status", "deleted_at"} {
		require.Contains(t, item, key)
	}
}

func TestProxyTrashHandlerMutationErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		method, path string
		err          error
		status       int
		reason       string
	}{
		{http.MethodPost, "/proxies/9/restore", nil, 200, ""},
		{http.MethodDelete, "/proxies/9/permanent", service.ErrProxyReferenced, 409, "PROXY_REFERENCED"},
		{http.MethodDelete, "/proxies/9/permanent", service.ErrProxyTrashNotFound, 404, "PROXY_TRASH_NOT_FOUND"},
		{http.MethodPost, "/proxies/0/restore", nil, 400, ""},
		{http.MethodDelete, "/proxies/-1/permanent", nil, 400, ""},
	} {
		t.Run(tc.method+tc.path+tc.reason, func(t *testing.T) {
			svc := &proxyTrashServiceStub{err: tc.err}
			h := NewProxyHandler(svc)
			router := gin.New()
			router.POST("/proxies/:id/restore", h.Restore)
			router.DELETE("/proxies/:id/permanent", h.PermanentDelete)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.reason != "" {
				require.Contains(t, w.Body.String(), `"reason":"`+tc.reason+`"`)
			}
			if tc.status == 400 {
				require.Zero(t, svc.restoreID)
				require.Zero(t, svc.permanentID)
			}
		})
	}
}

func TestProxyTrashRestoreWithoutConfiguredMihomo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, system := range []*SystemHandler{nil, {}} {
		svc := &proxyTrashServiceStub{}
		router := gin.New()
		router.POST("/proxies/:id/restore", system.ProxyTrashMihomoContext(), NewProxyHandler(svc).Restore)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/proxies/9/restore", nil))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.EqualValues(t, 9, svc.restoreID)
	}
}

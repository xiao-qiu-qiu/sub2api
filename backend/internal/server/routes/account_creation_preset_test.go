package routes

import (
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountCreationPresetRoutesRequireAdministrator(t *testing.T) {
	for _, observer := range []bool{false, true} {
		r := gin.New()
		group := r.Group("/api/v1/admin")
		if observer {
			group.Use(func(c *gin.Context) {
				c.Request = c.Request.WithContext(service.WithObserverScope(c.Request.Context(), []int64{1}))
			})
			group.Use((&admin.AccountHandler{}).AuthorizeObserver)
		} else {
			group.Use(gin.HandlerFunc(middleware.NewAdminAuthMiddleware(nil, nil, nil, nil)))
		}
		registerAccountCreationPresetRoutes(group, &handler.Handlers{Admin: &handler.AdminHandlers{AccountOps: admin.NewAccountOpsHandler(nil, nil)}})
		for _, method := range []string{"GET", "PUT"} {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(method, "/api/v1/admin/account-ops/creation-preset", nil))
			want := 401
			if observer {
				want = 403
			}
			require.Equal(t, want, w.Code)
		}
	}
}

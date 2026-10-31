package admin

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// ProxyTrashMihomoContext reuses the system handler's existing kernel. Register
// it on the restore route before ProxyHandler.Restore.
func (h *SystemHandler) ProxyTrashMihomoContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h != nil && h.kernel != nil {
			c.Request = c.Request.WithContext(service.WithProxyTrashMihomoBinder(c.Request.Context(), h.kernel))
		}
		c.Next()
	}
}

// ListTrash handles GET /api/v1/admin/proxies/trash.
func (h *ProxyHandler) ListTrash(c *gin.Context) {
	svc, ok := h.adminService.(service.ProxyTrashAdminService)
	if !ok {
		response.ErrorFrom(c, service.ErrProxyTrashUnavailable)
		return
	}
	page, size := response.ParsePagination(c)
	search := strings.TrimSpace(c.Query("search"))
	if len([]rune(search)) > 100 {
		search = string([]rune(search)[:100])
	}
	items, total, err := svc.ListProxyTrash(c.Request.Context(), page, size, search)
	if response.ErrorFrom(c, err) {
		return
	}
	if items == nil {
		items = []service.ProxyTrashItem{}
	}
	response.Paginated(c, items, total, page, size)
}

// Restore handles POST /api/v1/admin/proxies/:id/restore.
func (h *ProxyHandler) Restore(c *gin.Context) {
	h.mutateProxyTrash(c, false)
}

// PermanentDelete handles DELETE /api/v1/admin/proxies/:id/permanent.
func (h *ProxyHandler) PermanentDelete(c *gin.Context) {
	h.mutateProxyTrash(c, true)
}

func (h *ProxyHandler) mutateProxyTrash(c *gin.Context, permanent bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid proxy ID")
		return
	}
	svc, ok := h.adminService.(service.ProxyTrashAdminService)
	if !ok {
		response.ErrorFrom(c, service.ErrProxyTrashUnavailable)
		return
	}
	message := "Proxy restored successfully"
	if permanent {
		err = svc.PermanentlyDeleteProxy(c.Request.Context(), id)
		message = "Proxy permanently deleted successfully"
	} else {
		err = svc.RestoreProxy(c.Request.Context(), id)
	}
	if response.ErrorFrom(c, err) {
		return
	}
	response.Success(c, gin.H{"message": message})
}

package admin

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// ListTrash handles GET /admin/accounts/trash.
func (h *AccountHandler) ListTrash(c *gin.Context) {
	svc, ok := h.adminService.(service.AccountTrashAdminService)
	if !ok {
		response.ErrorFrom(c, service.ErrAccountTrashUnavailable)
		return
	}
	page, pageSize := response.ParsePagination(c)
	items, total, err := svc.ListAccountTrash(c.Request.Context(), page, pageSize, strings.TrimSpace(c.Query("search")))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if items == nil {
		items = []service.AccountTrashItem{}
	}
	response.Paginated(c, items, total, page, pageSize)
}

// Restore handles POST /admin/accounts/:id/restore.
func (h *AccountHandler) Restore(c *gin.Context) {
	h.accountTrashAction(c, false)
}

// PermanentDelete handles DELETE /admin/accounts/:id/permanent.
func (h *AccountHandler) PermanentDelete(c *gin.Context) {
	h.accountTrashAction(c, true)
}

func (h *AccountHandler) accountTrashAction(c *gin.Context, permanent bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	svc, ok := h.adminService.(service.AccountTrashAdminService)
	if !ok {
		response.ErrorFrom(c, service.ErrAccountTrashUnavailable)
		return
	}
	message := "Account restored successfully"
	if permanent {
		err = svc.PermanentlyDeleteAccount(c.Request.Context(), id)
		message = "Account permanently deleted successfully"
	} else {
		err = svc.RestoreAccount(c.Request.Context(), id)
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": message})
}

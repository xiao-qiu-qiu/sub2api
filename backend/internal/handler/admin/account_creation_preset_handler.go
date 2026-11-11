package admin

import (
	"io"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountOpsHandler) GetCreationPreset(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !creationPresetAdministrator(c) {
		return
	}
	preset, err := h.svc.GetAccountCreationPreset(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "Account creation preset unavailable")
		return
	}
	response.Success(c, preset)
}

func (h *AccountOpsHandler) SaveCreationPreset(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !creationPresetAdministrator(c) {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, service.MaxAccountCreationPresetBytes)
	data, err := io.ReadAll(c.Request.Body)
	if err != nil {
		response.BadRequest(c, "Invalid or oversized account creation preset")
		return
	}
	preset, err := service.DecodeAccountCreationPreset(data)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	saved, err := h.svc.SaveAccountCreationPreset(c.Request.Context(), preset)
	if err != nil {
		if infraerrors.Code(err) >= 500 {
			response.Error(c, http.StatusServiceUnavailable, "Could not save account creation preset")
		} else {
			response.ErrorFrom(c, err)
		}
		return
	}
	response.Success(c, saved)
}

func creationPresetAdministrator(c *gin.Context) bool {
	_, observer := service.ObserverGroupIDs(c.Request.Context())
	if observer || adminaccess.FromContext(c.Request.Context()) != nil {
		response.Forbidden(c, "Global creation presets require an administrator")
		return false
	}
	return true
}

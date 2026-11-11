package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
)

func registerAccountCreationPresetRoutes(admin *gin.RouterGroup, h *handler.Handlers) {
	admin.GET("/account-ops/creation-preset", h.Admin.AccountOps.GetCreationPreset)
	admin.PUT("/account-ops/creation-preset", h.Admin.AccountOps.SaveCreationPreset)
}

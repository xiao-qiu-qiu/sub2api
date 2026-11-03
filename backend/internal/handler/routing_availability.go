package handler

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// RoutingAvailability reports only routing state for the authenticated key.
// It deliberately bypasses inference admission, usage recording and bindings.
func (h *OpenAIGatewayHandler) RoutingAvailability(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || key == nil {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if key.GroupID == nil || key.Group == nil || key.Group.ID != *key.GroupID {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "API key must be assigned to a group")
		return
	}
	query := c.Request.URL.Query()
	for _, name := range []string{"model", "service_tier", "transport", "require_compact"} {
		if values, ok := query[name]; ok && (len(values) != 1 || values[0] == "") {
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Query parameters must have exactly one non-empty value")
			return
		}
	}
	request := service.RoutingAvailabilityRequest{Model: query.Get("model"), ServiceTier: query.Get("service_tier"), Transport: query.Get("transport")}
	switch query.Get("require_compact") {
	case "", "false":
	case "true":
		request.RequireCompact = true
	default:
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "require_compact must be true or false")
		return
	}
	if err := request.Validate(); err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	c.JSON(http.StatusOK, h.gatewayService.RoutingAvailability(c.Request.Context(), key, request))
}

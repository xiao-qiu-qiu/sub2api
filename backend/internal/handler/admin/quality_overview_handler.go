package admin

import (
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// AccountQualityOverview GET /admin/account-quality-overview.
func (h *ScheduledTestHandler) AccountQualityOverview(c *gin.Context) {
	filter := service.QualityOverviewFilter{Search: c.Query("search"), TestChannel: c.Query("test_channel")}
	for _, field := range []struct {
		name string
		dest *int
	}{{"page", &filter.Page}, {"page_size", &filter.PageSize}} {
		if raw, exists := c.GetQuery(field.name); exists {
			value, err := strconv.Atoi(raw)
			if err != nil || value <= 0 {
				response.BadRequest(c, "invalid "+field.name)
				return
			}
			*field.dest = value
		} else if _, exists = c.Request.URL.Query()[field.name]; exists {
			response.BadRequest(c, "invalid "+field.name)
			return
		}
	}
	if raw, exists := c.Request.URL.Query()["group_id"]; exists {
		id, err := strconv.ParseInt(raw[0], 10, 64)
		if err != nil || id < 0 {
			response.BadRequest(c, "invalid group_id")
			return
		}
		filter.GroupID = &id
	}
	if err := filter.Normalize(); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	page, err := h.scheduledTestSvc.AccountQualityOverview(c.Request.Context(), filter)
	if err != nil {
		response.InternalError(c, "Failed to load account quality overview")
		return
	}
	c.JSON(http.StatusOK, page)
}

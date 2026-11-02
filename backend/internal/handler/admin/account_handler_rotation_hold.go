package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func decodeAccountManualRestMinutes(body io.Reader) (int, error) {
	var request *struct {
		Minutes json.RawMessage `json:"minutes"`
	}
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if err == io.EOF {
			return 1, nil
		}
		return 0, fmt.Errorf("invalid request: %w", err)
	}
	if request == nil {
		return 0, fmt.Errorf("request must be an object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return 0, fmt.Errorf("request must contain a single JSON object")
	}
	minutes := 1
	if len(request.Minutes) != 0 {
		if bytes.Equal(bytes.TrimSpace(request.Minutes), []byte("null")) {
			return 0, fmt.Errorf("minutes must be an integer between 1 and 120")
		}
		if err := json.Unmarshal(request.Minutes, &minutes); err != nil {
			return 0, fmt.Errorf("minutes must be an integer between 1 and 120")
		}
	}
	if minutes < 1 || minutes > 120 {
		return 0, fmt.Errorf("minutes must be an integer between 1 and 120")
	}
	return minutes, nil
}

// StartAccountManualRest holds new requests until the account has drained and
// remained idle for the requested time. It does not change scheduling flags.
// POST /api/v1/admin/accounts/:id/rotation/rest
func (h *AccountHandler) StartAccountManualRest(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	minutes, err := decodeAccountManualRestMinutes(c.Request.Body)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if !h.accountExistsForManualRest(c, accountID) {
		return
	}
	if h.concurrencyService == nil {
		response.ErrorFrom(c, service.ErrAccountRotationUnsupported)
		return
	}
	state, err := h.concurrencyService.StartAccountManualRest(c.Request.Context(), accountID, minutes)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"account_rotation": state})
}

// CancelAccountManualRest resumes admissions held by a manual rest.
// DELETE /api/v1/admin/accounts/:id/rotation/rest
func (h *AccountHandler) CancelAccountManualRest(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if !h.accountExistsForManualRest(c, accountID) {
		return
	}
	if h.concurrencyService == nil {
		response.ErrorFrom(c, service.ErrAccountRotationUnsupported)
		return
	}
	state, err := h.concurrencyService.CancelAccountManualRest(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"account_rotation": state})
}

// GetAccountRestState refreshes only runtime rotation state for visible badges.
func (h *AccountHandler) GetAccountRestState(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if !h.accountExistsForManualRest(c, accountID) {
		return
	}
	states, err := h.concurrencyService.GetAccountRotationStates(c.Request.Context(), []int64{accountID})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"account_rotation": states[accountID]})
}

func (h *AccountHandler) accountExistsForManualRest(c *gin.Context, accountID int64) bool {
	account, err := h.adminService.GetAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return false
	}
	if account == nil {
		response.ErrorFrom(c, service.ErrAccountNotFound)
		return false
	}
	return true
}

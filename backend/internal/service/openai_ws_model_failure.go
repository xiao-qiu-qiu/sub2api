package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var ErrOpenAIWSModelUnavailable = errors.New("upstream websocket model unavailable")

type openAIWSModelRejectionError struct {
	status  int
	payload []byte
}

func (failure *openAIWSModelRejectionError) Error() string {
	return ErrOpenAIWSModelUnavailable.Error()
}

func (failure *openAIWSModelRejectionError) Unwrap() error {
	return ErrOpenAIWSModelUnavailable
}

func openAIWSModelFailureStatus(account *Account, payload []byte) int {
	event := gjson.GetBytes(payload, "type").String()
	if event != "error" && event != "response.failed" {
		return 0
	}
	if !gjson.ValidBytes(payload) {
		return 0
	}
	status := 0
	for _, path := range []string{"response.error.status_code", "response.error.status", "error.status_code", "error.status", "status_code", "status"} {
		if value := gjson.GetBytes(payload, path); value.Type == gjson.Number {
			status = int(value.Int())
			break
		}
	}
	if status == 0 {
		prefix := "error"
		if event == "response.failed" && gjson.GetBytes(payload, "response.error").IsObject() {
			prefix = "response.error"
		}
		code := strings.TrimSpace(gjson.GetBytes(payload, prefix+".code").String())
		errType := strings.TrimSpace(gjson.GetBytes(payload, prefix+".type").String())
		status = openAIWSErrorHTTPStatusFromRaw(code, errType)
		if strings.EqualFold(code, "model_not_found") {
			status = http.StatusNotFound
			if isOpenAICodexPlanGatedModelError(http.StatusBadRequest, payload) {
				status = http.StatusBadRequest
			}
		}
	}
	if isUpstreamModelNotFoundError(status, payload) ||
		(isOpenAIOAuthAccount(account) && isOpenAICodexPlanGatedModelError(status, payload)) {
		return status
	}
	return 0
}

func (s *OpenAIGatewayService) handleOpenAIWSModelFailure(ctx context.Context, account *Account, model string, payload []byte) bool {
	status := openAIWSModelFailureStatus(account, payload)
	if status == 0 || account == nil || strings.TrimSpace(model) == "" {
		return false
	}
	return s.handleOpenAIAccountUpstreamError(ctx, account, status, nil, payload, model)
}

func writeOpenAIWSModelFailure(c *gin.Context, status int, payload []byte, stream bool) {
	markOpenAIWSClientVisibleFailure(c, gjson.GetBytes(payload, "type").String(), payload)
	if stream || (c.Writer.Written() && strings.HasPrefix(c.Writer.Header().Get("Content-Type"), "text/event-stream")) {
		if !c.Writer.Written() {
			c.Header("Content-Type", "text/event-stream")
			c.Status(http.StatusOK)
		}
		_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", payload)
		c.Writer.Flush()
		return
	}
	c.Data(status, "application/json", payload)
}

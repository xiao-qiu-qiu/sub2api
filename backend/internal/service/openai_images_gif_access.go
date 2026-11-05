package service

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// terminalOpenAIImagesGIFAccessError surfaces a request-specific GIF rejection
// before account recovery, failover or custom error rules can consume it.
func terminalOpenAIImagesGIFAccessError(c *gin.Context, parsed *OpenAIImagesRequest, status int, header http.Header, body []byte) *OpenAIImagesUpstreamError {
	if parsed == nil || parsed.OutputFormat != "gif" || status != http.StatusBadRequest || !gjson.ValidBytes(body) {
		return nil
	}
	// Do not accept the extractor's top-level message fallback or raw-body echoes.
	messageField := gjson.GetBytes(body, "error.message")
	detailField := gjson.GetBytes(body, "detail")
	if !(messageField.Type == gjson.String && strings.TrimSpace(messageField.String()) != "") &&
		!(detailField.Type == gjson.String && strings.TrimSpace(detailField.String()) != "") {
		return nil
	}
	message := strings.Join(strings.Fields(extractUpstreamErrorMessage(body)), " ")
	message = strings.TrimSpace(strings.TrimSuffix(message, "."))
	if !strings.EqualFold(message, "GIF output is not enabled for this request") {
		return nil
	}

	upErr := openAIImagesUpstreamErrorFromHTTP(status, header, body)
	setOpsUpstreamError(c, status, upErr.Message, "")
	writeOpenAIImagesUpstreamErrorResponse(c, upErr)
	return upErr
}

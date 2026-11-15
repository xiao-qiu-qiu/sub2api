package service

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAIStickyMigrationWriteFailureKey = "openai_sticky_migration_write_failures"

// Gin records c.JSON/c.Data render write errors on the context instead of
// returning them to the forwarding method. Keep its existing return semantics.
func openAIStickyMigrationRenderErrors(c *gin.Context) int {
	if c == nil {
		return 0
	}
	return len(c.Errors) + c.GetInt(openAIStickyMigrationWriteFailureKey)
}

func markOpenAIStickyMigrationWriteFailure(c *gin.Context) {
	if c != nil {
		c.Set(openAIStickyMigrationWriteFailureKey, c.GetInt(openAIStickyMigrationWriteFailureKey)+1)
	}
}

// The Responses HTTP adapter accepts the Chat sentinel as an observed terminal.
func openAIStickyMigrationTerminalEvent(event string) string {
	if strings.TrimSpace(event) == "[DONE]" {
		return "response.completed"
	}
	return strings.TrimSpace(event)
}

// An event name alone cannot prove completion when its response contradicts it.
func openAIStickyMigrationInvalidResponseTerminal(payload []byte, event string) bool {
	switch strings.TrimSpace(event) {
	case "response.completed", "response.done":
		status := strings.TrimSpace(gjson.GetBytes(payload, "response.status").String())
		return status != "" && status != "completed" ||
			openAIStickyMigrationHasError(payload, "response.error") ||
			openAIStickyMigrationHasError(payload, "response.incomplete_details") ||
			openAIStickyMigrationHasError(payload, "error")
	default:
		return isOpenAICompatResponsesTerminalEvent(event)
	}
}

func openAIStickyMigrationHasError(payload []byte, path string) bool {
	value := gjson.GetBytes(payload, path)
	return value.Exists() && value.Type != gjson.Null
}

func openAIStickyMigrationInvalidJSONResponse(payload []byte) bool {
	return openAIStickyMigrationInvalidResponseStatus(gjson.GetBytes(payload, "status").String()) ||
		gjson.GetBytes(payload, "type").String() == "error" || openAIStickyMigrationHasError(payload, "error") ||
		openAIStickyMigrationHasError(payload, "incomplete_details") || openAIStickyMigrationInvalidChatFinish(payload)
}

func openAIStickyMigrationInvalidChatFinish(payload []byte) bool {
	for _, choice := range gjson.GetBytes(payload, "choices").Array() {
		switch strings.TrimSpace(choice.Get("finish_reason").String()) {
		case "", "stop", "tool_calls", "function_call":
		default:
			return true
		}
	}
	return false
}

func openAIStickyMigrationResponseStatus(status string) string {
	switch status = strings.TrimSpace(status); status {
	case "completed", "failed", "incomplete", "cancelled", "canceled":
		return "response." + status
	default:
		return ""
	}
}

func openAIStickyMigrationInvalidResponseStatus(status string) bool {
	status = strings.TrimSpace(status)
	return status != "" && status != "completed"
}

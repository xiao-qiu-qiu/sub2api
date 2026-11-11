package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// excelBPSChatResponse translates only the downstream protocol. BPS keeps
// ownership of authentication, transports, retries, usage and terminal errors.
type excelBPSChatResponse struct {
	originalModel string
	upstreamModel string
	state         *apicompat.ResponsesEventToChatState
	parser        openAICompatSSEFrameParser
	done          bool
}

func (s *OpenAIGatewayService) forwardChatCompletionsViaExcelBPS(
	ctx context.Context, c *gin.Context, account *Account, body []byte,
	promptCacheKey, originalModel, billingModel, upstreamModel string,
	stream bool, start time.Time,
) (*OpenAIForwardResult, error) {
	// BPS always streams upstream but owns aggregation for non-stream clients.
	body, err := sjson.SetBytes(body, "stream", stream)
	if err != nil {
		return nil, err
	}
	if promptCacheKey != "" && gjson.GetBytes(body, "prompt_cache_key").String() == "" {
		body, err = sjson.SetBytes(body, "prompt_cache_key", promptCacheKey)
		if err != nil {
			return nil, err
		}
	}
	c.Writer.Header().Del("X-Codex2API-Basispoints-Bypass")
	c.Writer.Header().Del("X-Codex2API-Upstream")
	SetActualOpenAIUpstreamEndpoint(c, "/basispoints/api/responses")
	chat := &excelBPSChatResponse{
		originalModel: originalModel, upstreamModel: upstreamModel,
		state: apicompat.NewResponsesEventToChatState(),
	}
	chat.state.Model = originalModel
	// Match the native Chat bridge: retain usage for downstream relay billing.
	chat.state.IncludeUsage = true
	ctx = withOpenAITransportMode(s.withOpenAIHTTPBudget(ctx, c), "bps")
	result, forwardErr := s.forwardExcelBPSWithChat(ctx, c, account, body, start, chat)
	if result != nil {
		result.Model = originalModel
		result.BillingModel = billingModel
		result.Stream = stream
		// BPS does not forward service_tier. Retain its own billing result;
		// the Chat client's requested tier must not introduce an unused surcharge.
	}
	if stream && (result == nil || !result.ClientDisconnect) {
		if frame, ok := chat.parser.Finish(); ok {
			err = chat.writeFrame(c, frame)
		}
		if err == nil && forwardErr == nil {
			err = chat.writeDone(c)
		}
		if err != nil {
			if result != nil {
				result.ClientDisconnect = true
			}
			if forwardErr == nil {
				forwardErr = err
			}
		}
	}
	return result, forwardErr
}

func (w *excelBPSChatResponse) writeResponse(c *gin.Context, body []byte) error {
	var response apicompat.ResponsesResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return err
	}
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.JSON(http.StatusOK, apicompat.ResponsesToChatCompletions(&response, w.originalModel))
	return nil
}

func (w *excelBPSChatResponse) writeLine(c *gin.Context, line string) error {
	if w.done {
		return nil
	}
	if strings.HasPrefix(line, ":") {
		_, err := c.Writer.WriteString(line + "\n\n")
		return err
	}
	if frame, ok := w.parser.AddLine(line); ok {
		return w.writeFrame(c, frame)
	}
	return nil
}

func (w *excelBPSChatResponse) writeFrame(c *gin.Context, frame openAICompatSSEFrame) error {
	if w.done || strings.TrimSpace(frame.Data) == "[DONE]" {
		return nil
	}
	payload := openAICompatPayloadWithEventType(frame.Data, frame.EventType)
	var event apicompat.ResponsesStreamEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return err
	}
	switch event.Type {
	case "response.incomplete":
		if isExcelBPSChatTruncation([]byte(payload)) {
			break
		}
		return w.writeFailure(c, "basispoints_protocol_error", "Excel BPS did not complete the response")
	case "response.failed", "response.cancelled", "error":
		detail := gjson.Get(payload, "response.error")
		if !detail.IsObject() {
			detail = gjson.Get(payload, "error")
		}
		if detail.IsObject() {
			return w.writeError(c, json.RawMessage(detail.Raw))
		}
		if event.Type == "error" {
			if failure := basispoints.ParseUpstreamFailure([]byte(payload)); failure != nil {
				return w.writeError(c, failure.Details())
			}
		}
		return w.writeFailure(c, "basispoints_protocol_error", "Excel BPS did not complete the response")
	}
	for _, chunk := range apicompat.ResponsesEventToChatChunks(&event, w.state) {
		wire, err := apicompat.ChatChunkToSSE(chunk)
		if err != nil {
			return err
		}
		if _, err = c.Writer.WriteString(wire); err != nil {
			return err
		}
	}
	return nil
}

// These terminal reasons are normal Chat finish reasons, not a transport or
// protocol failure. Unknown truncations and explicit upstream errors still fail.
func isExcelBPSChatTruncation(payload []byte) bool {
	if gjson.GetBytes(payload, "type").String() != "response.incomplete" ||
		gjson.GetBytes(payload, "response.status").String() != "incomplete" {
		return false
	}
	if detail := gjson.GetBytes(payload, "response.error"); detail.Exists() && detail.Type != gjson.Null {
		return false
	}
	switch gjson.GetBytes(payload, "response.incomplete_details.reason").String() {
	case "max_output_tokens", "content_filter":
		return true
	default:
		return false
	}
}

func (w *excelBPSChatResponse) writeFailure(c *gin.Context, code, message string) error {
	return w.writeError(c, gin.H{"type": "server_error", "code": code, "message": message})
}

func (w *excelBPSChatResponse) writeError(c *gin.Context, detail any) error {
	if w.done {
		return nil
	}
	body, err := json.Marshal(gin.H{"error": detail})
	if err != nil {
		return err
	}
	if _, err = c.Writer.WriteString("data: " + string(body) + "\n\n"); err != nil {
		return err
	}
	return w.writeDone(c)
}

func (w *excelBPSChatResponse) writeDone(c *gin.Context) error {
	if w.done {
		return nil
	}
	w.done = true
	_, err := c.Writer.WriteString("data: [DONE]\n\n")
	c.Writer.Flush()
	return err
}

//go:build unit

package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func stickyCompletionWire(event, status string) string {
	return `data: {"type":"response.output_text.delta","delta":"ok"}` + "\n\n" +
		fmt.Sprintf(`data: {"type":%q,"response":{"id":"resp_sticky","model":"gpt-5.4","status":%q,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":7,"output_tokens":2}}}`, event, status) + "\n\n"
}

func forwardStickyCompletionEvidence(t *testing.T, endpoint, wire string, disconnect bool, clientStream ...bool) (*OpenAIForwardResult, error) {
	t.Helper()
	stream := true
	if len(clientStream) > 0 {
		stream = clientStream[0]
	}
	body := []byte(fmt.Sprintf(`{"model":"gpt-5.4","stream":%t,"input":"hello"}`, stream))
	if endpoint == "chat/completions" {
		body = []byte(fmt.Sprintf(`{"model":"gpt-5.4","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, stream))
	} else if endpoint == "messages" {
		body = []byte(fmt.Sprintf(`{"model":"gpt-5.4","stream":%t,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`, stream))
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, bytes.NewReader(body))
	if endpoint == "responses/compact" {
		MarkOpenAICompactClientStream(c)
	}
	if disconnect {
		c.Writer = &openAIRawStreamDisconnectedWriter{ResponseWriter: c.Writer}
	}
	contentType := "text/event-stream"
	if !strings.HasPrefix(wire, "data:") {
		contentType = "application/json"
	}
	svc := &OpenAIGatewayService{
		cfg: rawChatCompletionsTestConfig(), toolCorrector: NewCodexToolCorrector(),
		httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}},
			Body: io.NopCloser(strings.NewReader(wire)),
		}},
	}
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{"openai_responses_supported": true}
	switch endpoint {
	case "responses", "responses/compact":
		return svc.Forward(context.Background(), c, account, body)
	case "chat/completions":
		return svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	default:
		return svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
	}
}

func TestOpenAIStickyMigration_NonStreamingWriteFailureEvidence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"responses", "responses/compact", "chat/completions", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			wire := stickyCompletionWire("response.completed", "completed")
			if endpoint == "responses/compact" {
				wire = `{"id":"resp_compact","model":"gpt-5.4","status":"completed","output":[{"type":"compaction","encrypted_content":"fixture"}],"usage":{"input_tokens":7,"output_tokens":2}}`
			}
			result, err := forwardStickyCompletionEvidence(t, endpoint, wire, true, false)
			require.NoError(t, err, "JSON render failures retain existing forwarding return semantics")
			require.NotNil(t, result)
			require.True(t, result.ClientDisconnect)
			require.False(t, result.SucceededForStickyMigration())
		})
		if endpoint == "chat/completions" || endpoint == "messages" {
			t.Run(endpoint+"/incomplete omits status", func(t *testing.T) {
				wire := strings.Replace(stickyCompletionWire("response.incomplete", "incomplete"), `"status":"incomplete",`, "", 1)
				result, err := forwardStickyCompletionEvidence(t, endpoint, wire, false, false)
				require.NoError(t, err)
				require.Equal(t, "response.incomplete", result.UpstreamTerminalEvent)
				require.False(t, result.SucceededForStickyMigration())
			})
		}
	}
	for _, tc := range []struct {
		name, status, detail string
		want                 bool
	}{
		{name: "success", status: "completed", detail: "null", want: true},
		{name: "incomplete", status: "incomplete", detail: "null"},
		{name: "still running", status: "in_progress", detail: "null"},
		{name: "error object", status: "completed", detail: `{"message":"generation failed"}`},
	} {
		t.Run("Responses JSON/"+tc.name, func(t *testing.T) {
			wire := fmt.Sprintf(`{"id":"resp_json","model":"gpt-5.4","status":%q,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":7,"output_tokens":2},"error":%s}`, tc.status, tc.detail)
			result, err := forwardStickyCompletionEvidence(t, "responses", wire, false, false)
			require.NoError(t, err)
			require.Equal(t, tc.want, result.SucceededForStickyMigration())
		})
	}
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprintf("raw Chat/disconnect=%t", disconnect), func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if disconnect {
				c.Writer = &openAIRawStreamDisconnectedWriter{ResponseWriter: c.Writer}
			}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chat_json","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`))}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
			result, err := svc.bufferRawChatCompletions(c, resp, rawChatCompletionsTestAccount(), "gpt-5.4", "gpt-5.4", "gpt-5.4", nil, nil, time.Now())
			require.NoError(t, err)
			require.Equal(t, disconnect, result.ClientDisconnect)
			require.Equal(t, !disconnect, result.SucceededForStickyMigration())
		})
	}
}

func TestOpenAIStickyMigration_StreamingCompletionEvidence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		for _, tc := range []struct {
			name, event, status string
			disconnect, want    bool
		}{
			{name: "completed", event: "response.completed", status: "completed", want: true},
			{name: "incomplete", event: "response.incomplete", status: "incomplete"},
			{name: "cancelled", event: "response.cancelled", status: "cancelled"},
			{name: "contradictory completion", event: "response.completed", status: "incomplete"},
			{name: "downstream write error", event: "response.completed", status: "completed", disconnect: true},
		} {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				result, err := forwardStickyCompletionEvidence(t, endpoint, stickyCompletionWire(tc.event, tc.status), tc.disconnect)
				require.NoError(t, err, "existing streaming settlement semantics must remain intact")
				require.NotNil(t, result)
				require.Equal(t, 7, result.Usage.InputTokens)
				require.Equal(t, tc.event, result.UpstreamTerminalEvent)
				require.Equal(t, tc.disconnect, result.ClientDisconnect)
				require.Equal(t, tc.want, result.SucceededForStickyMigration())
			})
		}
	}
}

func TestOpenAIStickyMigration_RawChatCompletionEvidence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, tail       string
		disconnect, want bool
	}{
		{name: "done", tail: "data: [DONE]\n\n", want: true},
		{name: "finish reason", tail: `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n", want: true},
		{name: "length then done", tail: `data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\ndata: [DONE]\n\n"},
		{name: "content filter then done", tail: `data: {"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}` + "\n\ndata: [DONE]\n\n"},
		{name: "usage only is insufficient", tail: `data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}` + "\n\n"},
		{name: "error then done", tail: `data: {"error":{"message":"generation failed"}}` + "\n\ndata: [DONE]\n\n"},
		{name: "downstream write error", tail: "data: [DONE]\n\n", disconnect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			if tc.disconnect {
				c.Writer = &openAIRawStreamDisconnectedWriter{ResponseWriter: c.Writer}
			}
			wire := `data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}` + "\n\n" + tc.tail
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire)),
			}}}
			result, err := svc.forwardAsRawChatCompletions(context.Background(), c, rawChatCompletionsTestAccount(), body, "")
			require.NoError(t, err)
			require.Equal(t, tc.disconnect, result.ClientDisconnect)
			require.Equal(t, tc.want, result.SucceededForStickyMigration())
		})
	}
}

func TestOpenAIStickyMigration_WSHTTPBridgeDisconnectEvidence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(stickyCompletionWire("response.completed", "completed"))),
	}}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	payload := []byte(`{"type":"response.create","model":"gpt-5.4","input":"hello"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := svc.proxyOpenAIWSHTTPBridgeTurn(ctx, c, rawChatCompletionsTestAccount(), "sk-test", payload, len(payload), "gpt-5.4", "", "", "", "", 1, func([]byte) error { return io.EOF })
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.ClientDisconnect)
	require.Equal(t, "response.completed", result.UpstreamTerminalEvent)
	require.False(t, result.SucceededForStickyMigration())
}

//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type rawChatFlushCountingRecorder struct {
	*httptest.ResponseRecorder
	flushes int
	onFlush func(string)
}

func (recorder *rawChatFlushCountingRecorder) Flush() {
	recorder.flushes++
	recorder.ResponseRecorder.Flush()
	if recorder.onFlush != nil {
		recorder.onFlush(recorder.Body.String())
	}
}

func rawStreamFlushTestEvent(content string) string {
	return `data: {"id":"chatcmpl_f","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"` + content + `"}}]}` + "\n\n"
}

func TestStreamRawChatCompletions_CoalescesFlushesWithinOneUpstreamRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	recorder := &rawChatFlushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	upstreamBody := rawStreamFlushTestEvent("a") + rawStreamFlushTestEvent("b") + rawStreamFlushTestEvent("c") +
		`data: {"id":"chatcmpl_f","object":"chat.completion.chunk","model":"gpt-5.4","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":3,"total_tokens":6}}` + "\n\n" +
		"data: [DONE]\n\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	result, err := svc.forwardAsRawChatCompletions(context.Background(), ctx, rawChatCompletionsTestAccount(), body, "")

	require.NoError(t, err)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.Equal(t, upstreamBody, recorder.Body.String())
	require.Equal(t, 1, recorder.flushes)
}

func TestStreamRawChatCompletions_FlushesBeforeWaitingForUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	firstEventFlushed := make(chan struct{})
	recorder := &rawChatFlushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	recorder.onFlush = func(written string) {
		if strings.Contains(written, `"content":"first"`) {
			select {
			case <-firstEventFlushed:
			default:
				close(firstEventFlushed)
			}
		}
	}
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	writerErr := make(chan error, 1)
	go func() {
		defer func() { _ = writer.Close() }()
		if _, err := io.WriteString(writer, rawStreamFlushTestEvent("first")); err != nil {
			writerErr <- err
			return
		}
		select {
		case <-firstEventFlushed:
		case <-time.After(5 * time.Second):
			writerErr <- errors.New("first event was not flushed before waiting for more upstream data")
			return
		}
		_, err := io.WriteString(writer, rawStreamFlushTestEvent("second")+"data: [DONE]\n\n")
		writerErr <- err
	}()
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       reader,
	}}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	_, err := svc.forwardAsRawChatCompletions(context.Background(), ctx, rawChatCompletionsTestAccount(), body, "")

	require.NoError(t, err)
	require.NoError(t, <-writerErr)
	require.Equal(t, rawStreamFlushTestEvent("first")+rawStreamFlushTestEvent("second")+"data: [DONE]\n\n", recorder.Body.String())
	require.Equal(t, 2, recorder.flushes)
}

func TestStreamRawChatCompletions_NoFlushBeforeUsableOutput(t *testing.T) {
	for _, upstreamBody := range []string{
		": keepalive\n\n",
		`data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n" +
			`data: {"choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n",
	} {
		t.Run(upstreamBody, func(t *testing.T) {
			body := largeRawChatCompletionsBody()
			recorder := &rawChatFlushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(upstreamBody)),
			}}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

			_, err := svc.forwardAsRawChatCompletions(context.Background(), ctx, rawChatCompletionsTestAccount(), body, "")

			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Empty(t, recorder.Body.String())
			require.Zero(t, recorder.flushes)
			require.False(t, ctx.Writer.Written())
		})
	}
}

func TestStreamRawChatCompletions_FlushesOutputBeforeReadErrorWithoutReplay(t *testing.T) {
	recorder := &rawChatFlushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	upstreamBody := rawStreamFlushTestEvent("partial")
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &openAIResponseFlushReadError{
		payload: []byte(upstreamBody), err: io.ErrUnexpectedEOF,
	}}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}

	result, err := svc.streamRawChatCompletions(ctx, resp, rawChatCompletionsTestAccount(), "gpt-5.4", "gpt-5.4", "gpt-5.4", nil, nil, time.Now(), 0)

	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover))
	require.NotNil(t, result)
	require.Equal(t, upstreamBody, recorder.Body.String())
	require.Equal(t, 1, recorder.flushes)
}

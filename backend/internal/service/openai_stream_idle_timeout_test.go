//go:build unit

package service

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRelayStallsIdleDeadlineTracksLastRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
			MaxLineSize: defaultMaxLineSize, StreamDataIntervalTimeout: 1,
		}}}
		reader, writer := io.Pipe()
		defer func() { _ = reader.Close() }()
		defer func() { _ = writer.Close() }()
		go func() {
			time.Sleep(250 * time.Millisecond)
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n")
		}()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		body := &firstOutputCloseTrackingBody{ReadCloser: reader, closed: make(chan struct{})}
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}
		account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
		start := time.Now()

		_, err := svc.handleStreamingResponse(ctx.Request.Context(), resp, ctx, account, start, "model", "model")

		require.Equal(t, 1250*time.Millisecond, time.Since(start))
		var failover *UpstreamFailoverError
		require.ErrorAs(t, err, &failover)
		require.True(t, failover.SafeToFailoverAfterWrite)
		require.Empty(t, recorder.Body.String())
		select {
		case <-body.closed:
		default:
			t.Fatal("idle failover must close the stalled upstream body")
		}
	})
}

func TestRelayStallsIdleFailoverBoundary(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		output string
	}{
		{name: "before_output"},
		{name: "after_output", output: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"},
		{name: "incomplete_output_event", output: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
					MaxLineSize: defaultMaxLineSize, StreamDataIntervalTimeout: 1,
				}}}
				reader, writer := io.Pipe()
				defer func() { _ = reader.Close() }()
				defer func() { _ = writer.Close() }()
				go func() {
					_, _ = io.WriteString(writer, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"+testCase.output)
				}()
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

				_, err := svc.handleStreamingResponse(ctx.Request.Context(), resp, ctx, account, time.Now(), "model", "model")

				require.Error(t, err)
				var failover *UpstreamFailoverError
				if testCase.output == "" {
					require.ErrorAs(t, err, &failover)
					require.True(t, failover.SafeToFailoverAfterWrite)
					require.Empty(t, recorder.Body.String())
				} else {
					require.False(t, errors.As(err, &failover))
					require.Contains(t, recorder.Body.String(), "hello")
					require.Contains(t, recorder.Body.String(), "stream_timeout")
				}
			})
		})
	}
}

func TestRelayStallsIdleFailoverAfterKeepaliveOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
			MaxLineSize: defaultMaxLineSize, StreamDataIntervalTimeout: 2, StreamKeepaliveInterval: 1,
		}}}
		reader, writer := io.Pipe()
		defer func() { _ = reader.Close() }()
		defer func() { _ = writer.Close() }()
		go func() {
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n")
		}()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
		account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

		_, err := svc.handleStreamingResponse(ctx.Request.Context(), resp, ctx, account, time.Now(), "model", "model")

		var failover *UpstreamFailoverError
		require.ErrorAs(t, err, &failover)
		require.True(t, failover.SafeToFailoverAfterWrite)
		body := recorder.Body.String()
		require.NotEmpty(t, body)
		require.Empty(t, strings.ReplaceAll(body, ":\n\n", ""))
		require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(ctx))
	})
}

func TestRelayStallsIdlePreservesUpstreamErrorAfterKeepalive(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		t.Run(accountType, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
					MaxLineSize: defaultMaxLineSize, StreamDataIntervalTimeout: 2, StreamKeepaliveInterval: 1,
				}}}
				reader, writer := io.Pipe()
				defer func() { _ = reader.Close() }()
				defer func() { _ = writer.Close() }()
				go func() {
					_, _ = io.WriteString(writer, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n")
					time.Sleep(1250 * time.Millisecond)
					_, _ = io.WriteString(writer, "data: {\"type\":\"error\",\"error\":{\"code\":\"invalid_request\",\"message\":\"original rejection\"}}\n\n")
				}()
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: accountType}

				_, err := svc.handleStreamingResponse(ctx.Request.Context(), resp, ctx, account, time.Now(), "model", "model")

				require.ErrorContains(t, err, "original rejection")
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover))
				body := recorder.Body.String()
				require.Contains(t, body, ":\n\n")
				require.Contains(t, body, "original rejection")
				require.NotContains(t, body, "stream_timeout")
				require.NotContains(t, body, "idle timeout")
			})
		})
	}
}

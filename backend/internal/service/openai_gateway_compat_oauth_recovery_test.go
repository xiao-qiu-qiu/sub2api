//go:build unit

package service

import (
	"context"
	"errors"
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

func TestOpenAICompatOAuthStreamFailureReplayBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"chat", "messages"} {
		for _, streaming := range []bool{false, true} {
			for _, failure := range []string{
				`{"type":"error","error":{"code":"token_revoked"}}`,
				`{"type":"response.failed","response":{"error":{"code":"token_invalidated","type":"invalid_request_error"}}}`,
			} {
				for _, tc := range []struct {
					name          string
					prefix        string
					replay        bool
					observedUsage bool
				}{
					{name: "empty", replay: true},
					{name: "metadata", prefix: `{"type":"response.created","response":{"id":"resp_staged","model":"gpt-5.1","status":"in_progress"}}`, replay: true},
					{name: "semantic_output", prefix: `{"type":"response.output_text.delta","delta":"already generated"}`},
					{name: "zero_usage", prefix: `{"type":"response.in_progress","response":{"usage":{"input_tokens":0,"output_tokens":0}}}`, observedUsage: true},
					{name: "usage", prefix: `{"type":"response.in_progress","response":{"usage":{"input_tokens":3,"output_tokens":1}}}`, observedUsage: true},
				} {
					t.Run(fmt.Sprintf("%s/stream=%v/%s/%s", endpoint, streaming, tc.name, failure), func(t *testing.T) {
						svc, guard, account := newOAuthRecoveryFixture(t)
						ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
						rec := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(rec)
						c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, nil).WithContext(ctx)
						wire := ""
						if tc.prefix != "" {
							wire = "data: " + tc.prefix + "\n\n"
						}
						wire += "data: " + failure + "\n\n"
						resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
						var result *OpenAIForwardResult
						var err error
						switch {
						case endpoint == "chat" && streaming:
							result, err = svc.handleChatStreamingResponse(resp, c, account, "gpt-5.1", "gpt-5.1", "gpt-5.1", time.Now(), 0)
						case endpoint == "chat":
							result, err = svc.handleChatBufferedStreamingResponse(resp, c, account, "gpt-5.1", "gpt-5.1", "gpt-5.1", time.Now())
						case streaming:
							result, err = svc.handleAnthropicStreamingResponse(resp, c, account, "gpt-5.1", "gpt-5.1", "gpt-5.1", time.Now())
						default:
							result, err = svc.handleAnthropicBufferedStreamingResponse(resp, c, account, "gpt-5.1", "gpt-5.1", "gpt-5.1", time.Now())
						}
						require.Error(t, err)
						var failover *UpstreamFailoverError
						if tc.replay {
							require.ErrorAs(t, err, &failover)
							require.Equal(t, http.StatusUnauthorized, failover.StatusCode)
							require.Nil(t, result)
							require.Empty(t, rec.Body.String(), "no lifecycle metadata or error may escape before safe replay")
						} else {
							require.False(t, errors.As(err, &failover), "upstream output or any observed usage prohibits replay")
							require.NotNil(t, result, "partial usage must reach the normal billing owner")
							require.Equal(t, tc.observedUsage, result.ObservedUsageOnError)
							if tc.name == "usage" {
								require.EqualValues(t, 3, result.Usage.InputTokens)
								require.EqualValues(t, 1, result.Usage.OutputTokens)
							}
						}
						guard.mu.Lock()
						creates := guard.creates
						guard.mu.Unlock()
						require.Equal(t, 1, creates, "a native credential failure still starts repair when its request cannot replay")
					})
				}
			}
		}
	}
}

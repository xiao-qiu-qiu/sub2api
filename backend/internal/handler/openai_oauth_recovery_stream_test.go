//go:build unit

package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthRecoveryHTTP_StreamAuthenticationEventsRecover(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		for _, streaming := range []bool{false, true} {
			for _, event := range []string{
				`{"type":"error","error":{"code":"token_revoked"}}`,
				`{"type":"response.failed","response":{"error":{"code":"token_invalidated","type":"invalid_request_error"}}}`,
			} {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", endpoint, streaming, event), func(t *testing.T) {
					ready := make(chan struct{})
					close(ready)
					upstream := &oauthRecoveryHandlerUpstream{firstResponse: func() *http.Response {
						return &http.Response{StatusCode: http.StatusOK,
							Header: http.Header{"Content-Type": {"text/event-stream"}},
							Body:   io.NopCloser(strings.NewReader("data: " + event + "\n\n"))}
					}}
					h, guard, _, usage := newOAuthRecoveryHandler(t, upstream, ready)
					c, rec := oauthRecoveryHandlerRequest(t, context.Background(), endpoint)
					body := fmt.Sprintf(`{"model":"gpt-5.1","stream":%v,"input":"preserve this input","messages":[{"role":"user","content":"preserve this input"}],"max_tokens":20}`, streaming)
					c.Request.Body = io.NopCloser(strings.NewReader(body))
					c.Request.ContentLength = int64(len(body))
					runOAuthRecoveryHandler(h, c, endpoint)
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					_, tokens, bodies := upstream.calls()
					require.Equal(t, []string{"Bearer old-token", "Bearer recovered-token"}, tokens)
					require.JSONEq(t, string(bodies[0]), string(bodies[1]))
					require.EqualValues(t, 1, guard.requests.Load())
					require.EqualValues(t, 1, usage.count.Load())
				})
			}
		}
	}
}

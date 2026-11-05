//go:build unit

package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIOAuthRecoveryWS_CredentialReplayBoundedPerTurn(t *testing.T) {
	turns := newOpenAIOAuthWSTurnRecoveryContext(context.Background())
	payload := []byte(`{"type":"response.create","model":"gpt-5.1","input":"current turn"}`)
	require.False(t, turns.allowCredentialReplay(payload, false, false), "an earlier turn must never be replayed")
	require.False(t, turns.allowCredentialReplay(payload, true, true), "observed output or usage cannot be replayed")
	require.False(t, turns.allowCredentialReplay(nil, true, false))
	require.False(t, turns.allowCredentialReplay([]byte(`{"type":"response.cancel"}`), true, false))
	require.True(t, turns.allowCredentialReplay(payload, true, false))
	require.False(t, turns.allowCredentialReplay(payload, true, false), "a logical turn can rebuild only once")
	turns.beginTurn()
	require.True(t, turns.allowCredentialReplay(payload, true, false), "the next client turn owns a fresh retry allowance")
}

type oauthRecoveryWSNetworkFrame struct {
	connection int32
	token      string
	payload    []byte
}

func TestOpenAIOAuthRecoveryWS_ExternalTokenChangeRebuildsCurrentTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{service.OpenAIWSIngressModeCtxPool, service.OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			var connections atomic.Int32
			frames := make(chan oauthRecoveryWSNetworkFrame, 8)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connection := connections.Add(1)
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				for {
					ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
					_, payload, readErr := conn.Read(ctx)
					cancel()
					if readErr != nil {
						return
					}
					frames <- oauthRecoveryWSNetworkFrame{connection: connection, token: r.Header.Get("Authorization"), payload: append([]byte(nil), payload...)}
					response := []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_external_%d","model":"gpt-5.1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer-%d"}]}],"usage":{"input_tokens":2,"output_tokens":1}}}`, connection, connection))
					ctx, cancel = context.WithTimeout(r.Context(), 3*time.Second)
					writeErr := conn.Write(ctx, coderws.MessageText, response)
					cancel()
					if writeErr != nil {
						return
					}
				}
			}))
			t.Cleanup(upstream.Close)
			target, err := url.Parse(upstream.URL)
			require.NoError(t, err)
			transport, ok := http.DefaultTransport.(*http.Transport)
			require.True(t, ok)
			transport = transport.Clone()
			t.Cleanup(transport.CloseIdleConnections)
			oldClient := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: issue600LocalTransport{target: target, transport: transport}}
			t.Cleanup(func() { http.DefaultClient = oldClient })
			ready := make(chan struct{})
			close(ready)
			h, guard, _, usage := newOAuthRecoveryHandler(t, nil, ready)
			h.cfg.Gateway.OpenAIWS.Enabled = true
			h.cfg.Gateway.OpenAIWS.OAuthEnabled = true
			h.cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			h.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			h.cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
			h.cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
			h.cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
			h.cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 5
			h.cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 2
			h.cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 2
			guard.repo.mutate(func(a *service.Account) {
				a.Extra = map[string]any{"openai_oauth_responses_websockets_v2_mode": mode, "openai_oauth_responses_websockets_v2_enabled": true, "codex_fingerprint_mode": "off"}
			})
			var authChecks atomic.Int32
			router := gin.New()
			router.GET("/v1/responses", func(c *gin.Context) {
				fixture, _ := oauthRecoveryHandlerRequest(t, c.Request.Context(), "responses")
				key, _ := middleware2.GetAPIKeyFromContext(fixture)
				// Continuation references are required to verify credential rebuilds.
				key.AllowLongContext = true
				c.Set(string(middleware2.ContextKeyAPIKey), key)
				c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 2})
				c.Request = c.Request.WithContext(service.WithAPIKeyQueueAuthRevalidator(c.Request.Context(), func(context.Context) (int, error) { authChecks.Add(1); return 0, nil }))
				h.ResponsesWebSocket(c)
			})
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.CloseNow() })
			for turn := 1; turn <= 3; turn++ {
				if turn > 1 {
					guard.repo.mutate(func(a *service.Account) { a.Credentials["access_token"] = fmt.Sprintf("external-token-%d", turn) })
				}
				payload := fmt.Sprintf(`{"type":"response.create","model":"gpt-5.1","input":[{"role":"user","content":"turn-%d"}]}`, turn)
				if turn > 1 {
					payload = fmt.Sprintf(`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_external_%d","input":[{"role":"user","content":"turn-%d"}]}`, turn-1, turn)
				}
				checksBefore := authChecks.Load()
				require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(payload)))
				for {
					_, response, readErr := conn.Read(ctx)
					require.NoError(t, readErr, string(response))
					require.NotEqual(t, "error", gjson.GetBytes(response, "type").String(), string(response))
					if gjson.GetBytes(response, "type").String() == "response.completed" {
						require.Equal(t, fmt.Sprintf("resp_external_%d", turn), gjson.GetBytes(response, "response.id").String())
						break
					}
				}
				select {
				case observed := <-frames:
					require.EqualValues(t, turn, observed.connection, "a changed token must use a new upstream socket")
					wantToken := "Bearer old-token"
					if turn > 1 {
						wantToken = fmt.Sprintf("Bearer external-token-%d", turn)
					}
					require.Equal(t, wantToken, observed.token)
					require.Contains(t, string(observed.payload), fmt.Sprintf("turn-%d", turn))
					if turn > 1 {
						require.Contains(t, string(observed.payload), "answer-1", "rebuilding must retain completed conversation context")
					}
				case <-ctx.Done():
					t.Fatal("upstream did not receive the current turn")
				}
				require.Greater(t, authChecks.Load(), checksBefore, "rebuilding must run fresh key/permission admission")
				require.Eventually(t, func() bool { return usage.count.Load() == int32(turn) }, time.Second, time.Millisecond, "each completed turn is recorded exactly once")
			}
			require.EqualValues(t, 3, connections.Load())
			require.Empty(t, frames, "no completed turn may be sent twice")
			require.Zero(t, guard.requests.Load(), "an external credential change must not start a login")
		})
	}
}

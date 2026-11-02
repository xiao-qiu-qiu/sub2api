//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func wsOAuthRecoveryFixture(t *testing.T, mode string) (*OpenAIGatewayService, *oauthRecoveryTestGuard, *Account) {
	t.Helper()
	s, guard, a := newOAuthRecoveryFixture(t)
	s.cfg = passthroughLifecycleConfig()
	s.cfg.Gateway.OpenAIWS.OAuthEnabled = true
	s.cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
	s.cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 5
	s.cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	s.cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 2
	s.cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 2
	s.cache = &stubGatewayCache{}
	s.httpUpstream = &httpUpstreamRecorder{}
	s.openaiWSResolver = NewOpenAIWSProtocolResolver(s.cfg)
	s.toolCorrector = NewCodexToolCorrector()
	a.Extra = map[string]any{"openai_oauth_responses_websockets_v2_mode": mode}
	a.Concurrency = 2
	guard.repo.mu.Lock()
	guard.repo.accounts[0] = *a
	guard.repo.mu.Unlock()
	t.Cleanup(s.CloseOpenAIWSPool)
	return s, guard, a
}

func TestWSOAuthRecoveryBridgeLater401Boundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name   string
		status int
		body   string
		replay bool
	}{
		{"http", 401, `{"error":{"code":"token_revoked"}}`, true},
		{"created_then_401", 200, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_discard\"}}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"token_revoked\"}}}\n\n", true},
		{"usage_then_401", 200, "data: {\"type\":\"response.failed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":0},\"error\":{\"code\":\"token_revoked\"}}}\n\n", false},
		{"output_then_401", 200, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"token_revoked\"}}}\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, guard, a := wsOAuthRecoveryFixture(t, OpenAIWSIngressModeHTTPBridge)
			s.httpUpstream = &httpUpstreamRecorder{resp: &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}}
			ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("GET", "/v1/responses", nil).WithContext(ctx)
			payload := []byte(`{"type":"response.create","model":"gpt-5.1","input":"second"}`)
			var writes [][]byte
			result, err := s.proxyOpenAIWSHTTPBridgeTurn(ctx, c, a, "old-test-token", payload, len(payload), "gpt-5.1", "", "", "", "", 2, func(p []byte) error { writes = append(writes, append([]byte(nil), p...)); return nil })
			var failure *UpstreamFailoverError
			require.Equal(t, tc.replay, errors.As(err, &failure))
			if tc.replay {
				require.Nil(t, result)
				require.Equal(t, 401, failure.StatusCode)
				require.Empty(t, writes)
			} else {
				require.NotNil(t, result)
				require.NotEmpty(t, writes)
				if tc.name == "usage_then_401" {
					require.True(t, result.ObservedUsageOnError)
				}
			}
			guard.mu.Lock()
			creates := guard.creates
			guard.mu.Unlock()
			require.Equal(t, 1, creates, "even an un-replayable rejected credential must begin repair")
		})
	}
}

type wsOAuthRecoveryDialer struct {
	mu       sync.Mutex
	conn     openAIWSClientConn
	status   int
	requests []http.Header
}

type wsOAuthNativeConn struct{ *stagedPassthroughConn }

func (c *wsOAuthNativeConn) WriteJSON(ctx context.Context, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.WriteFrame(ctx, coderws.MessageText, payload)
}

func (d *wsOAuthRecoveryDialer) Dial(_ context.Context, _ string, h http.Header, _ string) (openAIWSClientConn, int, http.Header, error) {
	d.mu.Lock()
	d.requests = append(d.requests, cloneHeader(h))
	d.mu.Unlock()
	if d.status != 0 {
		return nil, d.status, nil, &openAIWSHandshakeError{Body: []byte(`{"error":{"code":"token_revoked"}}`)}
	}
	return d.conn, 101, http.Header{}, nil
}

func startWSOAuthRecovery(t *testing.T, s *OpenAIGatewayService, a *Account, hooks *OpenAIWSIngressHooks) (*coderws.Conn, <-chan error) {
	t.Helper()
	ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
	server, done := startPassthroughLifecycleServer(t, ctx, s, a, hooks)
	t.Cleanup(server.Close)
	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.CloseNow() })
	require.NoError(t, conn.Write(dialCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":[{"role":"user","content":"first"}]}`)))
	return conn, done
}

func TestWSOAuthRecoveryHandshakeStartsImmediateTask(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool} {
		t.Run(mode, func(t *testing.T) {
			s, guard, a := wsOAuthRecoveryFixture(t, mode)
			d := &wsOAuthRecoveryDialer{status: 401}
			s.openaiWSPassthroughDialer = d
			s.openaiWSPool = newOpenAIWSConnPool(s.cfg)
			s.openaiWSPool.setClientDialerForTest(d)
			_, done := startWSOAuthRecovery(t, s, a, nil)
			select {
			case err := <-done:
				var failure *UpstreamFailoverError
				require.ErrorAs(t, err, &failure)
				require.Equal(t, 401, failure.StatusCode)
			case <-time.After(5 * time.Second):
				t.Fatal("handshake did not return")
			}
			guard.mu.Lock()
			creates := guard.creates
			guard.mu.Unlock()
			require.Equal(t, 1, creates)
			d.mu.Lock()
			authorization := d.requests[0].Get("Authorization")
			d.mu.Unlock()
			require.Equal(t, "Bearer old-test-token", authorization)
		})
	}
}

func TestWSOAuthRecoveryCurrentTurnAndChangedToken(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		changed bool
	}{
		{OpenAIWSIngressModePassthrough, false}, {OpenAIWSIngressModePassthrough, true},
		{OpenAIWSIngressModeCtxPool, false}, {OpenAIWSIngressModeCtxPool, true},
	} {
		t.Run(tc.mode+map[bool]string{false: "/401", true: "/changed_token"}[tc.changed], func(t *testing.T) {
			changed := tc.changed
			s, guard, a := wsOAuthRecoveryFixture(t, tc.mode)
			up := newStagedPassthroughConn()
			d := &wsOAuthRecoveryDialer{conn: &wsOAuthNativeConn{up}}
			s.openaiWSPassthroughDialer = d
			s.openaiWSPool = newOpenAIWSConnPool(s.cfg)
			s.openaiWSPool.setClientDialerForTest(d)
			conn, done := startWSOAuthRecovery(t, s, a, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			select {
			case <-up.writes:
			case <-ctx.Done():
				t.Fatal("first frame missing")
			}
			up.Send(`{"type":"response.completed","response":{"id":"resp_first","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first-answer"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`)
			_, p, err := conn.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, "response.completed", gjson.GetBytes(p, "type").String())
			if changed {
				guard.repo.mu.Lock()
				guard.repo.accounts[0].Credentials = map[string]any{"access_token": "new-token", "refresh_token": "new-refresh"}
				guard.repo.mu.Unlock()
			}
			require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_first","input":[{"role":"user","content":"second"}]}`)))
			if !changed {
				select {
				case <-up.writes:
				case <-ctx.Done():
					t.Fatal("second frame missing")
				}
				up.Send(`{"type":"response.created","response":{"id":"resp_discard"}}`)
				up.Send(`{"type":"response.failed","response":{"error":{"code":"token_revoked"}}}`)
			}
			select {
			case err := <-done:
				if changed {
					require.ErrorIs(t, err, ErrOpenAIWSOAuthCredentialChanged)
				} else {
					var failure *UpstreamFailoverError
					require.ErrorAs(t, err, &failure)
					require.Equal(t, 401, failure.StatusCode)
				}
				payload, current := OpenAIWSCurrentTurnRetryPayload(err)
				require.True(t, current)
				require.NotEmpty(t, payload)
				require.False(t, gjson.GetBytes(payload, "previous_response_id").Exists())
				require.Contains(t, string(payload), "first-answer")
				require.Contains(t, string(payload), "second")
				if changed {
					require.Empty(t, up.writes, "new turn must never go to the old connection")
				}
			case <-ctx.Done():
				t.Fatal("current turn was not returned")
			}
			guard.mu.Lock()
			creates := guard.creates
			guard.mu.Unlock()
			if changed {
				require.Zero(t, creates)
			} else {
				require.Equal(t, 1, creates)
			}
		})
	}
}

func TestWSOAuthRecoveryPoolDoesNotReuseOldCredential(t *testing.T) {
	s, _, a := wsOAuthRecoveryFixture(t, OpenAIWSIngressModeCtxPool)
	oldHeaders, newHeaders := http.Header{}, http.Header{}
	oldHeaders.Set("Authorization", "Bearer old-token")
	newHeaders.Set("Authorization", "Bearer new-token")
	require.NotEqual(t, normalizeOpenAIWSHandshakeCompatibility(a, oldHeaders), normalizeOpenAIWSHandshakeCompatibility(a, newHeaders))
	pool := newOpenAIWSConnPool(s.cfg)
	defer pool.Close()
	d := &wsOAuthRecoveryDialer{conn: newStagedPassthroughConn()}
	pool.setClientDialerForTest(d)
	req := openAIWSAcquireRequest{Account: a, WSURL: "wss://example.invalid/responses", Headers: oldHeaders}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	l1, err := pool.Acquire(ctx, req)
	require.NoError(t, err)
	l1.Release()
	req.Headers = newHeaders
	l2, err := pool.Acquire(ctx, req)
	require.NoError(t, err)
	defer l2.Release()
	require.NotEqual(t, l1.ConnID(), l2.ConnID())
	d.mu.Lock()
	requests := len(d.requests)
	d.mu.Unlock()
	require.Equal(t, 2, requests)
}

func TestWSOAuthRecoveryBridgeUsesCurrentCredential(t *testing.T) {
	s, guard, a := wsOAuthRecoveryFixture(t, OpenAIWSIngressModeHTTPBridge)
	guard.repo.mu.Lock()
	guard.repo.accounts[0].Credentials = map[string]any{"access_token": "new-native-token", "refresh_token": "new-refresh"}
	guard.repo.mu.Unlock()
	up := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_current\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"))}}
	s.httpUpstream = up
	ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v1/responses", nil).WithContext(ctx)
	payload := []byte(`{"type":"response.create","model":"gpt-5.1","input":"second"}`)
	_, err := s.proxyOpenAIWSHTTPBridgeTurn(ctx, c, a, "old-test-token", payload, len(payload), "gpt-5.1", "", "", "", "", 2, func([]byte) error { return nil })
	require.NoError(t, err)
	require.Equal(t, "Bearer new-native-token", up.lastReq.Header.Get("Authorization"))
}

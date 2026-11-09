package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWSIngressQualityInterruptAllModes(t *testing.T) {
	for i, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough, OpenAIWSIngressModeHTTPBridge} {
		t.Run(mode, func(t *testing.T) {
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 10
			cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 10
			cfg.Gateway.OpenAIWS.IngressModeDefault = mode
			upstream := newStagedPassthroughConn()
			defer func() { _ = upstream.Close() }()
			svc := newPassthroughLifecycleService(cfg, upstream)
			pool := newOpenAIWSConnPool(cfg)
			defer pool.Close()
			pool.setClientDialerForTest(&openAIWSSingleConnDialer{conn: upstream})
			svc.openaiWSPool = pool
			account := &Account{ID: int64(9961930 + i), Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-test"}, Extra: map[string]any{"openai_apikey_responses_websockets_v2_mode": mode}}
			created := `{"type":"response.created","response":{"id":"resp_ingress_quality","model":"gpt-5.5","status":"in_progress","usage":{"input_tokens":7,"output_tokens":2}}}`
			if mode == OpenAIWSIngressModeHTTPBridge {
				svc.httpUpstream = &bpsTestUpstream{send: func(req *http.Request, _ string) (*http.Response, error) {
					_ = req.Body.Close()
					wire := "data: " + created + "\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &qualityWaitBody{ctx: req.Context(), initial: strings.NewReader(wire)}}, nil
				}}
			} else {
				upstream.Send(created)
			}
			type outcome struct {
				result *OpenAIForwardResult
				err    error
			}
			afterTurn := make(chan outcome, 2)
			serverDone := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					serverDone <- err
					return
				}
				defer func() { _ = conn.CloseNow() }()
				_, first, err := conn.Read(r.Context())
				if err != nil {
					serverDone <- err
					return
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = r.Clone(r.Context())
				forwardErr := svc.ProxyResponsesWebSocketFromClient(r.Context(), c, conn, account, "sk-test", first, &OpenAIWSIngressHooks{
					ClientLifecycleContext: r.Context(),
					AfterTurn:              func(_ int, result *OpenAIForwardResult, err error) { afterTurn <- outcome{result, err} },
				})
				serverDone <- forwardErr
				var closeErr *OpenAIWSClientCloseError
				if errors.As(forwardErr, &closeErr) {
					_ = conn.Close(closeErr.StatusCode(), closeErr.Reason())
				}
			}))
			defer server.Close()
			client, _, err := coderws.Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer func() { _ = client.CloseNow() }()
			require.NoError(t, client.Write(t.Context(), coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.5","stream":true,"input":"test"}`)))
			readCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			_, frame, err := client.Read(readCtx)
			cancel()
			require.NoError(t, err, "must receive a partial response before interrupting")
			require.Contains(t, string(frame), "response.created")
			started := time.Now()
			require.Positive(t, accountrequest.Default.Cancel(account.ID, time.Now()))
			wireClose := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				for {
					if _, _, err := client.Read(ctx); err != nil {
						wireClose <- err
						return
					}
				}
			}()
			select {
			case err = <-serverDone:
				require.ErrorIs(t, err, accountrequest.ErrQualityDegraded)
				var closeErr *OpenAIWSClientCloseError
				require.ErrorAs(t, err, &closeErr)
				require.Equal(t, coderws.StatusTryAgainLater, closeErr.StatusCode())
			case <-time.After(time.Second):
				t.Fatal("account interrupt did not stop native websocket ingress")
			}
			require.Less(t, time.Since(started), time.Second)
			select {
			case closeErr := <-wireClose:
				require.Equal(t, coderws.StatusTryAgainLater, coderws.CloseStatus(closeErr), "wire close must match the quality control error")
			case <-time.After(3 * time.Second):
				t.Fatal("client did not receive the quality close frame")
			}
			select {
			case observed := <-afterTurn:
				require.ErrorIs(t, observed.err, accountrequest.ErrQualityDegraded)
				require.NotNil(t, observed.result, fmt.Sprintf("%s must retain already observed usage", mode))
				require.Equal(t, 7, observed.result.Usage.InputTokens)
				require.Equal(t, 2, observed.result.Usage.OutputTokens)
			case <-time.After(time.Second):
				t.Fatal("unfinished turn was not settled")
			}
			require.Zero(t, accountrequest.Default.Cancel(account.ID, time.Now()))
		})
	}
}

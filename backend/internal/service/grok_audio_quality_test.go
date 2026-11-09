package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

type qualityGrokDialer func(context.Context) (openAIWSClientConn, error)

func (d qualityGrokDialer) Dial(ctx context.Context, _ string, _ http.Header, _ string) (openAIWSClientConn, int, http.Header, error) {
	conn, err := d(ctx)
	return conn, http.StatusSwitchingProtocols, nil, err
}

type qualityGrokConn struct {
	*grokRealtimeFrameUpstreamStub
	closed chan struct{}
	once   sync.Once
}

func (c *qualityGrokConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func TestGrokVoiceQualityInterruptSurvivesHandshakeAndStopsRelay(t *testing.T) {
	for i, tts := range []bool{false, true} {
		t.Run(fmt.Sprintf("tts=%t", tts), func(t *testing.T) {
			account := &Account{ID: int64(9961960 + i), Platform: PlatformGrok, Type: AccountTypeAPIKey}
			conn := &qualityGrokConn{grokRealtimeFrameUpstreamStub: &grokRealtimeFrameUpstreamStub{
				toClient: make(chan grokRealtimeFrame, 1)}, closed: make(chan struct{})}
			conn.toClient <- grokRealtimeFrame{kind: coderws.MessageText, data: []byte(`{"type":"audio.delta","delta":"AAAA"}`)}
			svc := &OpenAIGatewayService{openaiWSPassthroughDialer: qualityGrokDialer(func(context.Context) (openAIWSClientConn, error) { return conn, nil })}
			probeCtx, cancelProbe := context.WithTimeout(t.Context(), time.Second)
			var upstream *GrokRealtimeUpstream
			var err error
			if tts {
				upstream, err = svc.OpenGrokTTSStream(probeCtx, account, "test-token", nil)
			} else {
				upstream, err = svc.OpenGrokRealtime(probeCtx, account, "test-token", "grok-voice-latest")
			}
			cancelProbe()
			require.NoError(t, err)
			defer func() { _ = upstream.Close() }()
			require.NoError(t, upstream.accountCtx.Err(), "the completed dial timeout does not own the voice session")
			type result struct {
				chars int64
				err   error
			}
			done := make(chan result, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				client, acceptErr := coderws.Accept(w, r, nil)
				if acceptErr != nil {
					done <- result{err: acceptErr}
					return
				}
				defer func() { _ = client.CloseNow() }()
				if tts {
					chars, proxyErr := svc.ProxyGrokTTSStreamConn(r.Context(), client, upstream)
					done <- result{chars: chars, err: proxyErr}
				} else {
					_, proxyErr := svc.ProxyGrokRealtimeConn(r.Context(), nil, client, upstream)
					done <- result{err: proxyErr}
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer func() { _ = client.CloseNow() }()
			_, _, err = client.Read(ctx)
			require.NoError(t, err)
			if tts {
				require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"text.delta","delta":"hello"}`)))
				require.Eventually(t, func() bool {
					conn.mu.Lock()
					defer conn.mu.Unlock()
					return len(conn.written) == 1
				}, time.Second, time.Millisecond)
			}
			require.Positive(t, accountrequest.Default.Cancel(account.ID, time.Now()))
			select {
			case got := <-done:
				require.ErrorIs(t, got.err, accountrequest.ErrQualityDegraded)
				if tts {
					require.EqualValues(t, 5, got.chars, "retain delivered text for billing")
				}
			case <-time.After(time.Second):
				t.Fatal("voice relay did not stop promptly")
			}
			select {
			case <-conn.closed:
			default:
				t.Fatal("voice upstream remained open")
			}
			require.Zero(t, accountrequest.Default.Cancel(account.ID, time.Now()))
		})
	}
}

func TestGrokVoiceQualityInterruptDuringHandshake(t *testing.T) {
	const accountID = int64(9961970)
	started := make(chan struct{})
	svc := &OpenAIGatewayService{openaiWSPassthroughDialer: qualityGrokDialer(func(ctx context.Context) (openAIWSClientConn, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})}
	done := make(chan error, 1)
	go func() {
		_, err := svc.OpenGrokRealtime(t.Context(), &Account{ID: accountID, Platform: PlatformGrok, Type: AccountTypeAPIKey}, "test-token", "")
		done <- err
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("dial ended before starting: %v", err)
	case <-time.After(time.Second):
		t.Fatal("dial did not start")
	}
	require.Positive(t, accountrequest.Default.Cancel(accountID, time.Now()))
	select {
	case err := <-done:
		require.ErrorIs(t, err, accountrequest.ErrQualityDegraded)
	case <-time.After(time.Second):
		t.Fatal("interrupted dial did not exit")
	}
}

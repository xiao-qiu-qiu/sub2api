package bpsws

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const testBearer = "test-bearer-NEVER-PRINT"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func preparedRequest(t *testing.T, ctx context.Context, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(`{"model":"test-model","input":[{"role":"user","content":"hello"}],"metadata":{"integer":9007199254740993},"stream":true}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testBearer)
	req.Header.Set("Chatgpt-Account-Id", "test-account")
	req.Header.Set("X-Openai-Account-Id", "test-account")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func acceptCreate(t *testing.T, w http.ResponseWriter, r *http.Request) (*websocket.Conn, map[string]json.RawMessage) {
	t.Helper()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, OriginPatterns: []string{"bps.openai.com"}})
	require.NoError(t, err)
	kind, data, err := c.Read(testContext(t))
	require.NoError(t, err)
	require.Equal(t, websocket.MessageText, kind)
	var frame map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &frame))
	require.JSONEq(t, `"response.create"`, string(frame["type"]))
	return c, frame
}

func sendJSON(t *testing.T, ctx context.Context, c *websocket.Conn, data string) {
	t.Helper()
	require.NoError(t, c.Write(ctx, websocket.MessageText, []byte(data)))
}

func readEvent(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var result strings.Builder
	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		result.WriteString(line)
		if line == "\n" {
			return result.String()
		}
	}
}

func awaitSignal(t *testing.T, ctx context.Context, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal("mock operation did not finish before cancellation")
	}
}

func TestOpenHandshakeProfileAndStreaming(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%t", useTLS), func(t *testing.T) {
			ctx := testContext(t)
			finish := make(chan struct{})
			closed := make(chan struct{})
			var attempts, traced, wrapped atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "/basispoints/api/responses", r.URL.Path)
				require.Equal(t, "Bearer "+testBearer, r.Header.Get("Authorization"))
				require.Equal(t, "test-account", r.Header.Get("Chatgpt-Account-Id"))
				require.Equal(t, "test-account", r.Header.Get("X-Openai-Account-Id"))
				require.Empty(t, r.Header.Get("Content-Type"))
				require.Equal(t, "*/*", r.Header.Get("Accept"))
				require.Equal(t, "https://bps.openai.com", r.Header.Get("Origin"))
				for _, name := range []string{"Cookie", "Cookie2", "X-Codex-Turn-State", "Originator", "Openai-Beta", "Proxy-Authorization"} {
					require.Empty(t, r.Header.Get(name), name)
				}
				require.ElementsMatch(t, []string{"responses", "openai-bearer." + testBearer}, strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ","))
				profile := map[string]string{
					"X-Openai-Internal-Basispoints-Client-Product":        "basispoints-excel-plugin",
					"X-Openai-Internal-Basispoints-Client-Platform":       "excel",
					"X-Openai-Internal-Basispoints-Client-Agent-Profile":  "excel",
					"X-Openai-Internal-Basispoints-Client-Editor":         "excel",
					"X-Openai-Internal-Basispoints-Client-Host":           "office",
					"X-Openai-Internal-Basispoints-Client-Runtime":        "desktop",
					"X-Openai-Internal-Basispoints-Client-Platform-Class": "PC",
					"X-Openai-Internal-Basispoints-Office-Host":           "Excel",
					"X-Openai-Internal-Basispoints-Office-Platform":       "PC",
					"X-Openai-Internal-Basispoints-Browser-Name":          "unknown",
				}
				if runtime.GOOS == "darwin" {
					profile["X-Openai-Internal-Basispoints-Client-Platform-Class"] = "Mac"
					profile["X-Openai-Internal-Basispoints-Office-Platform"] = "Mac"
				}
				var queryProfile map[string]string
				require.NoError(t, json.Unmarshal([]byte(r.URL.Query().Get("bps_client_info")), &queryProfile))
				require.Equal(t, profile, queryProfile)
				for name, value := range profile {
					require.Equal(t, value, r.Header.Get(name), name)
				}
				require.Equal(t, "chatgpt", r.URL.Query().Get("bps_auth_mode"))
				require.Equal(t, "upstream_sent,heartbeat", r.URL.Query().Get("bps_control_frames"))
				w.Header().Set("X-Request-Id", "handshake-request-id")
				w.Header().Set("X-Ratelimit-Remaining-Requests", "7")
				c, frame := acceptCreate(t, w, r)
				defer c.CloseNow()
				var id, resume string
				require.NoError(t, json.Unmarshal(frame["basispoints_request_id"], &id))
				parsedID, err := uuid.Parse(id)
				require.NoError(t, err)
				require.Equal(t, uuid.Version(4), parsedID.Version())
				require.Equal(t, id, r.URL.Query().Get("bps_ws_affinity"))
				require.NoError(t, json.Unmarshal(frame["basispoints_resume_token"], &resume))
				require.Regexp(t, `^resume_[a-f0-9]{32}$`, resume)
				require.JSONEq(t, `{"integer":9007199254740993}`, string(frame["metadata"]))
				require.JSONEq(t, `[{"role":"user","content":"hello"}]`, string(frame["input"]))
				for _, control := range []string{"resume_token", "metadata", "upstream_sent", "heartbeat"} {
					sendJSON(t, ctx, c, `{"type":"basispoints.response.`+control+`","resume_token":"transport-secret","basispoints_replay_cursor":"private-cursor"}`)
				}
				sendJSON(t, ctx, c, `{"type":"response.output_text.delta","delta":"hello <world>","basispoints_replay_cursor":"private-cursor"}`)
				select {
				case <-finish:
				case <-ctx.Done():
					return
				}
				sendJSON(t, ctx, c, `{"type":"response.completed","response":{"status":"completed","output":[]}}`)
				_, _, err = c.Read(ctx)
				require.Error(t, err, "terminal must close the socket without waiting for Body.Close")
				close(closed)
			}))
			if useTLS {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			client := server.Client()
			base := client.Transport
			if base == nil {
				base = http.DefaultTransport
			}
			// An opaque wrapper must see the physical upgrade, and its trace hooks
			// must compose with the adapter's connection cancellation hooks.
			client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				wrapped.Add(1)
				return base.RoundTrip(r)
			})
			jar, err := cookiejar.New(nil)
			require.NoError(t, err)
			jarURL, err := url.Parse(server.URL)
			require.NoError(t, err)
			jar.SetCookies(jarURL, []*http.Cookie{{Name: "jar-secret", Value: "never-send"}})
			client.Jar = jar
			reqCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { traced.Add(1) }})
			req := preparedRequest(t, reqCtx, server.URL+"/basispoints/api/responses")
			for _, name := range []string{"Cookie", "Cookie2", "X-Codex-Turn-State", "Originator", "Openai-Beta", "Proxy-Authorization"} {
				req.Header.Set(name, "must-not-be-sent")
			}
			req.Header.Set("Sec-WebSocket-Protocol", "old-ticket")
			response, err := Open(ctx, client, req, time.Second)
			require.NoError(t, err)
			defer response.Body.Close()
			require.Equal(t, http.StatusOK, response.StatusCode)
			require.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
			require.Equal(t, "handshake-request-id", response.Header.Get("X-Request-Id"))
			require.Equal(t, "7", response.Header.Get("X-Ratelimit-Remaining-Requests"))
			reader := bufio.NewReader(response.Body)
			first := readEvent(t, reader)
			require.Contains(t, first, "event: response.output_text.delta\n")
			require.Contains(t, first, `"delta":"hello <world>"`)
			require.NotContains(t, first, "basispoints")
			require.NotContains(t, first, "transport-secret")
			// The server cannot emit a terminal event until the delta has arrived.
			close(finish)
			rest, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.Contains(t, string(rest), "event: response.completed\n")
			awaitSignal(t, ctx, closed)
			require.EqualValues(t, 1, attempts.Load())
			require.EqualValues(t, 1, wrapped.Load())
			require.EqualValues(t, 1, traced.Load())
			require.Same(t, jar, client.Jar, "do not mutate the caller's client")
			require.Equal(t, "must-not-be-sent", req.Header.Get("Cookie"))
			require.Equal(t, "application/json", req.Header.Get("Content-Type"))
		})
	}
}

func TestOpenRejectsSuccessfulHTTPWithoutWebSocketUpgrade(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNoContent} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ctx := testContext(t)
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
			}))
			defer server.Close()
			response, err := Open(ctx, server.Client(), preparedRequest(t, ctx, server.URL), 0)
			require.ErrorContains(t, err, "did not upgrade")
			require.Nil(t, response, "a successful HTTP response is not a WebSocket session")
			require.EqualValues(t, 1, attempts.Load())
		})
	}
}

func TestOpenNon101ReturnsStatusHeadersAndBoundedBody(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ctx := testContext(t)
			var attempts atomic.Int32
			body := strings.Repeat(" ", 2048) + `{"error":{"code":"model_not_allowed","message":"model access denied"}}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("Retry-After", "45")
				w.Header().Set("X-Request-Id", "rejection-id")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			response, err := Open(ctx, server.Client(), preparedRequest(t, ctx, server.URL), 0)
			require.NoError(t, err)
			require.NotNil(t, response)
			defer response.Body.Close()
			require.Equal(t, status, response.StatusCode)
			require.Equal(t, "45", response.Header.Get("Retry-After"))
			require.Equal(t, "rejection-id", response.Header.Get("X-Request-Id"))
			data, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, body, string(data), "keep model access evidence beyond coder/websocket's 1024-byte capture")
			require.EqualValues(t, 1, attempts.Load())
		})
	}
	t.Run("limit-and-close-original", func(t *testing.T) {
		ctx := testContext(t)
		body := &trackedBody{Reader: strings.NewReader(strings.Repeat("x", maxRejectionBody+100))}
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 403, Header: make(http.Header), Body: body}, nil
		})}
		response, err := Open(ctx, client, preparedRequest(t, ctx, "https://upstream.invalid/responses"), 0)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.Len(t, data, maxRejectionBody)
		require.True(t, body.closed.Load())
		require.NoError(t, response.Body.Close())
	})
}

type trackedBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *trackedBody) Close() error { b.closed.Store(true); return nil }

func TestOpenNeverFollowsRedirectOrLeaksBearer(t *testing.T) {
	ctx := testContext(t)
	var redirected, originalCheck atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.WriteHeader(500)
	}))
	defer target.Close()
	var attempts atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Location", target.URL+"/?token="+testBearer)
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = io.WriteString(w, "redirect blocked")
	}))
	defer source.Close()
	client := source.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { originalCheck.Add(1); return nil }
	response, err := Open(ctx, client, preparedRequest(t, ctx, source.URL), 0)
	require.NoError(t, err)
	require.Equal(t, http.StatusTemporaryRedirect, response.StatusCode)
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, "redirect blocked", string(data))
	require.NoError(t, response.Body.Close())
	require.Zero(t, redirected.Load())
	require.Zero(t, originalCheck.Load())
	require.EqualValues(t, 1, attempts.Load())
}

func TestOpenSanitizesRawDialAndBadCredentials(t *testing.T) {
	for _, sentinel := range []error{nil, context.Canceled, context.DeadlineExceeded} {
		t.Run(fmt.Sprint(sentinel), func(t *testing.T) {
			ctx := testContext(t)
			var attempts atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				attempts.Add(1)
				poison := "https://user:proxy-password@upstream.invalid/?token=" + testBearer
				if sentinel != nil {
					return nil, fmt.Errorf("%s: %w", poison, sentinel)
				}
				return nil, errors.New(poison)
			})}
			response, err := Open(ctx, client, preparedRequest(t, ctx, "https://upstream.invalid/responses"), 0)
			require.Nil(t, response)
			require.Error(t, err)
			for _, secret := range []string{testBearer, "proxy-password", "upstream.invalid", "https://"} {
				require.NotContains(t, err.Error(), secret)
			}
			if sentinel != nil {
				require.ErrorIs(t, err, sentinel)
			}
			require.EqualValues(t, 1, attempts.Load())
		})
	}
	for _, auth := range []string{"Bearer " + testBearer + "\r\nSecret: yes", "Basic " + testBearer, "Bearer " + testBearer + ",echo"} {
		req := preparedRequest(t, testContext(t), "https://upstream.invalid/responses")
		req.Header.Set("Authorization", auth)
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Error("invalid bearer must not reach transport")
			return nil, errors.New(testBearer)
		})}
		_, err := Open(req.Context(), client, req, 0)
		require.Error(t, err)
		require.NotContains(t, err.Error(), testBearer)
	}
}

// A writable 101 body lets us inject raw IO failures, including error strings
// that contain credentials. Real HTTP/WS integration is exercised above.
type faultSocket struct {
	readErr, writeErr error
	closed            chan struct{}
	once              sync.Once
}

func (s *faultSocket) Read([]byte) (int, error) {
	if s.readErr != nil {
		return 0, s.readErr
	}
	<-s.closed
	return 0, io.EOF
}

func (s *faultSocket) Write(p []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return len(p), nil
}

func (s *faultSocket) Close() error { s.once.Do(func() { close(s.closed) }); return nil }

func mockUpgrade(socket io.ReadWriteCloser, protocol string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		digest := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		return &http.Response{StatusCode: 101, Header: http.Header{
			"Connection": {"Upgrade"}, "Upgrade": {"websocket"},
			"Sec-Websocket-Accept":   {base64.StdEncoding.EncodeToString(digest[:])},
			"Sec-Websocket-Protocol": {protocol},
		}, Body: socket}, nil
	})}
}

func TestOpenSanitizesRawWriteReadAndHandshakeErrors(t *testing.T) {
	for _, phase := range []string{"write", "read", "handshake"} {
		t.Run(phase, func(t *testing.T) {
			ctx := testContext(t)
			poison := errors.New("https://proxy-user:proxy-password@private.invalid/" + testBearer)
			socket := &faultSocket{closed: make(chan struct{})}
			protocol := "responses"
			switch phase {
			case "write":
				socket.writeErr = poison
			case "read":
				socket.readErr = poison
			case "handshake":
				protocol = "unexpected-" + testBearer
				socket.readErr = poison
			}
			response, err := Open(ctx, mockUpgrade(socket, protocol), preparedRequest(t, ctx, "https://private.invalid/responses"), 0)
			if phase != "read" {
				require.Error(t, err)
				require.Nil(t, response)
				for _, secret := range []string{testBearer, "proxy-password", "private.invalid"} {
					require.NotContains(t, err.Error(), secret)
				}
			} else {
				require.NoError(t, err)
				data, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.Contains(t, string(data), "event: response.failed")
				require.NotContains(t, string(data), testBearer)
				require.NotContains(t, string(data), "proxy-password")
				require.NotContains(t, string(data), "private.invalid")
				require.NoError(t, response.Body.Close())
			}
			awaitSignal(t, ctx, socket.closed)
		})
	}
}

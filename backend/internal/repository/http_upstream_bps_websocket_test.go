package repository

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func bpsWebSocketRequest(t *testing.T, ctx context.Context, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(`{"model":"local-test","stream":true,"input":[]}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer synthetic-bps-key")
	return req
}

func bpsWebSocketService(t *testing.T) *httpUpstreamService {
	t.Helper()
	s, ok := NewHTTPUpstream(nil).(*httpUpstreamService)
	require.True(t, ok)
	t.Cleanup(func() {
		for _, entry := range s.clients {
			entry.client.CloseIdleConnections()
		}
	})
	return s
}

func bpsWebSocketEntry(t *testing.T, s *httpUpstreamService, proxy string, id int64, concurrency int) *upstreamClientEntry {
	t.Helper()
	entry, err := s.getClientEntry(proxy, id, concurrency, service.HTTPUpstreamProfileExcelBPSWebSocket, false, true)
	require.NoError(t, err)
	return entry
}

func TestBPSWebSocketHTTP1CONNECTAndBodyLifecycle(t *testing.T) {
	type handshake struct{ method, proto, alpn, auth, cookie string }
	received := make(chan handshake, 1)
	serverErr := make(chan error, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err = conn.ReadMessage() // Exercises the upgraded body's Write method.
		if err != nil {
			serverErr <- err
			return
		}
		received <- handshake{r.Method, r.Proto, r.TLS.NegotiatedProtocol, r.Header.Get("Authorization"), r.Header.Get("Cookie")}
		if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"local-response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":0}}}`)); err != nil {
			serverErr <- err
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	proxyTarget := make(chan string, 1)
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		if r.Method != http.MethodConnect || r.Host != strings.TrimPrefix(server.URL, "https://") {
			http.Error(w, "unexpected proxy target", http.StatusBadGateway)
			return
		}
		target, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			http.Error(w, "local target unavailable", http.StatusBadGateway)
			return
		}
		defer target.Close()
		client, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		proxyTarget <- r.Host
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { _, _ = io.Copy(target, rw); _ = target.Close() }()
		_, _ = io.Copy(client, target)
	}))
	defer proxy.Close()

	s := bpsWebSocketService(t)
	entry := bpsWebSocketEntry(t, s, proxy.URL, 9971001, 2)
	transport := entry.client.Transport.(*http.Transport)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	req := bpsWebSocketRequest(t, t.Context(), server.URL+"/responses")
	jar.SetCookies(req.URL, []*http.Cookie{{Name: "arc_kr", Value: "synthetic-cookie"}})
	entry.client.Jar = jar
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	req.Header.Set("Cookie", "session=synthetic-request-cookie")
	resp, err := s.DoBPSWebSocket(req, proxy.URL, 9971001, 2, time.Second)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
	require.EqualValues(t, 1, atomic.LoadInt64(&entry.inFlight))
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "response.completed")
	require.EqualValues(t, 1, atomic.LoadInt64(&entry.inFlight), "EOF must retain the pool until Close")
	select {
	case got := <-received:
		require.Equal(t, http.MethodGet, got.method)
		require.Equal(t, "HTTP/1.1", got.proto)
		require.NotEqual(t, "h2", got.alpn)
		require.Equal(t, "Bearer synthetic-bps-key", got.auth)
		require.Empty(t, got.cookie, "neither explicit nor cached cookies may enter BPS")
	case err := <-serverErr:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("WebSocket handshake did not complete")
	}
	require.Equal(t, strings.TrimPrefix(server.URL, "https://"), <-proxyTarget)
	require.EqualValues(t, 1, proxyCalls.Load())
	require.NoError(t, resp.Body.Close())
	require.NoError(t, resp.Body.Close())
	requireNoUpstreamInFlight(t, s)
	require.Zero(t, accountrequest.Default.Cancel(9971001, time.Now()))
}

func TestBPSWebSocketHTTPRefusalAndRedirectRetainPool(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTemporaryRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls, redirected atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(204) }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", target.URL)
				w.Header().Set("X-Request-ID", "local-refusal")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "refused locally")
			}))
			defer server.Close()
			s := bpsWebSocketService(t)
			s.cfg = &config.Config{Gateway: config.GatewayConfig{MaxUpstreamClients: 1}}
			entry := bpsWebSocketEntry(t, s, "", 9971002, 1)
			resp, err := s.DoBPSWebSocket(bpsWebSocketRequest(t, t.Context(), server.URL), "", 9971002, 1, time.Second)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, status, resp.StatusCode)
			require.Equal(t, "local-refusal", resp.Header.Get("X-Request-ID"))
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, "refused locally", string(body))
			require.EqualValues(t, 1, atomic.LoadInt64(&entry.inFlight))
			_, err = s.acquireClient("", 9971003, 1)
			require.ErrorIs(t, err, errUpstreamClientLimitReached, "an open refusal body must prevent pool eviction")
			require.NoError(t, resp.Body.Close())
			requireNoUpstreamInFlight(t, s)
			require.Empty(t, entry.bpsWebSocketSlots)
			require.EqualValues(t, 1, calls.Load(), "refusal must not cause a retry")
			require.Zero(t, redirected.Load())
			require.Zero(t, accountrequest.Default.Cancel(9971002, time.Now()))
		})
	}
}

func TestBPSWebSocketHandshakeFailureAndCancellationReleasePool(t *testing.T) {
	for _, mode := range []string{"disconnect", "cancel", "quality"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				close(started)
				if mode == "disconnect" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
					return
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			s := bpsWebSocketService(t)
			const id int64 = 9971004
			entry := bpsWebSocketEntry(t, s, "", id, 1)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := s.DoBPSWebSocket(bpsWebSocketRequest(t, ctx, server.URL), "", id, 1, time.Second)
				done <- err
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("handshake did not reach the server")
			}
			if mode == "cancel" {
				cancel()
			} else if mode == "quality" {
				require.Positive(t, accountrequest.Default.Cancel(id, time.Now()))
			}
			select {
			case err := <-done:
				require.Error(t, err)
				if mode == "quality" {
					require.ErrorIs(t, err, accountrequest.ErrQualityDegraded)
				} else if mode == "cancel" {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("failed handshake left an attempt blocked")
			}
			require.Zero(t, atomic.LoadInt64(&entry.inFlight))
			require.Empty(t, entry.bpsWebSocketSlots)
			require.EqualValues(t, 1, calls.Load())
			require.Zero(t, accountrequest.Default.Cancel(id, time.Now()))
		})
	}
}

func TestBPSWebSocketStreamCancellationReleasesOnClose(t *testing.T) {
	for _, quality := range []bool{false, true} {
		t.Run(fmt.Sprintf("quality=%v", quality), func(t *testing.T) {
			closed := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(closed)
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				if _, _, err = conn.ReadMessage(); err != nil {
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"local"}`))
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()
			s := bpsWebSocketService(t)
			const id int64 = 9971005
			entry := bpsWebSocketEntry(t, s, "", id, 1)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			resp, err := s.DoBPSWebSocket(bpsWebSocketRequest(t, ctx, server.URL), "", id, 1, time.Second)
			require.NoError(t, err)
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			_, err = reader.ReadString('\n')
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() { _, err := io.Copy(io.Discard, reader); done <- err }()
			if quality {
				require.Positive(t, accountrequest.Default.Cancel(id, time.Now()))
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if quality {
					require.ErrorIs(t, err, accountrequest.ErrQualityDegraded)
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("stream reader did not stop on cancellation")
			}
			require.EqualValues(t, 1, atomic.LoadInt64(&entry.inFlight))
			require.NoError(t, resp.Body.Close())
			requireNoUpstreamInFlight(t, s)
			require.Zero(t, accountrequest.Default.Cancel(id, time.Now()))
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				t.Fatal("WebSocket connection remained open")
			}
		})
	}
}

func TestBPSWebSocketPoolIsolationAndBudgets(t *testing.T) {
	for _, isolation := range []string{config.ConnectionPoolIsolationProxy, config.ConnectionPoolIsolationAccount, config.ConnectionPoolIsolationAccountProxy} {
		t.Run(isolation, func(t *testing.T) {
			s := bpsWebSocketService(t)
			s.cfg = &config.Config{Gateway: config.GatewayConfig{ConnectionPoolIsolation: isolation, MaxConnsPerHost: 9}}
			const proxy = "http://127.0.0.1:19178"
			ws := bpsWebSocketEntry(t, s, proxy, 1, 3)
			sse, err := s.getClientEntry(proxy, 1, 3, service.HTTPUpstreamProfileExcelBPS, false, true)
			require.NoError(t, err)
			require.NotSame(t, ws, sse)
			s.recordBPSHTTP2Failure(t.Context(), proxy, bpsPolicyTrace("h2"), io.ErrUnexpectedEOF)
			sseH1, err := s.getClientEntry(proxy, 1, 3, service.HTTPUpstreamProfileExcelBPS, false, true)
			require.NoError(t, err)
			require.Equal(t, upstreamProtocolModeBPSH1, sseH1.protocolMode)
			require.NotSame(t, ws, sseH1)
			require.Same(t, ws, bpsWebSocketEntry(t, s, proxy, 1, 3))
			require.Equal(t, upstreamProtocolModeBPSWebSocketH1, ws.protocolMode)
			tr := ws.client.Transport.(*http.Transport)
			require.False(t, tr.ForceAttemptHTTP2)
			require.NotNil(t, tr.TLSNextProto)
			require.Empty(t, tr.TLSNextProto)
			if isolation == config.ConnectionPoolIsolationProxy {
				require.Equal(t, 9, tr.MaxConnsPerHost)
			} else {
				require.Equal(t, 3, tr.MaxConnsPerHost)
				require.Equal(t, 3, tr.MaxIdleConnsPerHost)
			}
		})
	}
}

func TestBPSWebSocketSharedBudgetWaitCancelAndResume(t *testing.T) {
	for _, isolation := range []string{config.ConnectionPoolIsolationProxy, config.ConnectionPoolIsolationAccountProxy} {
		t.Run(isolation, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, _, _ = conn.ReadMessage()
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()
			s := bpsWebSocketService(t)
			s.cfg = &config.Config{Gateway: config.GatewayConfig{ConnectionPoolIsolation: isolation, MaxConnsPerHost: 1}}
			const id int64 = 9971007
			entry := bpsWebSocketEntry(t, s, "", id, 1)
			require.Equal(t, 1, cap(entry.bpsWebSocketSlots))
			first, err := s.DoBPSWebSocket(bpsWebSocketRequest(t, t.Context(), server.URL), "", id, 1, time.Second)
			require.NoError(t, err)
			defer first.Body.Close()
			otherID := id
			if isolation == config.ConnectionPoolIsolationProxy {
				otherID++ // Two accounts share the same proxy budget.
			}
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			_, err = s.DoBPSWebSocket(bpsWebSocketRequest(t, ctx, server.URL), "", otherID, 1, time.Second)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.EqualValues(t, 1, calls.Load(), "a queued request must not open another cloned transport")
			require.EqualValues(t, 1, atomic.LoadInt64(&entry.inFlight))
			require.Len(t, entry.bpsWebSocketSlots, 1)
			type outcome struct {
				response *http.Response
				err      error
			}
			done := make(chan outcome, 1)
			req := bpsWebSocketRequest(t, t.Context(), server.URL)
			go func() {
				response, err := s.DoBPSWebSocket(req, "", otherID, 1, time.Second)
				done <- outcome{response, err}
			}()
			require.Eventually(t, func() bool { return atomic.LoadInt64(&entry.inFlight) == 2 }, time.Second, time.Millisecond)
			require.NoError(t, first.Body.Close())
			select {
			case got := <-done:
				require.NoError(t, got.err)
				require.EqualValues(t, 2, calls.Load())
				require.NoError(t, got.response.Body.Close())
			case <-time.After(3 * time.Second):
				t.Fatal("closing the active body did not release the queued WebSocket")
			}
			require.Empty(t, entry.bpsWebSocketSlots)
			requireNoUpstreamInFlight(t, s)
		})
	}
}

func TestBPSWebSocketRoutesStayOnSelectedProxy(t *testing.T) {
	var direct, regional, explicit atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1); w.WriteHeader(403) }))
	defer server.Close()
	region := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { regional.Add(1); w.WriteHeader(502) }))
	defer region.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { explicit.Add(1); w.WriteHeader(401) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", region.URL)
	t.Setenv("HTTPS_PROXY", region.URL)
	t.Setenv("ALL_PROXY", region.URL)
	cfg := regionalHTTPConfig(t, region.URL, "127.0.0.1")
	cfg.Gateway.CodexGatewayPin.Enabled = true // Exercises the production wrapper.
	up, ok := NewHTTPUpstream(cfg).(service.ExcelBPSWebSocketUpstream)
	require.True(t, ok)
	for _, selected := range []string{"", proxy.URL} {
		resp, err := up.DoBPSWebSocket(bpsWebSocketRequest(t, t.Context(), server.URL), selected, 9971006, 1, time.Second)
		require.NoError(t, err)
		if selected == "" {
			require.Equal(t, 403, resp.StatusCode)
		} else {
			require.Equal(t, 401, resp.StatusCode)
		}
		require.NoError(t, resp.Body.Close())
	}
	_, err := up.DoBPSWebSocket(bpsWebSocketRequest(t, t.Context(), server.URL), "://invalid", 9971006, 1, time.Second)
	require.Error(t, err)
	require.EqualValues(t, 1, direct.Load())
	require.EqualValues(t, 1, explicit.Load())
	require.Zero(t, regional.Load())
	s := up.(*astraRoutingUpstream).delegate.(*httpUpstreamService)
	require.False(t, s.useRegionalEgress("", 9971006, service.HTTPUpstreamProfileExcelBPSWebSocket))
	requireNoUpstreamInFlight(t, s)

	// No network request should escape host validation, even on a reused client.
	req := bpsWebSocketRequest(t, service.WithHTTPUpstreamPublicHostsOnly(t.Context()), server.URL)
	_, err = up.DoBPSWebSocket(req, "", 9971006, 1, time.Second)
	require.Error(t, err)
	require.EqualValues(t, 1, direct.Load())
	requireNoUpstreamInFlight(t, s)
	_, err = s.DoBPSWebSocket(nil, "", 0, 1, time.Second)
	require.Error(t, err)
}

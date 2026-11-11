package bpsws

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCancellationClosesDetachedCONNECTAndTLSSockets(t *testing.T) {
	for _, phase := range []string{"CONNECT", "TLS"} {
		t.Run(phase, func(t *testing.T) {
			deadline := testContext(t)
			ctx, cancel := context.WithCancel(deadline)
			defer cancel()
			clientConn, peer := net.Pipe()
			defer func() { _ = peer.Close() }()
			started, closed := make(chan struct{}), make(chan struct{})
			go func() {
				// Read real CONNECT headers or a real TLS ClientHello, then wait for
				// EOF: returning from Open alone would miss net/http's orphaned dial.
				buf := make([]byte, 4096)
				_, _ = peer.Read(buf)
				close(started)
				_, _ = io.Copy(io.Discard, peer)
				close(closed)
			}()
			transport := &http.Transport{
				DialContext: func(context.Context, string, string) (net.Conn, error) { return clientConn, nil },
			}
			if phase == "CONNECT" {
				proxy, err := url.Parse("http://proxy-user:proxy-secret@proxy.invalid:8080")
				require.NoError(t, err)
				transport.Proxy = http.ProxyURL(proxy)
			}
			client := &http.Client{Transport: transport}
			result := make(chan error, 1)
			go func() {
				_, err := Open(ctx, client, preparedRequest(t, ctx, "https://upstream.invalid/responses"), 0)
				result <- err
			}()
			awaitSignal(t, deadline, started)
			cancel()
			select {
			case err := <-result:
				require.ErrorIs(t, err, context.Canceled)
				require.NotContains(t, err.Error(), "proxy-secret")
				require.NotContains(t, err.Error(), "upstream.invalid")
			case <-deadline.Done():
				t.Fatal("cancelled handshake did not return")
			}
			awaitSignal(t, deadline, closed)
			require.Same(t, transport, client.Transport, "do not replace the caller's transport")
		})
	}
}

func TestStreamCancellationAndCloseReleaseSocketImmediately(t *testing.T) {
	for _, operation := range []string{"cancel-read", "cancel-without-read", "close-read", "close-without-read", "deadline-read"} {
		t.Run(operation, func(t *testing.T) {
			deadline := testContext(t)
			ctx, cancel := context.WithCancel(deadline)
			defer cancel()
			closed := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _ := acceptCreate(t, w, r)
				defer func() { _ = c.CloseNow() }()
				_, _, _ = c.Read(deadline)
				close(closed)
			}))
			defer server.Close()
			if operation == "deadline-read" {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 80*time.Millisecond)
				defer deadlineCancel()
			}
			response, err := Open(ctx, server.Client(), preparedRequest(t, ctx, server.URL), 0)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			reading := operation == "cancel-read" || operation == "close-read" || operation == "deadline-read"
			readResult := make(chan error, 1)
			if reading {
				go func() { _, err := io.ReadAll(response.Body); readResult <- err }()
			}
			switch operation {
			case "cancel-read", "cancel-without-read":
				cancel()
			case "close-read", "close-without-read":
				closeDone := make(chan struct{})
				go func() { _ = response.Body.Close(); close(closeDone) }()
				awaitSignal(t, deadline, closeDone)
			}
			awaitSignal(t, deadline, closed)
			if reading {
				select {
				case err := <-readResult:
					if operation == "deadline-read" {
						require.ErrorIs(t, err, context.DeadlineExceeded)
					} else {
						require.ErrorIs(t, err, context.Canceled)
					}
				case <-deadline.Done():
					t.Fatal("cancelled read did not return")
				}
			}
			// Multiple concurrent closes must not block or expose a socket error.
			var closers sync.WaitGroup
			for i := 0; i < 4; i++ {
				closers.Go(func() { require.NoError(t, response.Body.Close()) })
			}
			closers.Wait()
		})
	}
}

func TestCancelledOpenDoesNotDial(t *testing.T) {
	ctx, cancel := context.WithCancel(testContext(t))
	cancel()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("already cancelled request must not dial")
		return nil, context.Canceled
	})}
	_, err := Open(ctx, client, preparedRequest(t, ctx, "https://upstream.invalid/responses"), 0)
	require.ErrorIs(t, err, context.Canceled)
}

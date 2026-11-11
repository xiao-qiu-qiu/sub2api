package bpsws

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestStreamProtocolFailuresAreTerminalAndNeverReplay(t *testing.T) {
	cases := []struct {
		name, data, code string
		kind             websocket.MessageType
	}{
		{"draining", `{"type":"basispoints.response.server_draining","resume_token":"private"}`, "basispoints_ws_draining", websocket.MessageText},
		{"bad-json", `{not json`, "basispoints_ws_protocol_error", websocket.MessageText},
		{"binary", `{"type":"response.completed"}`, "basispoints_ws_protocol_error", websocket.MessageBinary},
		{"unknown", `{"type":"unexpected.event"}`, "basispoints_ws_protocol_error", websocket.MessageText},
		{"unknown-bps", `{"type":"basispoints.response.unknown"}`, "basispoints_ws_protocol_error", websocket.MessageText},
		{"missing-type", `{"data":"private"}`, "basispoints_ws_protocol_error", websocket.MessageText},
		{"non-string-type", `{"type":12}`, "basispoints_ws_protocol_error", websocket.MessageText},
		{"invalid-type", `{"type":"response.foo\nevent:injected"}`, "basispoints_ws_protocol_error", websocket.MessageText},
		{"long-type", `{"type":"response.` + strings.Repeat("x", 128) + `"}`, "basispoints_ws_protocol_error", websocket.MessageText},
		{"client-command", `{"type":"response.create"}`, "basispoints_ws_protocol_error", websocket.MessageText},
		{"json-null", `null`, "basispoints_ws_protocol_error", websocket.MessageText},
		{"disconnect", "", "basispoints_ws_disconnected", websocket.MessageText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testContext(t)
			var attempts, replays atomic.Int32
			closed := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				c, _ := acceptCreate(t, w, r)
				defer func() { _ = c.CloseNow() }()
				defer close(closed)
				if tc.name == "disconnect" {
					_ = c.Close(websocket.StatusPolicyViolation, "server echoed "+testBearer)
					return
				}
				require.NoError(t, c.Write(ctx, tc.kind, []byte(tc.data)))
				if _, _, err := c.Read(ctx); err == nil {
					replays.Add(1)
				}
			}))
			defer server.Close()
			response, err := Open(ctx, server.Client(), preparedRequest(t, ctx, server.URL), 0)
			require.NoError(t, err)
			data, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, 1, strings.Count(string(data), "event: response.failed\n"))
			require.Contains(t, string(data), `"code":"`+tc.code+`"`)
			require.NotContains(t, string(data), testBearer)
			require.NotContains(t, string(data), "private")
			require.NoError(t, response.Body.Close())
			awaitSignal(t, ctx, closed)
			require.EqualValues(t, 1, attempts.Load())
			require.Zero(t, replays.Load())
		})
	}
}

func TestStreamTerminalEventsAndFutureResponseEvents(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error"} {
		t.Run(terminal, func(t *testing.T) {
			ctx := testContext(t)
			closed := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _ := acceptCreate(t, w, r)
				defer func() { _ = c.CloseNow() }()
				sendJSON(t, ctx, c, `{"type":"response.future_event.delta","delta":"future"}`)
				sendJSON(t, ctx, c, `{"type":"`+terminal+`","response":{"status":"test"},"basispoints_replay_cursor":"private-cursor"}`)
				_, _, _ = c.Read(ctx)
				close(closed)
			}))
			defer server.Close()
			response, err := Open(ctx, server.Client(), preparedRequest(t, ctx, server.URL), 0)
			require.NoError(t, err)
			data, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Contains(t, string(data), "event: response.future_event.delta\n")
			require.Contains(t, string(data), "event: "+terminal+"\n")
			require.Equal(t, 2, strings.Count(string(data), "event: "))
			require.NotContains(t, string(data), "basispoints_replay_cursor")
			require.NotContains(t, string(data), "private-cursor")
			awaitSignal(t, ctx, closed)
			require.NoError(t, response.Body.Close())
		})
	}
}

func TestStreamIdleTimeout(t *testing.T) {
	for _, idle := range []time.Duration{0, 40 * time.Millisecond} {
		t.Run(fmt.Sprint(idle), func(t *testing.T) {
			ctx := testContext(t)
			finish := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _ := acceptCreate(t, w, r)
				defer func() { _ = c.CloseNow() }()
				if idle > 0 {
					_, _, _ = c.Read(ctx)
					return
				}
				select {
				case <-finish:
					sendJSON(t, ctx, c, `{"type":"response.completed"}`)
				case <-ctx.Done():
				}
			}))
			defer server.Close()
			response, err := Open(ctx, server.Client(), preparedRequest(t, ctx, server.URL), idle)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			result := make(chan string, 1)
			go func() {
				data, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				result <- string(data)
			}()
			if idle == 0 {
				select {
				case data := <-result:
					t.Fatalf("zero idle timeout ended stream: %s", data)
				case <-time.After(80 * time.Millisecond):
				}
				close(finish)
			}
			select {
			case data := <-result:
				if idle == 0 {
					require.Contains(t, data, "event: response.completed")
				} else {
					require.Contains(t, data, `"code":"basispoints_ws_timeout"`)
				}
			case <-ctx.Done():
				t.Fatal("idle test did not finish")
			}
		})
	}
}

func TestControlFramesResetIdleDeadline(t *testing.T) {
	ctx := testContext(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := acceptCreate(t, w, r)
		defer func() { _ = c.CloseNow() }()
		for i := 0; i < 4; i++ {
			select {
			case <-time.After(30 * time.Millisecond):
			case <-ctx.Done():
				return
			}
			sendJSON(t, ctx, c, `{"type":"basispoints.response.heartbeat"}`)
		}
		sendJSON(t, ctx, c, `{"type":"response.completed"}`)
	}))
	defer server.Close()
	response, err := Open(ctx, server.Client(), preparedRequest(t, ctx, server.URL), 90*time.Millisecond)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Contains(t, string(data), "event: response.completed")
	require.NotContains(t, string(data), "heartbeat")
	require.NotContains(t, string(data), "response.failed")
}

func TestStreamReadLimit(t *testing.T) {
	ctx := testContext(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := acceptCreate(t, w, r)
		defer func() { _ = c.CloseNow() }()
		// Deliberately larger than 16 MiB minus the SSE framing allowance.
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.output_text.delta","delta":"`+strings.Repeat("x", maxWSMessage)+`"}`))
	}))
	defer server.Close()
	response, err := Open(ctx, server.Client(), preparedRequest(t, ctx, server.URL), 0)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Contains(t, string(data), "event: response.failed")
	require.Less(t, len(data), 1024)
}

package bpsws

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// A pull reader: there is no pump goroutine or queue of response events.
// Only the event currently being consumed is retained between Read calls.
type eventBody struct {
	conn        *websocket.Conn
	ctx         context.Context
	cancel      context.CancelFunc
	release     func()
	stopParent  func() bool
	idleTimeout time.Duration
	pending     bytes.Reader
	terminal    bool
	readMu      sync.Mutex
	closeOnce   sync.Once
}

func (b *eventBody) shutdown() {
	b.closeOnce.Do(func() {
		b.stopParent()
		_ = b.conn.CloseNow()
		b.release()
	})
}

func (b *eventBody) Close() error {
	b.cancel()
	b.shutdown()
	return nil
}

func (b *eventBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.readMu.Lock()
	defer b.readMu.Unlock()
	for {
		if err := b.ctx.Err(); err != nil {
			b.shutdown()
			return 0, safeError(b.ctx, err, "BPS WebSocket stream cancelled")
		}
		if b.pending.Len() > 0 {
			return b.pending.Read(p)
		}
		if b.terminal {
			return 0, io.EOF
		}
		readCtx := b.ctx
		cancel := func() {}
		if b.idleTimeout > 0 {
			readCtx, cancel = context.WithTimeout(b.ctx, b.idleTimeout)
		}
		kind, data, err := b.conn.Read(readCtx)
		timedOut := readCtx.Err() == context.DeadlineExceeded
		cancel()
		if err != nil {
			if b.ctx.Err() != nil {
				continue
			}
			if timedOut {
				b.fail("basispoints_ws_timeout", "upstream WebSocket was idle too long; request was not retried")
			} else {
				b.fail("basispoints_ws_disconnected", "upstream WebSocket closed before a terminal event; request was not retried")
			}
			continue
		}
		var event map[string]json.RawMessage
		if kind != websocket.MessageText || json.Unmarshal(data, &event) != nil || event == nil {
			b.fail("basispoints_ws_protocol_error", "upstream sent an invalid WebSocket JSON event")
			continue
		}
		var eventType string
		if json.Unmarshal(event["type"], &eventType) != nil {
			b.fail("basispoints_ws_protocol_error", "upstream WebSocket event has an invalid type")
			continue
		}
		switch eventType {
		case "basispoints.response.resume_token", "basispoints.response.metadata", "basispoints.response.upstream_sent", "basispoints.response.heartbeat":
			continue
		case "basispoints.response.server_draining":
			b.fail("basispoints_ws_draining", "upstream is draining; automatic resume is not enabled")
			continue
		}
		terminal, known := responseEvent(eventType)
		if !known {
			b.fail("basispoints_ws_protocol_error", "unexpected upstream WebSocket event type")
			continue
		}
		delete(event, "basispoints_replay_cursor")
		// Disable HTML escaping so ordinary text does not grow sixfold in SSE.
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if encoder.Encode(event) != nil || encoded.Len()+len(eventType)+16 > maxSSEEvent {
			b.fail("basispoints_ws_protocol_error", "upstream WebSocket event exceeds the SSE event limit")
			continue
		}
		b.setEvent(eventType, bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'}))
		b.terminal = terminal
		if terminal {
			b.shutdown()
		}
	}
}

func (b *eventBody) setEvent(eventType string, data []byte) {
	var framed bytes.Buffer
	framed.Grow(len(eventType) + len(data) + 16)
	framed.WriteString("event: ")
	framed.WriteString(eventType)
	framed.WriteString("\ndata: ")
	framed.Write(data)
	framed.WriteString("\n\n")
	b.pending.Reset(framed.Bytes())
}

func (b *eventBody) fail(code, message string) {
	data, _ := json.Marshal(map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"status": "failed", "output": []any{},
			"error": map[string]string{"code": code, "message": message},
		},
	})
	b.setEvent("response.failed", data)
	b.terminal = true
	b.shutdown()
}

// Response event names can grow upstream. Validate their wire syntax without
// maintaining a brittle allowlist, and reject client commands as server output.
func responseEvent(name string) (terminal, known bool) {
	if len(name) == 0 || len(name) > 128 {
		return false, false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_') {
			return false, false
		}
	}
	switch name {
	case "response.completed", "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
		return true, true
	case "response.create", "response.cancel":
		return false, false
	default:
		return false, strings.HasPrefix(name, "response.")
	}
}

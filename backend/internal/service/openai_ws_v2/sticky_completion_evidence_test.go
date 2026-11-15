package openai_ws_v2

import (
	"context"
	"fmt"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestRelay_StickyMigrationTerminalStatusEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, status, extra string
		invalid             bool
	}{
		{name: "completed", status: "completed"},
		{name: "legacy omitted status"},
		{name: "incomplete", status: "incomplete", invalid: true},
		{name: "cancelled", status: "cancelled", invalid: true},
		{name: "error body", status: "completed", extra: `,"error":{"message":"generation failed"}`, invalid: true},
		{name: "error string", status: "completed", extra: `,"error":"generation failed"`, invalid: true},
		{name: "incomplete details", status: "completed", extra: `,"incomplete_details":{"reason":"max_output_tokens"}`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_status","status":%q,"usage":{"input_tokens":7,"output_tokens":2}%s}}`, tc.status, tc.extra)
			client := newPassthroughTestFrameConn(nil, false)
			upstream := newPassthroughTestFrameConn([]passthroughTestFrame{{msgType: coderws.MessageText, payload: []byte(wire)}}, true)
			var turns []RelayTurnResult
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, exit := Relay(ctx, client, upstream, []byte(`{"type":"response.create","model":"synthetic"}`), RelayOptions{OnTurnComplete: func(turn RelayTurnResult) { turns = append(turns, turn) }})
			require.Nil(t, exit)
			require.Len(t, turns, 1)
			require.Equal(t, "response.completed", turns[0].TerminalEventType)
			require.Equal(t, tc.invalid, turns[0].TerminalResponseInvalid)
			require.Equal(t, 7, turns[0].Usage.InputTokens)
		})
	}
}

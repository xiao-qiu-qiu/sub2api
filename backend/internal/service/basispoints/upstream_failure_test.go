package basispoints

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bpsws"
	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestUpstreamFailureClassificationBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		event  object
		status int
		code   string
	}{
		{"flat error", object{"type": "error", "code": "rate_limit_exceeded", "message": "PRIVATE"}, 429, "rate_limit_exceeded"},
		{"nested status", object{"type": "error", "error": object{"status": 401, "code": "PRIVATE", "type": "invalid_request_error"}}, 401, "basispoints_upstream_error"},
		{"explicit first", object{"type": "error", "status": 429, "error": object{"status": 400, "code": "invalid_value"}}, 429, "invalid_value"},
		{"invalid success", object{"type": "error", "status": 200, "error": object{"code": "rate_limit_exceeded"}}, 429, "rate_limit_exceeded"},
		{"invalid range", object{"type": "error", "status": 600}, 502, "basispoints_upstream_error"},
		{"invalid fraction", object{"type": "error", "status": 429.5}, 502, "basispoints_upstream_error"},
		{"invalid string", object{"type": "error", "status": "429 PRIVATE"}, 502, "basispoints_upstream_error"},
		{"type only", object{"type": "response.failed", "response": object{"error": object{"type": "rate_limit_error"}}}, 429, "basispoints_upstream_error"},
		{"no error", object{"type": "response.cancelled"}, 502, "basispoints_upstream_cancelled"},
		{"not failure", object{"type": "response.completed", "error": object{"code": "rate_limit_exceeded"}}, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.event)
			require.NoError(t, err)
			failure := ParseUpstreamFailure(raw)
			if tc.status == 0 {
				require.Nil(t, failure)
				return
			}
			require.NotNil(t, failure)
			require.Equal(t, tc.status, failure.Status)
			require.Equal(t, tc.code, failure.Code)
			require.NotContains(t, failure.Error(), "PRIVATE")
		})
	}
	require.Nil(t, ParseUpstreamFailure([]byte("invalid JSON")))
}

func TestUpstreamFailureWebSocketReadClassification(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		closeStatus              websocket.StatusCode
		status                   int
		code, errorType, message string
	}{
		{"remote message limit", websocket.StatusMessageTooBig, http.StatusRequestEntityTooLarge,
			"basispoints_ws_message_too_big", "invalid_request_error",
			"Excel BPS WebSocket request exceeds the upstream message size limit; request was not replayed"},
		{"ordinary disconnect", websocket.StatusPolicyViolation, http.StatusBadGateway,
			"basispoints_upstream_error", "server_error",
			"Excel BPS upstream failure: basispoints_upstream_error; request was not replayed"},
		{"local response limit", 0, http.StatusBadGateway,
			"basispoints_ws_response_too_big", "server_error",
			"Excel BPS upstream WebSocket response exceeds the local read limit; request was not replayed"},
	} {
		for _, clientStream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, clientStream), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				var connections, creates atomic.Int32
				closed := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					connections.Add(1)
					defer close(closed)
					conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
						Subprotocols: []string{"responses"}, OriginPatterns: []string{"bps.openai.com"},
					})
					require.NoError(t, err)
					defer func() { _ = conn.CloseNow() }()
					kind, data, err := conn.Read(ctx)
					require.NoError(t, err)
					require.Equal(t, websocket.MessageText, kind)
					var create object
					require.NoError(t, decode(data, &create))
					require.Equal(t, "response.create", create["type"])
					creates.Add(1)
					if tc.closeStatus != 0 {
						_ = conn.Close(tc.closeStatus, "PRIVATE_UPSTREAM_CLOSE_REASON Bearer PRIVATE_WS_BEARER")
						return
					}
					_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.output_text.delta","delta":"`+strings.Repeat("x", 16<<20)+`"}`))
				}))
				defer server.Close()
				source := testSource()
				source["stream"] = clientStream
				prepared, bridge := mustPrepare(t, source, "", nil)
				raw, err := json.Marshal(prepared)
				require.NoError(t, err)
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(string(raw)))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer PRIVATE_WS_BEARER")
				response, err := bpsws.Open(ctx, server.Client(), req, 0)
				require.NoError(t, err)
				defer func() { _ = response.Body.Close() }()
				repairs := 0
				body := bridge.StreamWithRepairs(ctx, response.Body,
					func(context.Context, object, error) (object, error) { repairs++; return nil, io.ErrUnexpectedEOF },
					func(context.Context) (io.ReadCloser, error) { repairs++; return nil, io.ErrUnexpectedEOF })
				events := repairEvents(t, body)
				require.Len(t, events, 1)
				require.Equal(t, "response.failed", events[0]["type"])
				raw, err = json.Marshal(events[0])
				require.NoError(t, err)
				require.NotContains(t, string(raw), "PRIVATE_")
				failure := ParseUpstreamFailure(raw)
				require.NotNil(t, failure)
				require.Equal(t, tc.status, failure.Status)
				require.Equal(t, tc.code, failure.Code)
				require.Equal(t, tc.errorType, failure.Type)
				require.Equal(t, tc.message, failure.Message)
				errorDetail := repairValue[object](t, repairValue[object](t, events[0]["response"])["error"])
				require.Equal(t, json.Number(fmt.Sprint(tc.status)), errorDetail["status"])
				require.Equal(t, tc.code, errorDetail["code"])
				require.Equal(t, tc.errorType, errorDetail["type"])
				require.Equal(t, tc.message, errorDetail["message"])
				require.Zero(t, repairs, "do not retry or replay an already sent request")
				select {
				case <-closed:
				case <-ctx.Done():
					t.Fatal("WebSocket server did not finish")
				}
				require.EqualValues(t, 1, connections.Load())
				require.EqualValues(t, 1, creates.Load())
			})
		}
	}
}

func TestUpstreamFailureErrorEventParamRedaction(t *testing.T) {
	for _, tc := range []struct {
		name  string
		param any
		want  any
	}{
		{"field", "input", "input"},
		{"indexed field", "input[2].content[0].image_url", "input[2].content[0].image_url"},
		{"tool schema", "tools[0].function.parameters", "tools[0].function.parameters"},
		{"null", nil, nil},
		{"empty", "", nil},
		{"wrong type", object{"private": "PRIVATE"}, nil},
		{"private identifier", "private_parameter", nil},
		{"private field", "input[0].PRIVATE", nil},
		{"URL", "https://example.invalid/?token=PRIVATE", nil},
		{"bearer", "Bearer PRIVATE", nil},
		{"control", "input\nPRIVATE", nil},
		{"path separator", "input/id", nil},
		{"invalid index", "input[1x].id", nil},
		{"oversized index", "input[" + strings.Repeat("1", 257) + "].id", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, bridge := mustPrepare(t, testSource(), "", nil)
			body := bridge.Stream(io.NopCloser(strings.NewReader(sse(object{
				"type": "error", "code": "invalid_value", "message": "PRIVATE", "param": tc.param,
			}))))
			defer func() { _ = body.Close() }()
			events := repairEvents(t, body)
			require.Len(t, events, 1)
			event := events[0]
			require.Equal(t, "error", event["type"])
			require.Equal(t, "invalid_value", event["code"])
			require.Equal(t, "Excel BPS upstream failure: invalid_value; request was not replayed", event["message"])
			require.Equal(t, tc.want, event["param"])
			require.Contains(t, event, "param")
			require.NotContains(t, event, "error")
			require.Equal(t, json.Number("0"), event["sequence_number"])
		})
	}
}

func TestUpstreamFailureStopsReadersWithoutEOF(t *testing.T) {
	for _, mode := range []string{"bridge", "tool correction", "unknown tool correction", "compaction"} {
		t.Run(mode, func(t *testing.T) {
			_, bridge := mustPrepare(t, testSource(), "", nil)
			reader, writer := io.Pipe()
			t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
			wire := sse(object{"type": "response.cancelled", "response": object{
				"id": "resp_cancelled", "status": "cancelled", "output": []any{},
				"error": object{"code": "rate_limit_exceeded", "message": "PRIVATE"},
				"usage": object{"input_tokens": 7},
			}})
			go func() { _, _ = io.WriteString(writer, wire) }()
			done := make(chan error, 1)
			go func() {
				switch mode {
				case "bridge":
					body := bridge.Stream(reader)
					defer func() { _ = body.Close() }()
					raw, err := io.ReadAll(body)
					if err == nil && (!strings.Contains(string(raw), "rate_limit_exceeded") || strings.Contains(string(raw), "PRIVATE")) {
						err = io.ErrUnexpectedEOF
					}
					done <- err
				case "tool correction":
					_, err := ReadToolRepairResponse(reader)
					done <- err
				case "unknown tool correction":
					_, err := readRepairResponse(reader)
					done <- err
				case "compaction":
					_, err := ReadImageCompaction(reader, nil)
					done <- err
				}
			}()
			select {
			case err := <-done:
				if mode == "bridge" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
					require.NotContains(t, err.Error(), "PRIVATE")
					if mode != "compaction" {
						var failure *UpstreamFailure
						require.ErrorAs(t, err, &failure)
						require.Equal(t, 429, failure.Status)
					}
				}
			case <-time.After(3 * time.Second):
				t.Fatal("reader waited for EOF after cancellation terminal")
			}
		})
	}
}

func TestUpstreamFailureWithholdsPendingToolsAndNeverRepairs(t *testing.T) {
	source := testSource()
	source["tools"] = []any{object{"type": "function", "name": "shell"}}
	_, bridge := mustPrepare(t, source, "", nil)
	tool := nativeCall(object{"name": "shell", "arguments": object{"cmd": "PRIVATE_TOOL_ARGUMENTS"}})
	wire := sse(object{"type": "response.output_item.done", "item": tool, "output_index": 0}) +
		sse(object{"type": "response.failed", "response": object{
			"status": "failed", "output": []any{tool}, "error": object{"code": "invalid_value", "message": "PRIVATE"},
		}})
	corrections := 0
	body := bridge.StreamWithRepairs(context.Background(), io.NopCloser(strings.NewReader(wire)),
		func(context.Context, object, error) (object, error) { corrections++; return nil, nil },
		func(context.Context) (io.ReadCloser, error) { corrections++; return nil, nil })
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Zero(t, corrections)
	require.NotContains(t, string(raw), "PRIVATE")
	require.NotContains(t, string(raw), "function_call")
	require.Contains(t, string(raw), "invalid_value")
}

func TestCompactedCancellationRetainsWindowAndUsage(t *testing.T) {
	wire := sse(object{"type": "response.cancelled", "response": object{
		"id": "resp_cancelled", "status": "cancelled", "output": []any{},
		"usage": object{"input_tokens": 7, "output_tokens": 2},
		"error": object{"code": "rate_limit_exceeded"},
	}})
	window := []any{object{"type": "compaction", "id": "compaction_test"}}
	body := WithCompactedWindow(context.Background(), io.NopCloser(strings.NewReader(wire)), window, object{"input_tokens": 5, "output_tokens": 1})
	defer func() { _ = body.Close() }()
	events := repairEvents(t, body)
	last := events[len(events)-1]
	require.Equal(t, "response.cancelled", last["type"])
	response := mustTestValue[object](t, last["response"])
	require.Equal(t, window, response["output"])
	usage := mustTestValue[object](t, response["usage"])
	require.Equal(t, json.Number("12"), usage["input_tokens"])
	require.Equal(t, json.Number("3"), usage["output_tokens"])
}

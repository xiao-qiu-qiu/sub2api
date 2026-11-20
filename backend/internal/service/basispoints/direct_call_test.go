package basispoints

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The model sometimes calls a declared client tool by its own name instead of
// wrapping it in run_officejs. The bridge recovers this into the client tool call
// rather than failing the whole response, and never executes anything.
func TestDirectFunctionCallRecoveredIntoClientTool(t *testing.T) {
	cache := new(ReplayCache)
	source := testSource()
	source["tools"] = []any{object{"type": "function", "name": "shell", "parameters": object{"type": "object"}}}
	_, bridge := mustPrepare(t, source, "scope", cache)
	for _, name := range []string{"shell", "functions.shell"} {
		native := object{"type": "function_call", "id": "fc_" + name, "call_id": "call_" + name, "name": name, "arguments": `{"command":["ls","-la"]}`}
		call, err := bridge.translateCall(native)
		if err != nil {
			t.Fatalf("%s: direct call not recovered: %v", name, err)
		}
		if call["type"] != "function_call" || call["name"] != "shell" || call["call_id"] != "call_"+name {
			t.Fatalf("%s: wrong client tool item: %+v", name, call)
		}
		var args object
		if decode([]byte(text(call["arguments"])), &args) != nil {
			t.Fatalf("%s: arguments not valid JSON object: %v", name, call["arguments"])
		}
		if cmd, _ := args["command"].([]any); len(cmd) != 2 {
			t.Fatalf("%s: arguments lost: %+v", name, args)
		}
	}
}

// A direct custom tool call is relayed as custom_tool_call with its exact input.
func TestDirectCustomCallRecoveredIntoClientTool(t *testing.T) {
	cache := new(ReplayCache)
	source := testSource()
	source["tools"] = []any{object{"type": "namespace", "name": "functions", "tools": []any{object{"type": "custom", "name": "apply_patch"}}}}
	_, bridge := mustPrepare(t, source, "scope", cache)
	patch := "*** Begin Patch\n*** Add File: a.txt\n+hi\n*** End Patch"
	native := object{"type": "custom_tool_call", "id": "ctc_a", "call_id": "call_a", "name": "functions.apply_patch", "input": patch}
	call, err := bridge.translateCall(native)
	if err != nil {
		t.Fatalf("direct custom call not recovered: %v", err)
	}
	if call["type"] != "custom_tool_call" || call["name"] != "apply_patch" || call["namespace"] != "functions" || call["input"] != patch {
		t.Fatalf("wrong custom tool item: %+v", call)
	}
}

// A direct call still refuses tools outside the catalog and genuine native tools.
func TestDirectCallRejectsUnknownAndMismatchedTools(t *testing.T) {
	source := testSource()
	source["tools"] = []any{object{"type": "function", "name": "shell"}, object{"type": "custom", "name": "patch"}}
	_, bridge := mustPrepare(t, source, "scope", nil)
	cases := []object{
		{"type": "function_call", "id": "1", "call_id": "c1", "name": "read_ranges", "arguments": "{}"},
		{"type": "function_call", "id": "2", "call_id": "c2", "name": "search_workbook", "arguments": "{}"},
		{"type": "custom_tool_call", "id": "3", "call_id": "c3", "name": "shell", "input": "ls"},  // function tool arriving as custom
		{"type": "function_call", "id": "4", "call_id": "c4", "name": "patch", "arguments": "{}"}, // custom tool arriving as function
	}
	for i, native := range cases {
		if _, err := bridge.translateCall(native); err == nil {
			t.Fatalf("case %d must be rejected: %+v", i, native)
		}
	}
}

func TestUnsupportedNativeToolStreamFailureIncludesSafeName(t *testing.T) {
	for _, test := range []struct {
		name     string
		toolName string
		wantName string
	}{
		{name: "qualified name", toolName: "functions.abc", wantName: "functions.abc"},
		{name: "unsafe name", toolName: "functions.abc?secret=private-name", wantName: "[redacted]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := testSource()
			source["stream"] = true
			source["tools"] = []any{object{"type": "function", "name": "shell"}}
			cache := new(ReplayCache)
			_, bridge := mustPrepare(t, source, "scope", cache)
			native := object{
				"type": "function_call", "id": "fc_other", "call_id": "call_other",
				"name": test.toolName, "arguments": `{"note":"private-tool-argument"}`,
			}
			wire := sse(object{"type": "response.created", "response": object{"id": "resp_original"}}) +
				sse(object{"type": "response.output_item.added", "item": native}) +
				sse(object{"type": "response.output_item.done", "item": native}) +
				sse(object{"type": "response.completed", "response": repairResponse("resp_original", 10, 2, native)})
			corrections, regenerations := 0, 0
			body := bridge.StreamWithRepairs(context.Background(), io.NopCloser(strings.NewReader(wire)), func(context.Context, object, error) (object, error) {
				corrections++
				return nil, nil
			}, func(context.Context) (io.ReadCloser, error) {
				regenerations++
				return io.NopCloser(strings.NewReader("")), nil
			})
			events := repairEvents(t, body)
			require.Len(t, events, 2)
			require.Equal(t, "response.created", events[0]["type"])
			require.Equal(t, "response.failed", events[1]["type"])
			response := repairValue[object](t, events[1]["response"])
			require.Empty(t, response["output"])
			detail := repairValue[object](t, response["error"])
			require.Equal(t, "basispoints_protocol_error", detail["code"])
			require.Equal(t, "bridge_tool_validation", detail["failure_stage"])
			require.Equal(t, test.wantName, detail["rejected_tool"])
			require.Contains(t, text(detail["message"]), test.wantName)
			require.NotContains(t, quoted(events), "private-tool-argument")
			require.NotContains(t, quoted(events), "private-name")
			require.Zero(t, corrections)
			require.Zero(t, regenerations)
			require.Nil(t, cache.get("scope", "call_other"))
		})
	}
}

// A direct call cached for replay must reappear as a BPS-known run_officejs wrapper
// on the next turn, and the recorded tool output must round-trip under that identity.
func TestDirectCallReplaysAsTransportWrapper(t *testing.T) {
	cache := new(ReplayCache)
	source := testSource()
	source["tools"] = []any{object{"type": "function", "name": "shell", "parameters": object{"type": "object"}}}
	_, bridge := mustPrepare(t, source, "account/key", cache)
	native := object{"type": "function_call", "id": "fc_d", "call_id": "call_d", "name": "shell", "arguments": `{"command":["pwd"]}`}
	call, err := bridge.translateCall(native)
	if err != nil {
		t.Fatal(err)
	}
	source["input"] = []any{message("user", "run"), call, object{"type": "function_call_output", "call_id": call["call_id"], "output": "/root"}}
	replayed, _ := mustPrepare(t, source, "account/key", cache)
	items := mustTestValue[[]any](t, replayed["input"])
	wrapper, _ := items[len(items)-2].(object)
	if text(wrapper["type"]) != "function_call" || text(wrapper["name"]) != "run_officejs" {
		t.Fatalf("direct call did not replay as a run_officejs transport item: %+v", wrapper)
	}
	if !strings.Contains(text(wrapper["arguments"]), `\"command\"`) {
		t.Fatalf("replayed transport lost the client arguments: %+v", wrapper["arguments"])
	}
	output, _ := items[len(items)-1].(object)
	if output["call_id"] != call["call_id"] || output["output"] != "/root" {
		t.Fatalf("tool output identity broken: %+v", output)
	}
	// Determinism: an identical follow-up request must serialize identically.
	again, _ := mustPrepare(t, source, "account/key", cache)
	if !reflect.DeepEqual(replayed, again) {
		t.Fatal("direct-call replay is not deterministic")
	}
}

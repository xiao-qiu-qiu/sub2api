package basispoints

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestClientInstructionsDefaultAndCustom(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []PrepareOptions
		want string
	}{
		{name: "ordinary prepare", want: DefaultClientInstructions},
		{name: "zero options", opts: []PrepareOptions{{}}, want: DefaultClientInstructions},
		{name: "blank", opts: []PrepareOptions{{ClientInstructions: " \t\r\n"}}, want: DefaultClientInstructions},
		{name: "unicode blank", opts: []PrepareOptions{{ClientInstructions: "\u00a0\u3000"}}, want: DefaultClientInstructions},
		{name: "custom", opts: []PrepareOptions{{ClientInstructions: "  Follow the current client's research workflow.\n"}}, want: "  Follow the current client's research workflow.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, withTools := range []bool{false, true} {
				name := "without tools"
				if withTools {
					name = "with workbook tool"
				}
				t.Run(name, func(t *testing.T) {
					source := object{
						"model": "gpt-6-astra", "instructions": "Request instructions",
						"input": []any{message("developer", "Conversation developer instructions"), message("user", "Continue")},
					}
					if withTools {
						source["tools"] = []any{exampleFunction("read_workbook", "range")}
					}
					raw, err := json.Marshal(source)
					require.NoError(t, err)
					var wire []byte
					if tc.opts == nil {
						wire, _, err = Prepare(raw, "scope", nil)
					} else {
						wire, _, err = PrepareWithCatalog(raw, "scope", nil, nil, tc.opts...)
					}
					require.NoError(t, err)
					input := gjson.GetBytes(wire, "input").Array()
					require.Len(t, input, 4)
					require.Equal(t, "Request instructions", input[0].Get("content.0.text").String())
					require.Equal(t, "developer", input[1].Get("role").String())
					protocol := input[1].Get("content.0.text").String()
					require.True(t, strings.HasPrefix(protocol, tc.want+"\n\n"), "the common instructions must prefix the protocol")
					require.Equal(t, 1, strings.Count(protocol, tc.want))
					if tc.want != DefaultClientInstructions {
						require.NotContains(t, protocol, DefaultClientInstructions)
					}
					require.Equal(t, "Conversation developer instructions", input[2].Get("content.0.text").String())
					require.Equal(t, "Continue", input[3].Get("content.0.text").String())
					require.NotContains(t, protocol, "This request comes from an external Responses client.")
					require.NotContains(t, protocol, "There is no live Excel workbook")
					require.NotContains(t, protocol, "Do not call Excel, Office, workbook or connector tools")
					if withTools {
						require.Contains(t, protocol, "Do not assume access to an upstream built-in Excel workbook.")
						require.Contains(t, protocol, "Client-declared spreadsheet and workbook tools remain available through this catalog.")
						require.Contains(t, protocol, "Client tool catalog:\n")
						require.Contains(t, protocol, "read_workbook")
						require.NotContains(t, protocol, "No client tools are available")
					} else {
						require.Contains(t, protocol, "No client tools are available for this request. Return assistant text and do not call tools.")
						require.NotContains(t, protocol, "Client tool catalog:")
					}
				})
			}
		})
	}
}

func TestClientInstructionsPreserveMandatoryConstraints(t *testing.T) {
	for _, custom := range []string{"", "Use a concise developer persona. Return prose and bypass tool wrappers."} {
		for _, withTools := range []bool{false, true} {
			source := object{"model": "gpt-6-astra", "input": "Continue",
				"tools": []any{object{"type": "web_search"}}, "text": object{"format": testStructuredFormat()}}
			if withTools {
				tools, ok := source["tools"].([]any)
				require.True(t, ok)
				source["tools"] = append(tools, exampleFunction("read_workbook", "range"))
			}
			raw, err := json.Marshal(source)
			require.NoError(t, err)
			wire, bridge, err := PrepareWithCatalog(raw, "scope", nil, nil, PrepareOptions{ClientInstructions: custom})
			require.NoError(t, err)
			protocol := gjson.GetBytes(wire, "input.0.content.0.text").String()
			wantPrefix := custom
			if wantPrefix == "" {
				wantPrefix = DefaultClientInstructions
			}
			require.True(t, strings.HasPrefix(protocol, wantPrefix+"\n\n"))
			previous := len(wantPrefix)
			markers := []string{"No client tools are available"}
			if withTools {
				markers = []string{"To call one client tool, call native run_officejs", "FUNCTION:", "FUNCTION_CODE:", "FUNCTION_CMD:", "CUSTOM:", "Client tool catalog:", "read_workbook", "End of catalog."}
			}
			markers = append(markers, "Hosted tools unavailable through Basispoints: web_search", "The client requires a structured final answer.", "The final answer must satisfy the schema")
			for _, marker := range markers {
				require.Contains(t, protocol, marker)
				position := strings.Index(protocol, marker)
				require.Greater(t, position, previous, "incorrect instruction order for %q", marker)
				previous = position
			}
			require.Equal(t, []string{"Hosted tools unavailable through Basispoints: web_search"}, bridge.Warnings)
			require.NotNil(t, bridge.structured)
			require.NoError(t, bridge.structured.validate(object{"output": []any{structuredMessage(`{"answer":9007199254740993}`)}}))
			require.Error(t, bridge.structured.validate(object{"output": []any{structuredMessage(`{"answer":1}`)}}))
			require.Error(t, bridge.structured.validate(object{"output": []any{structuredMessage("prose")}}))
			if withTools {
				call, err := bridge.translateCall(nativeCall(object{"name": "read_workbook", "arguments": object{"range": "A1"}}))
				require.NoError(t, err)
				require.Equal(t, "read_workbook", call["name"])
				_, err = bridge.translateCall(nativeCall(object{"name": "read_workbook", "arguments": object{"range": 42}}))
				require.Error(t, err, "custom instructions must not bypass schema validation")
			}
			_, err = bridge.translateCall(nativeCall(object{"name": "undeclared", "arguments": object{}}))
			require.Error(t, err, "custom instructions must not grant undeclared tools")
		}
	}
}

func TestClientInstructionsSkipAllInjectedInstructions(t *testing.T) {
	for _, custom := range []string{"", "\t \n", "Custom administrator instructions"} {
		for _, withTools := range []bool{false, true} {
			clientHistory := []any{message("developer", DefaultClientInstructions), message("user", "Continue")}
			source := object{"model": "gpt-6-astra", "instructions": "Request instructions", "input": clientHistory,
				"tools": []any{object{"type": "web_search"}}, "text": object{"format": testStructuredFormat()}}
			if withTools {
				tools, ok := source["tools"].([]any)
				require.True(t, ok)
				source["tools"] = append(tools, exampleFunction("read_workbook", "range"))
			}
			raw, err := json.Marshal(source)
			require.NoError(t, err)
			opts := PrepareOptions{ClientInstructions: custom, SkipProtocolInstructions: true}
			wire, bridge, err := PrepareWithCatalog(raw, "scope", nil, new(CatalogCache), opts)
			require.NoError(t, err)
			want, err := json.Marshal(append([]any{message("developer", "Request instructions")}, clientHistory...))
			require.NoError(t, err)
			require.JSONEq(t, string(want), gjson.GetBytes(wire, "input").Raw)
			require.Equal(t, opts, bridge.prepareOptions)
			require.Len(t, bridge.Warnings, 1)
			require.NotNil(t, bridge.structured, "skipping prompts must not skip structured-output validation")
			require.Error(t, bridge.structured.validate(object{"output": []any{structuredMessage("prose")}}))
			_, err = bridge.translateCall(nativeCall(object{"name": "undeclared", "arguments": object{}}))
			require.Error(t, err, "skipping prompts must not skip tool validation")
			reprepared, again, err := bridge.Reprepare(raw)
			require.NoError(t, err)
			require.Equal(t, opts, again.prepareOptions)
			require.JSONEq(t, gjson.GetBytes(wire, "input").Raw, gjson.GetBytes(reprepared, "input").Raw)
		}
	}
}

func TestClientInstructionsCatalogPathsAndReprepare(t *testing.T) {
	for _, tc := range []struct {
		name      string
		scope     string
		cache     bool
		inherited bool
		disabled  bool
		native    bool
	}{
		{name: "no cache", scope: "scope"},
		{name: "empty scope", cache: true},
		{name: "explicit catalog", scope: "scope", cache: true},
		{name: "inherited catalog", scope: "scope", cache: true, inherited: true},
		{name: "tool choice none", scope: "scope", cache: true, inherited: true, disabled: true},
		{name: "native images inherited", scope: "scope", cache: true, inherited: true, native: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, opts := range []PrepareOptions{{}, {ClientInstructions: "Current administrator instructions"}, {ClientInstructions: " \t\n"}, {ClientInstructions: "Skipped administrator instructions", SkipProtocolInstructions: true}} {
				var cache *CatalogCache
				if tc.cache {
					cache = new(CatalogCache)
				}
				source := object{"model": "gpt-6-astra", "input": "Continue", "tools": []any{exampleFunction("read_workbook", "range")}}
				if tc.inherited {
					seed, err := json.Marshal(source)
					require.NoError(t, err)
					_, _, err = PrepareWithCatalog(seed, tc.scope, nil, cache, PrepareOptions{ClientInstructions: "Previous administrator instructions"})
					require.NoError(t, err)
					delete(source, "tools")
				}
				if tc.disabled {
					source["tool_choice"] = "none"
				}
				raw, err := json.Marshal(source)
				require.NoError(t, err)
				var wire []byte
				var bridge *Bridge
				if tc.native {
					plan, err := PrepareNativeImages(raw)
					require.NoError(t, err)
					wire, bridge, err = plan.PrepareWithCatalog(tc.scope, nil, cache, opts)
					require.NoError(t, err)
				} else {
					wire, bridge, err = PrepareWithCatalog(raw, tc.scope, nil, cache, opts)
					require.NoError(t, err)
				}
				require.Equal(t, opts, bridge.prepareOptions)
				input := gjson.GetBytes(wire, "input").Array()
				if opts.SkipProtocolInstructions {
					require.Len(t, input, 1)
					require.Equal(t, "user", input[0].Get("role").String())
				} else {
					require.Len(t, input, 2)
					want := opts.ClientInstructions
					if strings.TrimSpace(want) == "" {
						want = DefaultClientInstructions
					}
					protocol := input[0].Get("content.0.text").String()
					require.True(t, strings.HasPrefix(protocol, want+"\n\n"))
					require.NotContains(t, protocol, "Previous administrator instructions")
				}
				if tc.disabled {
					require.Empty(t, bridge.tools)
				} else {
					require.Contains(t, bridge.tools, "read_workbook")
				}
				reprepared, again, err := bridge.Reprepare(raw)
				require.NoError(t, err)
				require.Equal(t, opts, again.prepareOptions)
				require.JSONEq(t, gjson.GetBytes(wire, "input").Raw, gjson.GetBytes(reprepared, "input").Raw)
			}
		})
	}
}

func TestClientInstructionsCompactionOptions(t *testing.T) {
	for _, opts := range []PrepareOptions{{}, {ClientInstructions: "Current administrator instructions"}, {ClientInstructions: " \t\n"}, {ClientInstructions: "Skipped administrator instructions", SkipProtocolInstructions: true}} {
		source := object{"model": "gpt-6-astra", "instructions": "Request instructions",
			"input": []any{message("user", "Earlier request"), message("assistant", "Earlier answer"), message("user", "Continue")},
			"tools": []any{exampleFunction("read_workbook", "range")}, "text": object{"format": testStructuredFormat()}}
		raw, err := json.Marshal(source)
		require.NoError(t, err)
		cache := new(CatalogCache)
		_, _, err = PrepareWithCatalog(raw, "scope", nil, cache)
		require.NoError(t, err)
		history, err := InspectImageHistory(raw)
		require.NoError(t, err)
		compact, err := history.WithInput(history.Input[:2], true)
		require.NoError(t, err)
		for _, native := range []bool{false, true} {
			var wire []byte
			var bridge *Bridge
			if native {
				plan, err := PrepareNativeImages(compact)
				require.NoError(t, err)
				wire, bridge, err = plan.PrepareWithCatalog("scope", nil, cache, opts)
				require.NoError(t, err)
			} else {
				wire, bridge, err = PrepareWithCatalog(compact, "scope", nil, cache, opts)
				require.NoError(t, err)
			}
			require.Equal(t, opts, bridge.prepareOptions)
			require.Empty(t, bridge.tools)
			require.Nil(t, bridge.structured)
			input := gjson.GetBytes(wire, "input").Array()
			require.Equal(t, "Request instructions", input[0].Get("content.0.text").String())
			require.Equal(t, "compaction_trigger", input[len(input)-1].Get("type").String())
			if opts.SkipProtocolInstructions {
				require.Len(t, input, 4)
			} else {
				require.Len(t, input, 5)
				want := opts.ClientInstructions
				if strings.TrimSpace(want) == "" {
					want = DefaultClientInstructions
				}
				protocol := input[1].Get("content.0.text").String()
				require.True(t, strings.HasPrefix(protocol, want+"\n\n"))
				require.Contains(t, protocol, "No client tools are available")
				require.NotContains(t, protocol, "Client tool catalog:")
				require.NotContains(t, protocol, "structured final answer")
			}
			reprepared, again, err := bridge.Reprepare(compact)
			require.NoError(t, err)
			require.Equal(t, opts, again.prepareOptions)
			require.JSONEq(t, gjson.GetBytes(wire, "input").Raw, gjson.GetBytes(reprepared, "input").Raw)
		}
		delete(source, "tools")
		raw, err = json.Marshal(source)
		require.NoError(t, err)
		_, continuation, err := PrepareWithCatalog(raw, "scope", nil, cache, opts)
		require.NoError(t, err)
		require.Contains(t, continuation.tools, "read_workbook", "compaction must not erase the cached catalog")
		require.Equal(t, opts, continuation.prepareOptions)
	}
}

package basispoints

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func configurationUpdateTestItem(fields object) object {
	item := object{"type": "configuration_update"}
	for key, value := range fields {
		item[key] = value
	}
	return item
}

func configurationUpdateTestRaw(t *testing.T, source object) []byte {
	t.Helper()
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func configurationUpdateTestFormat(name string) object {
	return object{"type": "json_schema", "name": name, "strict": true, "schema": object{
		"type": "object", "properties": object{"value": object{"type": "string", "enum": []any{name}}},
		"required": []any{"value"}, "additionalProperties": false,
	}}
}

func TestResolveConfigurationUpdatesWithoutUpdatesPreservesBytes(t *testing.T) {
	for _, raw := range []string{
		" { \"model\" : \"gpt-6-sol\", \"input\" : \"configuration_update stays literal text\" } \n",
		`{"model":"gpt-6-sol","input":[]}`,
		`{"model":"gpt-6-sol","input":[{"role":"user","content":"hello"},{"type":"reasoning","summary":[{"type":"summary_text","text":"kept"}],"encrypted_content":"reasoning-secret"},{"type":"compaction","encrypted_content":"compaction-secret"}],"metadata":{"large":9007199254740993}}`,
	} {
		before := []byte(raw)
		got, changed, err := ResolveConfigurationUpdates(before)
		if err != nil || changed || !bytes.Equal(got, before) {
			t.Fatalf("request without updates changed: changed=%v, err=%v, body=%s", changed, err, got)
		}
		got, changed, effortUpdated, err := ResolveConfigurationUpdatesWithEffort(before)
		if err != nil || changed || effortUpdated || !bytes.Equal(got, before) {
			t.Fatalf("effort-aware resolver changed a request without updates: changed=%v, effortUpdated=%v, err=%v, body=%s", changed, effortUpdated, err, got)
		}
	}
}

func TestResolveConfigurationUpdatesWithEffortReportsExplicitUpdates(t *testing.T) {
	for _, tc := range []struct {
		name          string
		updates       []object
		effortUpdated bool
		wantEffort    string
	}{
		{"no_updates", nil, false, "high"},
		{"empty_update", []object{{}}, false, "high"},
		{"id_only", []object{{"id": "config_1"}}, false, "high"},
		{"reasoning_null", []object{{"reasoning": nil}}, false, "high"},
		{"reasoning_empty", []object{{"reasoning": object{}}}, false, "high"},
		{"effort_null", []object{{"reasoning": object{"effort": nil}}}, false, "high"},
		{"text_null", []object{{"text": nil}}, false, "high"},
		{"text_empty", []object{{"text": object{}}}, false, "high"},
		{"format_null", []object{{"text": object{"format": nil}}}, false, "high"},
		{"schema_only", []object{{"text": object{"format": configurationUpdateTestFormat("after")}}}, false, "high"},
		{"json_object_only", []object{{"text": object{"format": object{"type": "json_object"}}}}, false, "high"},
		{"plain_text_only", []object{{"text": object{"format": object{"type": "text"}}}}, false, "high"},
		{"all_noops", []object{{}, {"reasoning": nil}, {"reasoning": object{"effort": nil}}, {"text": object{"format": nil}}}, false, "high"},
		{"same_as_initial_is_explicit", []object{{"reasoning": object{"effort": "high"}}}, true, "high"},
		{"different_from_initial", []object{{"reasoning": object{"effort": "low"}}}, true, "low"},
		{"alias_is_explicit", []object{{"reasoning": object{"effort": "ultra"}}}, true, "ultra"},
		{"later_null_does_not_reset_flag", []object{{"reasoning": object{"effort": "low"}}, {"reasoning": object{"effort": nil}}}, true, "low"},
		{"later_format_does_not_reset_flag", []object{{"reasoning": object{"effort": "low"}}, {"text": object{"format": object{"type": "text"}}}}, true, "low"},
		{"return_to_initial_is_still_explicit", []object{{"reasoning": object{"effort": "low"}}, {"reasoning": object{"effort": "high"}}}, true, "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := testSource()
			source["reasoning"] = object{"effort": "high"}
			// Text-only/noop events must not synchronize stale aliases either.
			source["reasoning_effort"] = "low"
			source["output_config"] = object{"effort": "medium"}
			source["text"] = object{"format": configurationUpdateTestFormat("before")}
			input := []any{message("user", "hello")}
			for _, fields := range tc.updates {
				input = append(input, configurationUpdateTestItem(fields))
			}
			source["input"] = input
			raw := configurationUpdateTestRaw(t, source)
			resolved, changed, effortUpdated, err := ResolveConfigurationUpdatesWithEffort(raw)
			if err != nil || changed != (len(tc.updates) != 0) || effortUpdated != tc.effortUpdated {
				t.Fatalf("changed=%v, effortUpdated=%v, err=%v; want changed=%v, effortUpdated=%v", changed, effortUpdated, err, len(tc.updates) != 0, tc.effortUpdated)
			}
			var normalized object
			if err := decode(resolved, &normalized); err != nil {
				t.Fatal(err)
			}
			if got := mustTestValue[object](t, normalized["reasoning"])["effort"]; got != tc.wantEffort {
				t.Fatalf("current effort=%v, want %q", got, tc.wantEffort)
			}
			wantLegacy, wantOutput := "low", "medium"
			if tc.effortUpdated {
				wantLegacy, wantOutput = tc.wantEffort, tc.wantEffort
			}
			if normalized["reasoning_effort"] != wantLegacy || mustTestValue[object](t, normalized["output_config"])["effort"] != wantOutput {
				t.Fatalf("legacy aliases changed inconsistently with effortUpdated=%v: %s", effortUpdated, resolved)
			}
			legacy, legacyChanged, err := ResolveConfigurationUpdates(raw)
			if err != nil || legacyChanged != changed || !bytes.Equal(legacy, resolved) {
				t.Fatal("legacy and effort-aware helpers resolved different requests")
			}
			second, changed, effortUpdated, err := ResolveConfigurationUpdatesWithEffort(resolved)
			if err != nil || changed || effortUpdated || !bytes.Equal(second, resolved) {
				t.Fatal("resolved effort update was reported again on an idempotent second pass")
			}
		})
	}
}

func TestConfigurationUpdateSynchronizesOnlyExistingEffortAliases(t *testing.T) {
	for _, tc := range []struct {
		name          string
		initial       object
		wantLegacy    bool
		wantOutput    any
		outputPresent bool
	}{
		{"no_aliases", object{}, false, nil, false},
		{"legacy_only", object{"reasoning_effort": "low"}, true, nil, false},
		{"output_only", object{"output_config": object{"effort": "low", "unrelated": "kept"}}, false, object{"effort": "ultra", "unrelated": "kept"}, true},
		{"both_aliases", object{"reasoning_effort": "low", "output_config": object{"effort": "medium"}}, true, object{"effort": "ultra"}, true},
		{"existing_null_aliases", object{"reasoning_effort": nil, "output_config": object{"effort": nil}}, true, object{"effort": "ultra"}, true},
		{"empty_output_is_not_an_effort_alias", object{"output_config": object{}}, false, object{}, true},
		{"unrelated_output_fields_are_not_an_effort_alias", object{"output_config": object{"unrelated": "kept"}}, false, object{"unrelated": "kept"}, true},
		{"null_output_is_not_an_effort_alias", object{"output_config": nil}, false, nil, true},
		{"legacy_with_unrelated_output", object{"reasoning_effort": "high", "output_config": object{"unrelated": "kept"}}, true, object{"unrelated": "kept"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := testSource()
			source["reasoning"] = object{"effort": "high", "mode": "standard"}
			for key, value := range tc.initial {
				source[key] = value
			}
			source["input"] = []any{
				configurationUpdateTestItem(object{"reasoning": object{"effort": "low"}}), message("user", "continue"),
				configurationUpdateTestItem(object{"reasoning": object{"effort": "ultra"}}),
				configurationUpdateTestItem(object{"reasoning": object{"effort": nil}}),
			}
			resolved, changed, effortUpdated, err := ResolveConfigurationUpdatesWithEffort(configurationUpdateTestRaw(t, source))
			if err != nil || !changed || !effortUpdated {
				t.Fatalf("resolve: changed=%v, effortUpdated=%v, err=%v", changed, effortUpdated, err)
			}
			var normalized object
			if err := decode(resolved, &normalized); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(normalized["reasoning"], object{"effort": "ultra", "mode": "standard"}) {
				t.Fatal("effort update lost unrelated reasoning fields or normalized before policy evaluation")
			}
			legacy, exists := normalized["reasoning_effort"]
			if exists != tc.wantLegacy || exists && legacy != "ultra" {
				t.Fatalf("reasoning_effort=%v, present=%v; want present=%v and raw effort ultra", legacy, exists, tc.wantLegacy)
			}
			output, exists := normalized["output_config"]
			if exists != tc.outputPresent || !reflect.DeepEqual(output, tc.wantOutput) {
				t.Fatalf("output_config=%v, present=%v; want %v, present=%v", output, exists, tc.wantOutput, tc.outputPresent)
			}
			_, bridge := mustPrepare(t, source, "configuration-test", nil)
			if bridge.RequestedEffort != "ultra" || bridge.Effort != "xhigh" {
				t.Fatal("Prepare did not normalize the final effort after resolving aliases")
			}
		})
	}
}

func TestConfigurationUpdateEffortFollowsInputOrder(t *testing.T) {
	for _, tc := range []struct {
		name      string
		initial   object
		updates   []object
		requested string
		effective string
	}{
		{"top_level_initial", object{"reasoning": object{"effort": "low"}, "reasoning_effort": "high"}, nil, "low", "low"},
		{"legacy_top_level_initial", object{"reasoning_effort": "high"}, nil, "high", "high"},
		{"default_initial", object{}, nil, "", "medium"},
		{"last_high_wins", object{"reasoning": object{"effort": "medium"}}, []object{{"reasoning": object{"effort": "low"}}, {"reasoning": object{"effort": "high"}}}, "high", "high"},
		{"last_low_wins", object{"reasoning": object{"effort": "high"}}, []object{{"reasoning": object{"effort": "high"}}, {"reasoning": object{"effort": "low"}}}, "low", "low"},
		{"max_normalizes", object{"reasoning": object{"effort": "low"}}, []object{{"reasoning": object{"effort": "max"}}}, "max", "xhigh"},
		{"ultra_normalizes", object{"reasoning_effort": "low"}, []object{{"reasoning": object{"effort": "ultra"}}}, "ultra", "xhigh"},
		{"trimmed_alias_normalizes", object{}, []object{{"reasoning": object{"effort": " ULTRA "}}}, " ULTRA ", "xhigh"},
		{"none_normalizes", object{}, []object{{"reasoning": object{"effort": "none"}}}, "none", "low"},
		{"nulls_do_not_reset_latest", object{"reasoning": object{"effort": "low"}}, []object{{"reasoning": object{"effort": "high"}}, {}, {"reasoning": nil}, {"reasoning": object{}}, {"reasoning": object{"effort": nil}}}, "high", "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := object{"model": "gpt-6-sol"}
			for key, value := range tc.initial {
				source[key] = value
			}
			input := []any{message("user", "first")}
			for _, fields := range tc.updates {
				input = append(input, configurationUpdateTestItem(fields), message("user", "continue"))
			}
			source["input"] = input
			raw := configurationUpdateTestRaw(t, source)
			resolved, changed, err := ResolveConfigurationUpdates(raw)
			if err != nil || changed != (len(tc.updates) != 0) {
				t.Fatalf("resolve: changed=%v, err=%v", changed, err)
			}
			for _, candidate := range [][]byte{raw, resolved} {
				body, bridge, err := Prepare(candidate, "configuration-test", nil)
				if err != nil {
					t.Fatal(err)
				}
				var wire object
				if err := decode(body, &wire); err != nil {
					t.Fatal(err)
				}
				if bridge.RequestedEffort != tc.requested || bridge.Effort != tc.effective || wire["reasoning_effort"] != tc.effective {
					t.Fatalf("requested=%q, effective=%q, wire=%v; want %q/%q", bridge.RequestedEffort, bridge.Effort, wire["reasoning_effort"], tc.requested, tc.effective)
				}
				for _, rawItem := range mustTestValue[[]any](t, wire["input"]) {
					if mustTestValue[object](t, rawItem)["type"] == "configuration_update" {
						t.Fatal("configuration control item leaked into upstream input")
					}
				}
			}
		})
	}
}

func TestConfigurationUpdateEmptyAndNullFieldsAreNoops(t *testing.T) {
	for _, fields := range []object{
		{}, {"id": nil}, {"id": ""}, {"id": "config_1"}, {"reasoning": nil}, {"reasoning": object{}},
		{"reasoning": object{"effort": nil}}, {"text": nil}, {"text": object{}},
		{"text": object{"format": nil}}, {"reasoning": nil, "text": nil},
	} {
		t.Run(string(configurationUpdateTestRaw(t, fields)), func(t *testing.T) {
			source := testStructuredSource(configurationUpdateTestFormat("before"))
			source["reasoning"] = object{"effort": "high"}
			source["input"] = []any{configurationUpdateTestItem(fields), message("user", "continue")}
			resolved, changed, err := ResolveConfigurationUpdates(configurationUpdateTestRaw(t, source))
			if err != nil || !changed {
				t.Fatalf("noop control item was not removed: changed=%v, err=%v", changed, err)
			}
			var normalized object
			if err := decode(resolved, &normalized); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(normalized["reasoning"], source["reasoning"]) || !reflect.DeepEqual(normalized["text"], source["text"]) {
				t.Fatalf("noop reset current configuration: %s", resolved)
			}
			_, bridge := mustPrepare(t, source, "configuration-test", nil)
			if bridge.Effort != "high" || bridge.structured == nil || !reflect.DeepEqual(bridge.structured.format, configurationUpdateTestFormat("before")) {
				t.Fatal("noop changed prepared reasoning or structured output")
			}
		})
	}
}

func TestConfigurationUpdateTextAndReasoningAreIndependent(t *testing.T) {
	before, after := configurationUpdateTestFormat("before"), configurationUpdateTestFormat("after")
	effort := object{"reasoning": object{"effort": "ultra"}}
	schema := object{"text": object{"format": after}}
	plain := object{"text": object{"format": object{"type": "text"}}}
	for _, tc := range []struct {
		name    string
		updates []object
		effort  string
		format  object
	}{
		{"effort_preserves_schema", []object{effort}, "xhigh", before},
		{"schema_preserves_effort", []object{schema}, "low", after},
		{"effort_then_schema", []object{effort, schema}, "xhigh", after},
		{"schema_then_effort", []object{schema, effort}, "xhigh", after},
		{"last_schema_replaces_whole_format", []object{schema, {"text": object{"format": object{"type": "json_object"}}}}, "low", object{"type": "json_object"}},
		{"plain_text_clears_schema", []object{schema, plain}, "low", nil},
		{"cleared_schema_stays_cleared_after_effort", []object{plain, effort}, "xhigh", nil},
		{"schema_can_be_reenabled", []object{plain, schema}, "low", after},
		{"null_does_not_clear_schema", []object{schema, {"text": object{"format": nil}}}, "low", after},
		{"one_item_updates_both", []object{{"reasoning": effort["reasoning"], "text": schema["text"]}}, "xhigh", after},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := testStructuredSource(before)
			source["reasoning"] = object{"effort": "low"}
			input := []any{message("user", "answer")}
			for _, fields := range tc.updates {
				input = append(input, configurationUpdateTestItem(fields))
			}
			source["input"] = input
			wire, bridge := mustPrepare(t, source, "configuration-test", nil)
			if bridge.Effort != tc.effort {
				t.Fatalf("effort=%q, want %q", bridge.Effort, tc.effort)
			}
			if tc.format == nil {
				if bridge.structured != nil {
					t.Fatal("plain text retained the old schema validator")
				}
				if bytes.Contains(configurationUpdateTestRaw(t, wire), []byte("exactly one JSON value")) {
					t.Fatal("plain text retained structured output instructions")
				}
			} else if bridge.structured == nil || !reflect.DeepEqual(bridge.structured.format, tc.format) {
				t.Fatalf("final format does not match %v", tc.format)
			}
			for _, field := range []string{"text", "response_format", "reasoning"} {
				if _, exists := wire[field]; exists {
					t.Fatalf("unsupported native field leaked: %s", field)
				}
			}
		})
	}
}

func TestConfigurationUpdateFinalSchemaValidatesAndEchoes(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		valid        bool
	}{
		{"current_schema", `{"value":"after"}`, true},
		{"old_schema", `{"value":"before"}`, false},
		{"plain_text", "not JSON", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			format := configurationUpdateTestFormat("after")
			source := testStructuredSource(configurationUpdateTestFormat("before"))
			source["input"] = []any{message("user", "answer"), configurationUpdateTestItem(object{"text": object{"format": format}})}
			_, bridge := mustPrepare(t, source, "configuration-test", nil)
			events := structuredEvents(t, bridge, structuredTerminal("response.completed", structuredMessage(tc.answer)))
			last := events[len(events)-1]
			if (last["type"] == "response.completed") != tc.valid {
				t.Fatalf("terminal=%v for %s", last["type"], tc.answer)
			}
			if tc.valid {
				response := mustTestValue[object](t, last["response"])
				config := mustTestValue[object](t, response["text"])
				if !reflect.DeepEqual(config["format"], format) {
					t.Fatal("response echoed the initial schema instead of the latest schema")
				}
			} else if len(events) != 1 || last["type"] != "response.failed" {
				t.Fatal("invalid structured answer leaked before failure")
			}
		})
	}
}

func TestConfigurationUpdatePreservesHistoryAndEncryptedState(t *testing.T) {
	reasoning := object{"type": "reasoning", "id": "rs_history", "summary": []any{object{"type": "summary_text", "text": "original summary"}}, "encrypted_content": "reasoning-ciphertext"}
	compaction := object{"type": "compaction", "id": "cmp_history", "encrypted_content": "compaction-ciphertext"}
	history := []any{
		message("user", "first"), reasoning, message("assistant", "checking"),
		object{"type": "function_call", "call_id": "call_lookup", "name": "lookup", "arguments": `{"large":9007199254740993}`},
		object{"type": "function_call_output", "call_id": "call_lookup", "output": "lookup result"}, compaction,
		object{"type": "custom_tool_call", "call_id": "call_custom", "name": "custom", "input": "raw tool input"},
		object{"type": "custom_tool_call_output", "call_id": "call_custom", "output": "custom result"},
		message("assistant", "done"), message("user", "continue"),
	}
	source := testSource()
	source["metadata"] = object{"large": json.Number("9007199254740993")}
	source["reasoning"] = object{"effort": "low", "mode": "standard"}
	source["text"] = object{"format": object{"type": "text"}, "verbosity": "medium"}
	input := []any{configurationUpdateTestItem(object{"reasoning": object{"effort": "medium"}})}
	for index, item := range history {
		input = append(input, item)
		if index == 3 {
			input = append(input, configurationUpdateTestItem(object{"text": object{"format": configurationUpdateTestFormat("after")}}))
		}
	}
	input = append(input, configurationUpdateTestItem(object{"reasoning": object{"effort": "max"}, "text": object{"format": object{"type": "text"}}}))
	source["input"] = input
	raw := configurationUpdateTestRaw(t, source)
	original := append([]byte(nil), raw...)
	resolved, changed, err := ResolveConfigurationUpdates(raw)
	if err != nil || !changed {
		t.Fatalf("resolve: changed=%v, err=%v", changed, err)
	}
	if !bytes.Equal(raw, original) {
		t.Fatal("resolver mutated caller-owned request bytes")
	}
	var normalized object
	if err := decode(resolved, &normalized); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalized["input"], history) {
		t.Fatal("resolver changed history order, ciphertext, or complete reasoning/compaction objects")
	}
	if !reflect.DeepEqual(normalized["metadata"], source["metadata"]) {
		t.Fatal("resolver lost unrelated metadata or large integer precision")
	}
	if got := mustTestValue[object](t, normalized["reasoning"]); got["mode"] != "standard" || got["effort"] != "max" {
		t.Fatalf("effort update replaced unrelated top-level reasoning fields: %v", got)
	}
	if mustTestValue[object](t, normalized["text"])["verbosity"] != "medium" {
		t.Fatal("format update replaced unrelated top-level text fields")
	}
	second, changed, err := ResolveConfigurationUpdates(resolved)
	if err != nil || changed || !bytes.Equal(second, resolved) {
		t.Fatal("resolver is not idempotent")
	}
	baseline := testSource()
	baseline["input"] = history
	want, _ := mustPrepare(t, baseline, "configuration-test", nil)
	got, bridge := mustPrepare(t, source, "configuration-test", nil)
	if !reflect.DeepEqual(got["input"], want["input"]) || bridge.Effort != "xhigh" {
		t.Fatal("configuration controls changed prepared message/tool history")
	}
	prepared := mustTestValue[[]any](t, got["input"])
	foundReasoning, foundCompaction := false, false
	for _, rawItem := range prepared {
		item := mustTestValue[object](t, rawItem)
		switch item["type"] {
		case "reasoning":
			foundReasoning = true
			if item["encrypted_content"] != reasoning["encrypted_content"] || len(mustTestValue[[]any](t, item["summary"])) != 0 {
				t.Fatal("Prepare changed the existing encrypted-reasoning lowering contract")
			}
		case "compaction":
			foundCompaction = true
			if !reflect.DeepEqual(item, compaction) {
				t.Fatal("Prepare changed encrypted compaction state")
			}
		case "configuration_update":
			t.Fatal("configuration control item leaked upstream")
		}
	}
	if !foundReasoning || !foundCompaction {
		t.Fatal("Prepare dropped encrypted reasoning or compaction")
	}
}

func TestConfigurationUpdateOnlyControlsProducesEmptyResolvedInput(t *testing.T) {
	source := testSource()
	source["input"] = []any{configurationUpdateTestItem(nil), configurationUpdateTestItem(object{"reasoning": object{"effort": "high"}})}
	resolved, changed, err := ResolveConfigurationUpdates(configurationUpdateTestRaw(t, source))
	if err != nil || !changed {
		t.Fatalf("resolve: changed=%v, err=%v", changed, err)
	}
	var normalized object
	if err := decode(resolved, &normalized); err != nil {
		t.Fatal(err)
	}
	if input := mustTestValue[[]any](t, normalized["input"]); len(input) != 0 {
		t.Fatalf("control-only input was not an empty array: %v", input)
	}
	wire, bridge := mustPrepare(t, source, "configuration-test", nil)
	if bridge.Effort != "high" || len(mustTestValue[[]any](t, wire["input"])) != 2 {
		t.Fatal("control-only request did not retain only the existing developer prologue")
	}
}

func TestConfigurationUpdateRejectsInvalidAndUnsupportedFields(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		fields      object
	}{
		{"unknown_item_key", "future_option", object{"future_option": true}},
		{"unknown_null_item_key", "future_option", object{"future_option": nil}},
		{"legacy_effort_key", "reasoning_effort", object{"reasoning_effort": "high"}},
		{"id_number", "id", object{"id": 42}},
		{"id_object", "id", object{"id": object{}}},
		{"reasoning_string", "reasoning", object{"reasoning": "high"}},
		{"reasoning_number", "reasoning", object{"reasoning": 42}},
		{"reasoning_boolean", "reasoning", object{"reasoning": false}},
		{"reasoning_array", "reasoning", object{"reasoning": []any{}}},
		{"effort_empty", "effort", object{"reasoning": object{"effort": ""}}},
		{"effort_whitespace", "effort", object{"reasoning": object{"effort": " \t\n"}}},
		{"effort_number", "effort", object{"reasoning": object{"effort": 42}}},
		{"effort_boolean", "effort", object{"reasoning": object{"effort": true}}},
		{"effort_object", "effort", object{"reasoning": object{"effort": object{}}}},
		{"effort_array", "effort", object{"reasoning": object{"effort": []any{"high"}}}},
		{"effort_unsupported", "effort", object{"reasoning": object{"effort": "super"}}},
		{"mode_standard_is_still_unknown", "mode", object{"reasoning": object{"mode": "standard"}}},
		{"mode_pro", "mode", object{"reasoning": object{"mode": "pro"}}},
		{"mode_null_is_still_unknown", "mode", object{"reasoning": object{"mode": nil}}},
		{"reasoning_summary", "summary", object{"reasoning": object{"summary": "auto"}}},
		{"text_string", "text", object{"text": "plain"}},
		{"text_boolean", "text", object{"text": false}},
		{"text_array", "text", object{"text": []any{}}},
		{"text_verbosity", "verbosity", object{"text": object{"verbosity": "low"}}},
		{"text_unknown_null", "verbosity", object{"text": object{"verbosity": nil}}},
		{"format_string", "format", object{"text": object{"format": "json_schema"}}},
		{"format_boolean", "format", object{"text": object{"format": true}}},
		{"format_array", "format", object{"text": object{"format": []any{}}}},
		{"format_empty", "format", object{"text": object{"format": object{}}}},
		{"format_unsupported_type", "format", object{"text": object{"format": object{"type": "grammar"}}}},
		{"json_object_extra_field", "format", object{"text": object{"format": object{"type": "json_object", "future_option": true}}}},
		{"text_format_extra_field", "format", object{"text": object{"format": object{"type": "text", "future_option": true}}}},
		{"schema_missing", "schema", object{"text": object{"format": object{"type": "json_schema", "name": "answer"}}}},
		{"schema_wrong_type", "schema", object{"text": object{"format": object{"type": "json_schema", "name": "answer", "schema": false}}}},
		{"schema_invalid", "schema", object{"text": object{"format": object{"type": "json_schema", "name": "answer", "schema": object{"type": "invalid"}}}}},
		{"schema_strict_wrong_type", "strict", object{"text": object{"format": object{"type": "json_schema", "name": "answer", "schema": object{}, "strict": "true"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := testSource()
			// A later valid update must not hide an invalid earlier item.
			source["input"] = []any{message("user", "hello"), configurationUpdateTestItem(tc.fields), configurationUpdateTestItem(object{
				"reasoning": object{"effort": "high"}, "text": object{"format": object{"type": "text"}},
			})}
			raw := configurationUpdateTestRaw(t, source)
			resolved, changed, resolveErr := ResolveConfigurationUpdates(raw)
			awareBody, awareChanged, effortUpdated, awareErr := ResolveConfigurationUpdatesWithEffort(raw)
			body, bridge, prepareErr := Prepare(raw, "configuration-test", nil)
			for _, err := range []error{resolveErr, awareErr, prepareErr} {
				if err == nil {
					t.Fatal("invalid configuration update was silently accepted")
				}
				for _, fragment := range []string{"input[1]", "configuration_update", tc.field} {
					if !strings.Contains(err.Error(), fragment) {
						t.Fatalf("error %q does not identify %q", err, fragment)
					}
				}
			}
			if changed || awareChanged || effortUpdated || len(resolved) != 0 || len(awareBody) != 0 || len(body) != 0 || bridge != nil {
				t.Fatal("invalid configuration exposed a partially prepared request")
			}
		})
	}
}

func TestResolveConfigurationUpdatesRejectsInvalidJSON(t *testing.T) {
	for _, raw := range []string{`{`, `null`, `[]`, `"request"`, `{} {}`} {
		if body, changed, err := ResolveConfigurationUpdates([]byte(raw)); err == nil || changed || len(body) != 0 {
			t.Fatalf("invalid request accepted: body=%s, changed=%v, err=%v", body, changed, err)
		}
		if body, changed, effortUpdated, err := ResolveConfigurationUpdatesWithEffort([]byte(raw)); err == nil || changed || effortUpdated || len(body) != 0 {
			t.Fatalf("invalid request accepted by effort-aware resolver: body=%s, changed=%v, effortUpdated=%v, err=%v", body, changed, effortUpdated, err)
		}
	}
}

func TestConfigurationUpdateCompactionReturnsOpaqueWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format object
	}{
		{"json_schema", configurationUpdateTestFormat("after")},
		{"json_object", object{"type": "json_object"}},
		{"text", object{"type": "text"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, order := range []string{"update_then_trigger", "trigger_then_update"} {
				t.Run(order, func(t *testing.T) {
					source := testStructuredSource(configurationUpdateTestFormat("before"))
					update := configurationUpdateTestItem(object{"text": object{"format": tc.format}, "reasoning": object{"effort": "ultra"}})
					trigger := object{"type": "compaction_trigger"}
					input := []any{message("user", "compact the current history")}
					if order == "update_then_trigger" {
						input = append(input, update, trigger)
					} else {
						input = append(input, trigger, update)
					}
					source["input"] = input
					wire, bridge := mustPrepare(t, source, "configuration-test", nil)
					if bridge.structured != nil || bridge.Effort != "xhigh" {
						t.Fatal("explicit compaction retained structured constraints or lost the effort update")
					}
					prepared := mustTestValue[[]any](t, wire["input"])
					if !reflect.DeepEqual(prepared[len(prepared)-1], trigger) {
						t.Fatal("explicit compaction trigger was not retained at the end of input")
					}
					if bytes.Contains(configurationUpdateTestRaw(t, wire), []byte("exactly one JSON value")) {
						t.Fatal("explicit compaction still instructs the model to emit JSON")
					}
					window := object{"type": "compaction", "id": "cmp_configuration_update", "encrypted_content": "opaque-compaction-window"}
					events := structuredEvents(t, bridge,
						sse(object{"type": "response.created", "response": object{"id": "resp_compaction", "output": []any{}}})+
							sse(object{"type": "response.output_item.done", "output_index": 0, "item": window})+
							structuredTerminal("response.completed", window))
					last := events[len(events)-1]
					if last["type"] != "response.completed" {
						t.Fatalf("opaque window failed JSON validation: %v", last)
					}
					response := mustTestValue[object](t, last["response"])
					if !reflect.DeepEqual(response["output"], []any{window}) {
						t.Fatal("opaque compaction output was changed or replaced with a fabricated JSON answer")
					}
					if _, exists := response["text"]; exists {
						t.Fatal("opaque compaction response echoed a user-facing format constraint")
					}
				})
			}
		})
	}
}

func TestConfigurationUpdateCompactionStillRejectsInvalidFormats(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format object
	}{
		{"unsupported", object{"type": "grammar"}},
		{"schema_missing", object{"type": "json_schema", "name": "answer"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, location := range []string{"top_level", "configuration_update"} {
				t.Run(location, func(t *testing.T) {
					source := testSource()
					input := []any{message("user", "compact"), object{"type": "compaction_trigger"}}
					if location == "top_level" {
						source["text"] = object{"format": tc.format}
					} else {
						input = append(input, configurationUpdateTestItem(object{"text": object{"format": tc.format}}))
					}
					source["input"] = input
					body, bridge, err := Prepare(configurationUpdateTestRaw(t, source), "configuration-test", nil)
					if err == nil || len(body) != 0 || bridge != nil {
						t.Fatal("compaction bypassed request format validation")
					}
				})
			}
		})
	}
}

func TestConfigurationUpdateDoesNotHideNewEncryptedAssignment(t *testing.T) {
	for _, location := range []string{"top_level", "content_part"} {
		t.Run(location, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				tail []any
			}{
				{"empty_update", []any{configurationUpdateTestItem(nil)}},
				{"effort_update", []any{configurationUpdateTestItem(object{"reasoning": object{"effort": "max"}})}},
				{"schema_update", []any{configurationUpdateTestItem(object{"text": object{"format": configurationUpdateTestFormat("after")}})}},
				{"multiple_metadata_items", []any{
					object{"type": "additional_tools", "tools": []any{}}, configurationUpdateTestItem(nil),
					object{"type": "tool_search_output", "tools": []any{}},
					configurationUpdateTestItem(object{"reasoning": object{"effort": "high"}}), object{"type": "compaction_trigger"},
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					const secret = "SYNTHETIC_CONFIGURATION_ASSIGNMENT_CIPHERTEXT"
					agent := object{"type": "agent_message", "author": "/root/worker", "recipient": "/root",
						"content": []any{object{"type": "input_text", "text": "Readable task header"}}}
					wantPath := "input[1].encrypted_content"
					if location == "top_level" {
						agent["encrypted_content"] = secret
					} else {
						agent["content"] = append(mustTestValue[[]any](t, agent["content"]), object{"type": "encrypted_content", "encrypted_content": secret})
						wantPath = "input[1].content[1]"
					}
					source := testSource()
					source["input"] = append([]any{message("user", "delegate the task"), agent}, tc.tail...)
					raw := configurationUpdateTestRaw(t, source)
					for _, check := range []struct {
						name string
						run  func(*testing.T) error
					}{
						{"Validate", func(*testing.T) error { return ValidateNewAgentMessage(raw) }},
						{"Strip", func(t *testing.T) error {
							body, err := StripEncryptedContent(raw)
							if len(body) != 0 {
								t.Fatal("Strip exposed an unreadable current task as omitted history")
							}
							return err
						}},
						{"Prepare", func(t *testing.T) error {
							body, bridge, err := Prepare(raw, "configuration-test", nil)
							if len(body) != 0 || bridge != nil {
								t.Fatal("Prepare forwarded an unreadable current task")
							}
							return err
						}},
					} {
						t.Run(check.name, func(t *testing.T) {
							err := check.run(t)
							var contentErr *ContentValidationError
							if !errors.As(err, &contentErr) || contentErr.ContentType != "encrypted_content" || contentErr.Path != wantPath {
								t.Fatalf("current encrypted assignment was not clearly rejected: %v", err)
							}
							if strings.Contains(err.Error(), secret) {
								t.Fatal("encrypted assignment payload leaked in the error")
							}
						})
					}
				})
			}
		})
	}
}

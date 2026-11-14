package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func convertChatBodyForReasoningTest(t *testing.T, body string) []byte {
	t.Helper()
	var req ChatCompletionsRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	out, err := ChatCompletionsToResponses(&req)
	require.NoError(t, err)
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	return raw
}

func TestChatCompletionsToResponsesReasoningEffortForms(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"nested medium", `{"model":"gpt-6-sol","messages":[{"role":"user","content":"hi"}],"reasoning":{"effort":"medium"}}`, "medium"},
		{"flat high", `{"model":"gpt-6-sol","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`, "high"},
		{"nested wins over flat", `{"model":"gpt-6-sol","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high","reasoning":{"effort":"medium"}}`, "medium"},
		{"empty nested falls back to flat", `{"model":"gpt-6-sol","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low","reasoning":{}}`, "low"},
		{"string wins over flat", `{"model":"gpt-6-sol","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low","reasoning":"high"}`, "high"},
		{"empty string falls back to flat", `{"model":"gpt-6-sol","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low","reasoning":""}`, "low"},
		{"whitespace string falls back to flat", `{"model":"gpt-6-sol","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low","reasoning":" \t "}`, "low"},
		{"null falls back to flat", `{"model":"gpt-6-sol","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low","reasoning":null}`, "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := convertChatBodyForReasoningTest(t, tc.body)
			assert.Equal(t, tc.want, gjson.GetBytes(raw, "reasoning.effort").String())
			assert.Equal(t, "auto", gjson.GetBytes(raw, "reasoning.summary").String())
		})
	}
}

func TestChatCompletionsToResponsesNoReasoningStaysUnset(t *testing.T) {
	raw := convertChatBodyForReasoningTest(t, `{"model":"gpt-6-sol","messages":[{"role":"user","content":"hi"}]}`)
	assert.False(t, gjson.GetBytes(raw, "reasoning").Exists())
}

func TestChatCompletionsRequestWithoutNestedReasoningOmitsField(t *testing.T) {
	raw, err := json.Marshal(&ChatCompletionsRequest{Model: "gpt-6-sol", ReasoningEffort: "high"})
	require.NoError(t, err)
	assert.False(t, gjson.GetBytes(raw, "reasoning").Exists())
}

func TestNormalizeChatReasoningEffortAliases(t *testing.T) {
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		t.Run(effort, func(t *testing.T) {
			body := `{"model":"gpt-6-sol","messages":[],"reasoning":"` + effort + `"}`
			normalized, err := NormalizeChatReasoning([]byte(body))
			require.NoError(t, err)
			assert.Equal(t, `{"model":"gpt-6-sol","messages":[],"reasoning":{"effort":"`+effort+`"}}`, string(normalized))

			// Direct decoding and raw-body normalization must use the same alias
			// semantics, including model-independent none/minimal acceptance.
			raw := convertChatBodyForReasoningTest(t, body)
			assert.Equal(t, effort, gjson.GetBytes(raw, "reasoning.effort").String())
		})
	}
}

func TestNormalizeChatReasoningPreservesPayload(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"object", "{ \"reasoning\" : {\"effort\":\"high\",\"unknown\":true}, \"messages\" : [] }\n", "{ \"reasoning\" : {\"effort\":\"high\",\"unknown\":true}, \"messages\" : [] }\n"},
		{"null", `{"messages":[],"reasoning":null,"reasoning_effort":"high"}`, `{"messages":[],"reasoning":null,"reasoning_effort":"high"}`},
		{"absent", `{"messages":[],"reasoning_effort":"high"}`, `{"messages":[],"reasoning_effort":"high"}`},
		{"empty alias", `{"messages":[],"reasoning":"","reasoning_effort":"high"}`, `{"messages":[],"reasoning":{},"reasoning_effort":"high"}`},
		{"case and whitespace", "{\n \"messages\":[], \"reasoning\" : \" \\tHiGh \", \"unknown\":1e+06 }\n", "{\n \"messages\":[], \"reasoning\" : {\"effort\":\"high\"}, \"unknown\":1e+06 }\n"},
		{"responses shape", `{"input":[],"reasoning":"private-responses-value"}`, `{"input":[],"reasoning":"private-responses-value"}`},
		{"responses shape nonstring", `{"input":[],"reasoning":false}`, `{"input":[],"reasoning":false}`},
		{"responses shape duplicate", `{"input":[],"reasoning":{},"reasoning":{}}`, `{"input":[],"reasoning":{},"reasoning":{}}`},
		{"messages shape wins", `{"messages":[],"input":[],"reasoning":"high"}`, `{"messages":[],"input":[],"reasoning":{"effort":"high"}}`},
		{"unrelated duplicate", `{"messages":[],"unknown":true,"unknown":false,"reasoning":"high"}`, `{"messages":[],"unknown":true,"unknown":false,"reasoning":{"effort":"high"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			normalized, err := NormalizeChatReasoning(body)
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(normalized))
			assert.Equal(t, tc.body, string(body), "normalization must not mutate the original payload")
		})
	}
}

func TestChatReasoningRejectsInvalidAliases(t *testing.T) {
	for _, value := range []string{`"private-client-text-marker"`, `"enabled"`, `"x-high"`, `0`, `true`, `false`, `[]`, `["high"]`} {
		t.Run(value, func(t *testing.T) {
			body := []byte(`{"messages":[],"reasoning":` + value + `}`)
			_, err := NormalizeChatReasoning(body)
			require.ErrorIs(t, err, ErrInvalidChatReasoning)
			assert.Equal(t, ErrInvalidChatReasoning.Error(), err.Error())

			var req ChatCompletionsRequest
			err = json.Unmarshal(body, &req)
			require.ErrorIs(t, err, ErrInvalidChatReasoning)
			assert.Equal(t, ErrInvalidChatReasoning.Error(), err.Error())
		})
	}
}

func TestChatReasoningAliasesKeepModelRestrictions(t *testing.T) {
	for _, effort := range []string{"none", "minimal"} {
		t.Run(effort, func(t *testing.T) {
			var req ChatCompletionsRequest
			require.NoError(t, json.Unmarshal([]byte(`{"model":"gpt-6.1-sol","messages":[],"reasoning":"`+effort+`"}`), &req))
			_, err := ChatCompletionsToResponses(&req)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "gpt-6.1-sol does not support reasoning effort")
		})
	}
}

func TestNormalizeChatReasoningRejectsAmbiguousEffortFields(t *testing.T) {
	for _, body := range []string{
		`{"messages":[],"reasoning":{"effort":"low"},"reasoning":"ultra"}`,
		`{"messages":[],"reasoning":"low","reas\u006fning":"ultra"}`,
		`{"messages":[],"reasoning_effort":"low","reasoning_\u0065ffort":"ultra"}`,
		`{"messages":[],"Reasoning":"high","reasoning_effort":"low"}`,
		`{"messages":[],"reasoning":"high","Reasoning_Effort":"low"}`,
	} {
		t.Run(body, func(t *testing.T) {
			_, err := NormalizeChatReasoning([]byte(body))
			require.ErrorIs(t, err, ErrInvalidChatReasoning)
			assert.Equal(t, ErrInvalidChatReasoning.Error(), err.Error())
		})
	}
}

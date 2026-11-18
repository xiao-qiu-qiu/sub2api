//go:build unit

package service

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func setStringFieldBySJSONLoop(t testing.TB, body []byte, field string, edits []jsonObjectStringFieldEdit, indexes []int) []byte {
	t.Helper()
	updated := body
	for editIndex, messageIndex := range indexes {
		next, err := sjson.SetBytes(updated, "messages."+strconv.Itoa(messageIndex)+"."+field, edits[editIndex].value)
		require.NoError(t, err)
		updated = next
	}
	return updated
}

func assistantFieldEdits(body []byte, field, value string) ([]jsonObjectStringFieldEdit, []int) {
	var edits []jsonObjectStringFieldEdit
	var indexes []int
	for messageIndex, message := range gjson.GetBytes(body, "messages").Array() {
		if strings.TrimSpace(message.Get("role").String()) != "assistant" || message.Get(field).String() != "" {
			continue
		}
		edits = append(edits, jsonObjectStringFieldEdit{object: message, value: value})
		indexes = append(indexes, messageIndex)
	}
	return edits, indexes
}

func TestSetStringFieldInJSONObjectsMatchesSJSON(t *testing.T) {
	cases := map[string]string{
		"compact": `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"a"},` +
			`{"role":"assistant","reasoning_content":""},{"role":"assistant","reasoning_content":null},` +
			`{"role":"assistant","reasoning_content":"keep"},{"role":"assistant","reasoning_content":false},{}]}`,
		"pretty": "{\n  \"messages\": [\n    { \"role\" : \"assistant\" ,\n      \"content\" : \"x\"\n    } ,\n" +
			"    {\"role\":\"assistant\"   }\t,\n    { \"role\": \"assistant\", \"reasoning_content\" :  \"\"  }\n  ],\n  \"stream\": true\n}",
		"nested":    `{"messages":[{"role":"assistant","content":[{"type":"text","reasoning_content":"nested"}]}]}`,
		"duplicate": `{"messages":[{"role":"assistant","reasoning_content":"","reasoning_content":"x"}]}`,
		"unicode":   `{"messages":[{"role":"assistant","content":"中文 \"q\" \\ <"},{"role":"assistant","content":"é"}]}`,
		"no edits":  `{"messages":[{"role":"user","content":"hi"}]}`,
	}
	values := []string{" ", "", "plain", `quote " and \ backslash`, "line\nbreak\ttab", "中文", "<tag>&amp;", "\u007f", "\u2028\u2029", string([]byte{0xff})}
	for name, body := range cases {
		for _, value := range values {
			t.Run(fmt.Sprintf("%s/%q", name, value), func(t *testing.T) {
				edits, indexes := assistantFieldEdits([]byte(body), "reasoning_content", value)
				got, ok := setStringFieldInJSONObjects([]byte(body), "reasoning_content", edits)
				require.True(t, ok)
				require.Equal(t, string(setStringFieldBySJSONLoop(t, []byte(body), "reasoning_content", edits, indexes)), string(got))
			})
		}
	}
}

func TestSetStringFieldInJSONObjectsMatchesSJSONRandomized(t *testing.T) {
	random := rand.New(rand.NewSource(20261117))
	pick := func(values []string) string { return values[random.Intn(len(values))] }
	whitespace := []string{"", " ", "\n", "\t", "  \n  "}
	states := []string{"", `"reasoning_content":""`, `"reasoning_content":null`, `"reasoning_content":"r"`, `"reasoning_content":1`}
	for iteration := 0; iteration < 300; iteration++ {
		var builder strings.Builder
		builder.WriteString("{" + pick(whitespace) + `"messages":` + pick(whitespace) + "[")
		count := random.Intn(30)
		for messageIndex := 0; messageIndex < count; messageIndex++ {
			if messageIndex > 0 {
				builder.WriteString(pick(whitespace) + "," + pick(whitespace))
			}
			members := []string{
				`"role"` + pick(whitespace) + ":" + pick(whitespace) + `"` + pick([]string{"assistant", "user", "tool", " assistant "}) + `"`,
				`"content":` + pick(whitespace) + pick([]string{`"x"`, `null`, `"中文"`, `"a \"b\" \\ c"`, `[{"type":"text","text":"t"}]`}),
			}
			if state := pick(states); state != "" {
				members = append(members, state)
			}
			random.Shuffle(len(members), func(left, right int) { members[left], members[right] = members[right], members[left] })
			builder.WriteString("{" + pick(whitespace) + strings.Join(members, pick(whitespace)+","+pick(whitespace)) + pick(whitespace) + "}")
		}
		builder.WriteString(pick(whitespace) + "]" + pick(whitespace) + "}")
		body := []byte(builder.String())
		require.True(t, gjson.ValidBytes(body))
		edits, indexes := assistantFieldEdits(body, "reasoning_content", "value "+strconv.Itoa(iteration)+pick([]string{"", "\"", "中"}))
		got, ok := setStringFieldInJSONObjects(body, "reasoning_content", edits)
		require.True(t, ok)
		require.Equal(t, string(setStringFieldBySJSONLoop(t, body, "reasoning_content", edits, indexes)), string(got))
	}
}

func TestSetStringFieldInJSONObjectsRejectsUnreliableEdits(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant"},{"role":"assistant"}]}`)
	messages := gjson.GetBytes(body, "messages").Array()
	for _, edits := range [][]jsonObjectStringFieldEdit{
		{{object: gjson.Parse(`{"role":"assistant"}`), value: "v"}},
		{{object: messages[1], value: "v"}, {object: messages[0], value: "v"}},
		{{object: gjson.GetBytes(body, "messages.0.role"), value: "v"}},
		{{object: messages[0], value: "v"}, {object: messages[0], value: "v"}},
	} {
		_, ok := setStringFieldInJSONObjects(body, "field", edits)
		require.False(t, ok)
	}
}

func TestSetStringFieldInJSONObjectsEmptyObjects(t *testing.T) {
	body := []byte(`{"messages":[{}, {  }, {"field":null}]}`)
	var edits []jsonObjectStringFieldEdit
	for _, message := range gjson.GetBytes(body, "messages").Array() {
		edits = append(edits, jsonObjectStringFieldEdit{object: message, value: "value"})
	}
	got, ok := setStringFieldInJSONObjects(body, "field", edits)
	require.True(t, ok)
	require.Equal(t, string(setStringFieldBySJSONLoop(t, body, "field", edits, []int{0, 1, 2})), string(got))
}

func TestEnsureDeepSeekChatReasoningPlaceholdersLargeConversation(t *testing.T) {
	var builder strings.Builder
	builder.WriteString(`{"model":"deepseek-chat","messages":[`)
	for messageIndex := 0; messageIndex < 200; messageIndex++ {
		if messageIndex > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `{"role":"user","content":"step %d"},{"role":"assistant","content":"%s"}`, messageIndex, strings.Repeat("content ", 128))
	}
	builder.WriteString(`]}`)
	body := []byte(builder.String())
	edits, indexes := assistantFieldEdits(body, "reasoning_content", deepSeekChatReasoningPlaceholderText)
	require.Len(t, edits, 200)

	got := ensureDeepSeekChatReasoningPlaceholders(openaiPlatformDeepSeekAccount(), body)

	require.Equal(t, string(setStringFieldBySJSONLoop(t, body, "reasoning_content", edits, indexes)), string(got))
}

func TestNormalizeOllamaCloudChatCompletionsRequestMatchesSJSON(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"q"},` +
		`{"role":"assistant","content":"a","reasoning_content":"think \"1\""},` +
		`{"role":"assistant","content":"b","reasoning_content":"think 2","reasoning":""},` +
		`{"role":"assistant","content":"c","reasoning_content":"think 3","thinking":"t"},` +
		`{"role":"assistant","content":"d","reasoning_content":"keep","reasoning":"existing"},` +
		`{"role":"assistant","content":"e"}]}`)
	want := setStringFieldBySJSONLoop(t, body, "reasoning", []jsonObjectStringFieldEdit{{value: `think "1"`}, {value: "think 2"}}, []int{1, 2})

	require.Equal(t, string(want), string(normalizeOllamaCloudChatCompletionsRequest(body)))
}

package service

import (
	"bytes"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Field orders follow the Codex serde request structs from codex-rs 16ff14c.
// Only top-level fields move; values and escaped key spellings remain raw.
var (
	codexResponsesFieldOrder = []string{
		"model", "instructions", "input", "tools", "tool_choice", "parallel_tool_calls",
		"reasoning", "store", "stream", "stream_options", "include", "service_tier",
		"prompt_cache_key", "text", "client_metadata", "access_programs",
	}
	codexCompactFieldOrder = []string{
		"model", "input", "instructions", "tools", "parallel_tool_calls", "reasoning",
		"service_tier", "prompt_cache_key", "text", "access_programs",
	}
	codexWSCreateFieldOrder = []string{
		"type", "model", "instructions", "previous_response_id", "input", "tools", "tool_choice",
		"parallel_tool_calls", "reasoning", "store", "stream", "stream_options", "include",
		"service_tier", "prompt_cache_key", "text", "generate", "client_metadata", "access_programs",
	}
)

// codexWireEndpoint checks the outbound endpoint, independently of the inbound
// route. OAuth images can, for example, be sent upstream through /responses.
func codexWireEndpoint(targetURL string) string {
	parsed, err := url.Parse(targetURL)
	if err != nil || (parsed.Scheme != "" && parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case strings.HasSuffix(path, "/responses/compact"):
		return "/responses/compact"
	case strings.HasSuffix(path, "/responses"):
		return "/responses"
	default:
		return ""
	}
}

// applyCodexBodyFieldOrder runs after plaintext transformations. The shared
// codex_wire_options gate owns account eligibility, including the OAuth limit.
// Disabled options return the caller's original slice without changing bytes.
func applyCodexBodyFieldOrder(c *gin.Context, account *Account, targetURL string, body []byte) []byte {
	if !codexWireOptionEnabled(c, account, "body_order") {
		return body
	}
	switch codexWireEndpoint(targetURL) {
	case "/responses":
		return reorderCodexTopLevelFields(body, codexResponsesFieldOrder)
	case "/responses/compact":
		return reorderCodexTopLevelFields(body, codexCompactFieldOrder)
	default:
		return body
	}
}

// prepareCodexWSWireBytes only reorders response.create frames. WebSocket frames
// never use HTTP request compression; other event types stay byte-for-byte intact.
func prepareCodexWSWireBytes(c *gin.Context, account *Account, payload []byte) []byte {
	if !codexWireOptionEnabled(c, account, "body_order") {
		return payload
	}
	typ := gjson.GetBytes(payload, "type")
	if typ.Type != gjson.String || typ.String() != "response.create" {
		return payload
	}
	return reorderCodexTopLevelFields(payload, codexWSCreateFieldOrder)
}

// reorderCodexTopLevelFields appends unknown fields in their original relative
// order. Duplicate top-level keys (including escaped aliases), malformed JSON,
// empty objects and non-objects are returned unchanged rather than repaired.
func reorderCodexTopLevelFields(body []byte, order []string) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	parsed := gjson.ParseBytes(body)
	if !parsed.IsObject() {
		return body
	}
	type field struct{ key, raw string }
	fields := make([]field, 0, len(order))
	index := make(map[string]int, len(order))
	duplicate := false
	parsed.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if _, ok := index[name]; ok {
			duplicate = true
			return false
		}
		index[name] = len(fields)
		fields = append(fields, field{key: key.Raw, raw: value.Raw})
		return true
	})
	if duplicate || len(fields) == 0 {
		return body
	}

	// An already canonical body can reuse its original allocation. Check the
	// complete raw form, since top-level separator whitespace is also normalized.
	position, canonical := 0, true
	match := func(raw string) {
		if !canonical {
			return
		}
		end := position + len(raw)
		if end > len(body) || !bytes.Equal(body[position:end], []byte(raw)) {
			canonical = false
			return
		}
		position = end
	}
	nextField := 0
	match("{")
	check := func(i int) {
		if i != nextField {
			canonical = false
			return
		}
		if nextField != 0 {
			match(",")
		}
		match(fields[i].key)
		match(":")
		match(fields[i].raw)
		nextField++
	}
	for _, name := range order {
		if i, ok := index[name]; ok {
			check(i)
		}
	}
	for i := nextField; canonical && i < len(fields); i++ {
		check(i)
	}
	match("}")
	if canonical && position == len(body) {
		return body
	}

	out := make([]byte, 0, len(body))
	out = append(out, '{')
	emitted := make([]bool, len(fields))
	emit := func(i int) {
		if len(out) > 1 {
			out = append(out, ',')
		}
		out = append(out, fields[i].key...)
		out = append(out, ':')
		out = append(out, fields[i].raw...)
		emitted[i] = true
	}
	for _, name := range order {
		if i, ok := index[name]; ok {
			emit(i)
		}
	}
	for i := range fields {
		if !emitted[i] {
			emit(i)
		}
	}
	return append(out, '}')
}

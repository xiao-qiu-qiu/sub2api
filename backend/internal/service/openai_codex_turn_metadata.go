package service

import (
	"encoding/json"
	"maps"
	"slices"
	"unicode/utf16"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// rewriteCodexTurnMetadataJSON patches identity values in place. Unknown JSON
// values, key order, whitespace and existing escape spellings remain untouched.
// All duplicate identity keys are replaced using encoding/json's last-value-wins
// interpretation, so different JSON consumers cannot observe different identities.
func rewriteCodexTurnMetadataJSON(raw string, rebuildInvalid bool, updates func(map[string]any) map[string]any) string {
	original := raw
	var metadata map[string]any
	if !json.Valid([]byte(raw)) || decodeOpenAIJSONUseNumber([]byte(raw), &metadata) != nil || metadata == nil {
		if !rebuildInvalid {
			return original
		}
		raw, metadata = "{}", map[string]any{}
	}
	fields := updates(metadata)
	if len(fields) == 0 {
		return original
	}
	encoded := make(map[string]string, len(fields))
	for name, value := range fields {
		next, err := marshalCodexTurnMetadataValue(value)
		if err != nil {
			return original
		}
		encoded[name] = next
	}
	out := make([]byte, 0, len(raw))
	offset := 0
	seen := make(map[string]bool, len(fields))
	gjson.Parse(raw).ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		next, ok := encoded[name]
		if !ok {
			return true
		}
		seen[name] = true
		if text, ok := fields[name].(string); ok && value.Type == gjson.String && value.Str == text {
			return true
		}
		out = append(out, raw[offset:value.Index]...)
		out = append(out, next...)
		offset = value.Index + len(value.Raw)
		return true
	})
	out = append(out, raw[offset:]...)
	next := string(out)
	for _, name := range slices.Sorted(maps.Keys(encoded)) {
		if seen[name] {
			continue
		}
		var err error
		next, err = sjson.SetRaw(next, name, encoded[name])
		if err != nil {
			return original
		}
	}
	return codexTurnMetadataASCII(next)
}

func marshalCodexTurnMetadataValue(value any) (string, error) {
	raw, err := marshalOpenAIUpstreamJSON(value)
	if err != nil {
		return "", err
	}
	return codexTurnMetadataASCII(string(raw)), nil
}

func codexTurnMetadataASCII(raw string) string {
	for i, b := range []byte(raw) {
		if b < 0x7f {
			continue
		}
		out := make([]byte, 0, len(raw)+16)
		out = append(out, raw[:i]...)
		appendEscape := func(r rune) {
			const hex = "0123456789abcdef"
			out = append(out, '\\', 'u', hex[r>>12&15], hex[r>>8&15], hex[r>>4&15], hex[r&15])
		}
		for _, r := range raw[i:] {
			switch {
			case r < 0x7f:
				out = append(out, byte(r))
			case r <= 0xffff:
				appendEscape(r)
			default:
				high, low := utf16.EncodeRune(r)
				appendEscape(high)
				appendEscape(low)
			}
		}
		return string(out)
	}
	return raw
}

// Turn metadata is also used as an HTTP header, including when carried in WS JSON.
func marshalCodexTurnMetadata(metadata map[string]any) ([]byte, error) {
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	for i, b := range raw {
		if b < 0x7f {
			continue
		}
		out := make([]byte, 0, len(raw)+16)
		out = append(out, raw[:i]...)
		appendEscape := func(r rune) {
			const hex = "0123456789abcdef"
			out = append(out, '\\', 'u', hex[r>>12&15], hex[r>>8&15], hex[r>>4&15], hex[r&15])
		}
		for _, r := range string(raw[i:]) {
			switch {
			case r < 0x7f:
				out = append(out, byte(r))
			case r <= 0xffff:
				appendEscape(r)
			default:
				high, low := utf16.EncodeRune(r)
				appendEscape(high)
				appendEscape(low)
			}
		}
		return out, nil
	}
	return raw, nil
}

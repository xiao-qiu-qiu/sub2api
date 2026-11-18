package service

import (
	"encoding/json"

	"github.com/tidwall/gjson"
)

type jsonObjectStringFieldEdit struct {
	object gjson.Result
	value  string
}

func setStringFieldInJSONObjects(body []byte, field string, edits []jsonObjectStringFieldEdit) ([]byte, bool) {
	if len(edits) == 0 {
		return body, true
	}
	out := make([]byte, 0, len(body)+len(edits)*(len(field)+8))
	last := 0
	for _, edit := range edits {
		object := edit.object
		start, end := object.Index, object.Index+len(object.Raw)
		if start <= 0 || start < last || end > len(body) || len(object.Raw) < 2 ||
			object.Raw[0] != '{' || object.Raw[len(object.Raw)-1] != '}' || string(body[start:end]) != object.Raw {
			return nil, false
		}
		if existing := object.Get(field); existing.Exists() {
			valueStart := existing.Index
			valueEnd := valueStart + len(existing.Raw)
			if valueStart <= start || valueEnd > end || string(body[valueStart:valueEnd]) != existing.Raw {
				return nil, false
			}
			out = append(out, body[last:valueStart]...)
			out = appendSJSONString(out, edit.value)
			last = valueEnd
			continue
		}
		out = append(out, body[last:end-1]...)
		if jsonObjectHasMembers(object.Raw) {
			out = append(out, ',')
		}
		out = appendSJSONString(out, field)
		out = append(out, ':')
		out = appendSJSONString(out, edit.value)
		out = append(out, '}')
		last = end
	}
	return append(out, body[last:]...), true
}

func jsonObjectHasMembers(raw string) bool {
	for offset := 1; offset < len(raw); offset++ {
		if raw[offset] <= ' ' {
			continue
		}
		return raw[offset] != '}'
	}
	return false
}

func appendSJSONString(buffer []byte, value string) []byte {
	for offset := 0; offset < len(value); offset++ {
		if value[offset] < ' ' || value[offset] > 0x7f || value[offset] == '"' || value[offset] == '\\' {
			encoded, _ := json.Marshal(value)
			return append(buffer, encoded...)
		}
	}
	buffer = append(buffer, '"')
	buffer = append(buffer, value...)
	return append(buffer, '"')
}

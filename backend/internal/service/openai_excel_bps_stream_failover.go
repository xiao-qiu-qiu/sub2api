package service

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/tidwall/gjson"
)

// Only initial control events and explicit empty rejection terminals can precede
// a replay. Unknown events and upstream tool/reasoning items stop this path even
// when the bridge has not released them to the client yet.
func excelBPSBeforeGeneration(payload []byte) bool {
	if !gjson.ValidBytes(payload) {
		return false
	}
	for _, path := range []string{"output", "response.output", "item", "delta"} {
		value := gjson.GetBytes(payload, path)
		if value.Exists() && value.Type != gjson.Null && !(value.IsArray() && len(value.Array()) == 0) && !(value.Type == gjson.String && value.Str == "") {
			return false
		}
	}
	for _, path := range []string{"usage", "response.usage"} {
		usage := gjson.GetBytes(payload, path)
		if usage.Exists() && usage.Type != gjson.Null {
			if !usage.IsObject() {
				return false
			}
			// Count every quantity, including cached/image/reasoning tokens and
			// future usage fields. Unknown nonnumeric evidence also stops replay.
			pending := []gjson.Result{usage}
			for len(pending) > 0 {
				value := pending[len(pending)-1]
				pending = pending[:len(pending)-1]
				if value.IsObject() {
					value.ForEach(func(_, child gjson.Result) bool {
						pending = append(pending, child)
						return true
					})
				} else if value.Type != gjson.Null && (value.Type != gjson.Number || value.Float() != 0) {
					return false
				}
			}
		}
	}
	switch gjson.GetBytes(payload, "type").String() {
	case "response.created", "response.in_progress",
		"basispoints.response.metadata", "basispoints.response.resume_token",
		"basispoints.response.upstream_sent", "basispoints.response.heartbeat":
		return true
	case "error", "response.failed":
		failure := basispoints.ParseUpstreamFailure(payload)
		return failure != nil && failure.Status == http.StatusTooManyRequests
	default:
		return false
	}
}

func (r *excelBPSQuotaResponseBody) beforeGeneration() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.generationObserved
}

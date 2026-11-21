package requestcapture

import (
	"encoding/json"
	"strings"
)

// Missing Content-Type is common on OAuth SSE responses. Retain at most one
// field prefix while detecting framing, never a whole line or response.
type bodyFraming struct {
	pending bool
	sse     bool
	prefix  []byte
}

func newBodyFraming(ct string) bodyFraming {
	ct = strings.ToLower(strings.TrimSpace(ct))
	return bodyFraming{pending: ct == "", sse: strings.Contains(ct, "text/event-stream")}
}

func (f *bodyFraming) detect(p []byte) (prefix, rest []byte) {
	if !f.pending {
		return nil, p
	}
	for len(p) > 0 {
		b := p[0]
		p = p[1:]
		if len(f.prefix) == 0 && (b == ' ' || b == '\n' || b == '\r' || b == '\t') {
			continue
		}
		f.prefix = append(f.prefix, b)
		prefix := string(f.prefix)
		if prefix == "\xef\xbb\xbf" {
			f.prefix = f.prefix[:0]
			continue
		}
		possible := strings.HasPrefix("\xef\xbb\xbf", prefix)
		for _, field := range []string{"data:", "event:", "id:", "retry:", ":"} {
			if prefix == field {
				f.sse = true
				f.pending = false
				break
			}
			possible = possible || strings.HasPrefix(field, prefix)
		}
		if f.pending && possible {
			continue
		}
		f.pending = false
		out := f.prefix
		f.prefix = nil
		return out, p
	}
	return nil, nil
}

// responseObserver validates JSON incrementally and extracts only shallow
// protocol fields. Large output strings never enter a diagnostic buffer.
// Its bounded token and depth storage use a separate stream charge.
type responseObserver struct {
	sawEvent        bool
	framing         bodyFraming
	json            responseJSON
	line            [5]byte
	lineLen         int
	data            bool
	skipLine        bool
	hasData         bool
	marker          [6]byte
	markerN         int
	markerSpace     bool
	terminal        string
	failed          bool
	expectsTerminal bool
	collectUsage    bool
	usage           responseUsage
	usageProtocol   usageProtocol
	chatUsage       usageCounts
	messageUsage    usageCounts
	geminiUsage     usageCounts
	geminiFinished  bool
}

func (o *responseObserver) write(p []byte) {
	prefix, rest := o.framing.detect(p)
	o.writeDetected(prefix)
	o.writeDetected(rest)
}

func (o *responseObserver) writeDetected(p []byte) {
	if !o.framing.sse {
		for _, b := range p {
			o.json.write(b)
		}
		return
	}
	for _, b := range p {
		if b == '\n' {
			if o.data {
				o.json.write('\n')
				if o.markerN > 0 {
					o.markerSpace = true
				}
			} else if o.lineLen == 0 && !o.skipLine {
				o.endEvent()
			}
			o.lineLen, o.data, o.skipLine = 0, false, false
			continue
		}
		if o.data {
			o.json.write(b)
			if b != ' ' && b != '\t' && b != '\r' {
				if o.markerSpace {
					o.markerN = len(o.marker) + 1
				}
				if o.markerN < len(o.marker) {
					o.marker[o.markerN] = b
				}
				if o.markerN <= len(o.marker) {
					o.markerN++
				}
			} else if o.markerN > 0 {
				o.markerSpace = true
			}
			continue
		}
		if o.skipLine || b == '\r' {
			continue
		}
		o.line[o.lineLen] = b
		o.lineLen++
		if string(o.line[:o.lineLen]) == "data:" {
			o.data, o.hasData = true, true
		} else if !strings.HasPrefix("data:", string(o.line[:o.lineLen])) {
			o.skipLine = true
		}
	}
}

const responseObserverCharge = 8 << 10

type usageProtocol uint8

const (
	unknownUsage usageProtocol = iota
	responsesUsage
	anthropicUsage
	chatUsage
	geminiUsage
)

func (o *responseObserver) endEvent() {
	if !o.hasData {
		return
	}
	if o.markerN == 6 && string(o.marker[:]) == "[DONE]" {
		o.terminal = "[DONE]"
		o.expectsTerminal = true
		if o.collectUsage && !o.usage.complete {
			switch o.usageProtocol {
			case chatUsage:
				o.usage = responseUsage{usageCounts: o.chatUsage, complete: true}
			case unknownUsage:
				o.usage = responseUsage{complete: true}
			}
		}
	} else if event, failed, valid := o.json.result(); valid {
		o.sawEvent = true
		o.expectsTerminal = o.expectsTerminal || strings.HasPrefix(o.json.event, "response.") || o.json.event == "message_start" || o.json.event == "message_delta" || o.json.event == "message_stop"
		if event != "" {
			o.terminal = event
		}
		o.failed = o.failed || failed
		if o.collectUsage && !failed {
			o.observeUsageEvent(event)
		}
	}
	o.json = responseJSON{collectUsage: o.collectUsage}
	o.hasData, o.markerN = false, 0
	o.markerSpace = false
}

func (o *responseObserver) end() {
	if o.framing.sse {
		o.endEvent()
		if o.usageProtocol == geminiUsage && o.geminiFinished {
			o.usage = responseUsage{usageCounts: o.geminiUsage, complete: true}
		}
		return
	}
	if event, failed, valid := o.json.result(); valid {
		o.terminal, o.failed = event, o.failed || failed
		if o.terminal == "" && !o.failed {
			o.terminal = "json"
		}
		if o.collectUsage && !o.failed {
			o.usage = responseUsage{usageCounts: o.json.finalUsage(), complete: true}
		}
	}
}

func (o *responseObserver) observeUsageEvent(event string) {
	protocol := o.json.usageProtocol(event)
	if o.usageProtocol != unknownUsage && o.usageProtocol != protocol {
		return
	}
	o.usageProtocol = protocol
	switch protocol {
	case responsesUsage:
		if event == "response.completed" || event == "response.done" {
			o.usage = responseUsage{usageCounts: o.json.finalUsage(), complete: true}
		}
	case anthropicUsage:
		switch o.json.event {
		case "message_start":
			o.messageUsage = o.json.messageUsage
			o.messageUsage.known &^= 1 << outputTokens
			o.messageUsage.present &^= 1 << outputTokens
		case "message_delta":
			o.messageUsage.update(o.json.rootUsage)
		case "message_stop":
			o.usage = responseUsage{usageCounts: o.messageUsage, complete: true}
		}
	case geminiUsage:
		if o.json.geminiUsage.seen {
			o.geminiUsage = o.json.geminiUsage
		}
		o.geminiFinished = o.geminiFinished || o.json.geminiFinished
	case chatUsage:
		if o.json.rootUsage.seen {
			o.chatUsage = o.json.rootUsage
		}
	}
}

func (o *responseObserver) successful() bool {
	return !o.failed && (o.terminal == "response.completed" || o.terminal == "response.done" || o.terminal == "message_stop" || o.terminal == "[DONE]" || o.terminal == "json")
}

type responseScope byte

const (
	ignoredScope responseScope = iota
	rootScope
	responseObjectScope
	errorScope
	messageScope
	rootUsageScope
	responseUsageScope
	messageUsageScope
	geminiUsageScope
	inputDetailsScope
	promptDetailsScope
	geminiCandidatesScope
	geminiCandidateScope
	responseScopeCount
)

type responseJSON struct {
	v                jsonValidator
	scopes           [128]responseScope
	keys             [responseScopeCount]string
	token            [258]byte
	tokenN           int
	capture          bool
	keyToken         bool
	field            string
	scope            responseScope
	invalid          bool
	hasError         bool
	event            string
	status           string
	collectUsage     bool
	rootUsage        usageCounts
	responseUsage    usageCounts
	messageUsage     usageCounts
	geminiUsage      usageCounts
	geminiFinished   bool
	chatChunk        bool
	chatChoices      bool
	geminiCandidates bool
}

func (j *responseJSON) write(b byte) {
	if j.invalid {
		return
	}
	depth, state, quoted := len(j.v.stack), j.v.state(), j.v.quoted
	scope := ignoredScope
	if depth > 0 {
		scope = j.scopes[depth-1]
	}
	field := j.keys[scope]
	protocolObject := scope == rootScope || scope == responseObjectScope
	if !quoted && b == '"' {
		j.keyToken = state == 1 || state == 5
		j.capture = protocolObject && (j.keyToken || field == "status" || field == "error" || scope == rootScope && field == "type") ||
			j.collectUsage && scope == rootScope && field == "object" ||
			j.collectUsage && scope >= messageScope && scope != geminiCandidatesScope && (j.keyToken || scope == geminiCandidateScope && field == "finishReason")
		j.tokenN, j.scope, j.field = 0, scope, field
		if j.keyToken && scope == errorScope {
			j.hasError = true
		}
	}
	if j.capture {
		if j.tokenN < len(j.token) {
			j.token[j.tokenN] = b
		}
		if j.tokenN <= len(j.token) {
			j.tokenN++
		}
	}
	if j.collectUsage && len(j.v.primitive) > 0 && (b == ',' || b == '}' || b == ']' || b == ' ' || b == '\n' || b == '\r' || b == '\t') {
		if counts, kind := j.usageField(scope, field, depth); counts != nil {
			counts.read(kind, j.v.primitive)
		}
	}
	if !j.v.accept(b) {
		j.invalid = true
		return
	}
	if quoted && !j.v.quoted && j.capture {
		var value string
		if j.tokenN <= len(j.token) {
			_ = json.Unmarshal(j.token[:j.tokenN], &value)
		}
		if j.keyToken {
			j.keys[j.scope] = value
			if j.collectUsage {
				j.resetUsageField(j.scope, value)
			}
			if counts, kind := j.usageField(j.scope, value, depth); counts != nil {
				counts.note(kind)
			}
		} else {
			switch j.field {
			case "type":
				j.event = value
			case "status":
				j.status = value
			case "error":
				j.hasError = j.hasError || value != "" || j.tokenN > len(j.token)
			case "finishReason":
				j.geminiFinished = j.geminiFinished || value != "" && value != "FINISH_REASON_UNSPECIFIED"
			case "object":
				j.chatChunk = value == "chat.completion.chunk"
			}
		}
		j.capture = false
	}
	if !quoted && (b == '{' || b == '[') {
		next := ignoredScope
		if b == '{' {
			switch {
			case depth == 0:
				next = rootScope
			case scope == rootScope && field == "response":
				next = responseObjectScope
			case protocolObject && field == "error":
				next = errorScope
			case j.collectUsage:
				next = j.usageScope(scope, field)
			}
		} else if j.collectUsage && protocolObject && field == "candidates" {
			next = geminiCandidatesScope
			j.geminiCandidates = true
		} else if j.collectUsage && scope == rootScope && field == "choices" {
			j.chatChoices = true
		}
		j.scopes[depth] = next
		if next != ignoredScope {
			j.keys[next] = ""
		}
	}
	if !quoted && state == 0 && protocolObject && field == "error" && b == 't' {
		j.hasError = true
	}
}

func (j *responseJSON) resetUsageField(scope responseScope, field string) {
	var counts *usageCounts
	switch {
	case scope == rootScope && field == "usage":
		counts = &j.rootUsage
	case scope == responseObjectScope && field == "usage", scope == rootScope && field == "response":
		counts = &j.responseUsage
	case scope == messageScope && field == "usage", scope == rootScope && field == "message":
		counts = &j.messageUsage
	case (scope == rootScope || scope == responseObjectScope) && field == "usageMetadata":
		counts = &j.geminiUsage
	case field == "input_tokens_details", field == "prompt_tokens_details":
		if usage := j.usageAt(scope); usage != nil {
			kind := inputDetailsCachedTokens
			if field == "prompt_tokens_details" {
				kind = promptDetailsCachedTokens
			}
			usage.known &^= 1 << kind
		}
	}
	if counts != nil && counts.seen {
		*counts = usageCounts{seen: true}
	}
}

func (j *responseJSON) usageField(scope responseScope, field string, depth int) (*usageCounts, tokenField) {
	if counts := j.usageAt(scope); counts != nil {
		if kind, ok := usageTokenField(field); ok {
			return counts, kind
		}
	} else if field == "cached_tokens" {
		switch scope {
		case inputDetailsScope:
			return j.usageAt(j.scopes[depth-2]), inputDetailsCachedTokens
		case promptDetailsScope:
			return j.usageAt(j.scopes[depth-2]), promptDetailsCachedTokens
		}
	}
	return nil, 0
}

func (j *responseJSON) usageProtocol(event string) usageProtocol {
	switch {
	case strings.HasPrefix(j.event, "response."), strings.HasPrefix(event, "response."):
		return responsesUsage
	case strings.HasPrefix(j.event, "message_"), strings.HasPrefix(j.event, "content_block_"):
		return anthropicUsage
	case j.chatChunk, j.event == "" && j.chatChoices:
		return chatUsage
	case j.geminiUsage.seen, j.geminiCandidates:
		return geminiUsage
	default:
		return unknownUsage
	}
}

func (j *responseJSON) usageScope(scope responseScope, field string) responseScope {
	next := ignoredScope
	switch {
	case scope == rootScope && field == "message":
		next = messageScope
	case scope == rootScope && field == "usage":
		next = rootUsageScope
	case scope == responseObjectScope && field == "usage":
		next = responseUsageScope
	case scope == messageScope && field == "usage":
		next = messageUsageScope
	case (scope == rootScope || scope == responseObjectScope) && field == "usageMetadata":
		next = geminiUsageScope
	case scope == geminiCandidatesScope:
		next = geminiCandidateScope
	case j.usageAt(scope) != nil && field == "input_tokens_details":
		next = inputDetailsScope
	case j.usageAt(scope) != nil && field == "prompt_tokens_details":
		next = promptDetailsScope
	}
	if counts := j.usageAt(next); counts != nil {
		*counts = usageCounts{seen: true}
	}
	return next
}

func (j *responseJSON) usageAt(scope responseScope) *usageCounts {
	switch scope {
	case rootUsageScope:
		return &j.rootUsage
	case responseUsageScope:
		return &j.responseUsage
	case messageUsageScope:
		return &j.messageUsage
	case geminiUsageScope:
		return &j.geminiUsage
	}
	return nil
}

func (j *responseJSON) finalUsage() usageCounts {
	if j.responseUsage.seen {
		return j.responseUsage
	}
	if j.rootUsage.seen {
		return j.rootUsage
	}
	return j.geminiUsage
}

func (j *responseJSON) result() (event string, failed, valid bool) {
	if j.invalid || !j.v.complete() {
		return "", false, false
	}
	switch j.event {
	case "response.completed", "response.done", "message_stop":
		event = j.event
	case "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
		event, failed = j.event, true
	}
	switch j.status {
	case "completed":
		if event == "" {
			event = "response.completed"
		}
	case "failed", "incomplete", "cancelled", "canceled":
		failed = true
	}
	return event, failed || j.hasError, true
}

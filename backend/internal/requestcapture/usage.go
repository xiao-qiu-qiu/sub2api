package requestcapture

import "strconv"

type tokenField uint8

const (
	inputTokens tokenField = iota
	outputTokens
	totalTokens
	cacheCreationTokens
	cacheReadTokens
	promptTokens
	completionTokens
	inputDetailsCachedTokens
	promptDetailsCachedTokens
	cacheReadAliasTokens
	flatCachedTokens
	promptCacheHitTokens
	geminiPromptTokens
	geminiCandidateTokens
	geminiThoughtTokens
	geminiCachedTokens
	geminiTotalTokens
	tokenFieldCount
)

type usageCounts struct {
	values  [tokenFieldCount]int64
	known   uint32
	present uint32
	seen    bool
}

func (u *usageCounts) note(field tokenField) {
	u.present |= 1 << field
	u.known &^= 1 << field
}

func (u *usageCounts) read(field tokenField, raw []byte) {
	value, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || value < 0 {
		return
	}
	u.values[field] = value
	u.known |= 1 << field
}

func (u usageCounts) first(fields ...tokenField) (int64, bool) {
	for _, field := range fields {
		if u.known&(1<<field) != 0 {
			return u.values[field], true
		}
	}
	return 0, false
}

func (u *usageCounts) update(next usageCounts) {
	for field, value := range next.values {
		if next.known&(1<<field) != 0 {
			u.values[field] = value
		}
	}
	u.known = u.known&^next.present | next.known
	u.present |= next.present
	u.seen = u.seen || next.seen
}

func (u usageCounts) output() (int64, bool) {
	if value, ok := u.first(outputTokens, completionTokens); ok {
		return value, true
	}
	candidates, ok := u.first(geminiCandidateTokens)
	thoughts, _ := u.first(geminiThoughtTokens)
	if !ok || candidates > (1<<63-1)-thoughts {
		return 0, false
	}
	return candidates + thoughts, true
}

func (u usageCounts) cacheRead() (int64, bool) {
	for _, field := range [...]tokenField{inputDetailsCachedTokens, promptDetailsCachedTokens, geminiCachedTokens} {
		if u.present&(1<<field) != 0 {
			return u.first(field)
		}
	}
	knownZero := false
	for _, field := range [...]tokenField{cacheReadTokens, cacheReadAliasTokens, flatCachedTokens, promptCacheHitTokens} {
		if value, ok := u.first(field); ok {
			if value > 0 {
				return value, true
			}
			knownZero = true
		} else if u.present&(1<<field) != 0 {
			return 0, false
		}
	}
	return 0, knownZero
}

func (u usageCounts) metadata() map[string]int64 {
	out := make(map[string]int64, 8)
	for field, key := range [...]string{"input_tokens", "output_tokens", "total_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "prompt_tokens", "completion_tokens"} {
		if value, ok := u.first(tokenField(field)); ok {
			out[key] = value
		}
	}
	if value, ok := u.output(); ok {
		out["output_tokens"] = value
	}
	if value, ok := u.cacheRead(); ok {
		out["cache_read_tokens"] = value
	}
	if value, ok := u.first(geminiPromptTokens); ok {
		out["prompt_tokens"] = value
	}
	if value, ok := u.first(geminiTotalTokens); ok {
		out["total_tokens"] = value
	}
	return out
}

func usageTokenField(key string) (tokenField, bool) {
	switch key {
	case "input_tokens":
		return inputTokens, true
	case "output_tokens":
		return outputTokens, true
	case "total_tokens":
		return totalTokens, true
	case "cache_creation_input_tokens":
		return cacheCreationTokens, true
	case "cache_read_input_tokens":
		return cacheReadTokens, true
	case "cache_read_tokens":
		return cacheReadAliasTokens, true
	case "cached_tokens":
		return flatCachedTokens, true
	case "prompt_tokens":
		return promptTokens, true
	case "completion_tokens":
		return completionTokens, true
	case "prompt_cache_hit_tokens":
		return promptCacheHitTokens, true
	case "promptTokenCount":
		return geminiPromptTokens, true
	case "candidatesTokenCount":
		return geminiCandidateTokens, true
	case "thoughtsTokenCount":
		return geminiThoughtTokens, true
	case "cachedContentTokenCount":
		return geminiCachedTokens, true
	case "totalTokenCount":
		return geminiTotalTokens, true
	}
	return 0, false
}

type responseUsage struct {
	usageCounts
	complete bool
	observed bool
}

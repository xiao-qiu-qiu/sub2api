package openai

import (
	"strings"
	"time"
)

// These IDs are reserved for compatibility ahead of their upstream release.
// Recognizing an ID does not establish account access, pricing, or model limits.
const (
	GPT62SolModel   = "gpt-6.2-sol"
	GPT62AstraModel = "gpt-6.2-astra"
)

func IsGPT62SolModelSpelling(model string) bool {
	return isGPT62ModelSpelling(model, GPT62SolModel)
}

func IsGPT62AstraModelSpelling(model string) bool {
	return isGPT62ModelSpelling(model, GPT62AstraModel)
}

func IsGPT62ModelSpelling(model string) bool {
	return IsGPT62SolModelSpelling(model) || IsGPT62AstraModelSpelling(model)
}

func isGPT62ModelSpelling(model, base string) bool {
	canonical := CanonicalizeOpenAIModelAliasSpelling(model)
	if canonical == base {
		return true
	}
	suffix, ok := strings.CutPrefix(canonical, base+"-")
	if !ok {
		return false
	}
	switch suffix {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "openai-compact":
		return true
	}
	_, err := time.Parse("2006-01-02", suffix)
	return err == nil
}

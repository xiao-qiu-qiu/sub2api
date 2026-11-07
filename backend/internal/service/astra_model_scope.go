package service

import "github.com/Wei-Shaw/sub2api/internal/pkg/openai"

// IsAstraSchedulingModel applies the Astra scheduling policy across generations.
// It does not qualify a model for another generation's tickets, routes or anchors.
func IsAstraSchedulingModel(model string) bool {
	return isOpenAIGPT6AstraModel(model) || openai.IsGPT62AstraModelSpelling(model)
}

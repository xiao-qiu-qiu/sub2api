package openai

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGPT62ModelSpellings(t *testing.T) {
	for _, base := range []string{GPT62SolModel, GPT62AstraModel} {
		for _, suffix := range []string{"", "-none", "-minimal", "-low", "-medium", "-high", "-xhigh", "-max", "-openai-compact", "-2026-11-08"} {
			model := "openai/" + base + suffix
			require.True(t, IsGPT62ModelSpelling(model), model)
			require.Equal(t, base == GPT62SolModel, IsGPT62SolModelSpelling(model), model)
			require.Equal(t, base == GPT62AstraModel, IsGPT62AstraModelSpelling(model), model)
		}
	}
	require.True(t, IsGPT62SolModelSpelling(" OPENAI/GPT_6.2_SOL_MAX "))
	require.True(t, IsGPT62AstraModelSpelling("GPT 6.2 ASTRA"))
	for _, model := range []string{
		"gpt-6.2", "gpt-6.2-terra", "gpt-6.2-solitude", "gpt-6.2-astra-preview",
		"gpt-6.2-sol-pro", "gpt-6.2-sol-max-extra", "gpt-6.2-sol-2026-02-30",
		"gpt-6.2-astra-2026-1-08", "gpt-6.1-sol", "gpt-6-astra", "",
	} {
		require.False(t, IsGPT62ModelSpelling(model), model)
	}
}

func TestGPT62DefaultModelsKeepIndependentIDs(t *testing.T) {
	for _, id := range []string{GPT62SolModel, GPT62AstraModel} {
		require.Contains(t, CodexBaseInstructionsForModel(id), "based on GPT-6")
		matches := 0
		for _, model := range DefaultModels {
			if model.ID == id {
				matches++
				require.Zero(t, model.Created, "the release timestamp is not known yet")
			}
		}
		require.Equal(t, 1, matches, id)
	}
	require.Equal(t, "gpt-5.6-sol", DefaultModelIDs()[0])
	require.Equal(t, "gpt-5.4", DefaultTestModel)
}

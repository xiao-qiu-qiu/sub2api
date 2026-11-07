package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func TestGPT62CompatIdentityAndReasoning(t *testing.T) {
	for _, model := range []string{openai.GPT62SolModel, openai.GPT62AstraModel} {
		t.Run(model, func(t *testing.T) {
			for _, spelling := range []string{model, "openai/" + model, model + "-max", model + "-openai-compact", model + "-2026-11-08"} {
				require.Equal(t, model, normalizeKnownOpenAICodexModel(spelling))
				require.Equal(t, model, normalizeCodexModel(spelling))
				require.Equal(t, model, NormalizeOpenAICompatRequestedModel(spelling))
				require.True(t, isOpenAIGPT6Model(spelling))
				require.False(t, isOpenAIGPT6AstraModel(spelling), "legacy prices and metadata must remain separate")
				require.Equal(t, "max", normalizeOpenAIReasoningEffortForModel("max", spelling))
			}
			body := []byte(`{"model":"` + model + `","input":"hello","reasoning":{"mode":"pro","effort":"max"},"temperature":0.3}`)
			got, changed, err := normalizeOpenAIResponsesReasoningMode(body, "")
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, body, got)
			effort := CanonicalRequestedReasoningEffort(body, model)
			require.NotNil(t, effort)
			require.Equal(t, "max", *effort)

			for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
				request := &apicompat.AnthropicRequest{Model: "openai/" + model + "-" + effort}
				applyOpenAICompatModelNormalization(request)
				require.Equal(t, model, request.Model)
				require.Equal(t, effort, request.OutputConfig.Effort)
				converted, err := apicompat.AnthropicToResponses(request)
				require.NoError(t, err)
				require.Equal(t, model, converted.Model)
				require.Equal(t, effort, converted.Reasoning.Effort)
				chat, err := apicompat.ChatCompletionsToResponses(&apicompat.ChatCompletionsRequest{Model: model, ReasoningEffort: effort})
				require.NoError(t, err)
				require.Equal(t, effort, chat.Reasoning.Effort)
			}
			for _, suffix := range []string{"-openai-compact", "-2026-11-08"} {
				request := &apicompat.AnthropicRequest{Model: model + suffix}
				applyOpenAICompatModelNormalization(request)
				require.Equal(t, model, request.Model)
				require.Nil(t, request.OutputConfig, "a date or compact suffix is not a reasoning effort")
			}
			request := &apicompat.AnthropicRequest{Model: model + "-max", OutputConfig: &apicompat.AnthropicOutputConfig{Effort: "low"}}
			applyOpenAICompatModelNormalization(request)
			require.Equal(t, "low", request.OutputConfig.Effort)
			request.Thinking = &apicompat.AnthropicThinking{Type: "disabled"}
			converted, err := apicompat.AnthropicToResponses(request)
			require.NoError(t, err)
			require.Equal(t, "none", converted.Reasoning.Effort)
		})
	}
	for _, model := range []string{"gpt-6.2", "gpt-6.2-sol-preview", "gpt-6.2-astra-pro"} {
		require.Equal(t, model, normalizeCodexModel(model))
		require.Empty(t, normalizeKnownOpenAICodexModel(model))
		require.False(t, isOpenAIGPT6Model(model))
	}
}

func TestGPT62CompatCacheAndAccountBoundaries(t *testing.T) {
	keys := make(map[string]bool)
	for _, model := range []string{"gpt-6.1-sol", "gpt-6-astra", openai.GPT62SolModel, openai.GPT62AstraModel} {
		require.True(t, shouldAutoInjectPromptCacheKeyForCompat(model))
		key := deriveCompatPromptCacheKey(&apicompat.ChatCompletionsRequest{Model: model}, model)
		require.NotEmpty(t, key)
		require.False(t, keys[key], "different upstream models must keep separate cache identities")
		keys[key] = true
	}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for _, model := range []string{openai.GPT62SolModel, openai.GPT62AstraModel} {
		require.True(t, account.IsModelSupported(model))
		require.Equal(t, model, account.GetMappedModel(model))
	}
	account.Credentials = map[string]any{"model_mapping": map[string]any{"gpt-6-astra": "gpt-6-astra"}}
	for _, model := range []string{openai.GPT62SolModel, openai.GPT62AstraModel} {
		require.False(t, account.IsModelSupported(model), "an existing explicit allowlist must stay explicit")
	}
}

func TestGPT62PricingDoesNotBorrowLegacyOrSiblingRates(t *testing.T) {
	for _, model := range []string{openai.GPT62SolModel, openai.GPT62AstraModel} {
		t.Run(model, func(t *testing.T) {
			sibling := openai.GPT62SolModel
			if model == sibling {
				sibling = openai.GPT62AstraModel
			}
			catalog := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
				"gpt-5.4": {InputCostPerToken: 1e-6}, "gpt-6-astra": {InputCostPerToken: 2e-6},
				"gpt-6.1-sol": {InputCostPerToken: 3e-6}, "gpt-6.2": {InputCostPerToken: 4e-6},
				sibling: {InputCostPerToken: 5e-6},
			}}
			billing := NewBillingService(&config.Config{}, catalog)
			for _, spelling := range []string{model, "openai/" + model, model + "-max", model + "-2026-11-08"} {
				require.Nil(t, catalog.GetModelPricing(spelling))
				require.Nil(t, catalog.GetIdentifiedModelPricing(spelling))
				_, err := billing.GetModelPricing(spelling)
				require.ErrorIs(t, err, ErrModelPricingUnavailable)
			}

			// Synthetic catalog values deliberately differ from every old model.
			rate := &LiteLLMModelPricing{
				InputCostPerToken: 7e-6, OutputCostPerToken: 13e-6,
				InputCostPerTokenPriority: 19e-6, OutputCostPerTokenPriority: 29e-6,
				CacheCreationInputTokenCostExplicit: true, CacheReadInputTokenCost: 0.7e-6,
				LongContextInputTokenThreshold: 12345, LongContextInputCostMultiplier: 3.5,
			}
			catalog.pricingData[model] = rate
			for _, spelling := range []string{model, "openai/" + model, model + "-max", model + "-2026-11-08"} {
				require.Same(t, rate, catalog.GetModelPricing(spelling))
				got, err := billing.GetModelPricing(spelling)
				require.NoError(t, err)
				require.Equal(t, rate.InputCostPerToken, got.InputPricePerToken)
				require.Equal(t, rate.InputCostPerTokenPriority, got.InputPricePerTokenPriority)
				require.Equal(t, rate.OutputCostPerTokenPriority, got.OutputPricePerTokenPriority)
				require.Zero(t, got.CacheCreationPricePerToken)
				require.True(t, got.CacheCreationPriceExplicit)
				require.Equal(t, rate.LongContextInputTokenThreshold, got.LongContextInputThreshold)
				require.Equal(t, rate.LongContextInputCostMultiplier, got.LongContextInputMultiplier)
			}
		})
	}
}

func TestGPT62ManualPricingWorksWithoutCatalog(t *testing.T) {
	for _, model := range []string{openai.GPT62SolModel, openai.GPT62AstraModel} {
		input, output, write, read := 7e-6, 13e-6, 9e-6, 0.7e-6
		pricing := ChannelModelPricing{
			Platform: PlatformOpenAI, Models: []string{model}, BillingMode: BillingModeToken,
			InputPrice: &input, OutputPrice: &output, CacheWritePrice: &write, CacheReadPrice: &read,
		}
		groupID := int64(62)
		channel := &ChannelService{}
		channel.cache.Store(populateChannelCache([]Channel{{
			ID: 1, Status: StatusActive,
			ModelPricing: []ChannelModelPricing{pricing}, GroupIDs: []int64{groupID},
		}}, map[int64]string{groupID: PlatformOpenAI}))
		resolver := NewModelPricingResolver(channel, NewBillingService(&config.Config{}, nil))
		for _, source := range []string{PricingSourceGroup, PricingSourceChannel} {
			request := PricingInput{Model: model, GroupID: &groupID}
			if source == PricingSourceGroup {
				request.Group = &Group{ID: groupID, ModelPricing: []ChannelModelPricing{pricing}}
			} else {
				request.Model += "-max"
			}
			got := resolver.Resolve(t.Context(), request)
			require.Equal(t, source, got.Source)
			require.NotNil(t, got.BasePricing)
			require.Equal(t, input, got.BasePricing.InputPricePerToken)
			require.Equal(t, output, got.BasePricing.OutputPricePerToken)
			require.Equal(t, write, got.BasePricing.CacheCreationPricePerToken)
			require.Equal(t, read, got.BasePricing.CacheReadPricePerToken)
		}
	}
}

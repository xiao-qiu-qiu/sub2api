//go:build unit

package service

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestGPTImage26PricingAndTokenSettlement(t *testing.T) {
	body, err := os.ReadFile("../../resources/model-pricing/model_prices_and_context_window.json")
	require.NoError(t, err)
	var catalog map[string]*LiteLLMModelPricing
	require.NoError(t, json.Unmarshal(body, &catalog))
	for _, model := range []string{"gpt-image-2.6-flare", "gpt-image-2.6-sunburst"} {
		t.Run(model, func(t *testing.T) {
			for _, prices := range []map[string]*LiteLLMModelPricing{
				catalog,
				{"gpt-image-2": {InputCostPerToken: 99, OutputCostPerImageToken: 99}},
			} {
				pricing := &PricingService{pricingData: prices}
				price := pricing.GetModelPricing(model)
				require.NotNil(t, price)
				require.Equal(t, 5e-6, price.InputCostPerToken)
				require.Equal(t, 1.25e-6, price.CacheReadInputTokenCost)
				require.Equal(t, 8e-6, price.InputCostPerImageToken)
				require.Equal(t, 2e-6, price.CacheReadInputImageTokenCost)
				require.Equal(t, 30e-6, price.OutputCostPerImageToken)
				require.Zero(t, price.OutputCostPerToken)

				billing := NewBillingService(&config.Config{}, pricing)
				resolved, err := billing.GetModelPricing(model)
				require.NoError(t, err)
				cost := billing.computeTokenBreakdown(resolved, UsageTokens{
					InputTokens: 100, ImageInputTokens: 80,
					CacheReadTokens: 50, ImageCacheReadTokens: 40,
					OutputTokens: 343, ImageOutputTokens: 343,
				}, 2, "", false)
				require.InDelta(t, 20*5e-6, cost.InputCost, 1e-12)
				require.InDelta(t, 80*8e-6, cost.ImageInputCost, 1e-12)
				require.InDelta(t, 10*1.25e-6+40*2e-6, cost.CacheReadCost, 1e-12)
				require.InDelta(t, 343*30e-6, cost.ImageOutputCost, 1e-12)
				require.Zero(t, cost.OutputCost, "image output must not also incur text output charges")
				require.InDelta(t, cost.TotalCost*2, cost.ActualCost, 1e-12)
			}
			custom := &LiteLLMModelPricing{InputCostPerToken: 7e-6}
			pricing := &PricingService{pricingData: map[string]*LiteLLMModelPricing{model: custom}}
			require.Same(t, custom, pricing.GetModelPricing(model), "explicit catalog prices retain precedence")
		})
	}
}

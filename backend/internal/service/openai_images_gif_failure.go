package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Preserve consumed tokens and an estimate in ops_error_logs.upstream_errors.
// RecordUsage also deducts customer balance, so rejected output must not be sent
// through the successful-request settlement path just to retain upstream cost.
func (s *OpenAIGatewayService) recordOpenAIImagesGIFOutputFailure(c *gin.Context, account *Account, resp *http.Response, parsed *OpenAIImagesRequest, originalBody []byte, err error) {
	var upstreamErr *OpenAIImagesUpstreamError
	if !errors.As(err, &upstreamErr) {
		return
	}
	usage, reported := codexDirectImagesUsage(originalBody)
	if reported {
		upstreamErr.UpstreamUsage = &usage
	}
	requestID := ""
	if resp != nil {
		requestID = strings.TrimSpace(resp.Header.Get("x-request-id"))
	}
	upstreamErr.UpstreamRequestID = requestID
	model := c.GetString(OpsUpstreamModelKey)
	if model == "" && account != nil && parsed != nil {
		model = account.GetMappedModel(parsed.Model)
	}
	detail := map[string]any{
		"upstream_model":  model,
		"customer_billed": false,
		"usage_reported":  reported,
	}
	if reported {
		// Existing Ops sanitization preserves these token-count names. Nest image
		// counts rather than broadening the global credential-redaction rules.
		detail["usage"] = map[string]any{
			"input_tokens":                usage.InputTokens,
			"output_tokens":               usage.OutputTokens,
			"cache_read_input_tokens":     usage.CacheReadInputTokens,
			"cache_creation_input_tokens": usage.CacheCreationInputTokens,
			"images": map[string]int{
				"input_tokens":            usage.ImageInputTokens,
				"output_tokens":           usage.ImageOutputTokens,
				"cache_read_input_tokens": usage.ImageCacheReadTokens,
			},
		}
		if s != nil && s.billingService != nil {
			if pricing, priceErr := s.billingService.GetModelPricing(model); priceErr == nil && pricing != nil {
				tokens := UsageTokens{
					InputTokens:      max(usage.InputTokens-usage.CacheReadInputTokens-usage.CacheCreationInputTokens, 0),
					ImageInputTokens: max(usage.ImageInputTokens-usage.ImageCacheReadTokens, 0),
					OutputTokens:     usage.OutputTokens, ImageOutputTokens: usage.ImageOutputTokens,
					CacheReadTokens: usage.CacheReadInputTokens, ImageCacheReadTokens: usage.ImageCacheReadTokens,
					CacheCreationTokens: usage.CacheCreationInputTokens,
				}
				cost := s.billingService.computeTokenBreakdown(pricing, tokens, 1, "", false)
				detail["estimated_cost_usd"] = cost.TotalCost
				detail["cost_basis"] = "model_token_prices"
				if account != nil {
					detail["estimated_account_cost_usd"] = cost.TotalCost * account.BillingRateMultiplier()
				}
			}
		}
	}
	encodedDetail, _ := json.Marshal(detail)
	setOpsUpstreamError(c, http.StatusBadGateway, upstreamErr.Message, string(encodedDetail))
	if account != nil {
		upstreamStatus := http.StatusOK
		if resp != nil {
			upstreamStatus = resp.StatusCode
		}
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
			ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
			UpstreamStatusCode: upstreamStatus, UpstreamRequestID: requestID,
			Kind: "http_error", Stage: "image_output_validation", Reason: upstreamErr.Code,
			Message: upstreamErr.Message, Detail: string(encodedDetail),
		})
	}
	writeOpenAIImagesUpstreamErrorResponse(c, upstreamErr)
}

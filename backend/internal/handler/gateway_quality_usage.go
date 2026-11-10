package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Error-only entrances which normally return before their success usage block.
func (h *OpenAIGatewayHandler) recordInterruptedOpenAIUsage(c *gin.Context, key *service.APIKey, account *service.Account, subscription *service.UserSubscription, result *service.OpenAIForwardResult) {
	if result == nil || !result.QualityInterrupted {
		return
	}
	input := &service.OpenAIRecordUsageInput{
		Result: result, APIKey: key, User: key.User, Account: account, Subscription: subscription,
		InboundEndpoint: GetInboundEndpoint(c), UpstreamEndpoint: resolveOpenAIUpstreamEndpoint(c, account, result),
		UserAgent: c.GetHeader("User-Agent"), IPAddress: ip.GetClientIP(c),
		SessionID: service.ExtractClientSessionID(c), APIKeyService: h.apiKeyService,
		QuotaPlatform: service.QuotaPlatform(c.Request.Context(), key),
	}
	h.submitMandatoryUsageRecordTask(c.Request.Context(), func(ctx context.Context) {
		if err := h.gatewayService.RecordUsage(ctx, input); err != nil {
			logger.L().Error("openai.quality_usage_failed", zap.Error(err))
		}
	})
}

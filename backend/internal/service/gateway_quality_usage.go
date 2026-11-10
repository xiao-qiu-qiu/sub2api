package service

import (
	"context"
	"errors"
	"strings"
	"time"
)

// An interrupted request with no observed billing units is an evidence row,
// never a bill. Bypass every tariff (including per-request/image prices), quota
// update and usage billing command; absent usage must not become a fake charge.
func recordQualityInterruptedUsage(ctx context.Context, repo UsageLogRepository, input *OpenAIRecordUsageInput) error {
	if repo == nil || input.APIKey == nil || input.User == nil || input.Account == nil {
		return errors.New("quality interruption usage requires repository, key, user and account")
	}
	r := input.Result
	r.RequestID = qualityInterruptionRequestID(r.RequestID)
	requestID := r.RequestID
	duration := int(r.Duration.Milliseconds())
	row := &UsageLog{
		UserID: input.User.ID, APIKeyID: input.APIKey.ID, AccountID: input.Account.ID,
		RequestID:        requestID,
		RequestStartedAt: usageRequestStartedAt(r.RequestStartedAt, ctx), QualityInterrupted: true,
		UpstreamRequestID: usageUpstreamRequestIDPtr(input.Account, r.UpstreamHeaders, r.OpenAIWSMode),
		Model:             r.Model, RequestedModel: input.OriginalModel,
		UpstreamModel:            optionalTrimmedStringPtr(r.UpstreamModel),
		ReasoningEffort:          r.ReasoningEffort,
		RequestedReasoningEffort: coalesceRequestedReasoningEffort(r.RequestedReasoningEffort, r.ReasoningEffort),
		Stream:                   r.Stream, OpenAIWSMode: r.OpenAIWSMode, DurationMs: &duration,
		InboundEndpoint:  optionalTrimmedStringPtr(input.InboundEndpoint),
		UpstreamEndpoint: optionalTrimmedStringPtr(input.UpstreamEndpoint),
		UserAgent:        optionalTrimmedStringPtr(input.UserAgent), IPAddress: optionalTrimmedStringPtr(input.IPAddress),
		SessionID: optionalTrimmedStringPtr(input.SessionID), GroupID: input.APIKey.GroupID,
		SubscriptionID:     optionalSubscriptionID(input.Subscription),
		NativeCompactionV2: input.NativeCompactionV2,
		ChannelID:          optionalInt64Ptr(input.ChannelID), ModelMappingChain: optionalTrimmedStringPtr(input.ModelMappingChain),
		CreatedAt: time.Now(),
	}
	if row.RequestedModel == "" {
		row.RequestedModel = r.Model
	}
	if input.Subscription != nil {
		row.BillingType = BillingTypeSubscription
	}
	row.SyncRequestTypeAndLegacyFields()
	// Quality cancellations arrive with a canceled context. The observation
	// must survive that cancellation just like ordinary asynchronous usage.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, err := repo.Create(ctx, row)
	return err
}

func qualityInterruptionRequestID(id string) string {
	if strings.HasPrefix(id, "quality-interrupt:") {
		return id
	}
	return "quality-interrupt:" + generateRequestID()
}

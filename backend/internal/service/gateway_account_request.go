package service

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/tidwall/gjson"
)

// Keep account control separate from the incoming client lifecycle. Detached
// transports must still stop when quality disposition interrupts this account.
func beginAccountGatewayRequest(ctx context.Context, account *Account) (context.Context, func()) {
	if turns, _ := ctx.Value(accountWSTurnKey{}).(*accountWSTurns); turns != nil {
		ctx = turns.bind(ctx)
	}
	ctx = withGatewayRequestStart(ctx)
	// Manual diagnostics remain usable while business requests are isolated.
	if account == nil || isManualAccountTest(ctx) {
		return ctx, func() {}
	}
	return accountrequest.Default.Track(ctx, account.ID)
}

func accountRequestQualityError(ctx context.Context, causes ...error) error {
	if accountrequest.QualityDegraded(ctx) {
		return accountrequest.ErrQualityDegraded
	}
	for _, cause := range causes {
		if errors.Is(cause, accountrequest.ErrQualityDegraded) {
			return accountrequest.ErrQualityDegraded
		}
	}
	return nil
}

type accountRequestMetadata struct {
	model            string
	stream           bool
	effort           *string
	forwardStartedAt time.Time
}

func openAIAccountRequestMetadata(body []byte) accountRequestMetadata {
	model := gjson.GetBytes(body, "model").String()
	return accountRequestMetadata{model: model, stream: gjson.GetBytes(body, "stream").Bool(), effort: extractOpenAIReasoningEffortFromBody(body, model, model), forwardStartedAt: time.Now()}
}

func finishAccountOpenAIRequest(ctx context.Context, result **OpenAIForwardResult, err *error, metadata accountRequestMetadata) {
	qualityErr := accountRequestQualityError(ctx, *err)
	if qualityErr != nil {
		*err = qualityErr
		if *result == nil {
			*result = &OpenAIForwardResult{Model: metadata.model, UsageUnavailable: true, ReasoningEffort: metadata.effort}
			start := metadata.forwardStartedAt
			if start.IsZero() {
				start = gatewayRequestStartedAt(ctx)
			}
			if !start.IsZero() {
				(*result).Duration = time.Since(start)
				if (*result).Duration < 0 {
					(*result).Duration = 0
				}
			}
		}
		(*result).Stream = metadata.stream
		if (*result).RequestedReasoningEffort == nil {
			(*result).RequestedReasoningEffort = metadata.effort
		}
		(*result).QualityInterrupted = true
		(*result).ClientDisconnect = false
		if !openAIResultHasObservedUnits(*result) {
			(*result).UsageUnavailable = true
			(*result).RequestID = qualityInterruptionRequestID((*result).RequestID)
		}
		(*result).ObservedUsageOnError = !(*result).UsageUnavailable
	}
	if *result != nil && (*result).RequestStartedAt.IsZero() {
		(*result).RequestStartedAt = gatewayRequestStartedAt(ctx)
	}
}

func openAIResultHasObservedUnits(r *OpenAIForwardResult) bool {
	u := r.Usage
	return u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheCreationInputTokens > 0 ||
		u.CacheReadInputTokens > 0 || u.ImageInputTokens > 0 || u.ImageOutputTokens > 0 ||
		r.ImageCount > 0 || r.VideoCount > 0 || r.SearchCount > 0 || r.WebSearchCalls > 0 || (r.AudioUsage != nil && r.AudioUsage.DurationOrUnits > 0)
}

func gatewayRequestStartedAt(ctx context.Context) time.Time {
	if ctx == nil {
		return time.Time{}
	}
	if start := accountrequest.StartedAt(ctx); !start.IsZero() {
		return start
	}
	start, _ := ctx.Value(ctxkey.RequestStartTime).(time.Time)
	return start
}

func withGatewayRequestStart(ctx context.Context) context.Context {
	start := gatewayRequestStartedAt(ctx)
	if start.IsZero() {
		// Internal callers without HTTP ingress still get one stable clock.
		start = time.Now()
	}
	return accountrequest.WithStartedAt(ctx, start)
}

func usageRequestStartedAt(start time.Time, ctx context.Context) *time.Time {
	if start.IsZero() {
		start = gatewayRequestStartedAt(ctx)
	}
	if start.IsZero() {
		return nil
	}
	return &start
}

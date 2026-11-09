package service

import (
	"context"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
)

// Keep account control separate from the incoming client lifecycle. Detached
// transports must still stop when quality disposition interrupts this account.
func beginAccountGatewayRequest(ctx context.Context, account *Account) (context.Context, func()) {
	if account == nil {
		return ctx, func() {}
	}
	return accountrequest.Default.Begin(ctx, account.ID)
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

func finishAccountOpenAIRequest(ctx context.Context, result *OpenAIForwardResult, err *error) {
	if qualityErr := accountRequestQualityError(ctx); qualityErr != nil {
		*err = qualityErr
		if result != nil {
			result.ClientDisconnect = false
		}
	}
}

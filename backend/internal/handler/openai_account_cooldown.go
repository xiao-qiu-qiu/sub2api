package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type openAIAccountCooldownGateway interface {
	WaitForOpenAIAccountCooldown(context.Context) (bool, error)
	QueueOpenAIAccountCooldown(context.Context, int64, string) bool
}

// The forwarding owner must release its account slot before calling this. The
// user slot also owns the API key lease, so neither remains held while waiting.
func (h *OpenAIGatewayHandler) retryOpenAIAccountCooldownHTTP(c *gin.Context, gateway openAIAccountCooldownGateway, slots *openAIOAuthRecoveryUserSlot, account *service.Account, model string, result *service.OpenAIForwardResult, failoverErr *service.UpstreamFailoverError, replaySafe bool, streamStarted *bool, anthropic bool) (handled, retry bool) {
	if !replaySafe || result != nil || account == nil || failoverErr == nil || failoverErr.StatusCode != http.StatusTooManyRequests || failoverErr.Reason != service.ExcelBPSRateLimitedReason || failoverClientGone(c) {
		return false, false
	}
	if !gateway.QueueOpenAIAccountCooldown(c.Request.Context(), account.ID, model) {
		return false, false
	}
	slots.release()
	_, err := h.waitForOpenAIAccountCooldownHTTP(c, gateway, streamStarted)
	if err != nil {
		h.handleOpenAIAccountCooldownHTTPError(c, err, *streamStarted, anthropic)
		return true, false
	}
	if failoverClientGone(c) || !slots.acquire() {
		return true, false
	}
	// The service's bool means a wait was present, including budget expiry.
	// Reselect with the same budget; after 30s selection may change accounts.
	return true, true
}

func (h *OpenAIGatewayHandler) handleOpenAIAccountCooldownSelectionError(c *gin.Context, gateway openAIAccountCooldownGateway, slots *openAIOAuthRecoveryUserSlot, err error, streamStarted *bool, anthropic bool) (handled, retry bool) {
	if !errors.Is(err, service.ErrOpenAIAccountCooldownAdmissionInterrupted) {
		return false, false
	}
	slots.release()
	if _, waitErr := h.waitForOpenAIAccountCooldownHTTP(c, gateway, streamStarted); waitErr != nil {
		h.handleOpenAIAccountCooldownHTTPError(c, waitErr, *streamStarted, anthropic)
		return true, false
	}
	return true, !failoverClientGone(c) && slots.acquire()
}

// Wait only on the request context: a key queue's shorter admission deadline
// must not shorten the shared cooldown budget. The caller owns no slots here.
func (h *OpenAIGatewayHandler) waitForOpenAIAccountCooldownHTTP(c *gin.Context, gateway openAIAccountCooldownGateway, streamStarted *bool) (bool, error) {
	ctx := c.Request.Context()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	isStream, _ := c.Get(opsStreamKey)
	stream, _ := isStream.(bool)
	if !stream || h.concurrencyHelper == nil || h.concurrencyHelper.pingFormat == "" {
		return gateway.WaitForOpenAIAccountCooldown(ctx)
	}
	type waitResult struct {
		recovered bool
		err       error
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	resultCh := make(chan waitResult, 1)
	go func() {
		recovered, err := gateway.WaitForOpenAIAccountCooldown(waitCtx)
		resultCh <- waitResult{recovered, err}
	}()
	ticker := time.NewTicker(h.concurrencyHelper.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case result := <-resultCh:
			return result.recovered, result.err
		case <-ctx.Done():
			cancel()
			<-resultCh
			return false, ctx.Err()
		case <-ticker.C:
			if err := h.concurrencyHelper.writeWaitPing(c, streamStarted); err != nil {
				cancel()
				<-resultCh
				return false, err
			}
		}
	}
}

func (h *OpenAIGatewayHandler) handleOpenAIAccountCooldownHTTPError(c *gin.Context, err error, streamStarted, anthropic bool) {
	if failoverClientGone(c) || errors.Is(err, context.Canceled) {
		return
	}
	streamStarted = streamStarted || c.Writer.Written()
	const message = "Upstream account cooldown wait failed; please retry"
	if anthropic {
		h.anthropicStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", message, streamStarted)
		return
	}
	h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "upstream_error", message, streamStarted)
}

func waitForOpenAIAccountCooldownWS(ctx context.Context, c *gin.Context, gateway openAIAccountCooldownGateway) (bool, error) {
	var recovered bool
	_, err := service.WaitOpenAIWSKeyAdmission(ctx, c, func(waitCtx context.Context) (*service.APIKeySlotReservation, error) {
		var waitErr error
		recovered, waitErr = gateway.WaitForOpenAIAccountCooldown(waitCtx)
		return nil, waitErr
	})
	return recovered, err
}

func waitForOpenAIAccountCooldownWSFailover(ctx context.Context, c *gin.Context, gateway openAIAccountCooldownGateway, releaseSlots func(), accountID int64, model string, failoverErr *service.UpstreamFailoverError, replaySafe bool) (bool, error) {
	if !replaySafe || ctx.Err() != nil || failoverErr == nil || failoverErr.StatusCode != http.StatusTooManyRequests || failoverErr.Reason != service.ExcelBPSRateLimitedReason {
		return false, nil
	}
	if !gateway.QueueOpenAIAccountCooldown(ctx, accountID, model) {
		return false, nil
	}
	releaseSlots()
	_, err := waitForOpenAIAccountCooldownWS(ctx, c, gateway)
	return true, err
}

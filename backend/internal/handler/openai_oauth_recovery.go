package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type openAIOAuthRecoveryGateway interface {
	RecoverOpenAIOAuth401(context.Context, *service.Account, []byte) (bool, error)
	WaitForOpenAIOAuthRecovery(context.Context, *int64, string) (bool, error)
}

type openAIOAuthRecoveryAdmissionKey struct{}

type openAIOAuthRecoveryAdmission struct {
	groupID     *int64
	model       string
	anthropic   bool
	fallbackErr error
}

// A long-lived ingress captures its context before later response.create
// frames arrive. Descendants must see the new logical turn's budget as well.
type openAIOAuthWSTurnRecoveryContext struct {
	context.Context
	budget            atomic.Pointer[context.Context]
	credentialRetries atomic.Int32
}

func newOpenAIOAuthWSTurnRecoveryContext(ctx context.Context) *openAIOAuthWSTurnRecoveryContext {
	turnCtx := &openAIOAuthWSTurnRecoveryContext{Context: ctx}
	turnCtx.beginTurn()
	return turnCtx
}

func (c *openAIOAuthWSTurnRecoveryContext) beginTurn() {
	budgetCtx := service.NewOpenAIOAuthRecoveryBudget(c.Context)
	budgetCtx = service.NewOpenAIAccountCooldownBudget(budgetCtx)
	c.budget.Store(&budgetCtx)
	c.credentialRetries.Store(0)
}

func (c *openAIOAuthWSTurnRecoveryContext) Value(key any) any {
	return (*c.budget.Load()).Value(key)
}

func (c *openAIOAuthWSTurnRecoveryContext) allowCredentialReplay(payload []byte, currentTurn, observedResult bool) bool {
	if !currentTurn || observedResult || !gjson.ValidBytes(payload) || gjson.GetBytes(payload, "type").String() != "response.create" {
		return false
	}
	return c.credentialRetries.CompareAndSwap(0, 1)
}

type openAIOAuthRecoveryUserSlot struct {
	acquireFunc func() (func(), bool)
	releaseFunc func()
}

func (s *openAIOAuthRecoveryUserSlot) acquire() bool {
	release, acquired := s.acquireFunc()
	if acquired {
		s.releaseFunc = release
	}
	return acquired
}

func (s *openAIOAuthRecoveryUserSlot) release() {
	if s.releaseFunc != nil {
		s.releaseFunc()
		s.releaseFunc = nil
	}
}

func openAIOAuth401RecoveryEligible(account *service.Account, result *service.OpenAIForwardResult, failoverErr *service.UpstreamFailoverError, replaySafe bool) bool {
	return replaySafe && result == nil && account != nil && account.Platform == service.PlatformOpenAI && account.Type == service.AccountTypeOAuth && failoverErr != nil && failoverErr.StatusCode == http.StatusUnauthorized && !failoverErr.IsCredentialFailure()
}

// A failover error is replayable only before semantic output and observed
// usage. In particular, never replay a partial result that must be billed.
func recoverOpenAIOAuth401(ctx context.Context, gateway openAIOAuthRecoveryGateway, account *service.Account, result *service.OpenAIForwardResult, failoverErr *service.UpstreamFailoverError, replaySafe bool) (bool, error) {
	if !openAIOAuth401RecoveryEligible(account, result, failoverErr, replaySafe) {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	return gateway.RecoverOpenAIOAuth401(ctx, account, failoverErr.ResponseBody)
}

func (h *OpenAIGatewayHandler) recoverOpenAIOAuthHTTP401(c *gin.Context, slots *openAIOAuthRecoveryUserSlot, account *service.Account, result *service.OpenAIForwardResult, failoverErr *service.UpstreamFailoverError, streamStarted, anthropic bool) (handled, retry bool) {
	if !openAIOAuth401RecoveryEligible(account, result, failoverErr, true) {
		return false, false
	}
	// The forwarding scope has already released the account slot. A login wait
	// owns no generation slots, including the API key lease held by user slots.
	slots.release()
	handled, err := recoverOpenAIOAuth401(c.Request.Context(), h.gatewayService, account, result, failoverErr, true)
	if err != nil {
		if !prepareOpenAIOAuthHTTPRecoveryFallback(c, err, true) {
			h.handleOpenAIOAuthRecoveryError(c, err, streamStarted, anthropic)
			return true, false
		}
		// Reselect normally with the recovery account excluded. This request
		// borrows a healthy account without replacing its original binding.
		handled = true
	}
	if failoverClientGone(c) || !slots.acquire() {
		return true, false
	}
	// An unsupported 401 retains ordinary failover handling with its normal
	// user/key admission restored. A recovered 401 reselects without exclusion.
	return handled, handled
}

func isOpenAIOAuthRecoveryError(err error) bool {
	return errors.Is(err, service.ErrOAuthRecoveryTimeout) || errors.Is(err, service.ErrOAuthRecoveryFailed) || errors.Is(err, service.ErrOAuthRecoveryQueueFull)
}

func prepareOpenAIOAuthRecoveryFallback(ctx context.Context, err error, replaySafe bool) bool {
	return replaySafe && ctx.Err() == nil && isOpenAIOAuthRecoveryError(err) && service.PrepareOpenAIOAuthRecoveryFallback(ctx, err)
}

func prepareOpenAIOAuthHTTPRecoveryFallback(c *gin.Context, err error, replaySafe bool) bool {
	if !prepareOpenAIOAuthRecoveryFallback(c.Request.Context(), err, replaySafe) {
		return false
	}
	if admission, ok := c.Request.Context().Value(openAIOAuthRecoveryAdmissionKey{}).(*openAIOAuthRecoveryAdmission); ok {
		admission.fallbackErr = err
	}
	return true
}

func clearOpenAIOAuthRecoveryFallback(c *gin.Context) {
	if admission, ok := c.Request.Context().Value(openAIOAuthRecoveryAdmissionKey{}).(*openAIOAuthRecoveryAdmission); ok {
		// A new upstream attempt owns any later error. Do not mask it with a
		// previous recovery's failure if ordinary failover is later exhausted.
		admission.fallbackErr = nil
	}
}

func openAIOAuthRecoveryErrorDetails(err error) (string, string) {
	if errors.Is(err, service.ErrOAuthRecoveryTimeout) {
		return "upstream_auth_recovery_timeout", "Upstream authentication recovery timed out; please retry"
	}
	if errors.Is(err, service.ErrOAuthRecoveryQueueFull) {
		return "upstream_auth_recovery_queue_full", "Upstream authentication recovery queue is full; please retry later"
	}
	return "upstream_auth_recovery_failed", "Upstream authentication recovery failed; please retry later"
}

// Install one lazy budget before either pre-admission waiting or 401 recovery.
// New requests wait without consuming user or API key concurrency slots.
func (h *OpenAIGatewayHandler) waitForOpenAIOAuthRecoveryBeforeAdmission(c *gin.Context, groupID *int64, model string, streamStarted, anthropic bool) bool {
	ctx := service.WithOpenAIOAuthRecoveryBudget(c.Request.Context())
	ctx = service.WithOpenAIAccountCooldownBudget(ctx)
	ctx = context.WithValue(ctx, openAIOAuthRecoveryAdmissionKey{}, &openAIOAuthRecoveryAdmission{groupID: groupID, model: model, anthropic: anthropic})
	ctx = service.WithOAuthRecoveryAdmissionCheck(ctx, func(checkCtx context.Context) bool {
		return h.gatewayService.HasPendingOpenAIOAuthRecovery(checkCtx, groupID, model)
	})
	c.Request = c.Request.WithContext(ctx)
	_, err := h.gatewayService.WaitForOpenAIOAuthRecovery(c.Request.Context(), groupID, model)
	if err != nil {
		h.handleOpenAIOAuthRecoveryError(c, err, streamStarted, anthropic)
		return false
	}
	return !failoverClientGone(c)
}

func (h *OpenAIGatewayHandler) waitForOpenAIOAuthRecoveryAfterAdmissionInterruption(c *gin.Context, streamStarted, selectionObserved bool) bool {
	admission, _ := c.Request.Context().Value(openAIOAuthRecoveryAdmissionKey{}).(*openAIOAuthRecoveryAdmission)
	if admission == nil {
		admission = &openAIOAuthRecoveryAdmission{}
	}
	_, err := h.gatewayService.WaitForOpenAIOAuthRecovery(c.Request.Context(), admission.groupID, admission.model)
	if err != nil {
		if prepareOpenAIOAuthHTTPRecoveryFallback(c, err, selectionObserved) {
			return !failoverClientGone(c)
		}
		h.handleOpenAIOAuthRecoveryError(c, err, streamStarted, admission.anthropic)
		return false
	}
	return !failoverClientGone(c)
}

// Selection can discover a bound account's recovery after user/key admission.
// Leave both slots before parking, then perform fresh admission before selecting
// again. Ordinary selection failures must keep their original error handling.
func (h *OpenAIGatewayHandler) handleOpenAIOAuthRecoverySelectionError(c *gin.Context, slots *openAIOAuthRecoveryUserSlot, err error, streamStarted, anthropic bool) (handled, retry bool) {
	if errors.Is(err, service.ErrOAuthRecoveryAdmissionInterrupted) {
		slots.release()
		if !h.waitForOpenAIOAuthRecoveryAfterAdmissionInterruption(c, streamStarted, true) {
			return true, false
		}
		return true, !failoverClientGone(c) && slots.acquire()
	}
	if isOpenAIOAuthRecoveryError(err) {
		slots.release()
		if prepareOpenAIOAuthHTTPRecoveryFallback(c, err, true) {
			return true, !failoverClientGone(c) && slots.acquire()
		}
		h.handleOpenAIOAuthRecoveryError(c, err, streamStarted, anthropic)
		return true, false
	}
	if admission, ok := c.Request.Context().Value(openAIOAuthRecoveryAdmissionKey{}).(*openAIOAuthRecoveryAdmission); ok && admission.fallbackErr != nil &&
		(errors.Is(err, service.ErrNoAvailableAccounts) || errors.Is(err, service.ErrNoAvailableCompactAccounts)) {
		slots.release()
		h.handleOpenAIOAuthRecoveryError(c, admission.fallbackErr, streamStarted, anthropic)
		return true, false
	}
	return false, false
}

func (h *OpenAIGatewayHandler) handleOpenAIOAuthRecoveryError(c *gin.Context, err error, streamStarted, anthropic bool) {
	if failoverClientGone(c) || (!isOpenAIOAuthRecoveryError(err) && errors.Is(err, context.Canceled)) {
		return
	}
	code, message := openAIOAuthRecoveryErrorDetails(err)
	streamStarted = streamStarted || c.Writer.Written()
	service.MarkOpsStreamFailure(c, "upstream_error", code, message, http.StatusServiceUnavailable)
	if anthropic {
		payload := gin.H{"type": "error", "error": gin.H{"type": "api_error", "code": code, "message": message}}
		if streamStarted {
			data, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", data)
			c.Writer.Flush()
		} else {
			c.JSON(http.StatusServiceUnavailable, payload)
		}
		return
	}
	h.handleStreamingAwareErrorWithCode(c, http.StatusServiceUnavailable, "upstream_error", code, message, streamStarted, true)
}

// Use the ingress' existing single-reader wait so cancellation, peer departure,
// lease loss and overlapping frames keep their ordinary admission semantics.
func waitForOpenAIOAuthWSRecovery(ctx context.Context, c *gin.Context, gateway openAIOAuthRecoveryGateway, groupID *int64, model string) error {
	_, err := service.WaitOpenAIWSKeyAdmission(ctx, c, func(waitCtx context.Context) (*service.APIKeySlotReservation, error) {
		_, err := gateway.WaitForOpenAIOAuthRecovery(waitCtx, groupID, model)
		return nil, err
	})
	return err
}

func recoverOpenAIOAuthWS401(ctx context.Context, c *gin.Context, gateway openAIOAuthRecoveryGateway, account *service.Account, failoverErr *service.UpstreamFailoverError) (bool, error) {
	var handled bool
	_, err := service.WaitOpenAIWSKeyAdmission(ctx, c, func(waitCtx context.Context) (*service.APIKeySlotReservation, error) {
		var recoveryErr error
		handled, recoveryErr = recoverOpenAIOAuth401(waitCtx, gateway, account, nil, failoverErr, true)
		return nil, recoveryErr
	})
	return handled, err
}

func closeOpenAIOAuthWSRecoveryError(ctx context.Context, c *gin.Context, conn *coderws.Conn, err error) {
	if errors.Is(err, service.ErrOpenAIWSIngressLeaseLost) {
		closeOpenAIWSAdmissionError(conn, nil, "", err)
		return
	}
	if ctx.Err() != nil || (!isOpenAIOAuthRecoveryError(err) && errors.Is(err, context.Canceled)) || service.IsOpenAIWSClientGoneError(err) || service.IsOpenAIWSSessionPreemptedError(err) {
		return
	}
	if !isOpenAIOAuthRecoveryError(err) {
		closeOpenAIWSAdmissionError(conn, nil, "", err)
		return
	}
	code, message := openAIOAuthRecoveryErrorDetails(err)
	service.MarkOpsStreamFailure(c, "upstream_error", code, message, http.StatusServiceUnavailable)
	payload, _ := json.Marshal(gin.H{
		"type":   "error",
		"status": http.StatusServiceUnavailable,
		"error":  gin.H{"type": "upstream_error", "code": code, "message": message},
	})
	if conn != nil {
		writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_ = conn.Write(writeCtx, coderws.MessageText, payload)
		cancel()
	}
	closeOpenAIClientWS(conn, coderws.StatusTryAgainLater, code)
}

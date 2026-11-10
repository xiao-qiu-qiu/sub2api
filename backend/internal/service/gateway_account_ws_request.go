package service

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
)

type accountWSTurnKey struct{}

// The relay has one control context, but only the active response.create owns
// an account registration. Keeping the registration out of the session's value
// chain prevents later turns from inheriting the first turn's timestamp.
type accountWSTurns struct {
	mu              sync.Mutex
	base            context.Context
	cancel          context.CancelCauseFunc
	account         *Account
	turn, completed int
	model           string
	requestID       string
	scope           context.Context
	release         func()
	stop            func() bool
}

func newAccountWSTurns(ctx context.Context, account *Account, hooks *OpenAIWSIngressHooks) (context.Context, *OpenAIWSIngressHooks, *accountWSTurns) {
	ctx, cancel := context.WithCancelCause(ctx)
	t := &accountWSTurns{cancel: cancel, account: account}
	ctx = context.WithValue(ctx, accountWSTurnKey{}, t)
	t.base = ctx
	wrapped := OpenAIWSIngressHooks{}
	if hooks != nil {
		wrapped = *hooks
	}
	arrival := wrapped.InitialFrameArrivedAt
	if arrival.IsZero() {
		arrival = wrapped.InitialTurnStartedAt
	}
	// No HTTP connection timestamp fallback: a WS request starts at its frame.
	if arrival.IsZero() {
		arrival = time.Now()
	}
	t.model = wrapped.InitialRequestModel
	t.begin(1, arrival)
	oldStarted, oldAfter := wrapped.TurnStarted, wrapped.AfterTurn
	wrapped.TurnStarted = func(turn int, at time.Time) {
		t.begin(turn, at)
		if oldStarted != nil {
			oldStarted(turn, at)
		}
	}
	wrapped.AfterTurn = func(turn int, result *OpenAIForwardResult, err error) {
		result, err = t.finish(turn, result, err)
		if oldAfter != nil {
			oldAfter(turn, result, err)
		}
	}
	oldParse := wrapped.BeforePayloadParse
	wrapped.BeforePayloadParse = func(turn int, payload []byte, model string) error {
		t.setModel(turn, model)
		if oldParse != nil {
			return oldParse(turn, payload, model)
		}
		return nil
	}
	oldRequest := wrapped.BeforeRequest
	wrapped.BeforeRequest = func(turn int, payload []byte, model string) error {
		t.setModel(turn, model)
		if oldRequest != nil {
			return oldRequest(turn, payload, model)
		}
		return nil
	}
	if oldClientRequest := wrapped.BeforeRequestWithClientPayload; oldClientRequest != nil {
		wrapped.BeforeRequestWithClientPayload = func(turn int, payload, clientPayload []byte, model string) error {
			t.setModel(turn, model)
			return oldClientRequest(turn, payload, clientPayload, model)
		}
	}
	return ctx, &wrapped, t
}

func (t *accountWSTurns) setModel(turn int, model string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.turn == turn && t.scope != nil && model != "" {
		t.model = model
	}
}

func (t *accountWSTurns) begin(turn int, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Repeated timing callbacks and rejected overlaps cannot replace a scope.
	if t.scope != nil || turn <= t.completed {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	t.turn = turn
	t.requestID = "quality-interrupt:" + generateRequestID()
	t.scope, t.release = accountrequest.Default.Track(accountrequest.WithStartedAt(t.base, at), t.account.ID)
	scope := t.scope
	t.stop = context.AfterFunc(scope, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.scope == scope && accountrequest.QualityDegraded(scope) {
			t.cancel(accountrequest.ErrQualityDegraded)
		}
	})
}

func (t *accountWSTurns) bind(ctx context.Context) context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.scope == nil {
		return ctx
	}
	ctx, _ = accountrequest.PreserveCancellation(t.scope, ctx)
	return ctx
}

func (t *accountWSTurns) finish(turn int, result *OpenAIForwardResult, err error) (*OpenAIForwardResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.scope == nil || t.turn != turn {
		return result, err
	}
	t.stop()
	t.release()
	finishAccountOpenAIRequest(t.scope, &result, &err, accountRequestMetadata{model: t.model, stream: true})
	if result != nil {
		if result.QualityInterrupted && !openAIResultHasObservedUnits(result) {
			result.RequestID = t.requestID
		}
		result.RequestStartedAt = accountrequest.StartedAt(t.scope)
		result.OpenAIWSMode = true
		result.Stream = true
		if result.QualityInterrupted {
			t.cancel(accountrequest.ErrQualityDegraded)
		}
	}
	t.scope, t.release, t.stop = nil, nil, nil
	t.completed = turn
	return result, err
}

func (t *accountWSTurns) close(hooks *OpenAIWSIngressHooks, err error) {
	t.mu.Lock()
	turn, active := t.turn, t.scope != nil
	t.mu.Unlock()
	if active {
		// A tracked frame may be rejected before the relay admits a model turn.
		// Release its quality scope without inventing an accounting callback.
		// A quality cancellation still needs the synthetic zero-usage evidence.
		result, finishErr := t.finish(turn, nil, err)
		if result != nil && result.QualityInterrupted && hooks != nil && hooks.AfterTurn != nil {
			hooks.AfterTurn(turn, result, finishErr)
		}
	}
	t.cancel(context.Canceled)
}

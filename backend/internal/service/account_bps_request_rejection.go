package service

import (
	"context"
	"sync"
	"time"
)

const (
	BPSRequestRejectedExtraKey           = "openai_bps_request_rejected"
	BPSRequestRejectedGenerationExtraKey = "openai_bps_request_rejected_generation"
)

// AccountBPSRequestRejectionRepository persists a rejection only while the
// observed account identity, credentials and explicit reset generation match.
type AccountBPSRequestRejectionRepository interface {
	MarkBPSRequestRejected(ctx context.Context, observed *Account, reason string) (bool, error)
}

// AccountBPSRequestRejectionResetRepository is used only by the explicit admin
// account-state reset, never by automatic recovery or ordinary limit clearing.
type AccountBPSRequestRejectionResetRepository interface {
	ResetBPSRequestRejected(ctx context.Context, id int64) error
}

type explicitAccountStateResetContextKey struct{}

// WithExplicitAccountStateReset is set only by the administrator clear-error
// endpoints. Automatic re-login and background ClearAccountError calls do not
// carry it and must preserve the rejection and its reset generation.
func WithExplicitAccountStateReset(ctx context.Context) context.Context {
	return context.WithValue(ctx, explicitAccountStateResetContextKey{}, true)
}

// BPSRequestRejectionWritesAllowed keeps administrative tests and observations
// from changing business scheduling state even if they reach a failure hook.
func BPSRequestRejectionWritesAllowed(ctx context.Context) bool {
	return !isManualAccountTest(ctx) && !isQualityObservation(ctx)
}

type bpsRequestRejectionSchedulingState struct {
	updatedAt time.Time
	blocked   bool
}

var bpsRequestRejectionSchedulingStates sync.Map

// PublishBPSRequestRejectionSchedulingState fences existing in-process account
// snapshots, including sticky selections and the short-lived turn admission
// cache. Call only with a committed row read under the publication lock.
func PublishBPSRequestRejectionSchedulingState(a *Account) {
	if a == nil || a.ID <= 0 {
		return
	}
	marker, _ := a.Extra[BPSRequestRejectedExtraKey].(map[string]any)
	blocked, _ := marker["blocked"].(bool)
	next := bpsRequestRejectionSchedulingState{updatedAt: a.UpdatedAt, blocked: blocked}
	for {
		previous, loaded := bpsRequestRejectionSchedulingStates.LoadOrStore(a.ID, next)
		if !loaded || previous.(bpsRequestRejectionSchedulingState).updatedAt.After(next.updatedAt) {
			return
		}
		if bpsRequestRejectionSchedulingStates.CompareAndSwap(a.ID, previous, next) {
			return
		}
	}
}

// IsBPSRequestRejected reports the persistent business scheduling gate.
func (a *Account) IsBPSRequestRejected() bool {
	if a == nil {
		return false
	}
	if local, ok := bpsRequestRejectionSchedulingStates.Load(a.ID); ok {
		state := local.(bpsRequestRejectionSchedulingState)
		if !a.UpdatedAt.After(state.updatedAt) {
			return state.blocked
		}
	}
	state, _ := a.Extra[BPSRequestRejectedExtraKey].(map[string]any)
	blocked, _ := state["blocked"].(bool)
	return blocked
}

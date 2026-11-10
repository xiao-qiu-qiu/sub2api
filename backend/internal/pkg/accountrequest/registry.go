// Package accountrequest interrupts in-flight business requests for an account
// within a quality transition's request creation window.
package accountrequest

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrQualityDegraded is the cancellation cause for an account quality interrupt.
var ErrQualityDegraded = errors.New("account quality degraded")

// Default is the process-wide registry used by business request entry points.
var Default = NewRegistry()

// Registry owns request scopes. Its zero value is ready for use. Do not copy it.
type Registry struct {
	mu       sync.Mutex
	requests map[int64]map[*requestScope]struct{}
	// Keep only timestamps, never completed scopes. Remembering even an empty
	// account prevents a late self-echo or duplicate event from killing new work.
	cutoffs map[int64]time.Time

	transportMu sync.Mutex
	transport   *subscription
}

func NewRegistry() *Registry { return &Registry{} }

type scopeKey struct{}
type startedAtKey struct{}

// WithStartedAt attaches the business request's creation time before Begin or
// Track. A nonzero value is used verbatim, even when it predates an already
// processed cutoff. It cannot change an existing same-account scope's time.
func WithStartedAt(ctx context.Context, started time.Time) context.Context {
	return context.WithValue(ctx, startedAtKey{}, started)
}

// StartedAt returns the scope's original creation time, or the time injected by
// WithStartedAt before registration. Without either it returns the zero time.
// context.WithoutCancel and PreserveCancellation retain a scope's timestamp.
func StartedAt(ctx context.Context) time.Time {
	if scope := scopeFrom(ctx); scope != nil {
		return scope.started
	}
	return injectedStartedAt(ctx)
}

func injectedStartedAt(ctx context.Context) time.Time {
	if ctx == nil {
		return time.Time{}
	}
	started, _ := ctx.Value(startedAtKey{}).(time.Time)
	return started
}

type binding struct{ cancel func(error) }

// The control signal is deliberately independent of the client's Done channel.
// Bindings use synchronous fanout and need no waiting goroutine per request.
type requestScope struct {
	accountID int64
	started   time.Time
	parent    *requestScope

	mu       sync.Mutex
	cause    error
	stopped  chan struct{}
	bindings map[*binding]struct{}
	cleanups []func()
}

// Begin registers a request until release, which must be called when forwarding
// (including detached work) finishes. Release is idempotent and cancels only this
// scope and its descendants. Client cancellation does not unregister the scope:
// detached upstream work still needs to receive a later quality interrupt.
// A nested Begin for the same account inherits the original registration time,
// including through context.WithoutCancel and across Registry instances.
func (r *Registry) Begin(ctx context.Context, accountID int64) (context.Context, func()) {
	return r.begin(ctx, accountID, context.Canceled)
}

// Track registers a forwarding request like Begin, but its completion function
// only releases tracking and bindings. It does not cancel successfully completed
// upstream contexts. Call it after closing the transport and consuming its body;
// use Begin when releasing ownership must also abort outstanding work.
func (r *Registry) Track(ctx context.Context, accountID int64) (context.Context, func()) {
	return r.begin(ctx, accountID, nil)
}

func (r *Registry) begin(ctx context.Context, accountID int64, releaseCause error) (context.Context, func()) {
	if accountID <= 0 {
		return ctx, func() {}
	}
	parent := scopeFrom(ctx)
	scope := &requestScope{accountID: accountID, parent: parent}
	r.mu.Lock()
	scope.started = injectedStartedAt(ctx)
	explicit := !scope.started.IsZero()
	if !explicit {
		scope.started = time.Now()
	}
	for ancestor := parent; ancestor != nil; ancestor = ancestor.parent {
		if ancestor.accountID == accountID {
			scope.started = ancestor.started
			break
		}
	}
	if !explicit && !hasAccount(parent, accountID) && !scope.started.After(r.cutoffs[accountID]) {
		// Wall clocks can have coarse ticks or move backwards. A root admitted
		// after a processed event belongs strictly after that event.
		scope.started = r.cutoffs[accountID].Add(time.Nanosecond)
	}
	if r.requests == nil {
		r.requests = make(map[int64]map[*requestScope]struct{})
	}
	if r.requests[accountID] == nil {
		r.requests[accountID] = make(map[*requestScope]struct{})
	}
	r.requests[accountID][scope] = struct{}{}
	r.mu.Unlock()

	scope.onStop(func() { r.remove(scope) })
	request, cancel := context.WithCancelCause(ctx)
	scope.bind(func(cause error) {
		if cause != nil {
			cancel(cause)
		}
	})
	if parent != nil {
		scope.onStop(parent.bind(func(cause error) { scope.stop(cause) }))
	}
	return context.WithValue(request, scopeKey{}, scope), sync.OnceFunc(func() { scope.stop(releaseCause) })
}

func hasAccount(scope *requestScope, accountID int64) bool {
	for ; scope != nil; scope = scope.parent {
		if scope.accountID == accountID {
			return true
		}
	}
	return false
}

func scopeFrom(ctx context.Context) *requestScope {
	if ctx == nil {
		return nil
	}
	scope, _ := ctx.Value(scopeKey{}).(*requestScope)
	return scope
}

func (r *Registry) remove(scope *requestScope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.requests[scope.accountID], scope)
	if len(r.requests[scope.accountID]) == 0 {
		delete(r.requests, scope.accountID)
	}
}

// Cancel synchronously interrupts registrations at or before cutoff. Cutoffs
// are per-account high-water marks: duplicate or older events are ignored.
// The count is the number of matching registrations removed from this registry.
func (r *Registry) Cancel(accountID int64, cutoff time.Time) int {
	if accountID <= 0 || cutoff.IsZero() {
		return 0
	}
	return r.cancel(accountID, time.Time{}, cutoff.Round(0), true)
}

// CancelWindow synchronously interrupts only active registrations created in
// (after, before]. Unlike Cancel it does not use a cutoff high-water mark: an
// older or wider window can still contain requests outside a later window.
// Empty or reversed windows are no-ops. The count excludes scopes whose release
// wins the race with cancellation, including successfully completed Tracks.
func (r *Registry) CancelWindow(accountID int64, after, before time.Time) int {
	after, before = after.Round(0), before.Round(0)
	if accountID <= 0 || before.IsZero() || !before.After(after) {
		return 0
	}
	return r.cancel(accountID, after, before, false)
}

func (r *Registry) cancel(accountID int64, after, before time.Time, legacy bool) int {
	r.mu.Lock()
	if legacy {
		if previous, ok := r.cutoffs[accountID]; ok && !before.After(previous) {
			r.mu.Unlock()
			return 0
		}
		if r.cutoffs == nil {
			r.cutoffs = make(map[int64]time.Time)
		}
		r.cutoffs[accountID] = before
	}
	var targets []*requestScope
	for scope := range r.requests[accountID] {
		if !scope.started.After(before) && (legacy || scope.started.After(after)) {
			// A release may already own the scope while its registry cleanup is
			// pending. Never count or relabel that completed request.
			scope.mu.Lock()
			active := scope.stopped == nil
			scope.mu.Unlock()
			delete(r.requests[accountID], scope)
			if active {
				targets = append(targets, scope)
			}
		}
	}
	if len(r.requests[accountID]) == 0 {
		delete(r.requests, accountID)
	}
	r.mu.Unlock()
	count := 0
	for _, scope := range targets {
		if scope.stop(ErrQualityDegraded) {
			count++
		}
	}
	return count
}

// PreserveCancellation adds the parent's account control to detached while
// retaining detached's values, deadline and cancellation. Ordinary cancellation
// of parent is ignored. Without a Begin scope this returns detached unchanged
// and a no-op release. Release removes only this binding and cancels its child;
// it never cancels parent. Begin's release also clears all outstanding bindings,
// allowing request builders with an existing no-op release contract to use it.
func PreserveCancellation(parent, detached context.Context) (context.Context, func()) {
	scope := scopeFrom(parent)
	if scope == nil {
		return detached, func() {}
	}
	request, cancel := context.WithCancelCause(detached)
	stop := scope.bind(func(cause error) {
		if cause != nil {
			cancel(cause)
		}
	})
	return context.WithValue(request, scopeKey{}, scope), sync.OnceFunc(func() {
		stop()
		cancel(context.Canceled)
	})
}

// QualityDegraded also detects an interrupt after the client has canceled first,
// when context.Cause(ctx) can no longer change from context.Canceled.
func QualityDegraded(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if scope := scopeFrom(ctx); scope != nil {
		scope.mu.Lock()
		degraded := errors.Is(scope.cause, ErrQualityDegraded)
		scope.mu.Unlock()
		// The scope freezes its outcome at release. Walking ancestors or using
		// context.Cause here would relabel a completed child after its parent
		// is interrupted (ordinary context cancellation still propagates).
		return degraded
	}
	return errors.Is(context.Cause(ctx), ErrQualityDegraded)
}

func (s *requestScope) bind(cancel func(error)) func() {
	s.mu.Lock()
	if s.stopped != nil {
		cause := s.cause
		s.mu.Unlock()
		cancel(cause)
		return func() {}
	}
	b := &binding{cancel: cancel}
	if s.bindings == nil {
		s.bindings = make(map[*binding]struct{})
	}
	s.bindings[b] = struct{}{}
	s.mu.Unlock()
	return sync.OnceFunc(func() {
		s.mu.Lock()
		delete(s.bindings, b)
		s.mu.Unlock()
	})
}

func (s *requestScope) onStop(cleanup func()) {
	s.mu.Lock()
	if s.stopped != nil {
		s.mu.Unlock()
		cleanup()
		return
	}
	s.cleanups = append(s.cleanups, cleanup)
	s.mu.Unlock()
}

// stop freezes the outcome before fanout; concurrent callers join the winner.
// It reports whether that final outcome is a quality interrupt.
func (s *requestScope) stop(cause error) bool {
	s.mu.Lock()
	if s.stopped != nil {
		done := s.stopped
		s.mu.Unlock()
		<-done
		return errors.Is(s.cause, ErrQualityDegraded)
	}
	s.cause = cause
	s.stopped = make(chan struct{})
	defer close(s.stopped)
	bindings, cleanups := s.bindings, s.cleanups
	s.bindings, s.cleanups = nil, nil
	s.mu.Unlock()
	for b := range bindings {
		b.cancel(cause)
	}
	for _, cleanup := range cleanups {
		cleanup()
	}
	return errors.Is(cause, ErrQualityDegraded)
}

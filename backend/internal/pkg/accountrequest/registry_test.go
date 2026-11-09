package accountrequest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func assertActive(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("unexpected cancellation: %v", context.Cause(ctx))
	default:
	}
}

func assertCause(t *testing.T, ctx context.Context, cause error) {
	t.Helper()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("context not synchronously canceled")
	}
	if !errors.Is(ctx.Err(), context.Canceled) || !errors.Is(context.Cause(ctx), cause) {
		t.Fatalf("Err=%v Cause=%v, want canceled with %v", ctx.Err(), context.Cause(ctx), cause)
	}
}

func setStarted(ctx context.Context, started time.Time) {
	scopeFrom(ctx).started = started
}

func assertEmpty(t *testing.T, r *Registry) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) != 0 {
		t.Fatalf("registry retained %d account(s)", len(r.requests))
	}
}

func assertScopeReleased(t *testing.T, ctx context.Context) {
	t.Helper()
	scope := scopeFrom(ctx)
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if len(scope.bindings) != 0 || len(scope.cleanups) != 0 {
		t.Fatalf("scope retained %d bindings and %d cleanups", len(scope.bindings), len(scope.cleanups))
	}
}

func TestCancelStopsRealHTTPRequest(t *testing.T) {
	r := NewRegistry()
	ctx, release := r.Begin(context.Background(), 1)
	defer release()
	started, stopped := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(started)
		<-req.Context().Done()
		close(stopped)
	}))
	defer server.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		response, err := server.Client().Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP request did not start")
	}
	if got := r.Cancel(1, time.Now()); got != 1 {
		t.Fatalf("Cancel=%d, want 1", got)
	}
	assertCause(t, ctx, ErrQualityDegraded)
	select {
	case err := <-result:
		if !errors.Is(err, ErrQualityDegraded) {
			t.Fatalf("HTTP request error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP transport did not stop")
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream connection did not close")
	}
	assertEmpty(t, r)
}

func TestCancelAccountIsolationAndInclusiveCutoff(t *testing.T) {
	r := NewRegistry()
	base := time.Now().Add(-time.Minute).Round(0)
	old, releaseOld := r.Begin(context.Background(), 1)
	edge, releaseEdge := r.Begin(context.Background(), 1)
	newer, releaseNew := r.Begin(context.Background(), 1)
	other, releaseOther := r.Begin(context.Background(), 2)
	defer releaseOld()
	defer releaseEdge()
	defer releaseNew()
	defer releaseOther()
	setStarted(old, base)
	setStarted(edge, base.Add(time.Second))
	setStarted(newer, base.Add(time.Second+time.Nanosecond))
	setStarted(other, base)
	if got := r.Cancel(1, base.Add(time.Second)); got != 2 {
		t.Fatalf("Cancel=%d, want 2", got)
	}
	assertCause(t, old, ErrQualityDegraded)
	assertCause(t, edge, ErrQualityDegraded)
	assertActive(t, newer)
	assertActive(t, other)
	if r.Cancel(1, base) != 0 || r.Cancel(1, base.Add(time.Second)) != 0 {
		t.Fatal("duplicate or reordered event canceled requests")
	}
	if r.Cancel(1, base.Add(2*time.Second)) != 1 {
		t.Fatal("later transition did not cancel remaining old work")
	}
	assertCause(t, newer, ErrQualityDegraded)
	assertActive(t, other)
}

func TestReleaseAndNonpositiveAccounts(t *testing.T) {
	var r Registry
	if Default == nil {
		t.Fatal("Default is nil")
	}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, id := range []int64{0, -1} {
		ctx, release := r.Begin(parent, id)
		release()
		release()
		if ctx != parent || r.Cancel(id, time.Now()) != 0 {
			t.Fatal("nonpositive account must be a no-op")
		}
		if n, err := r.InterruptAt(parent, id, time.Now()); n != 0 || err != nil {
			t.Fatalf("nonpositive interrupt: %d, %v", n, err)
		}
	}
	ctx, release := r.Begin(parent, 1)
	detached, cleanup := PreserveCancellation(ctx, context.WithoutCancel(ctx))
	defer cleanup()
	var group sync.WaitGroup
	for range 16 {
		group.Go(release)
	}
	group.Wait()
	assertCause(t, ctx, context.Canceled)
	assertCause(t, detached, context.Canceled)
	assertActive(t, parent)
	assertEmpty(t, &r)
	assertScopeReleased(t, ctx)
	if r.Cancel(1, time.Now()) != 0 || QualityDegraded(ctx) {
		t.Fatal("completed request counted as a quality interrupt")
	}
}

func TestNestedBeginCannotEscapeCutoff(t *testing.T) {
	r := NewRegistry()
	root, releaseRoot := r.Begin(context.Background(), 1)
	defer releaseRoot()
	cutoff := time.Now().Add(-time.Minute)
	setStarted(root, cutoff)
	child, releaseChild := r.Begin(context.WithoutCancel(root), 1)
	defer releaseChild()
	if !scopeFrom(child).started.Equal(cutoff) || r.Cancel(1, cutoff) != 2 {
		t.Fatal("nested request escaped original registration cutoff")
	}
	assertCause(t, root, ErrQualityDegraded)
	assertCause(t, child, ErrQualityDegraded)
	late, releaseLate := r.Begin(context.WithoutCancel(child), 1)
	defer releaseLate()
	assertCause(t, late, ErrQualityDegraded)
	assertEmpty(t, r)
}

func TestNestedBeginAcrossRegistriesAndAccounts(t *testing.T) {
	r1, r2 := NewRegistry(), NewRegistry()
	root, finishRoot := r1.Begin(context.Background(), 1)
	defer finishRoot()
	middle, finishMiddle := r2.Begin(context.WithoutCancel(root), 2)
	defer finishMiddle()
	child, finishChild := r2.Begin(context.WithoutCancel(middle), 1)
	defer finishChild()
	if !scopeFrom(child).started.Equal(scopeFrom(root).started) {
		t.Fatal("same account ancestor registration not inherited")
	}
	r1.Cancel(1, time.Now())
	assertCause(t, child, ErrQualityDegraded)
	assertEmpty(t, r1)
	assertEmpty(t, r2)
}

func TestPreserveIgnoresClientCancellation(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "detach_before_disconnect", true: "detach_after_disconnect"}[cancelFirst], func(t *testing.T) {
			r := NewRegistry()
			client, cancelClient := context.WithCancel(context.Background())
			defer cancelClient()
			ctx, finish := r.Begin(client, 1)
			defer finish()
			if cancelFirst {
				cancelClient()
			}
			detached, cleanup := PreserveCancellation(ctx, context.WithoutCancel(ctx))
			defer cleanup()
			cancelClient()
			assertCause(t, ctx, context.Canceled)
			assertActive(t, detached)
			if QualityDegraded(ctx) {
				t.Fatal("client disconnect counted as quality interrupt")
			}
			if r.Cancel(1, time.Now()) != 1 {
				t.Fatal("client disconnect unregistered active detached work")
			}
			assertCause(t, detached, ErrQualityDegraded)
			assertCause(t, ctx, context.Canceled)
			if !QualityDegraded(ctx) || !QualityDegraded(detached) {
				t.Fatal("quality interrupt hidden by earlier client disconnect")
			}
			late, cleanupLate := PreserveCancellation(ctx, context.WithoutCancel(ctx))
			defer cleanupLate()
			assertCause(t, late, ErrQualityDegraded)
			assertScopeReleased(t, ctx)
		})
	}
}

func TestPreserveReleaseAndDetachedSemantics(t *testing.T) {
	r := NewRegistry()
	parent, finish := r.Begin(context.Background(), 1)
	defer finish()
	type valueKey struct{}
	base, cancelBase := context.WithTimeout(context.WithValue(context.Background(), valueKey{}, "detached"), time.Minute)
	defer cancelBase()
	first, releaseFirst := PreserveCancellation(parent, base)
	second, releaseSecond := PreserveCancellation(parent, context.WithoutCancel(first))
	defer releaseSecond()
	if first.Value(valueKey{}) != "detached" {
		t.Fatal("detached values lost")
	}
	if got, ok := first.Deadline(); !ok || !got.Equal(deadlineOf(base)) {
		t.Fatal("detached deadline lost")
	}
	releaseFirst()
	releaseFirst()
	assertCause(t, first, context.Canceled)
	assertActive(t, parent)
	assertActive(t, second)
	r.Cancel(1, time.Now())
	assertCause(t, second, ErrQualityDegraded)
	plain, releasePlain := PreserveCancellation(context.Background(), base)
	releasePlain()
	if plain != base {
		t.Fatal("no-scope Preserve must return original detached context")
	}
	assertActive(t, base)
	if QualityDegraded(nil) || QualityDegraded(base) {
		t.Fatal("unrelated context counted as quality degraded")
	}
}

func deadlineOf(ctx context.Context) time.Time {
	deadline, _ := ctx.Deadline()
	return deadline
}

func TestPreserveHonorsDetachedCancellation(t *testing.T) {
	r := NewRegistry()
	parent, finish := r.Begin(context.Background(), 1)
	defer finish()
	base, cancel := context.WithCancelCause(context.Background())
	child, cleanup := PreserveCancellation(parent, base)
	defer cleanup()
	cause := errors.New("detached transport ended")
	cancel(cause)
	assertCause(t, child, cause)
	assertActive(t, parent)
	cleanup()
	scope := scopeFrom(parent)
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if len(scope.bindings) != 1 { // Only the original Begin context remains.
		t.Fatalf("released detached binding retained: %d", len(scope.bindings))
	}
}

func TestEventDedupAndCoarseClockAdmission(t *testing.T) {
	r := NewRegistry()
	old, finishOld := r.Begin(context.Background(), 1)
	defer finishOld()
	// A future timestamp deterministically simulates an unchanged/backwards wall
	// clock when a root Begin follows an already processed event.
	cutoff := time.Now().Add(time.Hour).Round(0)
	if r.Cancel(1, cutoff) != 1 {
		t.Fatal("initial interrupt did not cancel old request")
	}
	assertCause(t, old, ErrQualityDegraded)
	newer, finishNew := r.Begin(context.Background(), 1)
	defer finishNew()
	if !scopeFrom(newer).started.After(cutoff) {
		t.Fatal("new root not ordered after processed event")
	}
	if r.Cancel(1, cutoff) != 0 || r.Cancel(1, cutoff.Add(-time.Second)) != 0 {
		t.Fatal("duplicate/reordered event canceled a later request")
	}
	assertActive(t, newer)
	if r.Cancel(1, cutoff.Add(time.Second)) != 1 {
		t.Fatal("newer transition should still cancel the request")
	}
}

func TestConcurrentBeginCancelPreserveRelease(t *testing.T) {
	r := NewRegistry()
	var group sync.WaitGroup
	for worker := range 12 {
		group.Go(func() {
			for range 100 {
				ctx, release := r.Begin(context.Background(), int64(worker%3+1))
				detached, cleanup := PreserveCancellation(ctx, context.WithoutCancel(ctx))
				group.Go(func() { r.Cancel(int64(worker%3+1), time.Now()) })
				group.Go(cleanup)
				release()
				cleanup()
				if detached.Err() == nil {
					t.Error("finished scope left detached context active")
				}
			}
		})
	}
	group.Wait()
	assertEmpty(t, r)
}

func TestReleaseJoinsConcurrentCancellation(t *testing.T) {
	r := NewRegistry()
	ctx, release := r.Begin(context.Background(), 1)
	detached, cleanup := PreserveCancellation(ctx, context.WithoutCancel(ctx))
	defer cleanup()
	entered, proceed := make(chan struct{}), make(chan struct{})
	scopeFrom(ctx).bind(func(error) {
		close(entered)
		<-proceed
	})
	var group sync.WaitGroup
	group.Go(func() { r.Cancel(1, time.Now()) })
	<-entered
	released := make(chan struct{})
	group.Go(func() {
		release()
		close(released)
	})
	select {
	case <-released:
		t.Error("release returned before concurrent cancellation completed")
	case <-time.After(10 * time.Millisecond):
	}
	close(proceed)
	group.Wait()
	assertCause(t, ctx, ErrQualityDegraded)
	assertCause(t, detached, ErrQualityDegraded)
	assertScopeReleased(t, ctx)
	assertEmpty(t, r)
}

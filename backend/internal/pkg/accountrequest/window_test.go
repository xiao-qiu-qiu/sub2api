package accountrequest

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestCancelWindowBoundariesAndAccountIsolation(t *testing.T) {
	r := NewRegistry()
	after := time.Now().Add(-time.Minute).Round(0)
	before := after.Add(time.Second)
	for _, test := range []struct {
		name      string
		accountID int64
		started   time.Time
		cancel    bool
	}{
		{"older", 1, after.Add(-time.Nanosecond), false},
		{"exclusive_after", 1, after, false},
		{"inside", 1, after.Add(time.Nanosecond), true},
		{"inclusive_before", 1, before, true},
		{"newer", 1, before.Add(time.Nanosecond), false},
		{"other_account", 2, before, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, finish := r.Track(WithStartedAt(context.Background(), test.started), test.accountID)
			defer finish()
			want := 0
			if test.cancel {
				want = 1
			}
			if got := r.CancelWindow(1, after, before); got != want {
				t.Fatalf("CancelWindow=%d, want %d", got, want)
			}
			if QualityDegraded(ctx) != test.cancel {
				t.Fatalf("QualityDegraded=%v, want %v", QualityDegraded(ctx), test.cancel)
			}
			if test.cancel {
				assertCause(t, ctx, ErrQualityDegraded)
			} else {
				assertActive(t, ctx)
			}
			if got := StartedAt(ctx); !got.Equal(test.started) {
				t.Fatalf("StartedAt=%v, want %v", got, test.started)
			}
		})
	}
	assertEmpty(t, r)
}

func TestCancelWindowDoesNotDeduplicateByUpperBound(t *testing.T) {
	r := NewRegistry()
	base := time.Now().Add(-time.Minute).Round(0)
	var requests []context.Context
	for _, offset := range []time.Duration{1, 3, 5, 7} {
		ctx, finish := r.Track(WithStartedAt(context.Background(), base.Add(offset*time.Second)), 1)
		defer finish()
		requests = append(requests, ctx)
	}
	for _, window := range []struct {
		after, before time.Duration
		index         int
	}{
		{4, 6, 2}, // First process a later window.
		{2, 4, 1}, // An older upper bound still contains valid requests.
		{0, 6, 0}, // The same upper bound with a wider window is also valid.
		{6, 8, 3},
	} {
		if got := r.CancelWindow(1, base.Add(window.after*time.Second), base.Add(window.before*time.Second)); got != 1 {
			t.Fatalf("window (%v,%v] count=%d", window.after, window.before, got)
		}
		assertCause(t, requests[window.index], ErrQualityDegraded)
	}
	if got := r.CancelWindow(1, base, base.Add(8*time.Second)); got != 0 {
		t.Fatalf("duplicate window count=%d", got)
	}
	assertEmpty(t, r)
}

func TestStartedAtPreservesBusinessTimeAndNestedScopes(t *testing.T) {
	r1, r2 := NewRegistry(), NewRegistry()
	created := time.Now().Add(-time.Minute).Round(0)
	future := time.Now().Add(time.Hour).Round(0)
	r1.Cancel(1, future) // Legacy clock admission must not rewrite explicit time.
	plain := WithStartedAt(context.Background(), created)
	if StartedAt(plain) != created || !StartedAt(nil).IsZero() || !StartedAt(context.Background()).IsZero() { //nolint:staticcheck // SA1012: 验证 StartedAt 对 nil 上下文的兼容契约。
		t.Fatal("injected or absent creation time is incorrect")
	}
	root, finishRoot := r1.Track(plain, 1)
	defer finishRoot()
	child, finishChild := r1.Begin(WithStartedAt(context.WithoutCancel(root), future), 1)
	defer finishChild()
	middle, finishMiddle := r2.Begin(context.WithoutCancel(root), 2)
	defer finishMiddle()
	grandchild, finishGrandchild := r2.Track(context.WithoutCancel(middle), 1)
	defer finishGrandchild()
	detached, cleanup := PreserveCancellation(child, WithStartedAt(context.Background(), future))
	defer cleanup()
	for _, ctx := range []context.Context{root, child, grandchild, detached, context.WithoutCancel(detached)} {
		if StartedAt(ctx) != created {
			t.Fatalf("business creation time rewritten: %v, want %v", StartedAt(ctx), created)
		}
	}
	if got := r1.CancelWindow(1, created.Add(-time.Nanosecond), created); got != 2 {
		t.Fatalf("window suppressed by legacy cutoff or nested time: %d", got)
	}
	for _, ctx := range []context.Context{root, child, middle, grandchild, detached} {
		assertCause(t, ctx, ErrQualityDegraded)
		if !QualityDegraded(ctx) {
			t.Fatal("propagated interruption not recorded")
		}
	}
	late, finishLate := r2.Begin(context.WithoutCancel(grandchild), 1)
	defer finishLate()
	assertCause(t, late, ErrQualityDegraded)
	if StartedAt(late) != created || !QualityDegraded(late) {
		t.Fatal("late nested scope lost original request outcome")
	}
	assertEmpty(t, r1)
	assertEmpty(t, r2)
}

func TestIndependentWebSocketTurnScopes(t *testing.T) {
	r := NewRegistry()
	session, closeSession := context.WithCancel(context.Background())
	defer closeSession()
	firstStarted := time.Now().Add(-time.Minute).Round(0)
	first, finishFirst := r.Track(WithStartedAt(session, firstStarted), 1)
	defer finishFirst()
	read, finishRead := r.Begin(context.WithoutCancel(first), 1)
	defer finishRead()
	secondStarted := firstStarted.Add(time.Second)
	second, finishSecond := r.Track(WithStartedAt(session, secondStarted), 1)
	defer finishSecond()
	if got := r.CancelWindow(1, firstStarted.Add(-time.Nanosecond), firstStarted); got != 2 {
		t.Fatalf("first turn interruption count=%d", got)
	}
	assertCause(t, first, ErrQualityDegraded)
	assertCause(t, read, ErrQualityDegraded)
	assertActive(t, session)
	assertActive(t, second)
	if StartedAt(read) != firstStarted || StartedAt(second) != secondStarted {
		t.Fatal("independent turns did not retain their own creation times")
	}
	finishSecond()
	if got := r.CancelWindow(1, firstStarted, secondStarted); got != 0 || QualityDegraded(second) {
		t.Fatalf("completed second turn marked interrupted: count=%d", got)
	}
	assertActive(t, second)
	assertEmpty(t, r)
}

func TestCompletedScopeDoesNotInheritLaterInterruption(t *testing.T) {
	for _, withoutCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "context_parent", true: "detached_parent"}[withoutCancel], func(t *testing.T) {
			r := NewRegistry()
			started := time.Now().Add(-time.Second).Round(0)
			parent, finishParent := r.Begin(WithStartedAt(context.Background(), started), 1)
			defer finishParent()
			base := parent
			if withoutCancel {
				base = context.WithoutCancel(parent)
			}
			child, complete := r.Track(base, 1)
			complete()
			released, release := r.Begin(context.WithoutCancel(parent), 1)
			release()
			if got := r.CancelWindow(1, started.Add(-time.Nanosecond), started); got != 1 {
				t.Fatalf("completed scopes counted: %d", got)
			}
			if QualityDegraded(child) || QualityDegraded(released) {
				t.Fatal("completed child inherited parent's later interruption")
			}
			if withoutCancel {
				assertActive(t, child)
			} else {
				assertCause(t, child, ErrQualityDegraded) // Native context propagation only.
			}
			assertCause(t, released, context.Canceled)
			assertEmpty(t, r)
		})
	}
}

func TestWindowReleaseAndCancellationJoinWinner(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "completion_wins", true: "cancellation_wins"}[cancelFirst], func(t *testing.T) {
			r := NewRegistry()
			started := time.Now().Round(0)
			ctx, finish := r.Track(WithStartedAt(context.Background(), started), 1)
			defer finish()
			entered, proceed := make(chan struct{}), make(chan struct{})
			scopeFrom(ctx).bind(func(error) { close(entered); <-proceed })
			interrupt := func() int { return r.CancelWindow(1, started.Add(-time.Nanosecond), started) }
			count := make(chan int, 1)
			finished := make(chan struct{})
			if cancelFirst {
				go func() { count <- interrupt() }()
				<-entered
				go func() { finish(); close(finished) }()
				select {
				case <-finished:
					t.Error("Track completion did not join cancellation")
				case <-time.After(10 * time.Millisecond):
				}
			} else {
				go func() { finish(); close(finished) }()
				<-entered
				count <- interrupt() // Completion's registry cleanup has not run yet.
			}
			close(proceed)
			<-finished
			got := <-count
			want := 0
			if cancelFirst {
				want = 1
				assertCause(t, ctx, ErrQualityDegraded)
			} else {
				assertActive(t, ctx)
			}
			if got != want || QualityDegraded(ctx) != cancelFirst {
				t.Fatalf("count=%d quality=%v, want %d/%v", got, QualityDegraded(ctx), want, cancelFirst)
			}
			assertEmpty(t, r)
			assertScopeReleased(t, ctx)
		})
	}
}

func TestConcurrentWindowReleaseOutcome(t *testing.T) {
	for _, track := range []bool{false, true} {
		t.Run(map[bool]string{false: "Begin", true: "Track"}[track], func(t *testing.T) {
			r := NewRegistry()
			started := time.Now().Round(0)
			for range 300 {
				begin := r.Begin
				if track {
					begin = r.Track
				}
				ctx, finish := begin(WithStartedAt(context.Background(), started), 1)
				ready := make(chan struct{})
				count := make(chan int, 1)
				var group sync.WaitGroup
				group.Go(func() { <-ready; finish() })
				group.Go(func() { <-ready; count <- r.CancelWindow(1, started.Add(-time.Nanosecond), started) })
				group.Go(func() { <-ready; _ = QualityDegraded(ctx); _ = StartedAt(ctx) })
				close(ready)
				group.Wait()
				got := <-count
				if (got == 1) != QualityDegraded(ctx) || got < 0 || got > 1 {
					t.Fatalf("count=%d inconsistent with actual interruption=%v", got, QualityDegraded(ctx))
				}
				before := QualityDegraded(ctx)
				r.CancelWindow(1, started.Add(-time.Second), started.Add(time.Second))
				if QualityDegraded(ctx) != before {
					t.Fatal("released outcome changed")
				}
				assertScopeReleased(t, ctx)
			}
			assertEmpty(t, r)
		})
	}
}

type registrationBarrierContext struct {
	context.Context
	entered, proceed chan struct{}
	once             sync.Once
}

func (c *registrationBarrierContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered); <-c.proceed })
	return c.Context.Done()
}

func TestWindowCancellationDuringScopeInitialization(t *testing.T) {
	for _, track := range []bool{false, true} {
		t.Run(map[bool]string{false: "Begin", true: "Track"}[track], func(t *testing.T) {
			r := NewRegistry()
			started := time.Now().Round(0)
			base := &registrationBarrierContext{
				Context: WithStartedAt(context.Background(), started),
				entered: make(chan struct{}), proceed: make(chan struct{}),
			}
			result := make(chan context.Context, 1)
			go func() {
				begin := r.Begin
				if track {
					begin = r.Track
				}
				ctx, finish := begin(base, 1)
				defer finish()
				result <- ctx
			}()
			<-base.entered // Registered, but the context binding is not installed yet.
			if got := r.CancelWindow(1, started.Add(-time.Nanosecond), started); got != 1 {
				t.Fatalf("initializing registration missed: %d", got)
			}
			close(base.proceed)
			ctx := <-result
			assertCause(t, ctx, ErrQualityDegraded)
			if !QualityDegraded(ctx) || StartedAt(ctx) != started {
				t.Fatal("initializing scope lost interruption or creation time")
			}
			assertEmpty(t, r)
		})
	}
}

func TestInvalidWindowsAreNoops(t *testing.T) {
	r := NewRegistry()
	started := time.Now().Round(0)
	ctx, finish := r.Track(WithStartedAt(context.Background(), started), 1)
	defer finish()
	for _, test := range []struct {
		id            int64
		after, before time.Time
	}{
		{0, started.Add(-time.Second), started},
		{-1, started.Add(-time.Second), started},
		{1, started, started},
		{1, started.Add(time.Second), started},
		{1, time.Time{}, time.Time{}},
	} {
		if got := r.CancelWindow(test.id, test.after, test.before); got != 0 {
			t.Fatalf("invalid window canceled %d requests", got)
		}
		if got, err := r.InterruptWindow(context.Background(), test.id, test.after, test.before); got != 0 || err != nil {
			t.Fatalf("invalid InterruptWindow=(%d,%v)", got, err)
		}
		assertActive(t, ctx)
	}
	if got, err := r.InterruptWindow(context.Background(), 1, time.Time{}, started); got != 1 || err != nil {
		t.Fatalf("zero lower bound should be supported: (%d,%v)", got, err)
	}
	assertCause(t, ctx, ErrQualityDegraded)
}

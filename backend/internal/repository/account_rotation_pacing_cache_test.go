package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type accountRotationPacingFixture struct {
	*accountRotationCacheFixture
	nowMS int64
}

func newAccountRotationPacingFixture(t *testing.T) *accountRotationPacingFixture {
	t.Helper()
	f := &accountRotationPacingFixture{accountRotationCacheFixture: newAccountRotationCacheFixture(t)}
	f.nowMS = f.now*1000 + 123
	f.server.SetTime(time.UnixMilli(f.nowMS))
	f.policy.RecoveryMinIntervalMS, f.policy.RecoveryMaxIntervalMS = 137, 137
	return f
}

func (f *accountRotationPacingFixture) advanceMS(milliseconds int64) {
	f.t.Helper()
	require.GreaterOrEqual(f.t, milliseconds, int64(0))
	f.server.FastForward(time.Duration(milliseconds) * time.Millisecond)
	f.nowMS += milliseconds
	f.now = f.nowMS / 1000
	f.server.SetTime(time.UnixMilli(f.nowMS))
}

func (f *accountRotationPacingFixture) acquireState(id int64, request string, limit int, sticky bool) (bool, *service.AccountRotationState) {
	f.t.Helper()
	ok, state, err := f.cache.AcquireAccountSlotWithRotationState(f.t.Context(), id, limit, request, f.policy, sticky, "test-pool")
	require.NoError(f.t, err)
	require.NotNil(f.t, state)
	return ok, state
}

func (f *accountRotationPacingFixture) completeRest(id int64, requests ...string) *service.AccountRotationState {
	f.t.Helper()
	held := f.hold(id, 1)
	require.True(f.t, held.Hold)
	require.Equal(f.t, "resting", held.Phase)
	for _, request := range requests {
		ok, state := f.acquireState(id, request, 0, false)
		require.False(f.t, ok)
		require.True(f.t, state.Hold)
	}
	f.advanceMS(held.RestUntil*1000 - f.nowMS)
	state := f.check(id)
	require.False(f.t, state.Hold)
	require.Equal(f.t, len(requests) > 0, state.Recovering)
	require.Zero(f.t, state.RetryAfterMS, "the first recovery admission has no delay")
	return state
}

func TestAccountRotationRecoveryFixedMillisecondGateAndAtomicWaiters(t *testing.T) {
	f := newAccountRotationPacingFixture(t)
	state := f.completeRest(1, "first", "second")
	require.Equal(t, 2, state.WaitingCount)
	f.advanceMS(321) // Start between Redis second boundaries.
	ok, state := f.acquireState(1, "first", 0, false)
	require.True(t, ok)
	require.True(t, state.Recovering)
	require.False(t, state.Hold, "pacing is independent of an actual rest hold")
	require.Empty(t, state.Reason)
	require.Equal(t, int64(137), state.RetryAfterMS)
	require.Equal(t, 1, state.WaitingCount)
	require.Equal(t, []string{"second"}, f.client.ZRange(t.Context(), accountRotationWaiterKey(1), 0, -1).Val())

	for _, sticky := range []bool{false, true} {
		for range 2 {
			ok, state = f.acquireState(1, fmt.Sprintf("new-%t", sticky), 0, sticky)
			require.False(t, ok, "same-account newcomers cannot bypass recovery with spare capacity or sticky proof")
			require.False(t, state.Hold)
			require.True(t, state.Recovering)
			require.Equal(t, int64(137), state.RetryAfterMS)
		}
	}
	require.Equal(t, 3, state.WaitingCount, "retries renew one waiter instead of registering duplicates")
	require.Equal(t, int64(1), f.client.ZCard(t.Context(), accountSlotKey(1)).Val())
	for _, request := range []string{"other-first", "other-second"} {
		ok, other := f.acquireState(2, request, 0, false)
		require.True(t, ok, "another account in the same pool is not paced")
		require.False(t, other.Hold)
		require.False(t, other.Recovering)
	}

	f.advanceMS(136)
	ok, state = f.acquireState(1, "second", 0, false)
	require.False(t, ok)
	require.Equal(t, int64(1), state.RetryAfterMS)
	f.advanceMS(1)
	for i, request := range []string{"second", "new-false", "new-true"} {
		ok, state = f.acquireState(1, request, 0, false)
		require.True(t, ok, "admission is available at the exact millisecond deadline")
		require.Equal(t, 2-i, state.WaitingCount)
		require.True(t, state.Recovering)
		require.Equal(t, int64(137), state.RetryAfterMS)
		if i < 2 {
			f.advanceMS(137)
		}
	}
	require.False(t, f.server.Exists(accountRotationWaiterKey(1)))
	f.advanceMS(136)
	state = f.check(1)
	require.True(t, state.Recovering, "the last admission retains its delay even when the queue is empty")
	require.Equal(t, int64(1), state.RetryAfterMS)
	f.advanceMS(1)
	state = f.check(1)
	require.False(t, state.Recovering)
	require.Zero(t, state.RetryAfterMS)
	require.True(t, f.acquire(1, "normal-one", 0, false))
	require.True(t, f.acquire(1, "normal-two", 0, false), "ordinary admission resumes without an account-wide throttle")
}

func TestAccountRotationRecoveryTailRegistersNewcomers(t *testing.T) {
	f := newAccountRotationPacingFixture(t)
	f.completeRest(1, "last")
	ok, state := f.acquireState(1, "last", 0, false)
	require.True(t, ok)
	require.Zero(t, state.WaitingCount)
	f.advanceMS(136)
	ok, state = f.acquireState(1, "newcomer", 0, true)
	require.False(t, ok, "a newcomer cannot arrive immediately behind the final waiter")
	require.Equal(t, int64(1), state.RetryAfterMS)
	require.Equal(t, 1, state.WaitingCount)
	f.advanceMS(1)
	ok, state = f.acquireState(1, "newcomer", 0, true)
	require.True(t, ok)
	require.True(t, state.Recovering)
	require.Zero(t, state.WaitingCount)
	require.Equal(t, int64(137), state.RetryAfterMS)
	f.advanceMS(137)
	require.False(t, f.check(1).Recovering)
}

func TestAccountRotationRecoveryRandomIntervals(t *testing.T) {
	for _, test := range []struct {
		name      string
		min, max  int
		low, high int64
	}{
		{name: "zero-pair-defaults", low: 500, high: 2000},
		{name: "configured", min: 41, max: 89, low: 41, high: 89},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAccountRotationPacingFixture(t)
			f.policy.RecoveryMinIntervalMS, f.policy.RecoveryMaxIntervalMS = test.min, test.max
			// Even maximum default intervals must finish before unrefreshed waiters expire.
			const samples = 24
			requests := make([]string, samples)
			for i := range requests {
				requests[i] = fmt.Sprintf("queued-%d", i)
			}
			f.completeRest(1, requests...)
			intervals := make(map[int64]bool)
			for i, request := range requests {
				ok, state := f.acquireState(1, request, 0, false)
				require.True(t, ok)
				require.GreaterOrEqual(t, state.RetryAfterMS, test.low)
				require.LessOrEqual(t, state.RetryAfterMS, test.high)
				require.Equal(t, samples-i-1, state.WaitingCount)
				intervals[state.RetryAfterMS] = true
				require.Equal(t, state, f.check(1), "observing a sampled interval cannot resample it")
				f.advanceMS(state.RetryAfterMS)
			}
			require.Greater(t, len(intervals), 1, "successive admissions must sample delays rather than reuse a constant")
			require.False(t, f.check(1).Recovering)
		})
	}
}

func TestAccountRotationRecoveryCapacityAndExistingRetriesDoNotConsumeDelay(t *testing.T) {
	f := newAccountRotationPacingFixture(t)
	f.completeRest(1, "first", "second")
	ok, state := f.acquireState(1, "first", 1, false)
	require.True(t, ok)
	f.advanceMS(50)
	ok, state = f.acquireState(1, "first", 1, false)
	require.True(t, ok, "an existing request ID remains idempotent even at capacity during recovery")
	require.Equal(t, int64(87), state.RetryAfterMS)
	require.Equal(t, 1, state.WaitingCount)
	require.Equal(t, int64(1), f.client.ZCard(t.Context(), accountSlotKey(1)).Val())
	f.advanceMS(87)
	ok, state = f.acquireState(1, "second", 1, false)
	require.False(t, ok)
	require.True(t, state.Recovering)
	require.False(t, state.Hold)
	require.Zero(t, state.RetryAfterMS, "a capacity rejection cannot consume an available pacing slot")
	ok, state = f.acquireState(1, "new-at-capacity", 1, true)
	require.False(t, ok)
	require.Equal(t, 2, state.WaitingCount, "capacity denials during recovery must register newcomers atomically")
	f.advanceMS(411)
	ok, state = f.acquireState(1, "first", 1, true)
	require.True(t, ok)
	require.Zero(t, state.RetryAfterMS, "retrying an existing slot cannot reserve the next delay")
	f.release(1, "first")
	ok, state = f.acquireState(1, "second", 1, false)
	require.True(t, ok, "capacity becoming free permits immediate admission after the elapsed interval")
	require.Equal(t, int64(137), state.RetryAfterMS, "the next delay starts at this actual admission, without catch-up bursts")
	f.release(1, "second")
	ok, state = f.acquireState(1, "new-at-capacity", 1, false)
	require.False(t, ok)
	require.Equal(t, int64(137), state.RetryAfterMS)
	f.advanceMS(137)
	require.True(t, f.acquire(1, "new-at-capacity", 1, false))
}

func TestAccountRotationRecoveryCancelledWaitersKeepOnlyRemainingTail(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		t.Run(fmt.Sprintf("admitted=%t", admitted), func(t *testing.T) {
			f := newAccountRotationPacingFixture(t)
			f.completeRest(1, "first", "canceled")
			if admitted {
				require.True(t, f.acquire(1, "first", 0, false))
			}
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			for _, request := range []string{"first", "canceled", "canceled"} {
				require.NoError(t, f.cache.RemoveAccountRotationWaiter(canceled, 1, request))
			}
			state := f.check(1)
			require.Zero(t, state.WaitingCount)
			require.False(t, f.server.Exists(accountRotationWaiterKey(1)))
			if admitted {
				require.True(t, state.Recovering, "cancellation cannot erase the final successful admission's delay")
				f.advanceMS(136)
				require.Equal(t, int64(1), f.check(1).RetryAfterMS)
				f.advanceMS(1)
			}
			require.False(t, f.check(1).Recovering, "an empty queue cannot leave permanent pacing state")
			require.True(t, f.acquire(1, "normal-one", 0, false))
			require.True(t, f.acquire(1, "normal-two", 0, false))
		})
	}
}

func TestAccountRotationRecoveryWaiterExpiryIsIndependent(t *testing.T) {
	f := newAccountRotationPacingFixture(t)
	f.completeRest(1, "first", "stale", "live")
	require.True(t, f.acquire(1, "first", 0, false))
	f.advanceMS(20_000)
	require.NoError(t, f.cache.RefreshAccountRotationWaiter(t.Context(), 1, "live"))
	f.advanceMS(40_000)
	state := f.check(1)
	require.Equal(t, 1, state.WaitingCount)
	require.True(t, state.Recovering, "one expired waiter cannot end recovery while another lease is live")
	require.Zero(t, state.RetryAfterMS)
	require.Equal(t, []string{"live"}, f.client.ZRange(t.Context(), accountRotationWaiterKey(1), 0, -1).Val())
	f.advanceMS(20_000)
	state = f.check(1)
	require.Zero(t, state.WaitingCount)
	require.False(t, state.Recovering)
	require.Zero(t, state.RetryAfterMS)
	require.False(t, f.server.Exists(accountRotationWaiterKey(1)))
	require.True(t, f.acquire(1, "after-expiry-one", 0, false))
	require.True(t, f.acquire(1, "after-expiry-two", 0, false))
}

func TestAccountRotationRecoveryRequiresLiveBacklog(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%t", stale), func(t *testing.T) {
			f := newAccountRotationPacingFixture(t)
			held := f.hold(1, 120)
			if stale {
				require.False(t, f.acquire(1, "crashed", 0, false))
			}
			f.advanceMS(held.RestUntil*1000 - f.nowMS)
			state := f.check(1)
			require.False(t, state.Hold)
			require.False(t, state.Recovering, "completed rest without live waiters must resume ordinary admission")
			require.Zero(t, state.WaitingCount)
			require.True(t, f.acquire(1, "one", 0, false))
			require.True(t, f.acquire(1, "two", 0, false))
		})
	}
	t.Run("ordinary-waiters", func(t *testing.T) {
		f := newAccountRotationPacingFixture(t)
		require.NoError(t, f.cache.RefreshAccountRotationWaiter(t.Context(), 1, "ordinary"))
		require.True(t, f.acquire(1, "one", 0, false))
		require.True(t, f.acquire(1, "two", 0, false))
		require.False(t, f.check(1).Recovering, "waiters alone do not start recovery before a rest or cancellation")
	})
}

func TestAccountRotationRecoverySurvivesPolicyChanges(t *testing.T) {
	f := newAccountRotationPacingFixture(t)
	f.completeRest(1, "first", "second", "third")
	require.True(t, f.acquire(1, "first", 0, false))
	f.advanceMS(37)
	f.policy.Revision = "recovery-new-settings"
	f.policy.Mode, f.policy.WorkSeconds, f.policy.EarlyRestEnabled = "sticky", 0, false
	f.policy.RestMinSeconds, f.policy.RestMaxSeconds = 0, 0
	f.policy.RecoveryMinIntervalMS, f.policy.RecoveryMaxIntervalMS = 701, 701
	state := f.check(1)
	require.False(t, state.Hold)
	require.True(t, state.Recovering, "disabling the automatic work cycle cannot erase recovery")
	require.Equal(t, int64(100), state.RetryAfterMS, "a settings revision cannot resample an already pending interval")
	ok, state := f.acquireState(1, "new-policy-request", 0, true)
	require.False(t, ok)
	require.Equal(t, 3, state.WaitingCount)
	f.advanceMS(99)
	require.False(t, f.acquire(1, "second", 0, false))
	f.advanceMS(1)
	ok, state = f.acquireState(1, "second", 0, false)
	require.True(t, ok)
	require.True(t, state.Recovering)
	require.False(t, state.Hold)
	require.Equal(t, int64(701), state.RetryAfterMS, "future admissions use the new interval without dropping the active gate")
	f.policy.Revision, f.policy.WorkSeconds = "automatic-reenabled", 900
	f.policy.RestMinSeconds, f.policy.RestMaxSeconds = 20, 20
	require.Equal(t, int64(701), f.check(1).RetryAfterMS)
	require.False(t, f.acquire(1, "third", 0, false))
}

func TestAccountRotationRecoveryRestartedManualHoldHidesPacingUntilCancel(t *testing.T) {
	for _, phase := range []string{"draining", "resting"} {
		t.Run(phase, func(t *testing.T) {
			f := newAccountRotationPacingFixture(t)
			f.completeRest(1, "first", "second", "third")
			require.True(t, f.acquire(1, "first", 0, false))
			f.advanceMS(37)
			require.Equal(t, int64(100), f.check(1).RetryAfterMS)

			state := f.hold(1, 60)
			require.Equal(t, "draining", state.Phase)
			require.True(t, state.Hold)
			require.False(t, state.Recovering, "starting another hold must hide the suspended recovery state immediately")
			require.Zero(t, state.RetryAfterMS)
			if phase == "resting" {
				f.release(1, "first")
			}
			state = f.check(1)
			require.Equal(t, phase, state.Phase)
			require.True(t, state.Hold)
			require.Equal(t, "manual", state.Reason)
			require.False(t, state.Recovering)
			require.Zero(t, state.RetryAfterMS)
			ok, blocked := f.acquireState(1, "second", 0, true)
			require.False(t, ok)
			require.True(t, blocked.Hold)
			require.False(t, blocked.Recovering, "denied admission must also report only the actual hold")
			require.Zero(t, blocked.RetryAfterMS)
			require.Equal(t, 2, blocked.WaitingCount)

			state = f.cancelHold(1)
			require.False(t, state.Hold)
			require.True(t, state.Recovering)
			require.Equal(t, int64(100), state.RetryAfterMS, "canceling the new hold restores the pending pacing deadline")
			f.advanceMS(99)
			ok, state = f.acquireState(1, "second", 0, false)
			require.False(t, ok)
			require.Equal(t, int64(1), state.RetryAfterMS)
			f.advanceMS(1)
			ok, state = f.acquireState(1, "second", 0, false)
			require.True(t, ok)
			require.True(t, state.Recovering)
			require.False(t, state.Hold)
			require.Equal(t, int64(137), state.RetryAfterMS)
			require.Equal(t, 1, state.WaitingCount)
			require.False(t, f.acquire(1, "third", 0, true), "canceling a restarted hold cannot release the remaining queue as a burst")
		})
	}
}

func TestAccountRotationRecoveryManualCancelStartsPacing(t *testing.T) {
	for _, draining := range []bool{false, true} {
		t.Run(fmt.Sprintf("draining=%t", draining), func(t *testing.T) {
			f := newAccountRotationPacingFixture(t)
			require.True(t, f.acquire(1, "running", 3, false))
			original := f.check(1)
			f.hold(1, 60)
			for _, request := range []string{"first", "second"} {
				require.False(t, f.acquire(1, request, 0, true))
			}
			if !draining {
				f.release(1, "running")
			}
			state := f.cancelHold(1)
			require.False(t, state.Hold)
			require.Empty(t, state.Reason)
			require.True(t, state.Recovering)
			require.Zero(t, state.RetryAfterMS)
			require.Equal(t, original.StartedAt, state.StartedAt)
			require.Equal(t, original.DrainAt, state.DrainAt)
			if draining {
				ok, retry := f.acquireState(1, "running", 3, true)
				require.True(t, ok)
				require.Zero(t, retry.RetryAfterMS, "the pre-hold member must not consume the first recovery admission")
			}
			ok, state := f.acquireState(1, "first", 3, false)
			require.True(t, ok)
			require.Equal(t, 1, state.WaitingCount)
			require.Equal(t, int64(137), state.RetryAfterMS)
			f.advanceMS(37)
			state = f.cancelHold(1)
			require.Equal(t, int64(100), state.RetryAfterMS, "repeated cancellation cannot reset the pacing clock")
			require.False(t, f.acquire(1, "second", 3, true))
			f.advanceMS(100)
			require.True(t, f.acquire(1, "second", 3, true))
		})
	}
}

func TestAccountRotationRecoveryAfterEarlyRest(t *testing.T) {
	f := newAccountRotationPacingFixture(t)
	f.policy.WorkSeconds, f.policy.EarlyRestEnabled = 1800, true
	require.True(t, f.acquire(1, "work", 3, false))
	f.advanceMS(600_000)
	state := f.check(1)
	require.Equal(t, "early", state.Reason)
	require.True(t, state.Hold)
	require.False(t, f.acquire(1, "queued", 0, true))
	f.release(1, "work")
	until := f.check(1).RestUntil
	for range 4 {
		f.advanceMS(30_000)
		require.NoError(t, f.cache.RefreshAccountRotationWaiter(t.Context(), 1, "queued"))
	}
	require.GreaterOrEqual(t, f.nowMS, until*1000)
	ok, state := f.acquireState(1, "queued", 3, false)
	require.True(t, ok)
	require.False(t, state.Hold)
	require.Empty(t, state.Reason)
	require.True(t, state.Recovering)
	require.Zero(t, state.WaitingCount)
	require.Equal(t, int64(137), state.RetryAfterMS)
	require.Equal(t, f.now+1800, state.DrainAt)
	require.False(t, f.acquire(1, "early-newcomer", 3, true))
	f.advanceMS(137)
	require.True(t, f.acquire(1, "early-newcomer", 3, true))
}

func TestAccountRotationRecoveryAfterScheduledRestWithWaiters(t *testing.T) {
	f := newAccountRotationPacingFixture(t)
	f.policy.WorkSeconds, f.policy.RestMinSeconds, f.policy.RestMaxSeconds = 1, 1, 1
	require.True(t, f.acquire(1, "work", 2, false))
	f.advanceMS(1000)
	f.release(1, "work")
	state := f.check(1)
	require.Equal(t, "scheduled", state.Reason)
	require.False(t, state.Hold)
	require.NoError(t, f.cache.RefreshAccountRotationWaiter(t.Context(), 1, "queued"))
	f.advanceMS(state.RestUntil*1000 - f.nowMS)
	ok, state := f.acquireState(1, "queued", 2, false)
	require.True(t, ok)
	require.False(t, state.Hold)
	require.True(t, state.Recovering)
	require.Equal(t, int64(137), state.RetryAfterMS)
}

func TestAccountRotationRecoveryCoordinatesConcurrentCacheClients(t *testing.T) {
	f := newAccountRotationPacingFixture(t)
	peerClient := redis.NewClient(&redis.Options{Addr: f.server.Addr()})
	t.Cleanup(func() { _ = peerClient.Close() })
	peer := NewConcurrencyCache(peerClient, 15, 900).(*concurrencyCache)
	const requests = 16
	remaining := make([]string, requests)
	for i := range remaining {
		remaining[i] = fmt.Sprintf("queued-%d", i)
	}
	f.completeRest(1, remaining...)
	type result struct {
		request string
		ok      bool
		state   *service.AccountRotationState
		err     error
	}
	for round := 0; round < 3; round++ {
		results := make(chan result, len(remaining))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, request := range remaining {
			wg.Add(1)
			go func(i int, request string) {
				defer wg.Done()
				<-start
				cache := []*concurrencyCache{f.cache, peer}[i%2]
				ok, state, err := cache.AcquireAccountSlotWithRotationState(t.Context(), 1, 100, request, f.policy, i%2 == 0, "test-pool")
				results <- result{request, ok, state, err}
			}(i, request)
		}
		close(start)
		wg.Wait()
		close(results)
		admitted := 0
		var next []string
		for result := range results {
			require.NoError(t, result.err)
			require.NotNil(t, result.state)
			require.False(t, result.state.Hold)
			require.True(t, result.state.Recovering)
			require.Equal(t, int64(137), result.state.RetryAfterMS)
			if result.ok {
				admitted++
				require.Equal(t, redis.Nil, f.client.ZScore(t.Context(), accountRotationWaiterKey(1), result.request).Err())
			} else {
				next = append(next, result.request)
			}
		}
		require.Equal(t, 1, admitted, "separate clients must share one recovery admission per interval")
		require.Equal(t, requests-round-1, f.check(1).WaitingCount)
		require.Equal(t, int64(round+1), f.client.ZCard(t.Context(), accountSlotKey(1)).Val())
		other, err := peer.CheckAccountRotation(t.Context(), 1, f.policy)
		require.NoError(t, err)
		require.Equal(t, f.check(1), other)
		f.advanceMS(136)
		require.False(t, f.acquire(1, next[0], 100, false))
		require.Equal(t, int64(1), f.check(1).RetryAfterMS)
		f.advanceMS(1)
		remaining = next
	}
}

func TestAccountRotationRecoveryRoutingReadsPreserveGate(t *testing.T) {
	f := newAccountRotationPacingFixture(t)
	f.completeRest(1, "first", "second")
	require.True(t, f.acquire(1, "first", 0, false))
	f.advanceMS(136)
	before := f.server.Dump()
	state, err := f.cache.PeekAccountRotation(t.Context(), 1, f.policy)
	require.NoError(t, err)
	require.True(t, state.Recovering)
	require.False(t, state.Hold)
	require.Equal(t, int64(1), state.RetryAfterMS)
	require.Equal(t, 1, state.WaitingCount)
	require.Equal(t, before, f.server.Dump(), "routing probes cannot advance or consume the recovery gate")
	states, err := f.cache.CheckAccountRotationBatch(t.Context(), []int64{1, 2, 1}, f.policy)
	require.NoError(t, err)
	require.Equal(t, state, states[1])
	require.False(t, states[2].Recovering)
	require.Zero(t, states[2].RetryAfterMS)
	f.advanceMS(1)
	ok, state := f.acquireState(1, "second", 0, false)
	require.True(t, ok)
	require.Equal(t, int64(137), state.RetryAfterMS)
}

package repository

import (
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAccountRotationScheduledHoldSwitchAndStickyCompatibility(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, mode := range []string{"aggressive", "sticky"} {
			t.Run(fmt.Sprintf("hold=%t/%s", enabled, mode), func(t *testing.T) {
				f := newAccountRotationCacheFixture(t)
				f.policy.Mode, f.policy.WorkSeconds, f.policy.ScheduledHoldEnabled = mode, 10, enabled
				require.True(t, f.acquire(1, "work", 10, false))
				f.advance(10)
				state := f.check(1)
				require.True(t, state.AutomaticEnabled)
				require.Equal(t, "scheduled", state.Reason)
				require.Equal(t, enabled, state.Hold)
				require.Zero(t, state.LastAutomaticRestAt, "draining is not a rest start")
				require.False(t, f.acquire(1, "new", 10, false))
				stickyAllowed := !enabled && mode == "sticky"
				require.Equal(t, stickyAllowed, f.acquire(1, "sticky", 10, true))
				if stickyAllowed {
					f.release(1, "sticky")
				}
				f.advance(7)
				f.release(1, "work")
				state = f.check(1)
				require.Equal(t, "resting", state.Phase)
				require.Equal(t, f.now, state.LastAutomaticRestAt)
				require.Equal(t, "scheduled", state.LastAutomaticRestReason)
				require.Equal(t, f.now+20, state.RestUntil)
				require.False(t, f.acquire(1, "during-rest", 10, true))
			})
		}
	}
}

func TestAccountRotationScheduledHoldPolicyChangesCannotReleaseWaiters(t *testing.T) {
	for _, draining := range []bool{false, true} {
		t.Run(fmt.Sprintf("draining=%t", draining), func(t *testing.T) {
			f := newAccountRotationPacingFixture(t)
			f.policy.WorkSeconds, f.policy.ScheduledHoldEnabled = 10, true
			require.True(t, f.acquire(1, "work", 10, false))
			f.advanceMS(10_000)
			before := f.check(1)
			require.True(t, before.Hold)
			for _, request := range []string{"first", "second"} {
				require.False(t, f.acquire(1, request, 10, true))
			}
			if !draining {
				f.release(1, "work")
				before = f.check(1)
			}
			f.policy.Revision, f.policy.Mode = "automatic-disabled", "sticky"
			f.policy.WorkSeconds, f.policy.RestMinSeconds, f.policy.RestMaxSeconds = 0, 0, 0
			f.policy.ScheduledHoldEnabled = false
			after := f.check(1)
			require.True(t, after.Hold, "turning HOLD off only affects later cycles")
			require.False(t, after.AutomaticEnabled)
			require.Equal(t, before.RestUntil, after.RestUntil)
			require.Equal(t, before.RestSeconds, after.RestSeconds)
			require.False(t, f.acquire(1, "first", 10, true))
			require.True(t, f.cancelHold(1).Hold, "manual cancel cannot cancel scheduled HOLD")
			if draining {
				f.advanceMS(3_000)
				f.release(1, "work")
			}
			state := f.check(1)
			restedAt := f.now
			require.Equal(t, restedAt, state.LastAutomaticRestAt)
			f.advanceMS(state.RestUntil*1000 - f.nowMS)
			ok, state := f.acquireState(1, "first", 10, true)
			require.True(t, ok)
			require.False(t, state.Hold)
			require.False(t, state.AutomaticEnabled)
			require.Equal(t, restedAt, state.LastAutomaticRestAt)
			require.True(t, state.Recovering)
			require.Equal(t, int64(137), state.RetryAfterMS)
			require.False(t, f.acquire(1, "second", 10, true))
			f.advanceMS(137)
			require.True(t, f.acquire(1, "second", 10, true))
		})
	}
}

func TestAccountRotationCustomEarlyRestWindowThresholdAndDuration(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	f.policy.WorkSeconds, f.policy.EarlyRestEnabled = 600, true
	f.policy.EarlyRestWindowSeconds, f.policy.EarlyRestConcurrencyThreshold, f.policy.EarlyRestSeconds = 60, 3, 7
	for _, request := range []string{"first", "second", "last"} {
		require.True(t, f.acquire(1, request, 10, false))
	}
	require.Equal(t, f.now+540, f.check(1).EarlyEligibleAt)
	f.advance(539)
	require.False(t, f.check(1).Hold)
	f.advance(1)
	require.False(t, f.check(1).Hold, "concurrency equal to threshold must not qualify")
	f.release(1, "first")
	state := f.check(1)
	require.True(t, state.Hold)
	require.Equal(t, "early", state.Reason)
	require.Equal(t, 7, state.RestSeconds)
	require.Zero(t, state.LastAutomaticRestAt)
	require.False(t, f.acquire(1, "sticky", 10, true))
	f.advance(4)
	f.release(1, "second")
	require.Zero(t, f.check(1).LastAutomaticRestAt)
	f.release(1, "last")
	state = f.check(1)
	require.Equal(t, f.now, state.LastAutomaticRestAt)
	require.Equal(t, "early", state.LastAutomaticRestReason)
	require.Equal(t, f.now+7, state.RestUntil)

	// The active gap retains its sampled duration even when all early controls change.
	f.policy.Revision = "early-changed-during-rest"
	f.policy.EarlyRestWindowSeconds, f.policy.EarlyRestConcurrencyThreshold, f.policy.EarlyRestSeconds = 300, 1, 90
	f.policy.EarlyRestEnabled = false
	require.Equal(t, state.RestUntil, f.check(1).RestUntil)
	f.advance(7)
	state = f.check(1)
	require.False(t, state.Hold)
	require.Equal(t, f.now-7, state.LastAutomaticRestAt)
	require.Equal(t, "early", state.LastAutomaticRestReason)
}

func TestAccountRotationRestHistorySurvivesWindowPolicyCancellationAndDisable(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	ctx := t.Context()
	f.policy.WorkSeconds, f.policy.ScheduledHoldEnabled = 60, true
	require.True(t, f.acquire(1, "work", 10, false))
	f.advance(5)
	f.hold(1, 1)
	require.Zero(t, f.check(1).LastManualRestAt)
	f.advance(3)
	f.cancelHold(1)
	require.Zero(t, f.check(1).LastManualRestAt, "canceling a draining hold never records a rest")
	f.hold(1, 2)
	f.advance(4)
	f.release(1, "work")
	manualAt := f.now
	require.Equal(t, manualAt, f.check(1).LastManualRestAt)
	f.advance(1)
	f.cancelHold(1)
	require.Equal(t, manualAt, f.check(1).LastManualRestAt, "canceling an actual rest retains its start")
	require.True(t, f.acquire(1, "next", 10, false))
	f.advance(60)
	require.True(t, f.check(1).Hold)
	f.release(1, "next")
	automaticAt := f.now
	f.advance(20)
	state := f.check(1)
	require.Equal(t, "ready", state.Phase)

	assertHistory := func() {
		t.Helper()
		state := f.check(1)
		require.Equal(t, manualAt, state.LastManualRestAt)
		require.Equal(t, automaticAt, state.LastAutomaticRestAt)
		require.Equal(t, "scheduled", state.LastAutomaticRestReason)
		require.Equal(t, time.Duration(-1), f.client.TTL(ctx, accountRotationKey(1)).Val())
	}
	assertHistory()
	f.policy.Revision, f.policy.WorkSeconds = "new-policy", 3600
	assertHistory() // newPolicy
	require.True(t, f.acquire(1, "natural-idle", 1, false))
	f.release(1, "natural-idle")
	f.advance(301)
	require.Equal(t, "ready", f.check(1).Phase)
	assertHistory() // clearWindow after natural idle
	f.hold(1, 2)
	manualAt = f.now // Zero concurrency starts a real rest immediately.
	f.advance(2)
	assertHistory() // Completed manual gap
	f.policy.Revision, f.policy.WorkSeconds = "automatic-off", 0
	require.False(t, f.check(1).AutomaticEnabled)
	assertHistory()
	// History is one bounded hash per account, retained through inactivity and
	// recoverable by a fresh cache instance. Waiter/slot leases still expire.
	require.NoError(t, f.cache.RefreshAccountRotationWaiter(ctx, 1, "orphan"))
	f.advance(90 * 24 * 3600)
	assertHistory()
	require.Zero(t, f.check(1).WaitingCount)
	peer, ok := NewConcurrencyCache(f.client, 15, 900).(*concurrencyCache)
	require.True(t, ok)
	state, err := peer.CheckAccountRotation(ctx, 1, f.policy)
	require.NoError(t, err)
	require.Equal(t, automaticAt, state.LastAutomaticRestAt)
	require.Equal(t, manualAt, state.LastManualRestAt)
}

func TestAccountRotationScheduledHistoryWaitsForLiveDrainAndPreservesReadOnly(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	ctx := t.Context()
	f.policy.WorkSeconds, f.policy.ScheduledHoldEnabled = 10, true
	require.True(t, f.acquire(1, "regular", 10, false))
	ok, err := f.cache.AcquireLiveLease(ctx, 1, 10, 2, 10, 3, 0, "live", false)
	require.NoError(t, err)
	require.True(t, ok)
	f.advance(10)
	f.release(1, "regular")
	require.Zero(t, f.check(1).LastAutomaticRestAt)
	f.advance(3)
	require.NoError(t, f.cache.ReleaseLiveLease(ctx, 1, 2, 3, "live"))
	require.Equal(t, f.now, f.check(1).LastAutomaticRestAt)
	before := f.server.Dump()
	state, err := f.cache.PeekAccountRotation(ctx, 1, f.policy)
	require.NoError(t, err)
	require.Equal(t, f.now, state.LastAutomaticRestAt)
	require.Equal(t, before, f.server.Dump())
	f.advance(20)
	require.True(t, f.acquire(2, "crashed", 1, false))
	f.advance(901)
	before = f.server.Dump()
	_, err = f.cache.PeekAccountRotation(ctx, 2, f.policy)
	require.ErrorContains(t, err, "ROUTING_ROTATION_TRANSITION_REQUIRED")
	require.Equal(t, before, f.server.Dump())
	require.Equal(t, "0", f.client.HGet(ctx, accountRotationKey(2), "last_automatic_rest_at").Val())
	state = f.check(2)
	require.Equal(t, f.now, state.LastAutomaticRestAt, "expired leases begin the persisted gap when zero is observed")
	require.NoError(t, f.client.ZAdd(ctx, accountSlotKey(2), redis.Z{Score: float64(f.now - 9999), Member: "stale"}).Err())
	require.Equal(t, state.LastAutomaticRestAt, f.check(2).LastAutomaticRestAt)
}

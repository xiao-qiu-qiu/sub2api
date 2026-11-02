package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func (f *accountRotationCacheFixture) hold(id int64, seconds int) *service.AccountRotationState {
	f.t.Helper()
	state, err := f.cache.SetAccountRotationHold(context.Background(), id, seconds, f.policy)
	require.NoError(f.t, err)
	return state
}

func (f *accountRotationCacheFixture) cancelHold(id int64) *service.AccountRotationState {
	f.t.Helper()
	state, err := f.cache.CancelAccountRotationHold(context.Background(), id, f.policy)
	require.NoError(f.t, err)
	return state
}

func TestAccountRotationManualHoldDrainsThenRestartsSameAccount(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	ctx := context.Background()
	require.True(t, f.acquire(1, "first", 2, false))
	require.True(t, f.acquire(1, "last", 2, false))
	original := f.check(1)
	f.advance(7)
	state := f.hold(1, 60)
	require.Equal(t, "draining", state.Phase)
	require.Equal(t, "manual", state.Reason)
	require.True(t, state.Hold)
	require.Zero(t, state.RestUntil)
	require.Equal(t, original.StartedAt, state.StartedAt)
	require.Equal(t, original.DrainAt, state.DrainAt)

	for _, sticky := range []bool{false, true} {
		acquired, blocked, err := f.cache.AcquireAccountSlotWithRotationState(ctx, 1, 0, "queued", f.policy, sticky, "test-pool")
		require.NoError(t, err)
		require.False(t, acquired)
		require.True(t, blocked.Hold, "the rejected admission must include its atomic hold snapshot")
		require.Equal(t, "manual", blocked.Reason)
	}
	require.NoError(t, f.cache.RefreshAccountRotationWaiter(ctx, 1, "queued"))
	require.NoError(t, f.cache.RefreshAccountRotationWaiter(ctx, 1, "queued"))
	require.Equal(t, 1, f.check(1).WaitingCount)
	require.Equal(t, int64(2), f.client.ZCard(ctx, accountSlotKey(1)).Val())

	f.advance(11)
	require.NoError(t, f.cache.RefreshAccountRotationSlot(ctx, 1, "last"))
	require.True(t, f.acquire(1, "last", 2, true), "an existing real slot remains valid during a hold")
	f.release(1, "first")
	require.Equal(t, "draining", f.check(1).Phase)
	f.advance(4)
	state = f.hold(1, 600)
	require.Equal(t, 60, state.RestSeconds, "a duplicate start cannot change the duration before draining finishes")
	f.release(1, "last")
	until := f.now + 60
	require.Equal(t, until, f.check(1).RestUntil)
	require.Equal(t, "resting", f.check(1).Phase)

	for i := 0; i < 3; i++ {
		f.advance(15)
		require.NoError(t, f.cache.RefreshAccountRotationWaiter(ctx, 1, "queued"))
		acquired, blocked, err := f.cache.AcquireAccountSlotWithRotationState(ctx, 1, 1, "queued", f.policy, true, "test-pool")
		require.NoError(t, err)
		require.False(t, acquired)
		require.Equal(t, until, blocked.RestUntil)
		require.Equal(t, 1, blocked.WaitingCount)
		require.Zero(t, f.client.ZCard(ctx, accountSlotKey(1)).Val(), "waiting must not consume real concurrency")
		require.Equal(t, until, f.hold(1, 600).RestUntil, "repeated starts cannot extend the active rest")
		f.release(1, "last")
	}
	f.advance(15)
	acquired, resumed, err := f.cache.AcquireAccountSlotWithRotationState(ctx, 1, 1, "queued", f.policy, true, "test-pool")
	require.NoError(t, err)
	require.True(t, acquired)
	require.False(t, resumed.Hold)
	require.Empty(t, resumed.Reason)
	require.Equal(t, f.now, resumed.StartedAt)
	require.Equal(t, f.now+600, resumed.DrainAt)
	require.Equal(t, 1, resumed.WaitingCount)
	require.NoError(t, f.cache.RemoveAccountRotationWaiter(ctx, 1, "queued"))
	require.Zero(t, f.check(1).WaitingCount)
	require.False(t, f.acquire(1, "next-in-queue", 1, false), "capacity still applies after resuming the same account")
}

func TestAccountRotationManualHoldWithoutAutomaticRotation(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	f.policy.WorkSeconds, f.policy.RestMinSeconds, f.policy.RestMaxSeconds = 0, 0, 0
	f.policy.EarlyRestEnabled = true
	ctx := context.Background()
	require.Equal(t, "ready", f.check(1).Phase)
	require.False(t, f.server.Exists(accountRotationKey(1)))
	require.NoError(t, f.cache.RefreshAccountRotationWaiter(ctx, 1, "queued"))
	state := f.check(1)
	require.Equal(t, 1, state.WaitingCount)
	require.Zero(t, state.StartedAt)
	require.False(t, f.server.Exists(accountRotationKey(1)), "a waiting request cannot start a work window")

	state = f.hold(1, 60)
	require.Equal(t, "resting", state.Phase)
	require.True(t, state.Hold)
	require.Equal(t, f.now+60, state.RestUntil)
	require.Zero(t, state.StartedAt)
	require.Zero(t, state.DrainAt)
	require.Zero(t, state.EarlyEligibleAt)
	f.advance(59)
	require.False(t, f.acquire(1, "queued", 1, true))
	f.advance(1)
	require.True(t, f.acquire(1, "queued", 1, false))
	state = f.check(1)
	require.Equal(t, "ready", state.Phase)
	require.False(t, state.Hold)
	require.Zero(t, state.StartedAt)
	require.Zero(t, state.DrainAt)
	require.Zero(t, f.client.ZCard(ctx, accountRotationPoolKey("test-pool")).Val())

	state = f.hold(1, 30)
	require.Equal(t, "draining", state.Phase)
	f.advance(5)
	f.release(1, "queued")
	require.Equal(t, f.now+30, f.check(1).RestUntil)
	state = f.cancelHold(1)
	require.Equal(t, "ready", state.Phase)
	require.False(t, state.Hold)
	require.True(t, f.acquire(1, "resumed", 1, false))
}

func TestAccountRotationManualCancelPreservesOriginalWorkWindow(t *testing.T) {
	for _, draining := range []bool{false, true} {
		t.Run(fmt.Sprintf("draining=%t", draining), func(t *testing.T) {
			f := newAccountRotationCacheFixture(t)
			require.True(t, f.acquire(1, "work", 1, false))
			original := f.check(1)
			f.advance(25)
			f.hold(1, 60)
			if !draining {
				f.release(1, "work")
			}
			f.advance(10)
			state := f.cancelHold(1)
			require.Equal(t, "working", state.Phase)
			require.Equal(t, original.StartedAt, state.StartedAt)
			require.Equal(t, original.DrainAt, state.DrainAt)
			require.Equal(t, original.RestSeconds, state.RestSeconds)
			require.False(t, state.Hold)
			require.Empty(t, state.Reason)
			require.Zero(t, state.RestUntil)
			require.Equal(t, state, f.cancelHold(1), "repeated cancellation must preserve the original clock")
		})
	}
}

func TestAccountRotationManualCancelAfterDeadlineUsesScheduledRest(t *testing.T) {
	for _, draining := range []bool{false, true} {
		t.Run(fmt.Sprintf("draining=%t", draining), func(t *testing.T) {
			f := newAccountRotationCacheFixture(t)
			f.policy.WorkSeconds = 60
			require.True(t, f.acquire(1, "work", 1, false))
			original := f.check(1)
			f.advance(50)
			f.hold(1, 120)
			if !draining {
				f.release(1, "work")
			}
			f.advance(15)
			state := f.cancelHold(1)
			require.False(t, state.Hold)
			require.Equal(t, "scheduled", state.Reason)
			require.Equal(t, original.StartedAt, state.StartedAt)
			require.Equal(t, original.DrainAt, state.DrainAt)
			require.Equal(t, original.RestSeconds, state.RestSeconds)
			if draining {
				require.Equal(t, "draining", state.Phase)
				f.release(1, "work")
			} else {
				require.Equal(t, "resting", state.Phase)
			}
			require.Equal(t, f.now+int64(original.RestSeconds), f.check(1).RestUntil)
		})
	}
}

func TestAccountRotationManualCancelDoesNotCountForcedIdle(t *testing.T) {
	for _, draining := range []bool{false, true} {
		t.Run(fmt.Sprintf("draining=%t", draining), func(t *testing.T) {
			f := newAccountRotationCacheFixture(t)
			f.policy.WorkSeconds = 18000
			require.True(t, f.acquire(1, "work", 1, false))
			original := f.check(1)
			f.advance(25)
			f.hold(1, 600)
			if !draining {
				f.release(1, "work")
			}
			f.advance(360)
			canceledAt := f.now
			state := f.cancelHold(1)
			require.Equal(t, "working", state.Phase)
			require.False(t, state.Hold)
			require.Equal(t, original.StartedAt, state.StartedAt)
			require.Equal(t, original.DrainAt, state.DrainAt)
			require.Equal(t, original.RestSeconds, state.RestSeconds)
			if draining {
				require.Zero(t, state.IdleSince)
			} else {
				require.Equal(t, canceledAt, state.IdleSince, "six minutes of forced idle cannot count as a natural break")
			}
			require.Equal(t, state, f.check(1), "the next check must not reset the preserved clock")
			f.advance(300)
			require.Equal(t, original.StartedAt, f.check(1).StartedAt)
			require.Equal(t, original.DrainAt, f.check(1).DrainAt)
			f.advance(1)
			if draining {
				require.Equal(t, original.StartedAt, f.check(1).StartedAt)
			} else {
				require.Equal(t, "ready", f.check(1).Phase, "only a new natural break after cancellation may reset the window")
			}
		})
	}
}

func TestAccountRotationManualCannotReplaceAutomaticRest(t *testing.T) {
	for _, early := range []bool{false, true} {
		for _, draining := range []bool{false, true} {
			t.Run(fmt.Sprintf("early=%t/draining=%t", early, draining), func(t *testing.T) {
				f := newAccountRotationCacheFixture(t)
				f.policy.WorkSeconds = 60
				if early {
					f.policy.WorkSeconds, f.policy.EarlyRestEnabled = 1800, true
				}
				require.True(t, f.acquire(1, "work", 1, false))
				if early {
					f.advance(600)
				} else {
					f.advance(60)
				}
				state := f.check(1)
				if !draining {
					f.release(1, "work")
					state = f.check(1)
				}
				_, err := f.cache.SetAccountRotationHold(context.Background(), 1, 1, f.policy)
				require.True(t, infraerrors.IsConflict(err))
				require.Equal(t, "ACCOUNT_ALREADY_RESTING", infraerrors.Reason(err))
				require.Equal(t, state, f.check(1))
				require.Equal(t, state, f.cancelHold(1), "manual cancellation cannot shorten an automatic rest")
				changedPolicy := f.policy
				changedPolicy.Revision = "changed-during-manual-action"
				_, err = f.cache.SetAccountRotationHold(context.Background(), 1, 1, changedPolicy)
				require.True(t, infraerrors.IsConflict(err), "a manual start cannot replace automatic rest through a new revision")
				unchanged, err := f.cache.CancelAccountRotationHold(context.Background(), 1, changedPolicy)
				require.NoError(t, err)
				require.Equal(t, state, unchanged)
				require.Equal(t, state, f.check(1))
			})
		}
	}
}

func TestAccountRotationManualHoldWaitsForFinalLiveRelease(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			f := newAccountRotationCacheFixture(t)
			ctx := context.Background()
			require.True(t, f.acquire(1, "regular", 3, false))
			acquired, err := f.cache.AcquireLiveLease(ctx, 1, 3, 2, 3, 3, 0, "live", false)
			require.NoError(t, err)
			require.True(t, acquired)
			f.hold(1, 60)
			f.advance(40)
			refreshed, err := f.cache.RefreshLiveLease(ctx, 1, 2, 3, "live")
			require.NoError(t, err)
			require.True(t, refreshed)
			f.release(1, "regular")
			require.Equal(t, "draining", f.check(1).Phase)
			f.advance(30)
			require.Zero(t, f.check(1).RestUntil)
			if partial {
				require.NoError(t, f.cache.ReleaseLiveLeaseAccount(ctx, 1, "live"))
				for _, key := range []string{liveUserSlotKey(2), liveAPIKeySlotKey(3)} {
					require.Equal(t, []string{"live"}, f.client.ZRange(ctx, key, 0, -1).Val())
				}
			} else {
				require.NoError(t, f.cache.ReleaseLiveLease(ctx, 1, 2, 3, "live"))
			}
			until := f.now + 60
			require.Equal(t, until, f.check(1).RestUntil)
			f.advance(5)
			require.NoError(t, f.cache.ReleaseLiveLease(ctx, 1, 2, 3, "live"))
			require.Equal(t, until, f.check(1).RestUntil)
		})
	}
}

func TestAccountRotationEarlyRestStartsAtLowConcurrency(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	f.policy.Mode, f.policy.WorkSeconds, f.policy.EarlyRestEnabled = "sticky", 1800, true
	require.True(t, f.acquire(1, "first", 3, false))
	require.True(t, f.acquire(1, "last", 3, false))
	original := f.check(1)
	require.Equal(t, f.now+600, original.EarlyEligibleAt)
	f.advance(599)
	require.False(t, f.check(1).Hold)
	f.advance(1)
	require.Equal(t, "working", f.check(1).Phase, "two concurrent requests do not qualify")
	f.release(1, "first")
	state := f.check(1)
	require.Equal(t, "draining", state.Phase)
	require.Equal(t, "early", state.Reason)
	require.True(t, state.Hold)
	require.Equal(t, 120, state.RestSeconds)
	require.Equal(t, original.StartedAt, state.StartedAt)
	require.Equal(t, original.DrainAt, state.DrainAt)
	require.False(t, f.acquire(1, "sticky-new", 3, true))
	f.advance(40)
	f.release(1, "last")
	require.Equal(t, f.now+120, f.check(1).RestUntil)
	f.advance(120)
	state = f.check(1)
	require.Equal(t, "ready", state.Phase)
	require.False(t, state.Hold)
	require.Zero(t, state.StartedAt)
	require.True(t, f.acquire(1, "next-cycle", 3, false))
	require.Equal(t, f.now+1800, f.check(1).DrainAt)
}

func TestAccountRotationEarlyRestCountsLiveConcurrency(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	f.policy.WorkSeconds, f.policy.EarlyRestEnabled = 1800, true
	ctx := context.Background()
	require.True(t, f.acquire(1, "regular", 3, false))
	f.advance(590)
	acquired, err := f.cache.AcquireLiveLease(ctx, 1, 3, 2, 3, 3, 0, "live", false)
	require.NoError(t, err)
	require.True(t, acquired)
	f.advance(10)
	require.False(t, f.check(1).Hold, "one regular plus one Live request is concurrency two")
	f.release(1, "regular")
	require.Equal(t, "early", f.check(1).Reason)
	require.Equal(t, "draining", f.check(1).Phase)
	require.NoError(t, f.cache.ReleaseLiveLease(ctx, 1, 2, 3, "live"))
	require.Equal(t, f.now+120, f.check(1).RestUntil)
}

func TestAccountRotationLiveHeartbeatTriggersEarlyRestWithoutPolling(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			f := newAccountRotationCacheFixture(t)
			f.policy.WorkSeconds, f.policy.EarlyRestEnabled = 1800, enabled
			ctx := context.Background()
			require.True(t, f.acquire(1, "initial-request", 2, false))
			startedAt := f.now
			acquired, err := f.cache.AcquireLiveLease(ctx, 1, 2, 2, 2, 3, 0, "only-live", false)
			require.NoError(t, err)
			require.True(t, acquired)
			f.release(1, "initial-request")
			require.Zero(t, f.client.ZCard(ctx, accountSlotKey(1)).Val())
			// From now on only real Live heartbeats run. Raw Redis reads below
			// observe the state without performing a rotation check or admission.
			for i := 0; i < 30; i++ {
				f.advance(20)
				refreshed, err := f.cache.RefreshLiveLease(ctx, 1, 2, 3, "only-live")
				require.NoError(t, err)
				require.True(t, refreshed)
				if i < 29 {
					require.Equal(t, "working", f.client.HGet(ctx, accountRotationKey(1), "phase").Val())
				}
			}
			require.Equal(t, startedAt+600, f.now)
			if !enabled {
				require.Equal(t, "working", f.client.HGet(ctx, accountRotationKey(1), "phase").Val())
				require.Empty(t, f.client.HGet(ctx, accountRotationKey(1), "reason").Val())
				return
			}
			require.Equal(t, "draining", f.client.HGet(ctx, accountRotationKey(1), "phase").Val())
			require.Equal(t, "early", f.client.HGet(ctx, accountRotationKey(1), "reason").Val())
			require.Equal(t, "1", f.client.HGet(ctx, accountRotationKey(1), "hold").Val())
			require.Equal(t, "0", f.client.HGet(ctx, accountRotationKey(1), "rest_until").Val())
			for _, key := range []string{liveAccountSlotKey(1), liveUserSlotKey(2), liveAPIKeySlotKey(3)} {
				require.Equal(t, float64(f.now), f.client.ZScore(ctx, key, "only-live").Val())
			}
			f.advance(20)
			refreshed, err := f.cache.RefreshLiveLease(ctx, 1, 2, 3, "only-live")
			require.NoError(t, err)
			require.True(t, refreshed, "early admission hold must preserve the existing joint Live lease")
			require.Equal(t, "0", f.client.HGet(ctx, accountRotationKey(1), "rest_until").Val())
			require.NoError(t, f.cache.ReleaseLiveLease(ctx, 1, 2, 3, "only-live"))
			require.Equal(t, "resting", f.client.HGet(ctx, accountRotationKey(1), "phase").Val())
			require.Equal(t, fmt.Sprint(f.now+120), f.client.HGet(ctx, accountRotationKey(1), "rest_until").Val())
		})
	}
}

func TestAccountRotationEarlyRestIdleAndNaturalReset(t *testing.T) {
	for _, naturalBreak := range []bool{false, true} {
		t.Run(fmt.Sprintf("natural=%t", naturalBreak), func(t *testing.T) {
			f := newAccountRotationCacheFixture(t)
			f.policy.WorkSeconds, f.policy.EarlyRestEnabled = 1800, true
			require.True(t, f.acquire(1, "work", 1, false))
			if naturalBreak {
				f.advance(299)
				f.release(1, "work")
				f.advance(301)
			} else {
				f.advance(590)
				f.release(1, "work")
				f.advance(10)
			}
			state := f.check(1)
			if naturalBreak {
				require.Equal(t, "ready", state.Phase)
				require.False(t, state.Hold, "a completed natural break takes priority over early rest")
				require.Zero(t, state.EarlyEligibleAt)
			} else {
				require.Equal(t, "resting", state.Phase)
				require.Equal(t, "early", state.Reason)
				require.Equal(t, f.now+120, state.RestUntil)
			}
		})
	}
}

func TestAccountRotationEarlyRestDisabledOrDeadlineReached(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			f := newAccountRotationCacheFixture(t)
			f.policy.WorkSeconds, f.policy.EarlyRestEnabled = 1800, enabled
			ctx := context.Background()
			require.True(t, f.acquire(1, "one", 2, false))
			if enabled {
				require.True(t, f.acquire(1, "two", 2, false))
			}
			for i := 0; i < 2; i++ {
				f.advance(600)
				require.Equal(t, "working", f.check(1).Phase)
				require.NoError(t, f.cache.RefreshAccountRotationSlot(ctx, 1, "one"))
				if enabled {
					require.NoError(t, f.cache.RefreshAccountRotationSlot(ctx, 1, "two"))
				} else {
					require.Zero(t, f.check(1).EarlyEligibleAt)
				}
			}
			f.advance(600)
			state := f.check(1)
			require.Equal(t, "draining", state.Phase)
			require.Equal(t, "scheduled", state.Reason)
			require.False(t, state.Hold, "the due scheduled policy retains reroute admission")
		})
	}
}

func TestAccountRotationRevisionPreservesActiveHold(t *testing.T) {
	for _, early := range []bool{false, true} {
		for _, draining := range []bool{false, true} {
			t.Run(fmt.Sprintf("early=%t/draining=%t", early, draining), func(t *testing.T) {
				f := newAccountRotationCacheFixture(t)
				f.policy.WorkSeconds = 1800
				f.policy.EarlyRestEnabled = early
				require.True(t, f.acquire(1, "work", 1, false))
				var before *service.AccountRotationState
				if early {
					f.advance(600)
					before = f.check(1)
				} else {
					before = f.hold(1, 60)
				}
				if !draining {
					f.release(1, "work")
					before = f.check(1)
				}
				f.advance(10)
				f.policy.Revision = "rotation-next"
				f.policy.WorkSeconds, f.policy.EarlyRestEnabled = 300, false
				after := f.check(1)
				require.Equal(t, before.Phase, after.Phase)
				require.Equal(t, before.Reason, after.Reason)
				require.Equal(t, before.RestUntil, after.RestUntil)
				require.Equal(t, before.RestSeconds, after.RestSeconds)
				require.Equal(t, before.StartedAt, after.StartedAt)
				require.Equal(t, before.DrainAt, after.DrainAt)
				require.True(t, after.Hold)
				require.False(t, f.acquire(1, "wait", 2, true))
				if draining {
					f.release(1, "work")
				}
				f.advance(f.check(1).RestUntil - f.now)
				require.True(t, f.acquire(1, "wait", 1, true))
				require.Equal(t, f.now+300, f.check(1).DrainAt, "the next completed cycle uses the new policy")
			})
		}
	}
}

func TestAccountRotationWaiterLeasesAreIndependentAndCancellable(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	ctx := context.Background()
	require.NoError(t, f.cache.RefreshAccountRotationWaiter(ctx, 1, "stale"))
	f.advance(20)
	require.NoError(t, f.cache.RefreshAccountRotationWaiter(ctx, 1, "live"))
	require.NoError(t, f.cache.RefreshAccountRotationWaiter(ctx, 1, "live"))
	require.Equal(t, 2, f.check(1).WaitingCount)
	f.advance(accountRotationWaiterTTLSeconds - 20)
	state := f.check(1)
	require.Equal(t, 1, state.WaitingCount)
	require.Zero(t, state.StartedAt)
	require.Equal(t, "ready", state.Phase)
	require.False(t, f.server.Exists(accountRotationKey(1)))
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.NoError(t, f.cache.RemoveAccountRotationWaiter(canceled, 1, "live"))
	require.Zero(t, f.check(1).WaitingCount)
	require.NoError(t, f.cache.RefreshAccountRotationWaiter(ctx, 1, "crashed"))
	f.advance(accountRotationWaiterTTLSeconds)
	require.Zero(t, f.check(1).WaitingCount)
	require.False(t, f.server.Exists(accountRotationWaiterKey(1)))
}

func TestAccountRotationDisableAutomaticPolicyPreservesHold(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(fmt.Sprintf("early=%t", early), func(t *testing.T) {
			f := newAccountRotationCacheFixture(t)
			f.policy.WorkSeconds, f.policy.EarlyRestEnabled = 1800, early
			require.True(t, f.acquire(1, "work", 1, false))
			if early {
				f.advance(600)
				f.check(1)
			} else {
				f.hold(1, 60)
			}
			before := f.check(1)
			f.policy.Revision, f.policy.WorkSeconds = "automatic-disabled", 0
			f.policy.RestMinSeconds, f.policy.RestMaxSeconds = 0, 0
			f.policy.EarlyRestEnabled = false
			after := f.check(1)
			require.True(t, after.Hold)
			require.Equal(t, before.StartedAt, after.StartedAt)
			require.Equal(t, before.DrainAt, after.DrainAt)
			require.Equal(t, before.RestSeconds, after.RestSeconds)
			require.Equal(t, before.Reason, after.Reason)
			require.False(t, f.acquire(1, "queued", 2, true))
			f.release(1, "work")
			f.advance(int64(before.RestSeconds))
			require.True(t, f.acquire(1, "queued", 1, false))
			require.Equal(t, "ready", f.check(1).Phase)
			require.Zero(t, f.check(1).StartedAt)
			require.Zero(t, f.check(1).DrainAt)
		})
	}
}

func TestAccountRotationWaitersDoNotPreventNaturalReset(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	ctx := context.Background()
	require.True(t, f.acquire(1, "work", 1, false))
	f.advance(10)
	f.release(1, "work")
	for i := 0; i < 6; i++ {
		f.advance(50)
		require.NoError(t, f.cache.RefreshAccountRotationWaiter(ctx, 1, "waiting"))
	}
	require.Equal(t, "working", f.check(1).Phase, "the natural reset boundary is strictly greater than 300 seconds")
	f.advance(1)
	require.NoError(t, f.cache.RefreshAccountRotationWaiter(ctx, 1, "waiting"))
	state := f.check(1)
	require.Equal(t, "ready", state.Phase)
	require.Equal(t, 1, state.WaitingCount)
	require.Zero(t, state.StartedAt)
	require.Zero(t, state.DrainAt)
}

func TestAccountRotationNoManualCancelDoesNotResetWorkClock(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	require.True(t, f.acquire(1, "work", 1, false))
	before := f.check(1)
	f.advance(10)
	changedPolicy := f.policy
	changedPolicy.Revision, changedPolicy.WorkSeconds = "new-policy", 300
	after, err := f.cache.CancelAccountRotationHold(context.Background(), 1, changedPolicy)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestAccountRotationConcurrentResumeKeepsCapacity(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	f.hold(1, 10)
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		require.NoError(t, f.cache.RefreshAccountRotationWaiter(ctx, 1, fmt.Sprintf("queued-%d", i)))
	}
	f.advance(10)
	results, errs := make(chan bool, 12), make(chan error, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, _, err := f.cache.AcquireAccountSlotWithRotationState(ctx, 1, 2, fmt.Sprintf("queued-%d", i), f.policy, true, "test-pool")
			results <- ok
			errs <- err
		}(i)
	}
	wg.Wait()
	admitted := 0
	for i := 0; i < 12; i++ {
		require.NoError(t, <-errs)
		if <-results {
			admitted++
		}
	}
	require.Equal(t, 2, admitted)
	require.Equal(t, int64(2), f.client.ZCard(ctx, accountSlotKey(1)).Val())
	require.Equal(t, 12, f.check(1).WaitingCount, "waiting lease cleanup belongs to each admitted/canceled owner")
}

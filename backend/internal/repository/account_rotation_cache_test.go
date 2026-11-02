package repository

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type accountRotationCacheFixture struct {
	t      *testing.T
	server *miniredis.Miniredis
	client *redis.Client
	cache  *concurrencyCache
	now    int64
	policy service.AccountRotationPolicy
}

func newAccountRotationCacheFixture(t *testing.T) *accountRotationCacheFixture {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	f := &accountRotationCacheFixture{
		t: t, server: server, client: client, now: 1_800_000_000,
		cache: NewConcurrencyCache(client, 15, 900).(*concurrencyCache),
		policy: service.AccountRotationPolicy{Mode: "aggressive", WorkSeconds: 600,
			RestMinSeconds: 20, RestMaxSeconds: 20, Revision: "rotation-v1"},
	}
	server.SetTime(time.Unix(f.now, 0))
	return f
}

func (f *accountRotationCacheFixture) advance(seconds int64) {
	f.t.Helper()
	f.server.FastForward(time.Duration(seconds) * time.Second)
	f.now += seconds
	f.server.SetTime(time.Unix(f.now, 0))
}

func (f *accountRotationCacheFixture) check(id int64) *service.AccountRotationState {
	f.t.Helper()
	state, err := f.cache.CheckAccountRotation(context.Background(), id, f.policy)
	require.NoError(f.t, err)
	return state
}

func (f *accountRotationCacheFixture) acquire(id int64, request string, limit int, sticky bool) bool {
	f.t.Helper()
	ok, err := f.cache.AcquireAccountSlotWithRotation(context.Background(), id, limit, request, f.policy, sticky, "test-pool")
	require.NoError(f.t, err)
	return ok
}

func (f *accountRotationCacheFixture) release(id int64, request string) {
	f.t.Helper()
	require.NoError(f.t, f.cache.ReleaseAccountSlot(context.Background(), id, request))
}

func TestAccountRotationStartsOnlyAfterAdmission(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	require.Equal(t, "ready", f.check(1).Phase)
	f.advance(50)
	require.Equal(t, int64(0), f.check(1).StartedAt)
	require.False(t, f.server.Exists(accountRotationKey(1)), "candidate checks must not create a work cycle")

	require.True(t, f.acquire(1, "first", 1, false))
	start := f.now
	state := f.check(1)
	require.Equal(t, "working", state.Phase)
	require.Equal(t, start, state.StartedAt)
	require.Equal(t, start+600, state.DrainAt)
	f.advance(70)
	require.False(t, f.acquire(1, "full", 1, false))
	require.Equal(t, start, f.check(1).StartedAt)
	require.Equal(t, start+600, f.check(1).DrainAt)
}

func TestAccountRotationAdoptsExistingRegularAndLiveSlots(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("live=%t", live), func(t *testing.T) {
			f := newAccountRotationCacheFixture(t)
			ctx := context.Background()
			var acquired bool
			var err error
			if live {
				acquired, err = f.cache.AcquireLiveLease(ctx, 1, 1, 2, 1, 3, 0, "pre-enabled", false)
			} else {
				acquired, err = f.cache.AcquireAccountSlot(ctx, 1, 1, "pre-enabled")
			}
			require.NoError(t, err)
			require.True(t, acquired)
			f.advance(5)
			start := f.now
			state := f.check(1)
			require.Equal(t, start, state.StartedAt)
			require.Equal(t, start+600, state.DrainAt)
			require.False(t, f.acquire(1, "full", 1, false))
			f.advance(10)
			require.Equal(t, start, f.check(1).StartedAt)
			require.True(t, f.acquire(1, "join", 2, false))
			require.Equal(t, start, f.check(1).StartedAt, "attaching a pool must retain the adoption clock")
		})
	}
}

func TestAccountRotationAggressiveDrainAndRestBoundaries(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	f.policy.WorkSeconds = 60
	f.policy.RestMinSeconds, f.policy.RestMaxSeconds = 10, 30
	require.True(t, f.acquire(1, "running", 2, false))
	start := f.now
	restSeconds := int64(f.check(1).RestSeconds)
	f.advance(59)
	require.Equal(t, "working", f.check(1).Phase)
	f.advance(1)
	require.Equal(t, "draining", f.check(1).Phase)
	require.False(t, f.acquire(1, "new", 2, false))
	require.False(t, f.acquire(1, "old-sticky", 2, true))
	require.True(t, f.acquire(1, "running", 2, false), "retrying an existing member does not add concurrency")
	f.advance(12)
	require.NoError(t, f.cache.RefreshAccountRotationSlot(context.Background(), 1, "running"))
	require.Equal(t, int64(0), f.check(1).RestUntil, "work is never interrupted by the rest timer")
	f.release(1, "running")
	require.Equal(t, "resting", f.client.HGet(context.Background(), accountRotationKey(1), "phase").Val())
	state := f.check(1)
	require.Equal(t, "resting", state.Phase)
	require.Equal(t, start, state.StartedAt)
	require.Equal(t, f.now, state.IdleSince)
	require.Equal(t, f.now+restSeconds, state.RestUntil, "the random duration must be reused after a delayed drain")
	f.advance(restSeconds - 1)
	require.Equal(t, "resting", f.check(1).Phase)
	require.False(t, f.acquire(1, "too-early", 0, true))
	f.advance(1)
	state = f.check(1)
	require.Equal(t, "ready", state.Phase)
	require.Zero(t, state.StartedAt)
	require.Zero(t, state.DrainAt)
	f.advance(100)
	require.Zero(t, f.check(1).StartedAt)
	require.True(t, f.acquire(1, "new-cycle", 0, false))
	require.Equal(t, f.now, f.check(1).StartedAt)
	require.Equal(t, f.now+60, f.check(1).DrainAt)
}

func TestAccountRotationStickyWaitsForRegularAndLiveZero(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	f.policy.Mode, f.policy.WorkSeconds = "sticky", 60
	ctx := context.Background()
	require.True(t, f.acquire(1, "ordinary", 5, false))
	f.advance(50)
	acquired, err := f.cache.AcquireLiveLease(ctx, 1, 5, 2, 5, 3, 0, "live", false)
	require.NoError(t, err)
	require.True(t, acquired)
	f.advance(10)
	require.Equal(t, "draining", f.check(1).Phase)
	require.False(t, f.acquire(1, "fresh", 5, false))
	require.True(t, f.acquire(1, "continuation", 5, true))
	f.release(1, "ordinary")
	f.release(1, "continuation")
	require.Equal(t, "draining", f.check(1).Phase)
	require.Zero(t, f.check(1).RestUntil)

	// Live final release participates in the same zero-concurrency transition.
	f.advance(3)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.NoError(t, f.cache.ReleaseLiveLease(canceled, 1, 2, 3, "live"))
	require.Equal(t, "resting", f.client.HGet(ctx, accountRotationKey(1), "phase").Val())
	state := f.check(1)
	require.Equal(t, "resting", state.Phase)
	require.Equal(t, f.now+20, state.RestUntil)
	require.False(t, f.acquire(1, "sticky-after-zero", 5, true))
	for _, key := range []string{liveAccountSlotKey(1), liveUserSlotKey(2), liveAPIKeySlotKey(3)} {
		require.Zero(t, f.client.ZCard(ctx, key).Val())
	}
}

func TestAccountRotationNaturalIdleReset(t *testing.T) {
	for _, seconds := range []int64{299, 300, 301} {
		t.Run(strconv.FormatInt(seconds, 10), func(t *testing.T) {
			f := newAccountRotationCacheFixture(t)
			require.True(t, f.acquire(1, "work", 1, false))
			start := f.now
			f.advance(10)
			f.release(1, "work")
			idleSince := f.now
			f.advance(seconds)
			state := f.check(1)
			require.Equal(t, idleSince, state.IdleSince)
			if seconds > 300 {
				require.Equal(t, "ready", state.Phase)
				require.Zero(t, state.StartedAt)
				require.Zero(t, state.DrainAt)
				require.Zero(t, f.client.ZCard(context.Background(), accountRotationPoolKey("test-pool")).Val())
				f.advance(25)
				require.Zero(t, f.check(1).StartedAt, "checks must not restart or extend a reset window")
				require.True(t, f.acquire(1, "resumed", 1, false))
				require.Equal(t, f.now, f.check(1).StartedAt)
				require.Equal(t, f.now+600, f.check(1).DrainAt)
			} else {
				require.Equal(t, "working", state.Phase)
				require.Equal(t, start, state.StartedAt)
				require.True(t, f.acquire(1, "resumed", 1, false))
				require.Equal(t, start+600, f.check(1).DrainAt)
			}
		})
	}
}

func TestAccountRotationLiveAccountReleaseKeepsJointLease(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	f.policy.Mode, f.policy.WorkSeconds = "sticky", 10
	ctx := context.Background()
	acquired, err := f.cache.AcquireLiveLease(ctx, 1, 1, 2, 1, 3, 0, "live", false)
	require.NoError(t, err)
	require.True(t, acquired)
	require.Equal(t, f.now, f.check(1).StartedAt)
	f.advance(10)
	require.Equal(t, "draining", f.check(1).Phase)
	require.NoError(t, f.cache.releaseAccountRotationLiveSlot(ctx, 1, "live"))
	require.Equal(t, "resting", f.client.HGet(ctx, accountRotationKey(1), "phase").Val())
	require.Zero(t, f.client.ZCard(ctx, liveAccountSlotKey(1)).Val())
	for _, key := range []string{liveUserSlotKey(2), liveAPIKeySlotKey(3)} {
		require.Equal(t, []string{"live"}, f.client.ZRange(ctx, key, 0, -1).Val())
	}
	until := f.check(1).RestUntil
	f.advance(5)
	require.NoError(t, f.cache.ReleaseLiveLease(ctx, 1, 2, 3, "live"))
	require.Equal(t, until, f.check(1).RestUntil)
}

func TestAccountRotationExpiredLiveLeaseClosesStickyDrain(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	f.policy.Mode, f.policy.WorkSeconds = "sticky", 30
	acquired, err := f.cache.AcquireLiveLease(context.Background(), 1, 1, 2, 1, 3, 0, "live", false)
	require.NoError(t, err)
	require.True(t, acquired)
	f.check(1)
	f.advance(30)
	require.Equal(t, "draining", f.check(1).Phase)
	f.advance(30)
	require.Equal(t, "resting", f.check(1).Phase)
	require.False(t, f.acquire(1, "sticky-after-expiry", 1, true))
}

func TestAccountRotationNaturalIdlePrecedesLateDeadlineObservation(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	require.True(t, f.acquire(1, "work", 1, false))
	f.advance(100)
	f.release(1, "work")
	f.advance(1000)
	state := f.check(1)
	require.Equal(t, "ready", state.Phase)
	require.Zero(t, state.StartedAt)
	require.Zero(t, state.RestUntil, "a completed natural break must not be punished by a stale deadline")
	require.True(t, f.acquire(1, "next", 1, false))
	require.Equal(t, f.now+600, f.check(1).DrainAt)
}

func TestAccountRotationReleaseAtDeadlineStartsRest(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	f.policy.RestMinSeconds, f.policy.RestMaxSeconds = 600, 600
	require.True(t, f.acquire(1, "work", 1, false))
	f.advance(600)
	f.release(1, "work")
	until := f.now + 600
	require.Equal(t, "resting", f.check(1).Phase)
	f.advance(301)
	require.Equal(t, "resting", f.check(1).Phase)
	require.Equal(t, until, f.check(1).RestUntil, "idle reset applies only to working-phase breaks beginning before the deadline")
	require.False(t, f.acquire(1, "still-resting", 1, true))
}

func TestAccountRotationConcurrentAdmissionAndDeadline(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	f.policy.WorkSeconds = 60
	peerClient := redis.NewClient(&redis.Options{Addr: f.server.Addr()})
	t.Cleanup(func() { _ = peerClient.Close() })
	peer := NewConcurrencyCache(peerClient, 15, 900).(*concurrencyCache)
	const requests = 24
	results := make(chan bool, requests)
	errors := make(chan error, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cache := []*concurrencyCache{f.cache, peer}[i%2]
			ok, err := cache.AcquireAccountSlotWithRotation(context.Background(), 1, 3,
				fmt.Sprintf("request-%d", i), f.policy, false, "shared")
			results <- ok
			errors <- err
		}(i)
	}
	wg.Wait()
	count := 0
	for i := 0; i < requests; i++ {
		require.NoError(t, <-errors)
		if <-results {
			count++
		}
	}
	require.Equal(t, 3, count)
	f.advance(60)
	for i := 0; i < requests; i++ {
		ok, err := peer.AcquireAccountSlotWithRotation(context.Background(), 1, 100,
			fmt.Sprintf("late-%d", i), f.policy, true, "shared")
		require.NoError(t, err)
		require.False(t, ok)
	}
	require.Equal(t, "draining", f.check(1).Phase)
}

func TestAccountRotationFinalReleaseRacesStickyAdmission(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	f.policy.Mode, f.policy.WorkSeconds = "sticky", 10
	for id := int64(1); id <= 16; id++ {
		require.True(t, f.acquire(id, "old", 2, false))
		f.advance(10)
		start := make(chan struct{})
		acquired := make(chan bool, 1)
		errors := make(chan error, 2)
		go func(id int64) {
			<-start
			errors <- f.cache.ReleaseAccountSlot(context.Background(), id, "old")
		}(id)
		go func(id int64) {
			<-start
			ok, err := f.cache.AcquireAccountSlotWithRotation(context.Background(), id, 2, "next", f.policy, true, "test-pool")
			acquired <- ok
			errors <- err
		}(id)
		close(start)
		require.NoError(t, <-errors)
		require.NoError(t, <-errors)
		state := f.check(id)
		if <-acquired {
			require.Equal(t, "draining", state.Phase)
			require.Equal(t, int64(1), f.client.ZCard(context.Background(), accountSlotKey(id)).Val())
			f.release(id, "next")
		} else {
			require.Equal(t, "resting", state.Phase)
			require.Zero(t, f.client.ZCard(context.Background(), accountSlotKey(id)).Val())
		}
	}
}

func TestAccountRotationUnlimitedStillTracksAndReleasesAfterCancel(t *testing.T) {
	for _, limit := range []int{0, -1} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			f := newAccountRotationCacheFixture(t)
			require.True(t, f.acquire(1, "one", limit, false))
			require.True(t, f.acquire(1, "two", limit, false))
			require.Equal(t, int64(2), f.client.ZCard(context.Background(), accountSlotKey(1)).Val())
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			require.NoError(t, f.cache.ReleaseAccountSlot(canceled, 1, "one"))
			require.Zero(t, f.check(1).IdleSince)
			f.advance(5)
			require.NoError(t, f.cache.ReleaseAccountSlot(canceled, 1, "two"))
			require.Equal(t, f.now, f.check(1).IdleSince)
			f.advance(20)
			f.release(1, "two")
			require.Equal(t, f.now-20, f.check(1).IdleSince, "duplicate release must not move the idle timestamp")
		})
	}
}

func TestAccountRotationRefreshPreservesLongRequestWithoutRevival(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	f.policy.WorkSeconds = 18000
	ctx := context.Background()
	require.True(t, f.acquire(1, "long", 1, false))
	start := f.now
	for i := 0; i < 3; i++ {
		f.advance(899)
		require.NoError(t, f.cache.RefreshAccountRotationSlot(ctx, 1, "long"))
		require.Equal(t, float64(f.now), f.client.ZScore(ctx, accountSlotKey(1), "long").Val())
		require.Equal(t, start, f.check(1).StartedAt)
		require.False(t, f.acquire(1, "full", 1, false))
	}
	f.release(1, "long")
	require.NoError(t, f.cache.RefreshAccountRotationSlot(ctx, 1, "long"))
	require.False(t, f.server.Exists(accountSlotKey(1)))
	require.NoError(t, f.cache.RefreshAccountRotationSlot(ctx, 99, "never-existed"))
	require.False(t, f.server.Exists(accountSlotKey(99)))
	require.False(t, f.server.Exists(accountRotationKey(99)))

	require.True(t, f.acquire(2, "expired", 1, false))
	f.advance(900)
	require.NoError(t, f.cache.RefreshAccountRotationSlot(ctx, 2, "expired"))
	require.False(t, f.server.Exists(accountSlotKey(2)), "expired members must not be revived either")
}

func TestAccountRotationRevisionResetsPersistedPlan(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	require.True(t, f.acquire(1, "work", 2, false))
	oldPool := accountRotationPoolKey("test-pool")
	require.Equal(t, int64(1), f.client.ZCard(context.Background(), oldPool).Val())
	f.advance(30)
	f.policy.Revision, f.policy.WorkSeconds = "rotation-v2", 300
	state := f.check(1)
	require.Equal(t, f.now, state.StartedAt)
	require.Equal(t, f.now+300, state.DrainAt)
	require.Zero(t, f.client.ZCard(context.Background(), oldPool).Val())
	peer := NewConcurrencyCache(f.client, 15, 900).(*concurrencyCache)
	state2, err := peer.CheckAccountRotation(context.Background(), 1, f.policy)
	require.NoError(t, err)
	require.Equal(t, state, state2, "state is Redis-owned across cache instances")
	require.True(t, f.acquire(1, "second", 2, false))
	require.Equal(t, int64(1), f.client.ZCard(context.Background(), oldPool).Val())
}

func TestAccountRotationRandomBoundsAndStableCycleSample(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	f.policy.WorkSeconds, f.policy.StaggerPercent = 18000, 10
	f.policy.RestMinSeconds, f.policy.RestMaxSeconds = 600, 1800
	durations := make(map[int]bool)
	for id := int64(1); id <= 48; id++ {
		require.True(t, f.acquire(id, "work", 1, false))
		state := f.check(id)
		require.GreaterOrEqual(t, state.RestSeconds, 600)
		require.LessOrEqual(t, state.RestSeconds, 1800)
		require.GreaterOrEqual(t, state.DrainAt, f.now+16200)
		require.LessOrEqual(t, state.DrainAt, f.now+19800)
		durations[state.RestSeconds] = true
		require.Equal(t, state, f.check(id), "checks must not resample either duration or deadline")
	}
	require.Greater(t, len(durations), 20)
}

func rotationOverlap(states []*service.AccountRotationState) (int, int64) {
	type event struct {
		at    int64
		delta int
	}
	var events []event
	for _, state := range states {
		events = append(events, event{state.DrainAt, 1}, event{state.DrainAt + int64(state.RestSeconds), -1})
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].at == events[j].at {
			return events[i].delta < events[j].delta
		}
		return events[i].at < events[j].at
	})
	peak, depth, integral := 0, 0, int64(0)
	previous := events[0].at
	for _, point := range events {
		integral += (point.at - previous) * int64(depth*(depth-1)/2)
		depth += point.delta
		if depth > peak {
			peak = depth
		}
		previous = point.at
	}
	return peak, integral
}

func TestAccountRotationStaggerReducesActualRestOverlap(t *testing.T) {
	var scores [2]struct {
		peak    int
		overlap int64
	}
	for index, stagger := range []int{0, 30} {
		f := newAccountRotationCacheFixture(t)
		f.policy.WorkSeconds, f.policy.StaggerPercent = 3600, stagger
		f.policy.RestMinSeconds, f.policy.RestMaxSeconds = 180, 180
		var states []*service.AccountRotationState
		for id := int64(1); id <= 32; id++ {
			require.True(t, f.acquire(id, "work", 1, false))
			states = append(states, f.check(id))
		}
		scores[index].peak, scores[index].overlap = rotationOverlap(states)
	}
	require.Equal(t, 32, scores[0].peak)
	require.LessOrEqual(t, scores[1].peak, 5)
	require.Less(t, scores[1].overlap, scores[0].overlap/4)
	t.Logf("rest overlap peak %d -> %d; accumulated pair overlap %ds -> %ds",
		scores[0].peak, scores[1].peak, scores[0].overlap, scores[1].overlap)
}

func TestAccountRotationPoolsAreIsolatedAndCleanupIsBounded(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	f.policy.StaggerPercent = 30
	ctx := context.Background()
	poolKey := accountRotationPoolKey("test-pool")
	for i := 0; i < 256; i++ {
		require.NoError(t, f.client.ZAdd(ctx, poolKey, redis.Z{Score: float64(f.now - 1), Member: fmt.Sprintf("stale:%d", i)}).Err())
	}
	require.True(t, f.acquire(1, "work", 1, false))
	require.Equal(t, int64(129), f.client.ZCard(ctx, poolKey).Val())
	require.True(t, f.acquire(2, "work", 1, false))
	require.Equal(t, int64(2), f.client.ZCard(ctx, poolKey).Val())
	for id := int64(3); id <= 24; id++ {
		require.True(t, f.acquire(id, "work", 1, false))
	}
	ok, err := f.cache.AcquireAccountSlotWithRotation(ctx, 99, 1, "work", f.policy, false, "unrelated")
	require.NoError(t, err)
	require.True(t, ok)
	other := f.check(99)
	isolated := newAccountRotationCacheFixture(t)
	isolated.policy = f.policy
	ok, err = isolated.cache.AcquireAccountSlotWithRotation(ctx, 99, 1, "work", isolated.policy, false, "unrelated")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, isolated.check(99), other, "plans from a different opaque pool must not influence this account")
}

func TestAccountRotationBatchCheckAndRedisFailure(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	ctx := context.Background()
	f.policy.WorkSeconds = 60
	require.True(t, f.acquire(1, "work", 1, false))
	f.advance(60)
	require.NoError(t, f.client.ScriptFlush(ctx).Err())
	states, err := f.cache.CheckAccountRotationBatch(ctx, []int64{1, 2, 1}, f.policy)
	require.NoError(t, err)
	require.Len(t, states, 2)
	require.Equal(t, "draining", states[1].Phase)
	require.Equal(t, "ready", states[2].Phase)
	states, err = f.cache.CheckAccountRotationBatch(ctx, nil, f.policy)
	require.NoError(t, err)
	require.Empty(t, states)
	f.server.SetError("ERR unavailable")
	_, err = f.cache.CheckAccountRotation(ctx, 1, f.policy)
	require.Error(t, err)
	_, err = f.cache.CheckAccountRotationBatch(ctx, []int64{1, 2}, f.policy)
	require.Error(t, err)
	_, err = f.cache.AcquireAccountSlotWithRotation(ctx, 3, 0, "work", f.policy, false, "pool")
	require.Error(t, err)
	require.Error(t, f.cache.ReleaseAccountSlot(ctx, 1, "work"))
}

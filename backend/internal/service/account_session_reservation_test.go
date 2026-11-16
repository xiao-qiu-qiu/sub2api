package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type sessionReservationCacheStub struct {
	schedulerTestConcurrencyCache
	reserved       map[int64]int
	starts, ends   int
	observationErr error
	endContextErr  error
	key, requestID string
	persistent     bool
}

func (c *sessionReservationCacheStub) StartAccountSession(_ context.Context, _ int64, _ int, requestID, key string, persistent bool) error {
	c.starts++
	c.key, c.requestID, c.persistent = key, requestID, persistent
	return c.observationErr
}

func (c *sessionReservationCacheStub) EndAccountSession(ctx context.Context, _ int64, requestID, key string) error {
	c.ends++
	c.endContextErr = ctx.Err()
	if requestID != c.requestID || key != c.key {
		return errors.New("cleanup identity changed")
	}
	return nil
}

func (c *sessionReservationCacheStub) GetAccountSessionReservations(context.Context, []AccountWithConcurrency) (map[int64]int, error) {
	return c.reserved, c.observationErr
}

func TestAccountSessionReservationLifecycle(t *testing.T) {
	for _, writeFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "observed", true: "lost_write_response"}[writeFails], func(t *testing.T) {
			released := []int64{}
			cache := &sessionReservationCacheStub{schedulerTestConcurrencyCache: schedulerTestConcurrencyCache{releasedIDs: &released}}
			if writeFails {
				cache.observationErr = errors.New("observation unavailable")
			}
			svc := NewConcurrencyService(cache)
			ctx, cancel := context.WithCancel(WithAccountSessionAffinity(t.Context(), AccountSessionAffinity{Key: strings.Repeat("a", 64), PersistentRoute: true}))
			result, err := svc.AcquireAccountSlot(ctx, 7, 4)
			require.NoError(t, err)
			require.True(t, result.Acquired, "optional bookkeeping cannot reject admitted traffic")
			require.Equal(t, 1, cache.starts)
			require.True(t, cache.persistent)
			require.Equal(t, result.RequestID, cache.requestID)
			cancel()
			result.ReleaseFunc()
			result.ReleaseFunc()
			require.Equal(t, 1, cache.ends)
			require.NoError(t, cache.endContextErr, "disconnects still release observation")
			require.Equal(t, []int64{7}, released, "real slot release remains exactly once")
		})
	}
}

func TestAccountSessionReservationSkipsPollsAndAnonymousTraffic(t *testing.T) {
	cache := &sessionReservationCacheStub{schedulerTestConcurrencyCache: schedulerTestConcurrencyCache{acquireResults: map[int64]bool{7: false}}}
	svc := NewConcurrencyService(cache)
	ctx := WithAccountSessionAffinity(t.Context(), AccountSessionAffinity{Key: strings.Repeat("a", 64)})
	for range 5 {
		result, err := svc.AcquireAccountSlot(ctx, 7, 4)
		require.NoError(t, err)
		require.False(t, result.Acquired)
	}
	result, err := svc.AcquireAccountSlot(t.Context(), 8, 4)
	require.NoError(t, err)
	result.ReleaseFunc()
	for _, limit := range []int{0, 1} {
		result, err = svc.AcquireAccountSlot(ctx, 8, limit)
		require.NoError(t, err)
		result.ReleaseFunc()
	}
	require.Zero(t, cache.starts)
	require.Zero(t, cache.ends)
}

func TestAccountSessionReservationLoadsKeepRealOccupancy(t *testing.T) {
	base := &AccountLoadInfo{AccountID: 7, CurrentConcurrency: 2, WaitingCount: 1, LoadRate: 30}
	cache := &sessionReservationCacheStub{
		schedulerTestConcurrencyCache: schedulerTestConcurrencyCache{loadMap: map[int64]*AccountLoadInfo{7: base}},
		reserved:                      map[int64]int{7: 99},
	}
	svc := NewConcurrencyService(cache)
	loads, err := svc.GetAccountsLoadBatch(t.Context(), []AccountWithConcurrency{{ID: 7, MaxConcurrency: 10}})
	require.NoError(t, err)
	require.Equal(t, 5, loads[7].ReservedConcurrency)
	require.Equal(t, 2, loads[7].CurrentConcurrency)
	require.Equal(t, 30, loads[7].LoadRate)
	require.Equal(t, 80, accountSchedulingLoadRate(&Account{Concurrency: 10}, loads[7]))
	require.Zero(t, base.ReservedConcurrency, "shared raw snapshot must stay immutable")
	cache.observationErr = errors.New("soft cache offline")
	loads, err = svc.GetAccountsLoadBatchFresh(t.Context(), []AccountWithConcurrency{{ID: 7, MaxConcurrency: 10}})
	require.NoError(t, err)
	require.Zero(t, loads[7].ReservedConcurrency)
	require.Equal(t, 2, loads[7].CurrentConcurrency)
}

func TestAccountSessionReservationRoutesNewTrafficAndBorrowsAtCapacity(t *testing.T) {
	groupID := int64(11)
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 4, GroupIDs: []int64{groupID}},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 4, GroupIDs: []int64{groupID}},
	}
	for _, peerAvailable := range []bool{true, false} {
		t.Run(map[bool]string{true: "leave_room", false: "borrow_idle_reservation"}[peerAvailable], func(t *testing.T) {
			acquired := []int64{}
			cfg := DefaultPrioritySchedulingConfig()
			cfg.Enabled = true
			gateway := priorityGateway(cfg, &priorityReaderStub{})
			gateway.accountRepo = schedulerTestOpenAIAccountRepo{accounts: accounts}
			gateway.concurrencyService = NewConcurrencyService(&sessionReservationCacheStub{
				schedulerTestConcurrencyCache: schedulerTestConcurrencyCache{
					loadMap:        map[int64]*AccountLoadInfo{1: {AccountID: 1, CurrentConcurrency: 2, LoadRate: 50}, 2: {AccountID: 2}},
					acquireResults: map[int64]bool{1: true, 2: peerAvailable}, acquiredIDs: &acquired,
				}, reserved: map[int64]int{1: 2},
			})
			scheduler := &defaultOpenAIAccountScheduler{service: gateway, stats: newOpenAIAccountRuntimeStats()}
			selection, _, err := scheduler.Select(t.Context(), OpenAIAccountScheduleRequest{
				GroupID: &groupID, Platform: PlatformOpenAI, RequestedModel: "gpt-test", UseUpstreamTokenCost: true,
			})
			require.NoError(t, err)
			require.True(t, selection.Acquired)
			defer selection.ReleaseFunc()
			if peerAvailable {
				require.Equal(t, int64(2), selection.Account.ID)
				require.Equal(t, []int64{2}, acquired)
			} else {
				require.Equal(t, int64(1), selection.Account.ID, "soft reservation must not cause needless queueing")
				require.Equal(t, []int64{2, 1}, acquired)
			}
		})
	}
}

func TestAccountSessionReservationCapacityWeightIncludesIdleSessions(t *testing.T) {
	item := priorityCandidate(1, 0.1, 0)
	item.account.Concurrency = 10
	before := prioritySelectionWeight(item, time.Now())
	item.loadInfo.ReservedConcurrency = 5
	require.Equal(t, 5, priorityCapacityBand(item))
	require.Less(t, prioritySelectionWeight(item, time.Now()), before)
	require.Zero(t, item.loadInfo.CurrentConcurrency)
	score := scorePriorityCandidate(DefaultPrioritySchedulingConfig(), item, PrioritySchedulingSignal{}, time.Now())
	require.Equal(t, 0, *score.LoadPercent, "admin occupancy must remain the real in-flight count")
	require.Equal(t, 50, *score.RoutingLoadPercent)
	require.Equal(t, 5, score.ReservedConcurrency)
}

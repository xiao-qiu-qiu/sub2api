package service

import (
	"context"
	"sync"
	"time"
)

// AccountSessionReservationCache observes admitted session activity. Reservations
// are short-lived routing hints, never real slots or an admission authority.
// Implementations must exclude active sessions and bound idle state and lifetime.
type AccountSessionReservationCache interface {
	StartAccountSession(ctx context.Context, accountID int64, maxConcurrency int, requestID, sessionHash string, persistentRoute bool) error
	EndAccountSession(ctx context.Context, accountID int64, requestID, sessionHash string) error
	GetAccountSessionReservations(ctx context.Context, accounts []AccountWithConcurrency) (map[int64]int, error)
}

const accountSessionObservationTimeout = 150 * time.Millisecond

// Observe only successful slot acquisitions. Polls, failed admission, unlimited
// accounts and requests without a trustworthy session identity create no state.
// The release owner, including rotation's renewal owner, remains authoritative.
func (s *ConcurrencyService) trackAccountSession(ctx context.Context, accountID int64, maxConcurrency int, result *AcquireResult) {
	if s == nil || result == nil || !result.Acquired || result.RequestID == "" || result.ReleaseFunc == nil || maxConcurrency <= 1 {
		return
	}
	affinity, ok := AccountSessionAffinityFromContext(ctx)
	if !ok {
		return
	}
	cache, ok := s.cache.(AccountSessionReservationCache)
	if !ok {
		return
	}
	observeCtx, cancel := context.WithTimeout(ctx, accountSessionObservationTimeout)
	// A lost response may still have applied the observation. Always retain the
	// matching cleanup, even when the optional write fails.
	_ = cache.StartAccountSession(observeCtx, accountID, maxConcurrency, result.RequestID, affinity.Key, affinity.PersistentRoute)
	cancel()
	release := result.ReleaseFunc
	result.ReleaseFunc = sync.OnceFunc(func() {
		// Keep the real slot during bookkeeping: the load snapshot may temporarily
		// overestimate occupancy, but cannot mistake an active turn for free space.
		defer release()
		endCtx, endCancel := context.WithTimeout(context.Background(), accountSessionObservationTimeout)
		defer endCancel()
		_ = cache.EndAccountSession(endCtx, accountID, result.RequestID, affinity.Key)
	})
}

func (s *ConcurrencyService) addAccountSessionReservations(ctx context.Context, accounts []AccountWithConcurrency, loads map[int64]*AccountLoadInfo) map[int64]*AccountLoadInfo {
	cache, ok := s.cache.(AccountSessionReservationCache)
	if !ok || len(loads) == 0 {
		return loads
	}
	observeCtx, cancel := context.WithTimeout(ctx, accountSessionObservationTimeout)
	defer cancel()
	reserved, err := cache.GetAccountSessionReservations(observeCtx, accounts)
	if err != nil || len(reserved) == 0 {
		return loads
	}
	// The underlying cache may share immutable snapshots. Never mutate them.
	loads = cloneAccountLoadMap(loads)
	for _, account := range accounts {
		if load := loads[account.ID]; load != nil {
			load.ReservedConcurrency = min(max(0, reserved[account.ID]), min(max(0, account.MaxConcurrency/2), 64))
		}
	}
	return loads
}

func (l *AccountLoadInfo) schedulingConcurrency() int {
	if l == nil {
		return 0
	}
	return max(0, l.CurrentConcurrency) + max(0, l.ReservedConcurrency)
}

// Keep raw concurrency/LoadRate intact for admission and admin statistics. Only
// routing preferences include idle reservations, so a busy pool can borrow them.
func accountSchedulingLoadRate(account *Account, load *AccountLoadInfo) int {
	if load == nil {
		return 0
	}
	rate := load.LoadRate
	if account != nil && account.Concurrency > 0 && load.ReservedConcurrency > 0 {
		rate += load.ReservedConcurrency * 100 / account.Concurrency
	}
	return accountLoadPreferenceRate(account, rate)
}

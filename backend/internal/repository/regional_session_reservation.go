package repository

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Regional admission stays on the SQL slot authority. Redis activity is only a
// local-region routing hint; it never claims or changes a regional slot lease.
func (c *regionalConcurrencyCache) StartAccountSession(ctx context.Context, accountID int64, maxConcurrency int, requestID, sessionHash string, persistentRoute bool) error {
	if cache, ok := c.ConcurrencyCache.(service.AccountSessionReservationCache); ok {
		return cache.StartAccountSession(ctx, accountID, maxConcurrency, requestID, sessionHash, persistentRoute)
	}
	return nil
}

func (c *regionalConcurrencyCache) EndAccountSession(ctx context.Context, accountID int64, requestID, sessionHash string) error {
	if cache, ok := c.ConcurrencyCache.(service.AccountSessionReservationCache); ok {
		return cache.EndAccountSession(ctx, accountID, requestID, sessionHash)
	}
	return nil
}

func (c *regionalConcurrencyCache) GetAccountSessionReservations(ctx context.Context, accounts []service.AccountWithConcurrency) (map[int64]int, error) {
	if cache, ok := c.ConcurrencyCache.(service.AccountSessionReservationCache); ok {
		return cache.GetAccountSessionReservations(ctx, accounts)
	}
	return nil, nil
}

var _ service.AccountSessionReservationCache = (*regionalConcurrencyCache)(nil)

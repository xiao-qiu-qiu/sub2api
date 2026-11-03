package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// PeekAccountRotation runs the shared state machine in projection mode. Unlike
// CheckAccountRotation it neither advances a work/rest clock nor cleans leases.
// A rest whose start still needs persistence returns an error so the caller
// reports unknown instead of indefinitely projecting a new rest deadline.
func (c *concurrencyCache) PeekAccountRotation(ctx context.Context, accountID int64, policy service.AccountRotationPolicy) (*service.AccountRotationState, error) {
	if err := validateAccountRotationPolicy(policy); err != nil {
		return nil, err
	}
	raw, err := accountRotationScript.Run(ctx, c.rdb, accountRotationKeys(accountID),
		c.accountRotationArgs("peek", accountID, "", 0, false, "", policy)...).Text()
	if err != nil {
		return nil, err
	}
	return decodeAccountRotationState(raw)
}

// ReadRoutingAccountLoads counts only live leases, including Live sessions,
// without deleting expired members or refreshing any TTL.
func (c *concurrencyCache) ReadRoutingAccountLoads(ctx context.Context, accounts []service.AccountWithConcurrency) (map[int64]*service.AccountLoadInfo, error) {
	now, err := c.rdb.Time(ctx).Result()
	if err != nil {
		return nil, err
	}
	pipe := c.rdb.Pipeline()
	type commands struct {
		account       service.AccountWithConcurrency
		regular, live *redis.IntCmd
		waiting       *redis.StringCmd
	}
	pending := make([]commands, 0, len(accounts))
	for _, account := range accounts {
		pending = append(pending, commands{
			account: account,
			regular: pipe.ZCount(ctx, accountSlotKey(account.ID), "("+strconv.FormatInt(now.Unix()-int64(c.slotTTLSeconds), 10), "+inf"),
			live:    pipe.ZCount(ctx, liveAccountSlotKey(account.ID), "("+strconv.FormatInt(now.Unix()-liveLeaseTTLSeconds, 10), "+inf"),
			waiting: pipe.Get(ctx, accountWaitKeyPrefix+strconv.FormatInt(account.ID, 10)),
		})
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	loads := make(map[int64]*service.AccountLoadInfo, len(pending))
	for _, item := range pending {
		regular, err := item.regular.Result()
		if err != nil {
			return nil, err
		}
		live, err := item.live.Result()
		if err != nil {
			return nil, err
		}
		waiting, err := item.waiting.Int()
		if errors.Is(err, redis.Nil) {
			waiting, err = 0, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read account waiting count: %w", err)
		}
		load := &service.AccountLoadInfo{AccountID: item.account.ID, CurrentConcurrency: int(regular + live), WaitingCount: waiting}
		if item.account.MaxConcurrency > 0 {
			load.LoadRate = (load.CurrentConcurrency + waiting) * 100 / item.account.MaxConcurrency
		}
		loads[item.account.ID] = load
	}
	return loads, nil
}

package repository

import (
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAccountRotationLiveOwnerRefreshAfterAccountRelease(t *testing.T) {
	f := newAccountRotationCacheFixture(t)
	ctx := t.Context()
	ok, err := f.cache.AcquireLiveLease(ctx, 1, 1, 2, 1, 3, 1, "joint", false)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, f.cache.ReleaseLiveLeaseAccount(ctx, 1, "joint"))
	require.False(t, f.server.Exists(liveAccountSlotKey(1)))
	// Deliberate account0 sentinels prove owner renewal does not consult or
	// mutate a synthetic account, its rotation hash, waiter or active index.
	for _, key := range accountRotationKeys(0) {
		require.NoError(t, f.client.Set(ctx, key, "untouched", 24*time.Hour).Err())
	}
	for range 3 {
		f.advance(50)
		before := make(map[string]string)
		for _, key := range accountRotationKeys(0) {
			before[key] = f.client.Dump(ctx, key).Val()
		}
		ok, err = f.cache.RefreshLiveLease(ctx, 0, 2, 3, "joint")
		require.NoError(t, err)
		require.True(t, ok)
		for _, key := range []string{liveUserSlotKey(2), liveAPIKeySlotKey(3)} {
			require.Equal(t, float64(f.now), f.client.ZScore(ctx, key, "joint").Val())
			require.Equal(t, 60*time.Second, f.client.TTL(ctx, key).Val())
			require.Equal(t, int64(1), f.client.ZCard(ctx, key).Val())
		}
		for key, dump := range before {
			require.Equal(t, dump, f.client.Dump(ctx, key).Val())
		}
		require.False(t, f.server.Exists(liveAccountSlotKey(1)), "failed account must remain drained")
	}
	f.advance(60)
	ok, err = f.cache.RefreshLiveLease(ctx, 0, 2, 3, "joint")
	require.NoError(t, err)
	require.False(t, ok, "expired owners must not revive")
	require.False(t, f.server.Exists(liveUserSlotKey(2)))
	require.False(t, f.server.Exists(liveAPIKeySlotKey(3)))
}

func TestAccountRotationLiveOwnerRefreshMissingOrExpiredFailsAtomically(t *testing.T) {
	for _, owner := range []string{"user", "key"} {
		for _, failure := range []string{"key_missing", "member_missing", "expired"} {
			t.Run(owner+"/"+failure, func(t *testing.T) {
				f := newAccountRotationCacheFixture(t)
				ctx := t.Context()
				ok, err := f.cache.AcquireLiveLease(ctx, 1, 1, 2, 1, 3, 1, "joint", false)
				require.NoError(t, err)
				require.True(t, ok)
				require.NoError(t, f.cache.ReleaseLiveLeaseAccount(ctx, 1, "joint"))
				f.advance(20)
				key := liveUserSlotKey(2)
				if owner == "key" {
					key = liveAPIKeySlotKey(3)
				}
				switch failure {
				case "key_missing":
					require.NoError(t, f.client.Del(ctx, key).Err())
				case "member_missing":
					require.NoError(t, f.client.ZAdd(ctx, key, redis.Z{Score: float64(f.now), Member: "other-live"}).Err())
					require.NoError(t, f.client.ZRem(ctx, key, "joint").Err())
				case "expired":
					require.NoError(t, f.client.ZAdd(ctx, key, redis.Z{Score: float64(f.now - liveLeaseTTLSeconds), Member: "joint"}).Err())
				}
				before := f.server.Dump()
				ok, err = f.cache.RefreshLiveLease(ctx, 0, 2, 3, "joint")
				require.NoError(t, err)
				require.False(t, ok)
				require.Equal(t, before, f.server.Dump(), "missing owner must not partially renew its surviving peer")
				require.False(t, f.server.Exists(accountRotationKey(0)))
				require.False(t, f.server.Exists(liveAccountSlotKey(0)))
			})
		}
	}
}

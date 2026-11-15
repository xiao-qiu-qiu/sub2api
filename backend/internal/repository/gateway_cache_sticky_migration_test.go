package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newStickySessionMigrationTestCache(t *testing.T) (*gatewayCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	cache, ok := NewGatewayCache(client).(*gatewayCache)
	require.True(t, ok)
	_, ok = any(cache).(service.StickySessionMigrationCache)
	require.True(t, ok)
	return cache, mr
}

func stickySessionMigrationRevision(t *testing.T, cache *gatewayCache, groupID int64, primary, legacy string) string {
	t.Helper()
	_, revision, err := cache.ReadSessionAccountBinding(context.Background(), groupID, primary, legacy)
	require.NoError(t, err)
	return revision
}

func TestGatewayCacheStickySessionMigrationSuccessAndIsolation(t *testing.T) {
	cache, mr := newStickySessionMigrationTestCache(t)
	ctx := context.Background()
	const primary = "openai:session-fixture"
	const legacy = "openai:legacy-fixture"
	for _, binding := range []struct {
		group int64
		hash  string
		owner int64
	}{
		{7, primary, 101},
		{7, legacy, 101},
		{8, primary, 101},
		{8, legacy, 101},
		{7, "openai:response:response-fixture", 301},
		{7, "openai:http-response-owner:user:fixture", 401},
	} {
		require.NoError(t, cache.SetSessionAccountID(ctx, binding.group, binding.hash, binding.owner, time.Hour))
	}

	primaryTTL := 30*time.Minute + 321*time.Millisecond
	legacyTTL := 5*time.Minute + 123*time.Millisecond
	revision := stickySessionMigrationRevision(t, cache, 7, primary, legacy)
	migrated, err := cache.CompareAndSwapSessionAccountID(ctx, 7, primary, legacy, 101, revision, 201, primaryTTL, legacyTTL)
	require.NoError(t, err)
	require.True(t, migrated)
	for _, hash := range []string{primary, legacy} {
		owner, err := cache.GetSessionAccountID(ctx, 7, hash)
		require.NoError(t, err)
		require.EqualValues(t, 201, owner)
		owner, err = cache.GetSessionAccountID(ctx, 8, hash)
		require.NoError(t, err)
		require.EqualValues(t, 101, owner)
		require.Equal(t, time.Hour, mr.TTL(buildSessionKey(8, hash)))
	}
	require.Equal(t, primaryTTL, mr.TTL(buildSessionKey(7, primary)))
	require.Equal(t, legacyTTL, mr.TTL(buildSessionKey(7, legacy)))
	for hash, want := range map[string]int64{
		"openai:response:response-fixture":        301,
		"openai:http-response-owner:user:fixture": 401,
	} {
		owner, err := cache.GetSessionAccountID(ctx, 7, hash)
		require.NoError(t, err)
		require.Equal(t, want, owner)
		require.Equal(t, time.Hour, mr.TTL(buildSessionKey(7, hash)))
	}
	mr.FastForward(legacyTTL)
	require.False(t, mr.Exists(buildSessionKey(7, legacy)))
	require.True(t, mr.Exists(buildSessionKey(7, primary)))
	mr.FastForward(primaryTTL - legacyTTL)
	require.False(t, mr.Exists(buildSessionKey(7, primary)))
}

func TestGatewayCacheStickySessionMigrationConcurrentWinnerAndLateResult(t *testing.T) {
	cache, mr := newStickySessionMigrationTestCache(t)
	ctx := context.Background()
	const primary = "openai:concurrent-fixture"
	const legacy = "openai:concurrent-legacy-fixture"
	require.NoError(t, cache.SetSessionAccountID(ctx, 7, primary, 101, time.Hour))
	require.NoError(t, cache.SetSessionAccountID(ctx, 7, legacy, 101, time.Hour))
	revision := stickySessionMigrationRevision(t, cache, 7, primary, legacy)

	type result struct {
		owner    int64
		migrated bool
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for _, owner := range []int64{201, 301} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			migrated, err := cache.CompareAndSwapSessionAccountID(ctx, 7, primary, legacy, 101, revision, owner, 30*time.Minute, 5*time.Minute)
			results <- result{owner, migrated, err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	var winner int64
	for result := range results {
		require.NoError(t, result.err)
		if result.migrated {
			require.Zero(t, winner, "only one request may replace the expected owner")
			winner = result.owner
		}
	}
	require.NotZero(t, winner)
	for _, hash := range []string{primary, legacy} {
		owner, err := cache.GetSessionAccountID(ctx, 7, hash)
		require.NoError(t, err)
		require.Equal(t, winner, owner)
	}

	mr.FastForward(time.Minute)
	migrated, err := cache.CompareAndSwapSessionAccountID(ctx, 7, primary, legacy, 101, revision, 401, time.Hour, time.Hour)
	require.NoError(t, err)
	require.False(t, migrated, "a late successful request must not restore its stale owner")
	for _, hash := range []string{primary, legacy} {
		owner, err := cache.GetSessionAccountID(ctx, 7, hash)
		require.NoError(t, err)
		require.Equal(t, winner, owner)
	}
	require.Equal(t, 29*time.Minute, mr.TTL(buildSessionKey(7, primary)))
	require.Equal(t, 4*time.Minute, mr.TTL(buildSessionKey(7, legacy)))
}

func TestGatewayCacheStickySessionMigrationLegacyCompatibility(t *testing.T) {
	for _, test := range []struct {
		name       string
		primary    string
		legacy     string
		want       bool
		wantOwner  string
		wantLegacy string
	}{
		{"legacy-only", "", "101", true, "201", "201"},
		{"primary-precedes-stale-legacy", "301", "101", false, "301", "101"},
		{"primary-precedes-newer-legacy", "101", "301", true, "201", "201"},
		{"primary-with-missing-legacy", "101", "", true, "201", "201"},
		{"both-missing", "", "", false, "", ""},
		{"legacy-owner-mismatch", "", "301", false, "", "301"},
		{"invalid-primary-does-not-use-legacy", "invalid", "101", false, "invalid", "101"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache, mr := newStickySessionMigrationTestCache(t)
			primaryKey := buildSessionKey(7, "openai:current")
			legacyKey := buildSessionKey(7, "openai:legacy")
			for key, value := range map[string]string{primaryKey: test.primary, legacyKey: test.legacy} {
				if value != "" {
					require.NoError(t, mr.Set(key, value))
					mr.SetTTL(key, time.Hour)
				}
			}
			migrated, err := cache.CompareAndSwapSessionAccountID(context.Background(), 7, "openai:current", "openai:legacy", 101, "", 201, 30*time.Minute, 5*time.Minute)
			require.NoError(t, err)
			require.Equal(t, test.want, migrated)
			for key, want := range map[string]string{primaryKey: test.wantOwner, legacyKey: test.wantLegacy} {
				if want == "" {
					require.False(t, mr.Exists(key))
					continue
				}
				got, err := mr.Get(key)
				require.NoError(t, err)
				require.Equal(t, want, got)
				if !test.want {
					require.Equal(t, time.Hour, mr.TTL(key), "failed comparisons must not refresh either format")
				}
			}
		})
	}
}

func TestGatewayCacheStickySessionMigrationDeletedBindingsStayMissing(t *testing.T) {
	cache, mr := newStickySessionMigrationTestCache(t)
	ctx := context.Background()
	for _, hash := range []string{"openai:current", "openai:legacy"} {
		require.NoError(t, cache.SetSessionAccountID(ctx, 7, hash, 101, time.Hour))
		require.NoError(t, cache.DeleteSessionAccountID(ctx, 7, hash))
	}
	migrated, err := cache.CompareAndSwapSessionAccountID(ctx, 7, "openai:current", "openai:legacy", 101, "", 201, time.Hour, time.Minute)
	require.NoError(t, err)
	require.False(t, migrated)
	require.Empty(t, mr.Keys(), "a late migration cannot recreate cleared bindings")
}

func TestGatewayCacheStickySessionMigrationPrimaryDeletionStillAllowsLegacyFallback(t *testing.T) {
	cache, mr := newStickySessionMigrationTestCache(t)
	ctx := context.Background()
	for _, hash := range []string{"openai:current", "openai:legacy"} {
		require.NoError(t, cache.SetSessionAccountID(ctx, 7, hash, 101, time.Hour))
	}
	require.NoError(t, cache.DeleteSessionAccountID(ctx, 7, "openai:current"))
	revision := stickySessionMigrationRevision(t, cache, 7, "openai:current", "openai:legacy")
	migrated, err := cache.CompareAndSwapSessionAccountID(ctx, 7, "openai:current", "openai:legacy", 101, revision, 201, 30*time.Minute, 5*time.Minute)
	require.NoError(t, err)
	require.True(t, migrated, "a surviving legacy binding remains a valid fallback; clearing a session must delete both formats")
	for _, hash := range []string{"openai:current", "openai:legacy"} {
		owner, err := cache.GetSessionAccountID(ctx, 7, hash)
		require.NoError(t, err)
		require.EqualValues(t, 201, owner)
	}
	require.Equal(t, 30*time.Minute, mr.TTL(buildSessionKey(7, "openai:current")))
	require.Equal(t, 5*time.Minute, mr.TTL(buildSessionKey(7, "openai:legacy")))
}

func TestGatewayCacheStickySessionMigrationWithoutLegacyAndLargeAccountIDs(t *testing.T) {
	cache, mr := newStickySessionMigrationTestCache(t)
	ctx := context.Background()
	const expected int64 = 9007199254740993
	const newOwner int64 = 9007199254740995
	require.NoError(t, cache.SetSessionAccountID(ctx, 7, "openai:current", expected, time.Hour))
	require.NoError(t, cache.SetSessionAccountID(ctx, 7, "openai:legacy", expected, time.Hour))
	revision := stickySessionMigrationRevision(t, cache, 7, "openai:current", "")
	migrated, err := cache.CompareAndSwapSessionAccountID(ctx, 7, "openai:current", "", expected-1, revision, newOwner, time.Minute, 0)
	require.NoError(t, err)
	require.False(t, migrated, "account IDs must be compared as exact integers, not Lua floating-point numbers")
	migrated, err = cache.CompareAndSwapSessionAccountID(ctx, 7, "openai:current", "", expected, revision, newOwner, time.Minute, 0)
	require.NoError(t, err)
	require.True(t, migrated)
	owner, err := cache.GetSessionAccountID(ctx, 7, "openai:current")
	require.NoError(t, err)
	require.Equal(t, newOwner, owner)
	owner, err = cache.GetSessionAccountID(ctx, 7, "openai:legacy")
	require.NoError(t, err)
	require.Equal(t, expected, owner, "an omitted legacy hash must not write another key")
	require.Equal(t, time.Hour, mr.TTL(buildSessionKey(7, "openai:legacy")))

	revision = stickySessionMigrationRevision(t, cache, 7, "openai:current", "")
	migrated, err = cache.CompareAndSwapSessionAccountID(ctx, 7, "openai:current", "openai:current", newOwner, revision, expected, 2*time.Minute, time.Second)
	require.NoError(t, err)
	require.True(t, migrated)
	require.Equal(t, 2*time.Minute, mr.TTL(buildSessionKey(7, "openai:current")), "duplicate hashes must not shorten the primary TTL")
}

func TestGatewayCacheStickySessionMigrationErrorsDoNotWrite(t *testing.T) {
	cache, mr := newStickySessionMigrationTestCache(t)
	ctx := context.Background()
	const primary = "openai:current"
	const legacy = "openai:legacy"
	require.NoError(t, cache.SetSessionAccountID(ctx, 7, primary, 101, time.Hour))
	require.NoError(t, cache.SetSessionAccountID(ctx, 7, legacy, 101, time.Hour))
	revision := stickySessionMigrationRevision(t, cache, 7, primary, legacy)
	mr.SetError("synthetic cache failure")
	migrated, err := cache.CompareAndSwapSessionAccountID(ctx, 7, primary, legacy, 101, revision, 201, time.Minute, time.Minute)
	require.Error(t, err)
	require.False(t, migrated)
	mr.SetError("")
	for _, hash := range []string{primary, legacy} {
		owner, err := cache.GetSessionAccountID(ctx, 7, hash)
		require.NoError(t, err)
		require.EqualValues(t, 101, owner, "Redis errors cannot degrade into an unconditional SET")
		require.Equal(t, time.Hour, mr.TTL(buildSessionKey(7, hash)))
	}

	mr.Del(buildSessionKey(7, primary))
	mr.HSet(buildSessionKey(7, primary), "wrong", "type")
	migrated, err = cache.CompareAndSwapSessionAccountID(ctx, 7, primary, legacy, 101, revision, 201, time.Minute, time.Minute)
	require.Error(t, err)
	require.False(t, migrated)
	require.Equal(t, "type", mr.HGet(buildSessionKey(7, primary), "wrong"))
	owner, err := cache.GetSessionAccountID(ctx, 7, legacy)
	require.NoError(t, err)
	require.EqualValues(t, 101, owner)
}

func TestGatewayCacheStickySessionMigrationRejectsInvalidInput(t *testing.T) {
	cache, mr := newStickySessionMigrationTestCache(t)
	for _, test := range []struct {
		name     string
		primary  string
		legacy   string
		expected int64
		owner    int64
		ttl      time.Duration
		oldTTL   time.Duration
	}{
		{"empty-hash", "", "", 1, 2, time.Minute, 0},
		{"blank-hash", " ", "", 1, 2, time.Minute, 0},
		{"missing-expected", "openai:a", "", 0, 2, time.Minute, 0},
		{"negative-expected", "openai:a", "", -1, 2, time.Minute, 0},
		{"missing-owner", "openai:a", "", 1, 0, time.Minute, 0},
		{"missing-ttl", "openai:a", "", 1, 2, 0, 0},
		{"sub-millisecond-ttl", "openai:a", "", 1, 2, time.Microsecond, 0},
		{"negative-ttl", "openai:a", "", 1, 2, -time.Second, 0},
		{"blank-legacy", "openai:a", " ", 1, 2, time.Minute, time.Minute},
		{"missing-legacy-ttl", "openai:a", "openai:b", 1, 2, time.Minute, 0},
		{"sub-millisecond-legacy-ttl", "openai:a", "openai:b", 1, 2, time.Minute, time.Microsecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			migrated, err := cache.CompareAndSwapSessionAccountID(context.Background(), 7, test.primary, test.legacy, test.expected, "", test.owner, test.ttl, test.oldTTL)
			require.Error(t, err)
			require.False(t, migrated)
			require.Empty(t, mr.Keys())
		})
	}
	for _, unavailable := range []*gatewayCache{nil, {}} {
		migrated, err := unavailable.CompareAndSwapSessionAccountID(context.Background(), 7, "openai:a", "", 1, "", 2, time.Minute, 0)
		require.Error(t, err)
		require.False(t, migrated)
	}
}

func TestGatewayCacheStickySessionMigrationRevisionRejectsABA(t *testing.T) {
	for _, transition := range []string{"owner-cycle", "delete-recreate", "same-owner-write", "admin-clear-recreate"} {
		t.Run(transition, func(t *testing.T) {
			cache, mr := newStickySessionMigrationTestCache(t)
			ctx := context.Background()
			const hash = "openai:aba-fixture"
			require.NoError(t, cache.SetSessionAccountID(ctx, 7, hash, 101, time.Hour))
			revision := stickySessionMigrationRevision(t, cache, 7, hash, "")
			require.NotEmpty(t, revision)
			switch transition {
			case "owner-cycle":
				require.NoError(t, cache.SetSessionAccountID(ctx, 7, hash, 201, time.Hour))
			case "delete-recreate":
				require.NoError(t, cache.DeleteSessionAccountID(ctx, 7, hash))
				require.False(t, mr.Exists(buildSessionRevisionKey(7, hash)))
			case "admin-clear-recreate":
				deleted, err := cache.ClearStickySessionBindings(ctx, 7, nil)
				require.NoError(t, err)
				require.Equal(t, 1, deleted, "admin clear counts owners, not revision sidekeys")
				require.False(t, mr.Exists(buildSessionRevisionKey(7, hash)))
			}
			require.NoError(t, cache.SetSessionAccountID(ctx, 7, hash, 101, 30*time.Minute))
			latestRevision := stickySessionMigrationRevision(t, cache, 7, hash, "")
			require.NotEqual(t, revision, latestRevision)
			migrated, err := cache.CompareAndSwapSessionAccountID(ctx, 7, hash, "", 101, revision, 301, time.Hour, 0)
			require.NoError(t, err)
			require.False(t, migrated, "same owner in a newer generation must reject a stale request")
			owner, gotRevision, err := cache.ReadSessionAccountBinding(ctx, 7, hash, "")
			require.NoError(t, err)
			require.EqualValues(t, 101, owner)
			require.Equal(t, latestRevision, gotRevision)
			require.Equal(t, 30*time.Minute, mr.TTL(buildSessionKey(7, hash)))
			require.Equal(t, 30*time.Minute, mr.TTL(buildSessionRevisionKey(7, hash)))
		})
	}
}

func TestGatewayCacheStickySessionMigrationRevisionTTLAndLegacyUpgrade(t *testing.T) {
	cache, mr := newStickySessionMigrationTestCache(t)
	ctx := context.Background()
	const primary = "openai:current"
	const legacy = "openai:legacy"
	// An existing installation has only owner values. Reading it must not
	// mutate the record or give an arbitrary generation to an in-flight request.
	require.NoError(t, mr.Set(buildSessionKey(7, legacy), "101"))
	mr.SetTTL(buildSessionKey(7, legacy), time.Minute)
	owner, revision, err := cache.ReadSessionAccountBinding(ctx, 7, primary, legacy)
	require.NoError(t, err)
	require.EqualValues(t, 101, owner)
	require.Empty(t, revision)
	require.False(t, mr.Exists(buildSessionRevisionKey(7, legacy)))
	moved, err := cache.CompareAndSwapSessionAccountID(ctx, 7, primary, legacy, owner, revision, 201, 30*time.Minute, 5*time.Minute)
	require.NoError(t, err)
	require.True(t, moved)
	_, currentRevision, err := cache.ReadSessionAccountBinding(ctx, 7, primary, legacy)
	require.NoError(t, err)
	require.NotEmpty(t, currentRevision)
	_, legacyRevision, err := cache.ReadSessionAccountBinding(ctx, 7, legacy, "")
	require.NoError(t, err)
	require.Equal(t, currentRevision, legacyRevision, "one migration uses the same generation in both formats")
	for _, key := range []string{buildSessionKey(7, primary), buildSessionRevisionKey(7, primary)} {
		require.Equal(t, 30*time.Minute, mr.TTL(key))
	}
	for _, key := range []string{buildSessionKey(7, legacy), buildSessionRevisionKey(7, legacy)} {
		require.Equal(t, 5*time.Minute, mr.TTL(key))
	}
	mr.FastForward(time.Minute)
	require.NoError(t, cache.RefreshSessionTTL(ctx, 7, primary, 7*time.Minute))
	_, refreshedRevision, err := cache.ReadSessionAccountBinding(ctx, 7, primary, "")
	require.NoError(t, err)
	require.Equal(t, currentRevision, refreshedRevision, "a TTL refresh does not change the binding generation")
	require.Equal(t, 7*time.Minute, mr.TTL(buildSessionKey(7, primary)))
	require.Equal(t, 7*time.Minute, mr.TTL(buildSessionRevisionKey(7, primary)))
	mr.FastForward(4 * time.Minute)
	require.False(t, mr.Exists(buildSessionKey(7, legacy)))
	require.False(t, mr.Exists(buildSessionRevisionKey(7, legacy)))
	mr.FastForward(3 * time.Minute)
	_, _, err = cache.ReadSessionAccountBinding(ctx, 7, primary, legacy)
	require.ErrorIs(t, err, service.ErrStickySessionNotFound)
	require.Empty(t, mr.Keys())

	// Zero/negative expiration preserves the Redis SET persistence behavior.
	for _, ttl := range []time.Duration{0, -2 * time.Second} {
		require.NoError(t, cache.SetSessionAccountID(ctx, 7, primary, 101, ttl))
		require.Equal(t, time.Duration(0), mr.TTL(buildSessionKey(7, primary)))
		require.Equal(t, time.Duration(0), mr.TTL(buildSessionRevisionKey(7, primary)))
	}
	require.NoError(t, cache.SetSessionAccountID(ctx, 7, primary, 101, time.Minute))
	require.NoError(t, cache.SetSessionAccountID(ctx, 7, primary, 101, redis.KeepTTL))
	require.Equal(t, time.Minute, mr.TTL(buildSessionKey(7, primary)))
	require.Equal(t, time.Minute, mr.TTL(buildSessionRevisionKey(7, primary)))
	require.NoError(t, cache.RefreshSessionTTL(ctx, 7, primary, 0))
	require.False(t, mr.Exists(buildSessionKey(7, primary)))
	require.False(t, mr.Exists(buildSessionRevisionKey(7, primary)))
}

func TestGatewayCacheStickySessionMigrationBindingReadErrorsAndAuthority(t *testing.T) {
	cache, mr := newStickySessionMigrationTestCache(t)
	ctx := context.Background()
	const primary = "openai:current"
	const legacy = "openai:legacy"
	_, _, err := cache.ReadSessionAccountBinding(ctx, 7, primary, legacy)
	require.ErrorIs(t, err, service.ErrStickySessionNotFound)
	require.NoError(t, cache.SetSessionAccountID(ctx, 7, primary, 101, time.Hour))
	require.NoError(t, cache.SetSessionAccountID(ctx, 7, legacy, 201, time.Hour))
	owner, revision, err := cache.ReadSessionAccountBinding(ctx, 7, primary, legacy)
	require.NoError(t, err)
	require.EqualValues(t, 101, owner)
	mr.SetError("synthetic cache failure")
	_, _, err = cache.ReadSessionAccountBinding(ctx, 7, primary, legacy)
	require.Error(t, err)
	mr.SetError("")
	for _, invalid := range []string{"invalid", "0", "-1"} {
		require.NoError(t, mr.Set(buildSessionKey(7, primary), invalid))
		_, _, err := cache.ReadSessionAccountBinding(ctx, 7, primary, legacy)
		require.Error(t, err, "an invalid primary owner must not silently fall back to legacy")
	}
	require.NoError(t, mr.Set(buildSessionKey(7, primary), "101"))
	mr.Del(buildSessionRevisionKey(7, primary))
	mr.HSet(buildSessionRevisionKey(7, primary), "wrong", "type")
	_, _, err = cache.ReadSessionAccountBinding(ctx, 7, primary, legacy)
	require.Error(t, err)
	moved, err := cache.CompareAndSwapSessionAccountID(ctx, 7, primary, legacy, 101, revision, 301, time.Minute, time.Minute)
	require.Error(t, err)
	require.False(t, moved)
	got, err := mr.Get(buildSessionKey(7, primary))
	require.NoError(t, err)
	require.Equal(t, "101", got)
	got, err = mr.Get(buildSessionKey(7, legacy))
	require.NoError(t, err)
	require.Equal(t, "201", got)
}

func TestGatewayCacheStickySessionMigrationAdminClearRemovesRevisionWithoutCountingIt(t *testing.T) {
	cache, mr := newStickySessionMigrationTestCache(t)
	ctx := context.Background()
	for _, binding := range []struct {
		group int64
		hash  string
		owner int64
	}{
		{7, "openai:a", 101}, {7, "openai:b", 201},
		{8, "openai:a", 101}, {7, "openai:response:fixture", 101},
		{7, "openai:http-response-owner:fixture", 101},
	} {
		require.NoError(t, cache.SetSessionAccountID(ctx, binding.group, binding.hash, binding.owner, time.Hour))
	}
	counts, err := cache.CountStickySessionBindings(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, map[int64]int{101: 1, 201: 1}, counts)
	deleted, err := cache.ClearStickySessionBindings(ctx, 7, map[int64]struct{}{101: {}})
	require.NoError(t, err)
	require.Equal(t, 1, deleted)
	require.False(t, mr.Exists(buildSessionKey(7, "openai:a")))
	require.False(t, mr.Exists(buildSessionRevisionKey(7, "openai:a")))
	for _, identity := range []struct {
		group        int64
		hash         string
		wantRevision bool
	}{
		{7, "openai:b", true}, {8, "openai:a", true},
		{7, "openai:response:fixture", false}, {7, "openai:http-response-owner:fixture", false},
	} {
		require.True(t, mr.Exists(buildSessionKey(identity.group, identity.hash)))
		require.Equal(t, identity.wantRevision, mr.Exists(buildSessionRevisionKey(identity.group, identity.hash)))
	}
}

func TestGatewayCacheStickySessionMigrationReservedOwnersKeepOriginalRedisBehavior(t *testing.T) {
	for _, hash := range []string{"openai:response:fixture", "openai:http-response-owner:user:fixture"} {
		t.Run(hash, func(t *testing.T) {
			cache, mr := newStickySessionMigrationTestCache(t)
			ctx := context.Background()
			ownerKey, revisionKey := buildSessionKey(7, hash), buildSessionRevisionKey(7, hash)
			require.NoError(t, cache.SetSessionAccountID(ctx, 7, hash, 101, time.Hour))
			require.False(t, mr.Exists(revisionKey), "strong ownership keys do not need migration revisions")
			require.Equal(t, time.Hour, mr.TTL(ownerKey))
			require.NoError(t, cache.RefreshSessionTTL(ctx, 7, hash, 2*time.Hour))
			require.Equal(t, 2*time.Hour, mr.TTL(ownerKey))
			require.False(t, mr.Exists(revisionKey))
			owner, err := cache.GetSessionAccountID(ctx, 7, hash)
			require.NoError(t, err)
			require.EqualValues(t, 101, owner)
			for _, keys := range [][2]string{{hash, ""}, {"openai:soft-session", hash}} {
				_, _, err := cache.ReadSessionAccountBinding(ctx, 7, keys[0], keys[1])
				require.Error(t, err, "migration snapshots cannot consume strong ownership state")
				moved, err := cache.CompareAndSwapSessionAccountID(ctx, 7, keys[0], keys[1], 101, "", 201, time.Minute, time.Minute)
				require.Error(t, err)
				require.False(t, moved)
			}
			owner, err = cache.GetSessionAccountID(ctx, 7, hash)
			require.NoError(t, err)
			require.EqualValues(t, 101, owner)
			require.NoError(t, cache.DeleteSessionAccountID(ctx, 7, hash))
			require.False(t, mr.Exists(ownerKey))
			require.False(t, mr.Exists(revisionKey))
			require.NoError(t, cache.SetSessionAccountID(ctx, 7, hash, 101, time.Second))
			mr.FastForward(time.Second)
			require.Empty(t, mr.Keys(), "the reserved owner still expires normally without a sidekey")
		})
	}
	for _, hash := range []string{"plain-session", "anthropic:session", "gemini:session", "openai:session"} {
		t.Run(hash, func(t *testing.T) {
			cache, mr := newStickySessionMigrationTestCache(t)
			require.NoError(t, cache.SetSessionAccountID(context.Background(), 7, hash, 101, time.Hour))
			require.True(t, mr.Exists(buildSessionRevisionKey(7, hash)), "soft sessions keep revisions across namespaces")
		})
	}
}

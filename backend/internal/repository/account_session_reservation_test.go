package repository

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type accountSessionReservationFixture struct {
	t      *testing.T
	server *miniredis.Miniredis
	client *redis.Client
	cache  *concurrencyCache
	now    time.Time
}

func newAccountSessionReservationFixture(t *testing.T) *accountSessionReservationFixture {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	f := &accountSessionReservationFixture{t: t, server: server, client: client,
		cache: NewConcurrencyCache(client, 1, 60).(*concurrencyCache), now: time.Unix(1_800_000_000, 0)}
	server.SetTime(f.now)
	return f
}

func (f *accountSessionReservationFixture) advance(delta time.Duration) {
	f.t.Helper()
	f.now = f.now.Add(delta)
	f.server.FastForward(delta)
	f.server.SetTime(f.now)
}

func accountSessionTestHash(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func (f *accountSessionReservationFixture) start(id int64, max int, request, session string, persistent bool) {
	f.t.Helper()
	require.NoError(f.t, f.cache.StartAccountSession(context.Background(), id, max, request, accountSessionTestHash(session), persistent))
}

func (f *accountSessionReservationFixture) end(id int64, request, session string) {
	f.t.Helper()
	require.NoError(f.t, f.cache.EndAccountSession(context.Background(), id, request, accountSessionTestHash(session)))
}

func (f *accountSessionReservationFixture) count(id int64, max int) int {
	f.t.Helper()
	counts, err := f.cache.GetAccountSessionReservations(context.Background(), []service.AccountWithConcurrency{{ID: id, MaxConcurrency: max}})
	require.NoError(f.t, err)
	return counts[id]
}

func TestAccountSessionReservationHTTPRequiresSeparatedAdmissions(t *testing.T) {
	f := newAccountSessionReservationFixture(t)
	f.start(1, 6, "first", "http-session", false)
	f.end(1, "first", "http-session")
	require.Zero(t, f.count(1, 6))
	f.advance(10 * time.Second)
	// A retried/polled completed admission cannot turn into another request.
	for range 3 {
		f.start(1, 6, "first", "http-session", false)
		f.end(1, "first", "http-session")
	}
	require.Zero(t, f.count(1, 6))
	f.start(1, 6, "second", "http-session", false)
	require.Zero(t, f.count(1, 6), "running stable sessions are already counted by real slots")
	f.end(1, "second", "http-session")
	require.Equal(t, 1, f.count(1, 6))
	f.advance(59 * time.Second)
	require.Equal(t, 1, f.count(1, 6))
	f.end(1, "second", "http-session")
	f.start(1, 6, "second", "http-session", false)
	f.advance(time.Second)
	require.Zero(t, f.count(1, 6), "reads and duplicate release/start do not refresh the 60s idle window")

	f.start(2, 4, "early-first", "http-early", false)
	f.end(2, "early-first", "http-early")
	f.advance(10*time.Second - time.Millisecond)
	f.start(2, 4, "early-second", "http-early", false)
	f.end(2, "early-second", "http-early")
	require.Zero(t, f.count(2, 4))
	f.advance(time.Millisecond)
	f.start(2, 4, "late-third", "http-early", false)
	f.end(2, "late-third", "http-early")
	require.Equal(t, 1, f.count(2, 4))
}

func TestAccountSessionReservationSharesAdmissionsAcrossClients(t *testing.T) {
	f := newAccountSessionReservationFixture(t)
	peerClient := redis.NewClient(&redis.Options{Addr: f.server.Addr()})
	t.Cleanup(func() { require.NoError(t, peerClient.Close()) })
	peer := NewConcurrencyCache(peerClient, 1, 60).(*concurrencyCache)
	ctx := context.Background()
	session := accountSessionTestHash("shared-explicit-session")
	admit := func(cache *concurrencyCache, request string) {
		t.Helper()
		acquired, err := cache.AcquireAccountSlot(ctx, 1, 2, request)
		require.NoError(t, err)
		require.True(t, acquired)
		require.NoError(t, cache.StartAccountSession(ctx, 1, 2, request, session, false))
	}
	admit(f.cache, "first-process")
	f.advance(10 * time.Second)
	admit(peer, "second-process")
	acquired, err := peer.AcquireAccountSlot(ctx, 1, 2, "rejected")
	require.NoError(t, err)
	require.False(t, acquired)
	require.Zero(t, f.count(1, 2))
	require.NoError(t, f.cache.ReleaseAccountSlot(ctx, 1, "first-process"))
	require.NoError(t, f.cache.EndAccountSession(ctx, 1, "first-process", session))
	require.Zero(t, f.count(1, 2), "another process is still serving this session")
	require.NoError(t, peer.ReleaseAccountSlot(ctx, 1, "second-process"))
	require.NoError(t, peer.EndAccountSession(ctx, 1, "second-process", session))
	for _, cache := range []*concurrencyCache{f.cache, peer} {
		counts, err := cache.GetAccountSessionReservations(ctx, []service.AccountWithConcurrency{{ID: 1, MaxConcurrency: 2}})
		require.NoError(t, err)
		require.Equal(t, 1, counts[1])
		n, err := cache.GetAccountConcurrency(ctx, 1)
		require.NoError(t, err)
		require.Zero(t, n, "soft reservation cannot hold or resurrect a real slot")
	}
	// Idle preference may be borrowed when real capacity is available.
	acquired, err = peer.AcquireAccountSlot(ctx, 1, 2, "borrow")
	require.NoError(t, err)
	require.True(t, acquired)
}

func TestAccountSessionReservationPersistentConcurrentRelease(t *testing.T) {
	f := newAccountSessionReservationFixture(t)
	f.start(1, 8, "one", "ws", true)
	f.start(1, 8, "two", "ws", true)
	require.Zero(t, f.count(1, 8))
	f.end(1, "one", "wrong-session")
	f.end(1, "one", "ws")
	f.end(1, "one", "ws")
	require.Zero(t, f.count(1, 8))
	f.end(1, "two", "ws")
	require.Equal(t, 1, f.count(1, 8))
	f.start(1, 8, "three", "ws", true)
	require.Zero(t, f.count(1, 8), "starting removes this session's idle reservation atomically")
	f.end(1, "three", "ws")
	require.Equal(t, 1, f.count(1, 8))

	// An end observed ahead of a delayed start is a closure fence.
	f.end(2, "late-start", "late-ws")
	f.start(2, 8, "late-start", "late-ws", true)
	require.Zero(t, f.count(2, 8))
	require.False(t, f.server.Exists(accountSessionReservationKeys(2)[2]))
}

func TestAccountSessionReservationExpirationNeverInventsIdle(t *testing.T) {
	f := newAccountSessionReservationFixture(t)
	f.start(1, 8, "expired", "ws", true)
	f.advance(50 * time.Second)
	f.start(1, 8, "still-running", "ws", true)
	f.advance(10 * time.Second)
	require.Zero(t, f.count(1, 8))
	f.end(1, "expired", "ws")
	require.Zero(t, f.count(1, 8))
	f.end(1, "still-running", "ws")
	require.Zero(t, f.count(1, 8), "one expired request makes remaining releases ineligible for idle preference")
	f.start(1, 8, "next-turn", "ws", true)
	f.end(1, "next-turn", "ws")
	require.Zero(t, f.count(1, 8), "a later turn cannot immediately erase uncertainty about an expired request")
	f.start(1, 8, "expired-alone", "another-ws", true)
	f.advance(time.Minute)
	f.end(1, "expired-alone", "another-ws")
	require.Zero(t, f.count(1, 8), "late release after active TTL loses preference rather than inventing idle")
	f.advance(121 * time.Second)
	for _, key := range accountSessionReservationKeys(1) {
		require.False(t, f.server.Exists(key), key)
	}
}

func TestAccountSessionReservationCapacityClampsAndStaysBounded(t *testing.T) {
	f := newAccountSessionReservationFixture(t)
	for i := range 140 {
		request := fmt.Sprintf("request-%03d", i)
		f.start(1, 512, request, request, true)
		f.end(1, request, request)
	}
	require.Equal(t, accountSessionIdleLimit, f.count(1, 512))
	require.Equal(t, 2, f.count(1, 5), "odd limits floor to half")
	require.Equal(t, 1, f.count(1, 2))
	require.Zero(t, f.count(1, 1))
	require.Zero(t, f.count(1, 512), "growing capacity cannot resurrect trimmed reservations")
	for i := range 540 {
		request := fmt.Sprintf("inactive-%03d", i)
		f.start(2, 1000, request, request, true)
		f.end(2, request, request)
	}
	keys := accountSessionReservationKeys(2)
	ctx := context.Background()
	require.LessOrEqual(t, f.client.ZCard(ctx, keys[0]).Val(), int64(accountSessionRegistryLimit))
	require.LessOrEqual(t, f.client.HLen(ctx, keys[1]).Val(), int64(accountSessionRegistryLimit+1))
	require.LessOrEqual(t, f.client.ZCard(ctx, keys[5]).Val(), int64(accountSessionRegistryLimit))
	require.Equal(t, accountSessionIdleLimit, f.count(2, 1000))

	// The active-member limit is independent of the number of sessions.
	for i := range accountSessionRegistryLimit {
		f.start(3, 1000, fmt.Sprintf("active-%03d", i), "same-ws", true)
	}
	require.Equal(t, int64(accountSessionRegistryLimit), f.client.ZCard(ctx, accountSessionReservationKeys(3)[2]).Val())
	f.start(3, 1000, "overflow", "same-ws", true)
	for i := range accountSessionRegistryLimit {
		f.end(3, fmt.Sprintf("active-%03d", i), "same-ws")
	}
	require.Zero(t, f.count(3, 1000), "untracked active overflow cannot leave a phantom idle session")
	f.end(3, "overflow", "same-ws")
	f.advance(time.Minute)
	f.start(3, 1000, "after-suspension", "fresh-ws", true)
	f.end(3, "after-suspension", "fresh-ws")
	require.Equal(t, 1, f.count(3, 1000))
}

func TestAccountSessionReservationAtomicConcurrentClients(t *testing.T) {
	f := newAccountSessionReservationFixture(t)
	ctx := context.Background()
	const workers = 16
	var wg sync.WaitGroup
	errors := make(chan error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := redis.NewClient(&redis.Options{Addr: f.server.Addr()})
			defer client.Close()
			cache := NewConcurrencyCache(client, 1, 60).(*concurrencyCache)
			request := fmt.Sprintf("concurrent-%d", i)
			acquired, err := cache.AcquireAccountSlot(ctx, 1, workers, request)
			if err != nil || !acquired {
				errors <- fmt.Errorf("acquire %s: acquired=%v: %v", request, acquired, err)
				return
			}
			hash := accountSessionTestHash("shared-ws")
			if err := cache.StartAccountSession(ctx, 1, workers, request, hash, true); err != nil {
				errors <- err
				return
			}
			if err := cache.ReleaseAccountSlot(ctx, 1, request); err != nil {
				errors <- err
				return
			}
			if err := cache.EndAccountSession(ctx, 1, request, hash); err != nil {
				errors <- err
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.Equal(t, 1, f.count(1, workers))
	count, err := f.cache.GetAccountConcurrency(ctx, 1)
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestAccountSessionReservationSharedProcesses(t *testing.T) {
	f := newAccountSessionReservationFixture(t)
	runProcess := func(op, request string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAccountSessionReservationProcessHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "SUB2API_SESSION_REGISTRY_TEST_ADDR="+f.server.Addr(),
			"SUB2API_SESSION_REGISTRY_TEST_OP="+op, "SUB2API_SESSION_REGISTRY_TEST_REQUEST="+request)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	runProcess("start", "process-one")
	f.advance(10 * time.Second)
	runProcess("start", "process-two")
	require.Zero(t, f.count(1, 2))
	runProcess("end", "process-one")
	require.Zero(t, f.count(1, 2), "the other process still owns a request for this session")
	runProcess("end", "process-two")
	require.Equal(t, 1, f.count(1, 2))
	count, err := f.cache.GetAccountConcurrency(context.Background(), 1)
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestAccountSessionReservationProcessHelper(t *testing.T) {
	addr := os.Getenv("SUB2API_SESSION_REGISTRY_TEST_ADDR")
	if addr == "" {
		t.Skip("subprocess helper")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	cache := NewConcurrencyCache(client, 1, 60).(*concurrencyCache)
	ctx := context.Background()
	request := os.Getenv("SUB2API_SESSION_REGISTRY_TEST_REQUEST")
	hash := accountSessionTestHash("explicit-cross-process-session")
	if os.Getenv("SUB2API_SESSION_REGISTRY_TEST_OP") == "start" {
		acquired, err := cache.AcquireAccountSlot(ctx, 1, 2, request)
		require.NoError(t, err)
		require.True(t, acquired)
		require.NoError(t, cache.StartAccountSession(ctx, 1, 2, request, hash, false))
	} else {
		require.NoError(t, cache.ReleaseAccountSlot(ctx, 1, request))
		require.NoError(t, cache.EndAccountSession(ctx, 1, request, hash))
	}
}

func TestAccountSessionReservationHashValidationAndSnapshotErrors(t *testing.T) {
	f := newAccountSessionReservationFixture(t)
	ctx := context.Background()
	for _, session := range []string{"", "raw-sensitive-session", "thread_123", string(make([]byte, 64))} {
		require.NoError(t, f.cache.StartAccountSession(ctx, 1, 8, "request", session, true))
		require.NoError(t, f.cache.EndAccountSession(ctx, 1, "request", session))
	}
	require.Empty(t, f.server.Keys(), "unhashed client identifiers must never reach Redis")
	require.Zero(t, f.count(2, 8))
	require.Empty(t, f.server.Keys(), "polling absent accounts must not create observation state")
	f.start(1, 8, "valid", "valid", true)
	f.end(1, "valid", "valid")
	for attempt := range 3 {
		if attempt == 2 {
			require.NoError(t, f.client.ScriptFlush(ctx).Err())
		}
		require.Equal(t, 1, f.count(1, 8))
	}
	require.NoError(t, f.client.Set(ctx, accountSessionReservationKeys(2)[0], "wrong type", 0).Err())
	counts, err := f.cache.GetAccountSessionReservations(ctx, []service.AccountWithConcurrency{{ID: 1, MaxConcurrency: 8}, {ID: 2, MaxConcurrency: 8}})
	require.Error(t, err)
	require.Nil(t, counts)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	counts, err = f.cache.GetAccountSessionReservations(canceled, []service.AccountWithConcurrency{{ID: 1, MaxConcurrency: 8}})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, counts)
}

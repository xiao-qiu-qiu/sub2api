package accountrequest

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func redisClient(t *testing.T, server *miniredis.Miniredis) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{
		Addr: server.Addr(), MaxRetries: -1,
		DialTimeout: 150 * time.Millisecond, ReadTimeout: 150 * time.Millisecond,
		WriteTimeout: 150 * time.Millisecond, ContextTimeoutEnabled: true,
	})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func startRegistry(t *testing.T, r *Registry, ctx context.Context, client *redis.Client) func() {
	t.Helper()
	stop, err := r.Start(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return stop
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func publishEvent(t *testing.T, client *redis.Client, event interruptEvent) {
	t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Publish(context.Background(), interruptChannel, payload).Err(); err != nil {
		t.Fatal(err)
	}
}

func TestRedisNilClientsKeepLocalInterruption(t *testing.T) {
	for name, client := range map[string]redis.UniversalClient{
		"interface": nil,
		"client":    (*redis.Client)(nil),
		"cluster":   (*redis.ClusterClient)(nil),
		"ring":      (*redis.Ring)(nil),
	} {
		t.Run(name, func(t *testing.T) {
			r := NewRegistry()
			stop, err := r.Start(context.Background(), client)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			ctx, finish := r.Begin(context.Background(), 1)
			defer finish()
			if count, err := r.Interrupt(context.Background(), 1); count != 1 || err != nil {
				t.Fatalf("local interruption: count=%d error=%v", count, err)
			}
			assertCause(t, ctx, ErrQualityDegraded)
			stop()
			stop()
		})
	}
}

func processed(r *Registry, accountID int64, cutoff time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.cutoffs[accountID].Before(cutoff)
}

func TestRedisInterruptAcrossRegistries(t *testing.T) {
	server := miniredis.RunT(t)
	client1, client2 := redisClient(t, server), redisClient(t, server)
	r1, r2 := NewRegistry(), NewRegistry()
	startRegistry(t, r1, context.Background(), client1)
	startRegistry(t, r2, context.Background(), client2)
	if got := server.PubSubNumSub(interruptChannel)[interruptChannel]; got != 2 {
		t.Fatalf("Start returned before ready: %d subscribers", got)
	}
	local, finishLocal := r1.Begin(context.Background(), 1)
	remote, finishRemote := r2.Begin(context.Background(), 1)
	other, finishOther := r2.Begin(context.Background(), 2)
	defer finishLocal()
	defer finishRemote()
	defer finishOther()
	count, err := r1.Interrupt(context.Background(), 1)
	if err != nil || count != 1 {
		t.Fatalf("Interrupt=(%d, %v), want (1, nil)", count, err)
	}
	assertCause(t, local, ErrQualityDegraded)
	eventually(t, func() bool { return remote.Err() != nil })
	assertCause(t, remote, ErrQualityDegraded)
	assertActive(t, other)
}

func TestRedisDelayedDuplicateAndReorderedEvents(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisClient(t, server)
	r1, r2 := NewRegistry(), NewRegistry()
	startRegistry(t, r1, context.Background(), client)
	startRegistry(t, r2, context.Background(), redisClient(t, server))
	base := time.Now().Add(-time.Minute).Round(0)
	old1, finishOld1 := r1.Begin(context.Background(), 1)
	old2, finishOld2 := r2.Begin(context.Background(), 1)
	defer finishOld1()
	defer finishOld2()
	setStarted(old1, base)
	setStarted(old2, base)
	cutoff := base.Add(time.Second)
	new1, finishNew1 := r1.Begin(context.Background(), 1)
	new2, finishNew2 := r2.Begin(context.Background(), 1)
	defer finishNew1()
	defer finishNew2()
	setStarted(new1, cutoff.Add(time.Nanosecond))
	setStarted(new2, cutoff.Add(time.Nanosecond))
	if n, err := r1.InterruptAt(context.Background(), 1, cutoff); n != 1 || err != nil {
		t.Fatalf("InterruptAt=(%d, %v)", n, err)
	}
	eventually(t, func() bool { return old2.Err() != nil })
	assertCause(t, old2, ErrQualityDegraded)
	assertActive(t, new1)
	assertActive(t, new2)
	publishEvent(t, client, interruptEvent{AccountID: 1, Cutoff: cutoff})
	publishEvent(t, client, interruptEvent{AccountID: 1, Cutoff: base})
	// A marker on the same connection/channel proves both preceding messages
	// have been consumed; no arbitrary sleep is needed to test noncancellation.
	publishEvent(t, client, interruptEvent{AccountID: 99, Cutoff: cutoff})
	eventually(t, func() bool { return processed(r1, 99, cutoff) && processed(r2, 99, cutoff) })
	assertActive(t, new1)
	assertActive(t, new2)
}

func TestRedisCoarseClockSelfEcho(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisClient(t, server)
	r := NewRegistry()
	startRegistry(t, r, context.Background(), client)
	cutoff := time.Now().Add(time.Hour).Round(0)
	if _, err := r.InterruptAt(context.Background(), 1, cutoff); err != nil {
		t.Fatal(err)
	}
	newer, finish := r.Begin(context.Background(), 1)
	defer finish()
	publishEvent(t, client, interruptEvent{AccountID: 1, Cutoff: cutoff})
	publishEvent(t, client, interruptEvent{AccountID: 99, Cutoff: cutoff})
	eventually(t, func() bool { return processed(r, 99, cutoff) })
	assertActive(t, newer)
}

func TestRedisPublishFailureStillCancelsLocally(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisClient(t, server)
	r := NewRegistry()
	startRegistry(t, r, context.Background(), client)
	ctx, finish := r.Begin(context.Background(), 1)
	defer finish()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Interrupt(context.Background(), 1); n != 1 || err == nil {
		t.Fatalf("failed publish=(%d, %v), want (1, error)", n, err)
	}
	assertCause(t, ctx, ErrQualityDegraded)
	r.transportMu.Lock()
	defer r.transportMu.Unlock()
	if r.transport == nil {
		t.Fatal("receive failure silently disabled publisher")
	}
}

func TestRedisReconnectAndStop(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisClient(t, server)
	r := NewRegistry()
	stop := startRegistry(t, r, context.Background(), client)
	ctx, finish := r.Begin(context.Background(), 1)
	defer finish()
	server.Close()
	// Prove the registry has not silently fallen back to local-only mode.
	if _, err := r.Interrupt(context.Background(), 2); err == nil {
		t.Fatal("publication during outage should return an error")
	}
	if err := server.Restart(); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return server.PubSubNumSub(interruptChannel)[interruptChannel] == 1 })
	publishEvent(t, client, interruptEvent{AccountID: 1, Cutoff: time.Now()})
	eventually(t, func() bool { return ctx.Err() != nil })
	assertCause(t, ctx, ErrQualityDegraded)
	stop()
	eventually(t, func() bool { return server.PubSubNumSub(interruptChannel)[interruptChannel] == 0 })
}

func TestRedisLifecycle(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisClient(t, server)
	r := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	stop := startRegistry(t, r, ctx, client)
	if _, err := r.Start(ctx, client); err == nil {
		t.Fatal("duplicate Start accepted")
	}
	r.transportMu.Lock()
	run := r.transport
	r.transportMu.Unlock()
	cancel()
	select {
	case <-run.done:
	case <-time.After(3 * time.Second):
		t.Fatal("context cancellation did not join subscription")
	}
	var group sync.WaitGroup
	for range 8 {
		group.Go(stop)
	}
	group.Wait()
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Stop closed caller-owned Redis client: %v", err)
	}
	if _, err := r.Start(ctx, client); err == nil {
		t.Fatal("Start with canceled context accepted")
	}
	stopAgain := startRegistry(t, r, context.Background(), client)
	stopAgain()
	if stopLocal, err := r.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	} else {
		stopLocal()
		stopLocal()
	}
	if n, err := r.Interrupt(context.Background(), 1); n != 0 || err != nil {
		t.Fatalf("local-only interrupt=(%d, %v)", n, err)
	}
}

func TestRedisInitialSubscribeFailureAndRetry(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisClient(t, server)
	server.Close()
	r := NewRegistry()
	if stop, err := r.Start(context.Background(), client); stop != nil || err == nil {
		t.Fatalf("Start=(%v, %v), want (nil, error)", stop != nil, err)
	}
	r.transportMu.Lock()
	retained := r.transport != nil
	r.transportMu.Unlock()
	if retained {
		t.Fatal("failed Start retained subscription state")
	}
	if err := server.Restart(); err != nil {
		t.Fatal(err)
	}
	startRegistry(t, r, context.Background(), client)
}

func TestRedisInvalidEventsDoNotStopSubscriber(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisClient(t, server)
	r := NewRegistry()
	startRegistry(t, r, context.Background(), client)
	ctx, finish := r.Begin(context.Background(), 1)
	defer finish()
	for _, payload := range []string{"invalid", "null", "{}", `{"account_id":-1,"cutoff":"2026-01-01T00:00:00Z"}`} {
		if err := client.Publish(context.Background(), interruptChannel, payload).Err(); err != nil {
			t.Fatal(err)
		}
	}
	publishEvent(t, client, interruptEvent{AccountID: 99, Cutoff: time.Now()})
	eventually(t, func() bool { return processed(r, 99, time.Time{}.Add(time.Second)) })
	assertActive(t, ctx)
	publishEvent(t, client, interruptEvent{AccountID: 1, Cutoff: time.Now()})
	eventually(t, func() bool { return ctx.Err() != nil })
	assertCause(t, ctx, ErrQualityDegraded)
}

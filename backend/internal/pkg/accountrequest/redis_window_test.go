package accountrequest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func publishWindowEvent(t *testing.T, client *redis.Client, event interruptEvent) {
	t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Publish(context.Background(), interruptWindowChannel, payload).Err(); err != nil {
		t.Fatal(err)
	}
}

// Use a v2 marker to order assertions after v2 broadcasts on each connection.
func windowMarker(t *testing.T, client *redis.Client, registries ...*Registry) {
	t.Helper()
	started := time.Now().Round(0)
	var markers []context.Context
	for _, registry := range registries {
		ctx, finish := registry.Track(WithStartedAt(context.Background(), started), 99)
		t.Cleanup(finish)
		markers = append(markers, ctx)
	}
	after := started.Add(-time.Nanosecond)
	publishWindowEvent(t, client, interruptEvent{AccountID: 99, After: &after, Cutoff: started})
	eventually(t, func() bool {
		for _, marker := range markers {
			if marker.Err() == nil {
				return false
			}
		}
		return true
	})
}

func TestRedisInterruptWindowPreservesBothBounds(t *testing.T) {
	server := miniredis.RunT(t)
	client1, client2 := redisClient(t, server), redisClient(t, server)
	r1, r2 := NewRegistry(), NewRegistry()
	startRegistry(t, r1, context.Background(), client1)
	startRegistry(t, r2, context.Background(), client2)
	// Inspect the actual wire payload as well as its effect at the subscriber.
	if got := server.PubSubNumSub(interruptWindowChannel)[interruptWindowChannel]; got != 2 {
		t.Fatalf("Start returned before v2 ready: subscribers=%d", got)
	}
	wire := client1.Subscribe(context.Background(), interruptWindowChannel)
	defer wire.Close()
	if _, err := wire.ReceiveTimeout(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	after := time.Now().Add(-time.Minute).Round(0)
	before := after.Add(time.Second)
	var requests []context.Context
	for _, registry := range []*Registry{r1, r2} {
		for _, started := range []time.Time{after.Add(-time.Second), after, after.Add(time.Nanosecond), before, before.Add(time.Nanosecond)} {
			ctx, finish := registry.Track(WithStartedAt(context.Background(), started), 1)
			t.Cleanup(finish)
			requests = append(requests, ctx)
		}
	}
	if count, err := r1.InterruptWindow(context.Background(), 1, after, before); count != 2 || err != nil {
		t.Fatalf("InterruptWindow=(%d,%v), want (2,nil)", count, err)
	}
	receiveCtx, cancelReceive := context.WithTimeout(context.Background(), time.Second)
	defer cancelReceive()
	message, err := wire.ReceiveMessage(receiveCtx)
	if err != nil {
		t.Fatal(err)
	}
	var event interruptEvent
	if err := json.Unmarshal([]byte(message.Payload), &event); err != nil {
		t.Fatal(err)
	}
	if event.After == nil || !event.After.Equal(after) || !event.Cutoff.Equal(before) {
		t.Fatalf("wire bounds differ from original window: %s", message.Payload)
	}
	eventually(t, func() bool { return requests[7].Err() != nil && requests[8].Err() != nil })
	// Duplicate and reordered broadcasts must preserve all three outside points.
	publishWindowEvent(t, client1, event)
	olderAfter := after.Add(-time.Hour)
	publishWindowEvent(t, client1, interruptEvent{AccountID: 1, After: &olderAfter, Cutoff: after.Add(-time.Hour + time.Second)})
	windowMarker(t, client1, r1, r2)
	for i, ctx := range requests {
		interrupted := i%5 == 2 || i%5 == 3
		if interrupted {
			assertCause(t, ctx, ErrQualityDegraded)
		} else {
			assertActive(t, ctx)
		}
		if QualityDegraded(ctx) != interrupted {
			t.Fatalf("request %d quality=%v, want %v", i, QualityDegraded(ctx), interrupted)
		}
	}
}

func TestRedisReorderedWindowsDoNotSuppressValidOlderWindow(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisClient(t, server)
	r1, r2 := NewRegistry(), NewRegistry()
	startRegistry(t, r1, context.Background(), client)
	startRegistry(t, r2, context.Background(), redisClient(t, server))
	base := time.Now().Add(-time.Minute).Round(0)
	registries := []*Registry{r1, r2}
	var requests [2][4]context.Context
	for i, registry := range registries {
		for j, offset := range []time.Duration{0, 1, 3, 5} {
			ctx, finish := registry.Track(WithStartedAt(context.Background(), base.Add(offset*time.Second)), 1)
			t.Cleanup(finish)
			requests[i][j] = ctx
		}
	}
	laterAfter := base.Add(4 * time.Second)
	publishWindowEvent(t, client, interruptEvent{AccountID: 1, After: &laterAfter, Cutoff: base.Add(6 * time.Second)})
	eventually(t, func() bool { return requests[0][3].Err() != nil && requests[1][3].Err() != nil })
	// Remote delivery order differs from transition order.
	earlierAfter := base.Add(2 * time.Second)
	publishWindowEvent(t, client, interruptEvent{AccountID: 1, After: &earlierAfter, Cutoff: base.Add(4 * time.Second)})
	// A wider event with the same upper bound must also be honored.
	publishWindowEvent(t, client, interruptEvent{AccountID: 1, After: &base, Cutoff: base.Add(6 * time.Second)})
	windowMarker(t, client, r1, r2)
	for _, instance := range requests {
		assertActive(t, instance[0])
		for _, ctx := range instance[1:] {
			assertCause(t, ctx, ErrQualityDegraded)
		}
	}
}

func TestRedisWindowPublishFailureKeepsLocalOutcome(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisClient(t, server)
	r := NewRegistry()
	startRegistry(t, r, context.Background(), client)
	started := time.Now().Add(-time.Second).Round(0)
	ctx, finish := r.Track(WithStartedAt(context.Background(), started), 1)
	defer finish()
	old, finishOld := r.Track(WithStartedAt(context.Background(), started.Add(-time.Hour)), 1)
	defer finishOld()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if count, err := r.InterruptWindow(context.Background(), 1, started.Add(-time.Nanosecond), started); count != 1 || err == nil {
		t.Fatalf("failed publish=(%d,%v), want (1,error)", count, err)
	}
	assertCause(t, ctx, ErrQualityDegraded)
	assertActive(t, old)
}

func TestRedisZeroLowerBoundIsAWindow(t *testing.T) {
	r := NewRegistry()
	started := time.Now().Round(0)
	// An existing legacy high-water mark must not deduplicate the window.
	r.Cancel(1, started.Add(time.Hour))
	ctx, finish := r.Track(WithStartedAt(context.Background(), started), 1)
	defer finish()
	after := time.Time{}
	payload, err := json.Marshal(interruptEvent{AccountID: 1, After: &after, Cutoff: started})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.handleEvent(interruptWindowChannel, string(payload)); err != nil {
		t.Fatal(err)
	}
	assertCause(t, ctx, ErrQualityDegraded)
}

func TestRedisRejectsInvalidWindowWithoutWideningToLegacy(t *testing.T) {
	r := NewRegistry()
	started := time.Now().Round(0)
	ctx, finish := r.Track(WithStartedAt(context.Background(), started), 1)
	defer finish()
	for _, after := range []time.Time{started, started.Add(time.Second)} {
		payload, err := json.Marshal(interruptEvent{AccountID: 1, After: &after, Cutoff: started})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.handleEvent(interruptWindowChannel, string(payload)); err == nil {
			t.Fatalf("invalid window accepted: %s", payload)
		}
		assertActive(t, ctx)
	}
	legacy, err := json.Marshal(interruptEvent{AccountID: 1, Cutoff: started})
	if err != nil {
		t.Fatal(err)
	}
	after := started.Add(-time.Nanosecond)
	window, err := json.Marshal(interruptEvent{AccountID: 1, After: &after, Cutoff: started})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ channel, payload string }{
		{interruptWindowChannel, string(legacy)},
		{interruptWindowChannel, `{"account_id":1,"cutoff":"` + started.Format(time.RFC3339Nano) + `","after":null}`},
		{interruptChannel, string(window)},
		{"unknown", string(window)},
	} {
		if err := r.handleEvent(test.channel, test.payload); err == nil {
			t.Fatalf("invalid channel/payload accepted: %s %s", test.channel, test.payload)
		}
		assertActive(t, ctx)
	}
}

func TestRedisWindowNeverPublishesToLegacySubscriber(t *testing.T) {
	server := miniredis.RunT(t)
	client := redisClient(t, server)
	r := NewRegistry()
	stop := startRegistry(t, r, context.Background(), client)
	legacyWire := client.Subscribe(context.Background(), interruptChannel)
	defer legacyWire.Close()
	if _, err := legacyWire.ReceiveTimeout(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Second).Round(0)
	old, finishOld := r.Track(WithStartedAt(context.Background(), started.Add(-time.Minute)), 1)
	defer finishOld()
	inside, finishInside := r.Track(WithStartedAt(context.Background(), started), 1)
	defer finishInside()
	if count, err := r.InterruptWindow(context.Background(), 1, started.Add(-time.Nanosecond), started); count != 1 || err != nil {
		t.Fatalf("InterruptWindow=(%d,%v)", count, err)
	}
	assertCause(t, inside, ErrQualityDegraded)
	assertActive(t, old)
	// A following legacy publish is the first event visible to the old process.
	// If the window leaked to v1, its after field appears here and fails the test.
	if count, err := r.InterruptAt(context.Background(), 1, started); count != 1 || err != nil {
		t.Fatalf("legacy InterruptAt=(%d,%v)", count, err)
	}
	receiveCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	message, err := legacyWire.ReceiveMessage(receiveCtx)
	if err != nil {
		t.Fatal(err)
	}
	var event interruptEvent
	if err := json.Unmarshal([]byte(message.Payload), &event); err != nil {
		t.Fatal(err)
	}
	if event.After != nil || event.AccountID != 1 || !event.Cutoff.Equal(started) {
		t.Fatalf("window leaked to legacy subscriber: %s", message.Payload)
	}
	assertCause(t, old, ErrQualityDegraded)
	stop()
	eventually(t, func() bool { return server.PubSubNumSub(interruptWindowChannel)[interruptWindowChannel] == 0 })
}

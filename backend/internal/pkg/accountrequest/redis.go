package accountrequest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const interruptChannel = "accountrequest:quality:interrupt:v1"

// Legacy subscribers ignore unknown JSON fields, so publishing a window on v1
// would widen it to an unbounded cutoff. Window events exclusively use v2;
// every participating process must support v2 for cross-process windows.
const interruptWindowChannel = "accountrequest:quality:interrupt:v2"

const redisTimeout = 3 * time.Second

type interruptEvent struct {
	AccountID int64     `json:"account_id"`
	Cutoff    time.Time `json:"cutoff"`
	// A pointer distinguishes legacy events (no lower bound) from a window
	// whose explicit lower bound is the zero time.
	After *time.Time `json:"after,omitempty"`
}

type subscription struct {
	rdb    redis.UniversalClient
	pubsub *redis.PubSub
	cancel context.CancelFunc
	done   chan struct{}
}

// Start confirms both legacy v1 and window v2 subscriptions before returning.
// The returned stop is idempotent, closes the subscription and joins its worker;
// canceling ctx also
// stops it. The caller retains ownership of rdb. A nil rdb enables local-only
// operation. Only one subscription may run per registry; Start may be called
// again after stop. Receive errors are logged and retried with exponential
// backoff capped at three seconds; go-redis reconnects and resubscribes. The
// publisher remains available throughout recovery.
// Pubsub is best effort: events during disconnection are not replayed. Processes
// must share a synchronized wall clock for cross-process cutoff comparisons.
func (r *Registry) Start(ctx context.Context, rdb redis.UniversalClient) (func(), error) {
	if rdb == nil {
		return func() {}, nil
	}
	// Wiring supplies *redis.Client; a nil pointer retains its dynamic type
	// when converted to UniversalClient. Keep the documented local-only mode.
	if client := reflect.ValueOf(rdb); client.Kind() == reflect.Pointer && client.IsNil() {
		return func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.transportMu.Lock()
	if r.transport != nil {
		r.transportMu.Unlock()
		return nil, errors.New("accountrequest: subscription already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	run := &subscription{rdb: rdb, cancel: cancel, done: make(chan struct{})}
	r.transport = run
	r.transportMu.Unlock()

	run.pubsub = rdb.Subscribe(runCtx, interruptChannel, interruptWindowChannel)
	closePubSub := sync.OnceFunc(func() { _ = run.pubsub.Close() })
	stopClose := context.AfterFunc(runCtx, closePubSub)
	finish := func() {
		cancel()
		stopClose()
		closePubSub()
		r.transportMu.Lock()
		if r.transport == run {
			r.transport = nil
		}
		r.transportMu.Unlock()
		close(run.done)
	}
	pending := map[string]bool{interruptChannel: true, interruptWindowChannel: true}
	var err error
	for len(pending) > 0 {
		var message any
		message, err = run.pubsub.ReceiveTimeout(runCtx, redisTimeout)
		if err != nil {
			break
		}
		ready, ok := message.(*redis.Subscription)
		if !ok || ready.Kind != "subscribe" || !pending[ready.Channel] {
			err = errors.New("unexpected Redis subscription acknowledgement")
			break
		}
		delete(pending, ready.Channel)
	}
	if err == nil {
		err = runCtx.Err()
	}
	if err != nil {
		finish()
		slog.Error("accountrequest subscription failed", "error", err)
		return nil, fmt.Errorf("accountrequest: subscribe: %w", err)
	}
	go func() {
		defer finish()
		backoff := 100 * time.Millisecond
		for {
			message, err := run.pubsub.ReceiveMessage(runCtx)
			if err != nil {
				if runCtx.Err() != nil {
					return
				}
				slog.Warn("accountrequest subscription reconnecting", "error", err, "retry_in", backoff)
				timer := time.NewTimer(backoff)
				select {
				case <-runCtx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				backoff = min(backoff*2, redisTimeout)
				continue
			}
			backoff = 100 * time.Millisecond
			if runCtx.Err() != nil {
				return
			}
			if err := r.handleEvent(message.Channel, message.Payload); err != nil {
				slog.Warn("accountrequest ignored invalid interrupt event", "error", err)
			}
		}
	}()
	return sync.OnceFunc(func() { cancel(); <-run.done }), nil
}

// Interrupt uses the current time as cutoff. Quality transactions should call
// InterruptAt immediately after a successful Commit, before follow-up work.
func (r *Registry) Interrupt(ctx context.Context, accountID int64) (int, error) {
	return r.InterruptAt(ctx, accountID, time.Now())
}

// InterruptAt cancels locally before any Redis I/O and broadcasts the original
// cutoff, never the publication time. A publish failure returns the local count
// and error; it does not undo cancellation. Without Start it operates locally.
func (r *Registry) InterruptAt(ctx context.Context, accountID int64, cutoff time.Time) (int, error) {
	if accountID <= 0 || cutoff.IsZero() {
		return 0, nil
	}
	count := r.Cancel(accountID, cutoff)
	return r.publishInterrupt(ctx, interruptEvent{AccountID: accountID, Cutoff: cutoff.Round(0)}, count)
}

// InterruptWindow cancels registrations created in (after, before] locally
// before Redis I/O and broadcasts both original bounds. A delayed, duplicated,
// or reordered broadcast cannot include requests outside that window. Windows
// publish only on v2; legacy processes will not receive them and must be upgraded
// to participate in cross-process window interruption.
// Publish errors preserve local cancellation and return its count.
func (r *Registry) InterruptWindow(ctx context.Context, accountID int64, after, before time.Time) (int, error) {
	after, before = after.Round(0), before.Round(0)
	if accountID <= 0 || before.IsZero() || !before.After(after) {
		return 0, nil
	}
	count := r.CancelWindow(accountID, after, before)
	return r.publishInterrupt(ctx, interruptEvent{AccountID: accountID, Cutoff: before, After: &after}, count)
}

func (r *Registry) publishInterrupt(ctx context.Context, event interruptEvent, count int) (int, error) {
	r.transportMu.Lock()
	run := r.transport
	r.transportMu.Unlock()
	if run == nil {
		return count, nil
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return count, fmt.Errorf("accountrequest: encode interrupt: %w", err)
	}
	publishCtx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()
	channel := interruptChannel
	if event.After != nil {
		channel = interruptWindowChannel
	}
	if err := run.rdb.Publish(publishCtx, channel, payload).Err(); err != nil {
		return count, fmt.Errorf("accountrequest: publish interrupt: %w", err)
	}
	return count, nil
}

func (r *Registry) handleEvent(channel, payload string) error {
	var event interruptEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return err
	}
	if event.AccountID <= 0 || event.Cutoff.IsZero() {
		return errors.New("missing account ID or cutoff")
	}
	switch channel {
	case interruptWindowChannel:
		if event.After == nil {
			return errors.New("missing interrupt window lower bound")
		}
		if !event.Cutoff.After(*event.After) {
			return errors.New("empty or reversed interrupt window")
		}
		r.CancelWindow(event.AccountID, *event.After, event.Cutoff)
	case interruptChannel:
		if event.After != nil {
			return errors.New("window events require the v2 channel")
		}
		r.Cancel(event.AccountID, event.Cutoff)
	default:
		return errors.New("unknown interrupt channel")
	}
	return nil
}

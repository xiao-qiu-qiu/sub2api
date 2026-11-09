package accountrequest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const interruptChannel = "accountrequest:quality:interrupt:v1"

const redisTimeout = 3 * time.Second

type interruptEvent struct {
	AccountID int64     `json:"account_id"`
	Cutoff    time.Time `json:"cutoff"`
}

type subscription struct {
	rdb    redis.UniversalClient
	pubsub *redis.PubSub
	cancel context.CancelFunc
	done   chan struct{}
}

// Start confirms the Redis subscription before returning. The returned stop is
// idempotent, closes the subscription and joins its worker; canceling ctx also
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

	run.pubsub = rdb.Subscribe(runCtx, interruptChannel)
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
	message, err := run.pubsub.ReceiveTimeout(runCtx, redisTimeout)
	if err == nil {
		ready, ok := message.(*redis.Subscription)
		if !ok || ready.Kind != "subscribe" || ready.Channel != interruptChannel {
			err = errors.New("unexpected Redis subscription acknowledgement")
		}
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
			if err := r.handleEvent(message.Payload); err != nil {
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
	r.transportMu.Lock()
	run := r.transport
	r.transportMu.Unlock()
	if run == nil {
		return count, nil
	}
	payload, err := json.Marshal(interruptEvent{AccountID: accountID, Cutoff: cutoff.Round(0)})
	if err != nil {
		return count, fmt.Errorf("accountrequest: encode interrupt: %w", err)
	}
	publishCtx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()
	if err := run.rdb.Publish(publishCtx, interruptChannel, payload).Err(); err != nil {
		return count, fmt.Errorf("accountrequest: publish interrupt: %w", err)
	}
	return count, nil
}

func (r *Registry) handleEvent(payload string) error {
	var event interruptEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return err
	}
	if event.AccountID <= 0 || event.Cutoff.IsZero() {
		return errors.New("missing account ID or cutoff")
	}
	r.Cancel(event.AccountID, event.Cutoff)
	return nil
}

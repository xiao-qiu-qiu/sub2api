package service

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccountRotationRecoveryWaitUsesMilliseconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, cache := newRotationHoldConcurrency(false)
		cache.setState(7, AccountRotationState{Phase: "ready", Recovering: true, RetryAfterMS: 500})
		requestCtx, cancel := context.WithCancel(t.Context())
		defer cancel()
		queueCtx, cancelQueue := context.WithTimeout(requestCtx, 100*time.Millisecond)
		defer cancelQueue()
		done := make(chan rotationHoldAcquireOutcome, 1)
		go func() {
			result, err := svc.AcquireAccountSlot(WithAccountRotationWaitContext(queueCtx, requestCtx), 7, 2)
			done <- rotationHoldAcquireOutcome{result, err}
		}()
		synctest.Wait()
		cache.assertCounts(t, 7, 0, 1)
		time.Sleep(499 * time.Millisecond)
		synctest.Wait()
		requireRotationHoldPending(t, done)
		cache.setState(7, AccountRotationState{Phase: "ready"})
		time.Sleep(time.Millisecond)
		synctest.Wait()
		select {
		case outcome := <-done:
			require.NoError(t, outcome.err)
			require.True(t, outcome.result.Acquired)
			outcome.result.ReleaseFunc()
		default:
			t.Fatal("recovery must retry at 500ms instead of rounding to a one-second poll")
		}
		cache.assertCounts(t, 7, 0, 0)
	})
}

func TestAccountRotationRecoveryCancellationCleansWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, cache := newRotationHoldConcurrency(false)
		cache.setState(7, AccountRotationState{Phase: "working", Recovering: true, RetryAfterMS: 60000})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := svc.AcquireAccountSlot(ctx, 7, 0)
			done <- err
		}()
		synctest.Wait()
		cache.assertCounts(t, 7, 0, 1)
		time.Sleep(21 * time.Second)
		synctest.Wait()
		cache.mu.Lock()
		calls := len(cache.calls)
		cache.mu.Unlock()
		require.Equal(t, 2, calls, "long intervals must still renew waiter leases")
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		cache.assertCounts(t, 7, 0, 0)
	})
}

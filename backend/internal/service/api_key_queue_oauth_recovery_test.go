//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type apiKeyQueueOAuthRecoveryFailureCache struct {
	*apiKeyQueueCacheStub
	active         map[string]bool
	abortSawActive bool
}

func (c *apiKeyQueueOAuthRecoveryFailureCache) AdvanceAPIKeyQueue(ctx context.Context, keyID int64, requestID string, mode APIKeyQueueMode, deadlineMs int64, limit, maxWaiting int) (APIKeyQueueOutcome, error) {
	outcome, err := c.apiKeyQueueCacheStub.AdvanceAPIKeyQueue(ctx, keyID, requestID, mode, deadlineMs, limit, maxWaiting)
	if err == nil && outcome == APIKeyQueueOutcomeAcquired {
		c.mu.Lock()
		c.active[requestID] = true
		c.mu.Unlock()
	}
	return outcome, err
}

func (c *apiKeyQueueOAuthRecoveryFailureCache) ReleaseAPIKeySlot(_ context.Context, _ int64, requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releasedIDs = append(c.releasedIDs, requestID)
	return errors.New("regular slot delete unavailable")
}

func (c *apiKeyQueueOAuthRecoveryFailureCache) AbortAPIKeyQueueAttempt(_ context.Context, _ int64, requestID string, _ int64, removeRegular bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.abortedIDs = append(c.abortedIDs, requestID)
	c.abortRemove = append(c.abortRemove, removeRegular)
	c.abortSawActive = c.abortSawActive || c.active[requestID]
	if c.abortErr != nil {
		return c.abortErr
	}
	if removeRegular {
		delete(c.active, requestID)
	}
	return nil
}

func TestAPIKeyQueueOAuthRecoveryAcquiredSlotRequiresConfirmedFence(t *testing.T) {
	for _, abortFails := range []bool{false, true} {
		name := "fence_deletes_after_release_failure"
		if abortFails {
			name = "fence_failure_is_unavailable"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				recovering := false
				cache := &apiKeyQueueOAuthRecoveryFailureCache{
					apiKeyQueueCacheStub: &apiKeyQueueCacheStub{
						outcomes:    []APIKeyQueueOutcome{APIKeyQueueOutcomeAcquired},
						advanceHook: func() { recovering = true },
					},
					active: make(map[string]bool),
				}
				if abortFails {
					cache.abortErr = errors.New("atomic abort unavailable")
				}
				svc := NewConcurrencyService(cache)
				svc.SetAPIKeyQueuePolicy(APIKeyQueuePolicy{MaxWaiting: 5, Timeout: 10 * time.Second})
				ctx, cancel := WithAPIKeyAdmissionOwner(context.Background())
				defer cancel()
				ctx = WithOAuthRecoveryAdmissionCheck(ctx, func(context.Context) bool { return recovering })

				reservation, err := svc.ReserveAPIKeySlotWithWait(ctx, 5, 1)

				require.Nil(t, reservation)
				require.NoError(t, ctx.Err(), "interruption must not cancel the original request")
				require.Len(t, cache.advanceIDs, 1)
				require.Len(t, cache.releasedIDs, 3, "regular deletion must exhaust its retries before the fence removes its residue")
				require.True(t, cache.abortSawActive, "the test must leave a real active member for the abort to clean up")
				for _, id := range cache.releasedIDs {
					require.Equal(t, cache.advanceIDs[0], id)
				}
				for i, id := range cache.abortedIDs {
					require.Equal(t, cache.advanceIDs[0], id)
					require.True(t, cache.abortRemove[i], "OAuth interruption must remove an acquired regular member too")
				}
				if abortFails {
					require.True(t, IsAPIKeyQueueErrorKind(err, APIKeyQueueErrorUnavailable))
					require.NotErrorIs(t, err, ErrOAuthRecoveryAdmissionInterrupted, "unconfirmed cleanup must not allow recovery re-admission")
					require.ErrorIs(t, err, cache.abortErr)
					require.Len(t, cache.abortedIDs, apiKeyQueueAbortAttempts)
					require.Len(t, cache.active, 1)
				} else {
					require.ErrorIs(t, err, ErrOAuthRecoveryAdmissionInterrupted)
					require.Len(t, cache.abortedIDs, 1)
					require.Empty(t, cache.active, "no old key slot may survive a successful interruption")
				}
			})
		})
	}
}

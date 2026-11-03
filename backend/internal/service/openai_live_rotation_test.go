//go:build unit

package service

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type liveRotationCreateCache struct {
	*rotationHoldTestCache
	live              liveTransferCacheStub
	joint             map[string]int64
	liveRefreshCalls  int
	refreshedAccounts []int64
	liveLeaseLost     bool
}

func (c *liveRotationCreateCache) AcquireLiveLease(ctx context.Context, a int64, am int, u int64, um int, k int64, km int, id string, replacing bool) (bool, error) {
	return c.live.AcquireLiveLease(ctx, a, am, u, um, k, km, id, replacing)
}

func (c *liveRotationCreateCache) AcquireLiveLeaseTransferring(ctx context.Context, request LiveLeaseTransferRequest) (bool, error) {
	ok, err := c.live.AcquireLiveLeaseTransferring(ctx, request)
	if err != nil || !ok {
		return ok, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.slots[request.AccountID][request.AccountRequestID] {
		return false, ErrLiveLeaseSourceLost
	}
	delete(c.slots[request.AccountID], request.AccountRequestID)
	c.joint[request.LeaseID] = request.AccountID
	return true, nil
}

func (c *liveRotationCreateCache) MigrateLiveLeaseAccount(ctx context.Context, request LiveLeaseTransferRequest) (bool, error) {
	ok, err := c.live.MigrateLiveLeaseAccount(ctx, request)
	if err != nil || !ok {
		return ok, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.joint[request.LeaseID] != request.ReplacedAccountID || !c.slots[request.AccountID][request.AccountRequestID] {
		return false, ErrLiveLeaseSourceLost
	}
	delete(c.slots[request.AccountID], request.AccountRequestID)
	c.joint[request.LeaseID] = request.AccountID
	return true, nil
}

func (c *liveRotationCreateCache) RefreshLiveLease(_ context.Context, a, _, _ int64, id string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.liveRefreshCalls++
	c.refreshedAccounts = append(c.refreshedAccounts, a)
	accountID, exists := c.joint[id]
	return !c.liveLeaseLost && exists && accountID == a, nil
}

func (c *liveRotationCreateCache) ReleaseLiveLease(ctx context.Context, a, u, k int64, id string) error {
	c.mu.Lock()
	delete(c.joint, id)
	c.mu.Unlock()
	return c.live.ReleaseLiveLease(ctx, a, u, k, id)
}

func (c *liveRotationCreateCache) ReleaseLiveLeaseAccount(ctx context.Context, a int64, id string) error {
	c.mu.Lock()
	if c.joint[id] == a {
		c.joint[id] = 0
	}
	c.mu.Unlock()
	return c.live.ReleaseLiveLeaseAccount(ctx, a, id)
}

func (c *liveRotationCreateCache) AbortLiveLeaseTransfer(ctx context.Context, request LiveLeaseTransferRequest) error {
	return c.live.AbortLiveLeaseTransfer(ctx, request)
}

type liveRotationCreateScheduler struct {
	OpenAIAccountScheduler
	selectFn func(context.Context) (*AccountSelectionResult, error)
	calls    int
}

func (s *liveRotationCreateScheduler) Select(ctx context.Context, _ OpenAIAccountScheduleRequest) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	s.calls++
	selection, err := s.selectFn(ctx)
	return selection, OpenAIAccountScheduleDecision{}, err
}

type liveRotationCreateOutcome struct {
	created *LiveCallCreated
	err     error
}

// Force the scheduler's WaitPlan boundary independently of its full-candidate
// filtering, so scheduler fixes cannot accidentally remove this Live regression.
func newLiveRotationCreateFixture(t *testing.T, retry bool) (*OpenAIGatewayService, *liveRotationCreateCache, *liveRotationCreateScheduler, *liveLoopUpstreamStub, *liveLoopStoreStub) {
	t.Helper()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	upstream := &liveLoopUpstreamStub{}
	if retry {
		upstream.failures = 1
	}
	svc, _, store := newLiveLoopFixture(t, upstream, true)
	cache := &liveRotationCreateCache{
		rotationHoldTestCache: newRotationHoldTestCache(),
		live:                  liveTransferCacheStub{transferResult: true, migrateResult: true},
		joint:                 make(map[string]int64),
	}
	svc.concurrencyService = NewConcurrencyService(cache)
	settings := *DefaultAccountRotationSettings()
	settings.Enabled = true
	svc.concurrencyService.SetAccountRotation(openAIRotationSettingsStub{settings}, nil)
	svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true", "false")
	scheduler := &liveRotationCreateScheduler{}
	scheduler.selectFn = func(ctx context.Context) (*AccountSelectionResult, error) {
		id := int64(1)
		if retry && scheduler.calls > 1 {
			id = 2
		}
		account := &Account{
			ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
			Status: StatusActive, Schedulable: true,
			Credentials: map[string]any{"access_token": "live-test-token", "chatgpt_account_id": "live-test-account"},
		}
		if retry && scheduler.calls == 1 {
			result, err := svc.concurrencyService.AcquireAccountSlot(ctx, id, 1)
			if err != nil {
				return nil, err
			}
			return &AccountSelectionResult{Account: account, Acquired: result.Acquired, ReleaseFunc: result.ReleaseFunc, AccountRequestID: result.RequestID}, nil
		}
		return &AccountSelectionResult{
			Account:  account,
			WaitPlan: &AccountWaitPlan{AccountID: id, MaxConcurrency: 1, Timeout: time.Second},
		}, nil
	}
	svc.openaiScheduler = scheduler
	return svc, cache, scheduler, upstream, store
}

func TestCreateLiveCallRotationWaitPlanWaitsAndTransfersExactAccountSlot(t *testing.T) {
	for _, reason := range []string{"manual", "early", "scheduled", "recovery"} {
		t.Run(reason, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, cache, scheduler, upstream, store := newLiveRotationCreateFixture(t, false)
				state := AccountRotationState{Phase: "draining", Hold: true, Reason: reason}
				if reason == "recovery" {
					state = AccountRotationState{Phase: "ready", Recovering: true, RetryAfterMS: 500}
				}
				cache.setState(1, state)
				cache.full[1] = true
				identity := liveLoopIdentity()
				done := make(chan liveRotationCreateOutcome, 1)
				go func() {
					created, err := svc.CreateLiveCall(t.Context(), liveLoopRequest(), identity, 1)
					done <- liveRotationCreateOutcome{created, err}
				}()
				synctest.Wait()
				cache.assertCounts(t, 1, 0, 1)
				time.Sleep(61 * time.Second)
				synctest.Wait()
				requireRotationHoldPending(t, done)
				require.Zero(t, upstream.calls)
				require.Zero(t, cache.live.transferCalls)
				require.Equal(t, "key-req", identity.KeyReservation.RequestID())
				cache.setState(1, AccountRotationState{Phase: "ready"})
				time.Sleep(time.Second)
				synctest.Wait()
				requireRotationHoldPending(t, done)
				cache.mu.Lock()
				cache.full[1] = false
				cache.mu.Unlock()
				time.Sleep(time.Second)
				outcome := <-done
				require.NoError(t, outcome.err)
				require.EqualValues(t, 1, outcome.created.Account.ID)
				require.Equal(t, 1, scheduler.calls, "HOLD must not select a fallback account")
				require.Equal(t, 1, cache.live.transferCalls)
				transfer := cache.live.lastTransfer
				require.NotEmpty(t, transfer.AccountRequestID)
				require.Equal(t, cache.calls[len(cache.calls)-1].requestID, transfer.AccountRequestID)
				require.Equal(t, "user-req", transfer.UserRequestID)
				require.Equal(t, "key-req", transfer.KeyRequestID)
				require.Empty(t, identity.KeyReservation.RequestID())
				require.EqualValues(t, 1, cache.joint[transfer.LeaseID])
				cache.assertCounts(t, 1, 0, 0)
				require.Zero(t, cache.live.releaseLeaseCall)
				require.Equal(t, 1, store.saves)
				for _, call := range cache.calls {
					require.EqualValues(t, 1, call.accountID)
				}
			})
		})
	}
}

func TestCreateLiveCallOrdinaryWaitPlanStillRejectsCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, cache, scheduler, upstream, store := newLiveRotationCreateFixture(t, false)
		cache.full[1] = true
		created, err := svc.CreateLiveCall(t.Context(), liveLoopRequest(), liveLoopIdentity(), 1)
		require.Nil(t, created)
		require.ErrorIs(t, err, ErrLiveConcurrencyFull)
		require.Equal(t, 1, scheduler.calls)
		require.Empty(t, cache.calls, "ordinary WaitPlans must not attempt acquisition or start a queue")
		require.Zero(t, cache.live.transferCalls)
		require.Zero(t, upstream.calls)
		require.Zero(t, store.saves)
	})
}

func TestCreateLiveCallRotationWaitCancellationCleansWaiterAndRetainedJoint(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_transfer", true: "retained_joint"}[retry], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, cache, scheduler, _, store := newLiveRotationCreateFixture(t, retry)
				id := int64(1)
				if retry {
					id = 2
				}
				cache.setState(id, AccountRotationState{Phase: "draining", Hold: true, Reason: "scheduled"})
				cache.full[id] = true
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				identity := liveLoopIdentity()
				done := make(chan error, 1)
				go func() {
					_, err := svc.CreateLiveCall(ctx, liveLoopRequest(), identity, 1)
					done <- err
				}()
				synctest.Wait()
				cache.assertCounts(t, id, 0, 1)
				time.Sleep(61 * time.Second)
				synctest.Wait()
				requireRotationHoldPending(t, done)
				if retry {
					require.GreaterOrEqual(t, cache.liveRefreshCalls, 3, "retained user/key owners must renew throughout HOLD")
					require.Len(t, cache.joint, 1)
					require.Zero(t, cache.joint[cache.live.lastTransfer.LeaseID], "failed account must already be released")
					for _, accountID := range cache.refreshedAccounts {
						require.Zero(t, accountID, "waiting must not renew a failed account member")
					}
				}
				cancel()
				require.ErrorIs(t, <-done, context.Canceled)
				cache.assertCounts(t, id, 0, 0)
				require.Empty(t, cache.joint)
				require.Equal(t, map[bool]int{false: 1, true: 2}[retry], scheduler.calls)
				require.Zero(t, store.saves)
				if retry {
					require.Equal(t, 1, cache.live.releaseLeaseCall)
					require.Equal(t, cache.live.lastTransfer.LeaseID, cache.live.releasedLeaseID)
				} else {
					require.Equal(t, "key-req", identity.KeyReservation.RequestID(), "ordinary key source remains owned by the caller")
					identity.KeyReservation.Release()
				}
			})
		})
	}
}

func TestCreateLiveCallRotationRetryMigratesRetainedJointAndHandlesLeaseLoss(t *testing.T) {
	for _, loseLease := range []bool{false, true} {
		t.Run(map[bool]string{false: "migrate", true: "lease_lost"}[loseLease], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, cache, scheduler, upstream, store := newLiveRotationCreateFixture(t, true)
				cache.setState(2, AccountRotationState{Phase: "resting", Hold: true, Reason: "scheduled"})
				done := make(chan liveRotationCreateOutcome, 1)
				go func() {
					created, err := svc.CreateLiveCall(t.Context(), liveLoopRequest(), liveLoopIdentity(), 1)
					done <- liveRotationCreateOutcome{created, err}
				}()
				synctest.Wait()
				cache.assertCounts(t, 2, 0, 1)
				if loseLease {
					cache.mu.Lock()
					cache.liveLeaseLost = true
					cache.mu.Unlock()
					time.Sleep(20 * time.Second)
					outcome := <-done
					require.ErrorIs(t, outcome.err, ErrAPIKeySlotLeaseLost)
					require.Nil(t, outcome.created)
					require.Empty(t, cache.joint)
					require.Equal(t, 1, cache.live.releaseLeaseCall)
					require.Zero(t, cache.live.migrateCalls)
					require.Zero(t, store.saves)
				} else {
					time.Sleep(61 * time.Second)
					synctest.Wait()
					requireRotationHoldPending(t, done)
					require.GreaterOrEqual(t, cache.liveRefreshCalls, 3)
					cache.setState(2, AccountRotationState{Phase: "ready"})
					time.Sleep(time.Second)
					outcome := <-done
					require.NoError(t, outcome.err)
					require.EqualValues(t, 2, outcome.created.Account.ID)
					require.Equal(t, 1, cache.live.transferCalls)
					require.Equal(t, 1, cache.live.migrateCalls)
					migration := cache.live.migrateRequest
					require.Equal(t, cache.calls[len(cache.calls)-1].requestID, migration.AccountRequestID)
					require.Zero(t, migration.ReplacedAccountID, "the failed account was released before the wait")
					require.Equal(t, cache.live.lastTransfer.LeaseID, migration.LeaseID)
					require.EqualValues(t, 2, cache.joint[migration.LeaseID])
					require.Zero(t, cache.live.releaseLeaseCall)
					require.Equal(t, 1, cache.live.accountRelease)
					require.Equal(t, 2, upstream.calls)
					require.Equal(t, 1, store.saves)
				}
				cache.assertCounts(t, 2, 0, 0)
				require.Equal(t, 2, scheduler.calls)
			})
		})
	}
}

var _ LiveLeaseTransferCache = (*liveRotationCreateCache)(nil)
var _ LiveConcurrencyCache = (*liveRotationCreateCache)(nil)

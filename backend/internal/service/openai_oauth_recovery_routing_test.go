//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type oauthRecoveryRoutingRepo struct {
	*oauthRecoveryTestRepo
	beforeList func()
}

func (r *oauthRecoveryRoutingRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]Account, error) {
	if r.beforeList != nil {
		hook := r.beforeList
		r.beforeList = nil
		hook()
	}
	accounts, err := r.ListAllWithFilters(ctx, platform, "", "", "", groupID, "")
	var out []Account
	for _, account := range accounts {
		if account.Platform == platform && account.IsSchedulable() {
			out = append(out, account)
		}
	}
	return out, err
}

func newOAuthRecoveryRoutingFixture(t *testing.T, advanced bool) (*OpenAIGatewayService, *oauthRecoveryTestGuard, *Account, *schedulerTestGatewayCache) {
	t.Helper()
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	openAIAdvancedSchedulerSettingCache.Store(&cachedOpenAIAdvancedSchedulerSetting{
		enabled: advanced, expiresAt: time.Now().Add(time.Hour).UnixNano(),
	})
	svc, guard, account := newOAuthRecoveryFixture(t)
	account.Concurrency = 40
	account.Priority = 0
	backup := *account
	backup.ID, backup.Priority = 2, 1
	guard.repo.accounts = []Account{*account, backup}
	svc.accountRepo = &oauthRecoveryRoutingRepo{oauthRecoveryTestRepo: guard.repo}
	svc.cfg = newSchedulerTestOpenAIWSV2Config()
	svc.cfg.Gateway.OpenAIWS.SessionHashReadOldFallback = true
	svc.cfg.Gateway.OpenAIWS.SessionHashDualWriteOld = true
	svc.cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc.concurrencyService = NewConcurrencyService(schedulerTestConcurrencyCache{})
	cache := &schedulerTestGatewayCache{}
	svc.cache = cache
	return svc, guard, account, cache
}

func startOAuthRecoveryRoutingFixture(t *testing.T, svc *OpenAIGatewayService, guard *oauthRecoveryTestGuard, account *Account) *oauthRecoveryFlight {
	t.Helper()
	flight, started := svc.oauthRecovery.begin(WithOpenAIOAuthRecoveryBudget(context.Background()), account, oauthRecoveryTest401)
	require.True(t, started)
	guard.repo.mu.Lock()
	guard.repo.accounts[0].ErrorMessage = ImmediateOAuthRecoveryErrorMessage
	guard.repo.mu.Unlock()
	return flight
}

func selectOAuthRecoveryRoutingAccount(svc *OpenAIGatewayService, ctx context.Context, hash string) (*AccountSelectionResult, error) {
	groupID := int64(7)
	selection, _, err := svc.SelectAccountWithSchedulerForCapability(ctx, &groupID, "", hash, "gpt-test", nil,
		OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilityResponses, false, false, true)
	return selection, err
}

func TestOAuthRecoveryBoundSessionWaitsDespiteHealthyBackup(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "advanced"}[advanced], func(t *testing.T) {
			svc, guard, account, cache := newOAuthRecoveryRoutingFixture(t, advanced)
			groupID := int64(7)
			const hash = "bound-session"
			require.NoError(t, svc.BindStickySession(context.Background(), &groupID, hash, account.ID))
			startOAuthRecoveryRoutingFixture(t, svc, guard, account)
			ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
			selection, err := selectOAuthRecoveryRoutingAccount(svc, ctx, hash)
			require.Nil(t, selection)
			require.ErrorIs(t, err, ErrOAuthRecoveryAdmissionInterrupted)
			require.Equal(t, account.ID, cache.sessionBindings[svc.openAISessionCacheKey(hash)])
			require.Empty(t, cache.deletedSessions)
			guard.complete(OpenAIOAuthReauthStatusSucceeded)
			waited, err := svc.WaitForOpenAIOAuthRecovery(ctx, &groupID, "gpt-test")
			require.True(t, waited)
			require.NoError(t, err)
			selection, err = selectOAuthRecoveryRoutingAccount(svc, ctx, hash)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, account.ID, selection.Account.ID)
			releaseLegacySchedulerDecisionSelection(selection)
		})
	}
}

func TestOAuthRecoveryTemporaryBackupKeepsBindingAndReturnsAfterRecovery(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "advanced"}[advanced], func(t *testing.T) {
			svc, guard, account, cache := newOAuthRecoveryRoutingFixture(t, advanced)
			svc.oauthRecovery.waitTime = 15 * time.Millisecond
			groupID := int64(7)
			const hash = "bound-session"
			ctx := withOpenAILegacySessionHash(WithOpenAIOAuthRecoveryBudget(context.Background()), "legacy-bound-session")
			require.NoError(t, svc.BindStickySession(ctx, &groupID, hash, account.ID))
			flight := startOAuthRecoveryRoutingFixture(t, svc, guard, account)
			selection, err := selectOAuthRecoveryRoutingAccount(svc, ctx, hash)
			require.Nil(t, selection)
			require.ErrorIs(t, err, ErrOAuthRecoveryAdmissionInterrupted)
			_, err = svc.WaitForOpenAIOAuthRecovery(ctx, &groupID, "gpt-test")
			require.ErrorIs(t, err, ErrOAuthRecoveryTimeout)
			require.True(t, PrepareOpenAIOAuthRecoveryFallback(ctx, err))
			selection, err = selectOAuthRecoveryRoutingAccount(svc, ctx, hash)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.EqualValues(t, 2, selection.Account.ID)
			require.NoError(t, svc.BindStickySessionAfterProfitAdmission(ctx, &groupID, hash, selection.Account.ID))
			releaseLegacySchedulerDecisionSelection(selection)
			require.Equal(t, account.ID, cache.sessionBindings[svc.openAISessionCacheKey(hash)])
			require.Equal(t, account.ID, cache.sessionBindings[svc.openAILegacySessionCacheKey(ctx, hash)])
			require.Empty(t, cache.deletedSessions)
			waited, err := svc.WaitForOpenAIOAuthRecovery(ctx, &groupID, "gpt-test")
			require.False(t, waited, "fallback must not join the same failed round again")
			require.NoError(t, err)
			guard.complete(OpenAIOAuthReauthStatusSucceeded)
			select {
			case <-flight.done:
			case <-time.After(time.Second):
				t.Fatal("recovery did not finish")
			}
			selection, err = selectOAuthRecoveryRoutingAccount(svc, WithOpenAIOAuthRecoveryBudget(context.Background()), hash)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, account.ID, selection.Account.ID)
			releaseLegacySchedulerDecisionSelection(selection)
		})
	}
}

func TestOAuthRecoveryUnboundRequestsUseHealthyBackup(t *testing.T) {
	svc, guard, account, _ := newOAuthRecoveryRoutingFixture(t, false)
	startOAuthRecoveryRoutingFixture(t, svc, guard, account)
	selection, err := selectOAuthRecoveryRoutingAccount(svc, WithOpenAIOAuthRecoveryBudget(context.Background()), "new-session")
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.EqualValues(t, 2, selection.Account.ID)
	releaseLegacySchedulerDecisionSelection(selection)
}

func TestOAuthRecoveryStartingDuringSelectionReleasesBackupSlot(t *testing.T) {
	for _, finishDuringSelection := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "failed"}[finishDuringSelection], func(t *testing.T) {
			svc, guard, account, cache := newOAuthRecoveryRoutingFixture(t, false)
			groupID := int64(7)
			const hash = "bound-session"
			require.NoError(t, svc.BindStickySession(context.Background(), &groupID, hash, account.ID))
			var released []int64
			svc.concurrencyService = NewConcurrencyService(schedulerTestConcurrencyCache{releasedIDs: &released})
			svc.accountRepo.(*oauthRecoveryRoutingRepo).beforeList = func() {
				flight := startOAuthRecoveryRoutingFixture(t, svc, guard, account)
				if finishDuringSelection {
					guard.complete(OpenAIOAuthReauthStatusFailed)
					select {
					case <-flight.done:
					case <-time.After(time.Second):
						t.Fatal("recovery did not fail during selection")
					}
				}
			}
			ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
			selection, err := selectOAuthRecoveryRoutingAccount(svc, ctx, hash)
			require.Nil(t, selection)
			require.ErrorIs(t, err, ErrOAuthRecoveryAdmissionInterrupted)
			require.Equal(t, []int64{2}, released, "the backup slot must be returned before recovery waiting")
			require.Equal(t, account.ID, cache.sessionBindings[svc.openAISessionCacheKey(hash)])
			require.Empty(t, cache.deletedSessions)
			if finishDuringSelection {
				_, err := svc.WaitForOpenAIOAuthRecovery(ctx, &groupID, "gpt-test")
				require.ErrorIs(t, err, ErrOAuthRecoveryFailed)
				require.True(t, PrepareOpenAIOAuthRecoveryFallback(ctx, err))
				selection, err = selectOAuthRecoveryRoutingAccount(svc, ctx, hash)
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.EqualValues(t, 2, selection.Account.ID)
				releaseLegacySchedulerDecisionSelection(selection)
				require.Equal(t, account.ID, cache.sessionBindings[svc.openAISessionCacheKey(hash)])
			}
		})
	}
}

type oauthRecoveryAcquireHook struct {
	schedulerTestConcurrencyCache
	beforeAcquire func(int64)
}

func (c oauthRecoveryAcquireHook) AcquireAccountSlot(ctx context.Context, accountID int64, limit int, requestID string) (bool, error) {
	if c.beforeAcquire != nil {
		c.beforeAcquire(accountID)
	}
	return c.schedulerTestConcurrencyCache.AcquireAccountSlot(ctx, accountID, limit, requestID)
}

func TestOAuthRecoveryFinishingBeforeBackupBindingKeepsOriginalOwner(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "advanced"}[advanced], func(t *testing.T) {
			svc, guard, account, cache := newOAuthRecoveryRoutingFixture(t, advanced)
			if advanced {
				openAIAdvancedSchedulerSettingCache.Store(&cachedOpenAIAdvancedSchedulerSetting{
					enabled: true, stickyWeightedEnabled: true, expiresAt: time.Now().Add(time.Hour).UnixNano(),
				})
			}
			groupID := int64(7)
			const hash = "bound-session"
			require.NoError(t, svc.BindStickySession(context.Background(), &groupID, hash, account.ID))
			var flight *oauthRecoveryFlight
			svc.accountRepo.(*oauthRecoveryRoutingRepo).beforeList = func() {
				flight = startOAuthRecoveryRoutingFixture(t, svc, guard, account)
			}
			svc.concurrencyService = NewConcurrencyService(oauthRecoveryAcquireHook{beforeAcquire: func(id int64) {
				if id != 2 {
					return
				}
				guard.complete(OpenAIOAuthReauthStatusSucceeded)
				select {
				case <-flight.done:
				case <-time.After(time.Second):
					t.Fatal("recovery did not finish before backup binding")
				}
			}})
			selection, err := selectOAuthRecoveryRoutingAccount(svc, WithOpenAIOAuthRecoveryBudget(context.Background()), hash)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.EqualValues(t, 2, selection.Account.ID)
			releaseLegacySchedulerDecisionSelection(selection)
			require.Equal(t, account.ID, cache.sessionBindings[svc.openAISessionCacheKey(hash)])
			require.Empty(t, cache.deletedSessions)
			selection, err = selectOAuthRecoveryRoutingAccount(svc, WithOpenAIOAuthRecoveryBudget(context.Background()), hash)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, account.ID, selection.Account.ID, "the following request must return to the recovered owner")
			releaseLegacySchedulerDecisionSelection(selection)
		})
	}
}

func TestOAuthRecoveryHardOwnershipAndCancellationCannotFallback(t *testing.T) {
	for _, hard := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "ownership"}[hard], func(t *testing.T) {
			ctx, cancel := context.WithCancel(WithOpenAIOAuthRecoveryBudget(context.Background()))
			defer cancel()
			b := recoveryBudget(ctx)
			b.observed = []*oauthRecoveryFlight{{accountID: 1}}
			b.routing.hardBound = hard
			if !hard {
				cancel()
			}
			require.False(t, PrepareOpenAIOAuthRecoveryFallback(ctx, ErrOAuthRecoveryTimeout))
			require.Empty(t, b.routing.excluded)
		})
	}
}

func TestOAuthRecoveryPreviousResponseOwnerWaitsWithoutChangingOwnership(t *testing.T) {
	svc, guard, account, _ := newOAuthRecoveryRoutingFixture(t, false)
	groupID := int64(7)
	const responseID = "resp_recovering_owner"
	ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
	store := svc.getOpenAIWSStateStore()
	require.NoError(t, store.BindResponseAccount(ctx, groupID, responseID, account.ID, time.Hour))
	flight := startOAuthRecoveryRoutingFixture(t, svc, guard, account)
	selection, _, err := svc.SelectAccountWithSchedulerForCapability(ctx, &groupID, responseID, "", "gpt-test", nil,
		OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilityResponses, false, false, true)
	require.Nil(t, selection)
	require.ErrorIs(t, err, ErrOAuthRecoveryAdmissionInterrupted)
	guard.complete(OpenAIOAuthReauthStatusFailed)
	select {
	case <-flight.done:
	case <-time.After(time.Second):
		t.Fatal("recovery did not finish")
	}
	_, err = svc.WaitForOpenAIOAuthRecovery(ctx, &groupID, "gpt-test")
	require.ErrorIs(t, err, ErrOAuthRecoveryFailed)
	require.False(t, PrepareOpenAIOAuthRecoveryFallback(ctx, err))
	ownerID, err := store.GetResponseAccount(ctx, groupID, responseID)
	require.NoError(t, err)
	require.Equal(t, account.ID, ownerID)
}

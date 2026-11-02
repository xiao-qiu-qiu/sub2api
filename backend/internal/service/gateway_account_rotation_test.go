package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type gatewayRotationSettingsStub struct{ mode string }

func (s gatewayRotationSettingsStub) GetAccountRotationSettings(context.Context) (*AccountRotationSettings, error) {
	settings := DefaultAccountRotationSettings()
	settings.Enabled, settings.Mode = true, s.mode
	return settings, nil
}

type gatewayRotationCacheStub struct {
	ConcurrencyCache
	states       map[int64]string
	refuseFirst  bool
	checkErr     error
	stickyProofs map[int64]bool
}

func (c *gatewayRotationCacheStub) CheckAccountRotation(_ context.Context, id int64, _ AccountRotationPolicy) (*AccountRotationState, error) {
	return &AccountRotationState{Phase: c.states[id]}, c.checkErr
}

func (c *gatewayRotationCacheStub) AcquireAccountSlotWithRotation(_ context.Context, id int64, _ int, _ string, _ AccountRotationPolicy, sticky bool, _ string) (bool, error) {
	c.stickyProofs[id] = sticky
	if c.refuseFirst && id == 1 {
		c.states[id] = "draining"
		return false, nil
	}
	return true, nil
}

func (*gatewayRotationCacheStub) RefreshAccountRotationSlot(context.Context, int64, string) error {
	return nil
}

func (*gatewayRotationCacheStub) ReleaseAccountSlot(context.Context, int64, string) error {
	return nil
}

func (*gatewayRotationCacheStub) GetAccountWaitingCount(context.Context, int64) (int, error) {
	return 0, nil
}

func (*gatewayRotationCacheStub) GetAccountsLoadBatch(_ context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	loads := make(map[int64]*AccountLoadInfo)
	for _, a := range accounts {
		loads[a.ID] = &AccountLoadInfo{AccountID: a.ID}
	}
	return loads, nil
}

type gatewayRotationAccountsStub struct {
	AccountRepository
	accounts []Account
}

func (r *gatewayRotationAccountsStub) GetByID(_ context.Context, id int64) (*Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			return &r.accounts[i], nil
		}
	}
	return nil, ErrAccountNotFound
}

func (r *gatewayRotationAccountsStub) ListSchedulableUngroupedByPlatforms(context.Context, []string) ([]Account, error) {
	return append([]Account(nil), r.accounts...), nil
}

func (r *gatewayRotationAccountsStub) ListSchedulableUngroupedByPlatform(context.Context, string) ([]Account, error) {
	return append([]Account(nil), r.accounts...), nil
}

type gatewayRotationBindingStub struct {
	GatewayCache
	accountID int64
}

func (c *gatewayRotationBindingStub) GetSessionAccountID(context.Context, int64, string) (int64, error) {
	return c.accountID, nil
}

func (c *gatewayRotationBindingStub) SetSessionAccountID(_ context.Context, _ int64, _ string, id int64, _ time.Duration) error {
	c.accountID = id
	return nil
}

func (*gatewayRotationBindingStub) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}

func (*gatewayRotationBindingStub) DeleteSessionAccountID(context.Context, int64, string) error {
	return errors.New("rotation must not delete an existing binding")
}

func newGatewayRotationFixture(mode, platform string) (*GatewayService, *gatewayRotationCacheStub, *gatewayRotationAccountsStub) {
	cache := &gatewayRotationCacheStub{states: map[int64]string{}, stickyProofs: map[int64]bool{}}
	accounts := &gatewayRotationAccountsStub{accounts: []Account{
		{ID: 1, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 1, Concurrency: 2},
		{ID: 2, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 2, Concurrency: 2},
	}}
	concurrency := &ConcurrencyService{cache: cache, rotationSettings: gatewayRotationSettingsStub{mode: mode}}
	return &GatewayService{accountRepo: accounts, concurrencyService: concurrency}, cache, accounts
}

func TestGatewayRotationOldStickyProofAndAggressiveSwitch(t *testing.T) {
	for _, mode := range []string{AccountRotationModeAggressive, AccountRotationModeSticky} {
		t.Run(mode, func(t *testing.T) {
			svc, cache, accounts := newGatewayRotationFixture(mode, PlatformAnthropic)
			cache.states[1] = "draining"
			svc.cache = &gatewayRotationBindingStub{accountID: 1}
			selection, err := svc.SelectAccountWithLoadAwareness(t.Context(), nil, "old-session", "", nil, "", 0)
			require.NoError(t, err)
			require.True(t, selection.Acquired)
			t.Cleanup(selection.ReleaseFunc)
			expected := int64(2)
			if mode == AccountRotationModeSticky {
				expected = 1
			}
			require.Equal(t, expected, selection.Account.ID)
			require.Equal(t, expected == 1, cache.stickyProofs[expected])
			replayed := ContextWithSelectionProfitGate(t.Context(), selection)
			require.True(t, HasAccountRotationTracking(replayed))
			require.Equal(t, expected == 1, AccountRotationSticky(replayed, expected))
			require.True(t, accounts.accounts[0].Schedulable)
			require.False(t, accounts.accounts[0].rotationTracked, "shared accounts must not acquire request metadata")
		})
	}
}

func TestGatewayRotationAdmissionRaceRetriesInsteadOfWaiting(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "load"}[batch], func(t *testing.T) {
			svc, cache, _ := newGatewayRotationFixture(AccountRotationModeAggressive, PlatformAnthropic)
			cache.refuseFirst = true
			svc.cfg = &config.Config{Gateway: config.GatewayConfig{Scheduling: config.GatewaySchedulingConfig{LoadBatchEnabled: batch}}}
			selection, err := svc.SelectAccountWithLoadAwareness(t.Context(), nil, "", "", nil, "", 0)
			require.NoError(t, err)
			require.EqualValues(t, 2, selection.Account.ID)
			require.True(t, selection.Acquired)
			require.Nil(t, selection.WaitPlan)
			selection.ReleaseFunc()
		})
	}
}

func TestGeminiRotationOldSelectionClonesProofAndRejectsCacheFailure(t *testing.T) {
	base, cache, accounts := newGatewayRotationFixture(AccountRotationModeSticky, PlatformGemini)
	cache.states[1] = "draining"
	svc := &GeminiMessagesCompatService{accountRepo: accounts, cache: &gatewayRotationBindingStub{accountID: 1}}
	svc.SetConcurrencyService(base.concurrencyService)
	selected, err := svc.SelectAccountForModel(t.Context(), nil, "old-session", "")
	require.NoError(t, err)
	require.EqualValues(t, 1, selected.ID)
	require.NotSame(t, &accounts.accounts[0], selected)
	ctx := ContextWithAccountRotationSelection(t.Context(), selected)
	require.True(t, AccountRotationSticky(ctx, 1))
	require.True(t, HasAccountRotationTracking(ctx))
	require.False(t, accounts.accounts[0].rotationSticky)
	cache.checkErr = errors.New("rotation cache unavailable")
	_, err = svc.SelectAccountForModel(t.Context(), nil, "old-session", "")
	require.ErrorContains(t, err, "rotation cache unavailable")
}

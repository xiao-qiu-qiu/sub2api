package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type openAIRotationSettingsStub struct{ settings AccountRotationSettings }

func (s openAIRotationSettingsStub) GetAccountRotationSettings(context.Context) (*AccountRotationSettings, error) {
	settings := s.settings
	return &settings, nil
}

type openAIRotationAcquireCall struct {
	id     int64
	max    int
	sticky bool
	policy AccountRotationPolicy
	pool   string
}

// Only models admission outcomes. Work/idle/rest clocks are exercised by the
// Redis cache tests, rather than duplicated in a second implementation here.
type openAIRotationConcurrencyCache struct {
	schedulerTestConcurrencyCache
	phases    map[int64]string
	checks    []int64
	calls     []openAIRotationAcquireCall
	onAcquire func(int64)
	checkErr  error
}

func (c *openAIRotationConcurrencyCache) CheckAccountRotation(_ context.Context, id int64, _ AccountRotationPolicy) (*AccountRotationState, error) {
	c.checks = append(c.checks, id)
	return &AccountRotationState{Phase: c.phases[id]}, c.checkErr
}

func (c *openAIRotationConcurrencyCache) AcquireAccountSlotWithRotation(ctx context.Context, id int64, max int, requestID string, policy AccountRotationPolicy, sticky bool, pool string) (bool, error) {
	c.calls = append(c.calls, openAIRotationAcquireCall{id, max, sticky, policy, pool})
	if c.onAcquire != nil {
		c.onAcquire(id)
	}
	if c.phases[id] == "resting" || (c.phases[id] == "draining" && (policy.Mode != AccountRotationModeSticky || !sticky)) {
		return false, nil
	}
	return c.AcquireAccountSlot(ctx, id, max, requestID)
}

func (*openAIRotationConcurrencyCache) RefreshAccountRotationSlot(context.Context, int64, string) error {
	return nil
}

func newOpenAIRotationTestService(t *testing.T, advanced, batch, enabled bool, mode string) (*OpenAIGatewayService, *openAIRotationConcurrencyCache) {
	t.Helper()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	accounts := []Account{
		{ID: 48101, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 2, Priority: 0},
		{ID: 48102, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 2, Priority: 10},
	}
	repo := schedulerTestOpenAIAccountRepo{accounts: accounts}
	cache := &openAIRotationConcurrencyCache{phases: make(map[int64]string)}
	concurrency := NewConcurrencyService(cache)
	settings := *DefaultAccountRotationSettings()
	settings.Enabled, settings.Mode = enabled, mode
	concurrency.SetAccountRotation(openAIRotationSettingsStub{settings}, repo)
	cfg := newSchedulerTestOpenAIWSV2Config()
	cfg.Gateway.Scheduling = config.GatewaySchedulingConfig{
		LoadBatchEnabled: batch, StickySessionMaxWaiting: 3, StickySessionWaitTimeout: 45 * time.Second,
		FallbackMaxWaiting: 100, FallbackWaitTimeout: 30 * time.Second,
	}
	cfg.Gateway.OpenAIWS.LBTopK = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights = config.GatewayOpenAIWSSchedulerScoreWeights{Priority: 1}
	return &OpenAIGatewayService{
		accountRepo: repo, cache: &schedulerTestGatewayCache{}, cfg: cfg,
		concurrencyService: concurrency,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(fmt.Sprint(advanced), "false"),
	}, cache
}

func selectOpenAIRotationTestAccount(svc *OpenAIGatewayService, ctx context.Context, session string) (*AccountSelectionResult, error) {
	selection, _, err := svc.SelectAccountWithScheduler(ctx, nil, "", session, "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	return selection, err
}

func cleanupOpenAIRotationSelection(t *testing.T, selection *AccountSelectionResult) {
	t.Helper()
	if selection != nil && selection.ReleaseFunc != nil {
		t.Cleanup(selection.ReleaseFunc)
	}
}

func TestOpenAIAccountRotationSchedulersFilterRestingAndOffKeepsPriority(t *testing.T) {
	for _, path := range []struct {
		name            string
		advanced, batch bool
	}{{"advanced", true, true}, {"legacy_load", false, true}, {"legacy_priority", false, false}} {
		for _, enabled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/enabled=%t", path.name, enabled), func(t *testing.T) {
				svc, cache := newOpenAIRotationTestService(t, path.advanced, path.batch, enabled, AccountRotationModeAggressive)
				cache.phases[48101] = "resting"
				selection, err := selectOpenAIRotationTestAccount(svc, t.Context(), "")
				cleanupOpenAIRotationSelection(t, selection)
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.True(t, selection.Acquired)
				if enabled {
					require.EqualValues(t, 48102, selection.Account.ID)
					require.NotEmpty(t, cache.checks)
					require.Len(t, cache.calls, 1, "resting account must be filtered before reservation")
					require.EqualValues(t, 48102, cache.calls[0].id)
					require.True(t, HasAccountRotationTracking(ContextWithSelectionProfitGate(t.Context(), selection)))
				} else {
					require.EqualValues(t, 48101, selection.Account.ID)
					require.Empty(t, cache.checks)
					require.Empty(t, cache.calls)
					require.False(t, HasAccountRotationTracking(ContextWithSelectionProfitGate(t.Context(), selection)))
				}
			})
		}
	}
}

func TestOpenAIAccountRotationStickyRequiresExistingBinding(t *testing.T) {
	for _, advanced := range []bool{true, false} {
		for _, mode := range []string{AccountRotationModeSticky, AccountRotationModeAggressive} {
			for _, existing := range []bool{true, false} {
				t.Run(fmt.Sprintf("advanced=%t/%s/existing=%t", advanced, mode, existing), func(t *testing.T) {
					svc, cache := newOpenAIRotationTestService(t, advanced, true, true, mode)
					cache.phases[48101] = "draining"
					bindings, ok := svc.cache.(*schedulerTestGatewayCache)
					require.True(t, ok)
					if existing {
						require.NoError(t, svc.setStickySessionAccountID(t.Context(), nil, "session", 48101, time.Hour))
					}
					selection, err := selectOpenAIRotationTestAccount(svc, t.Context(), "session")
					cleanupOpenAIRotationSelection(t, selection)
					require.NoError(t, err)
					require.NotNil(t, selection)
					require.True(t, selection.Acquired)
					allowed := existing && mode == AccountRotationModeSticky
					want := int64(48102)
					if allowed {
						want = 48101
						id, err := svc.getStickySessionAccountID(t.Context(), nil, "session")
						require.NoError(t, err)
						require.EqualValues(t, 48101, id)
					}
					require.Equal(t, want, selection.Account.ID)
					require.Empty(t, bindings.deletedSessions, "temporary rotation must not invalidate the old binding")
					require.Equal(t, allowed, AccountRotationSticky(ContextWithSelectionProfitGate(t.Context(), selection), want))
					require.Len(t, cache.calls, 1)
					require.Equal(t, allowed, cache.calls[0].sticky, "a newly supplied session hash cannot create sticky proof")
				})
			}
		}
	}
}

func TestOpenAIAccountRotationStickyWaitPlanRetainsServerProof(t *testing.T) {
	for _, path := range []struct {
		name            string
		advanced, batch bool
	}{{"advanced", true, true}, {"legacy_load", false, true}, {"legacy_priority", false, false}} {
		t.Run(path.name, func(t *testing.T) {
			svc, cache := newOpenAIRotationTestService(t, path.advanced, path.batch, true, AccountRotationModeSticky)
			cache.phases[48101] = "draining"
			cache.acquireResults = map[int64]bool{48101: false}
			require.NoError(t, svc.setStickySessionAccountID(t.Context(), nil, "old-session", 48101, time.Hour))
			selection, err := selectOpenAIRotationTestAccount(svc, t.Context(), "old-session")
			cleanupOpenAIRotationSelection(t, selection)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.EqualValues(t, 48101, selection.Account.ID)
			require.False(t, selection.Acquired)
			require.NotNil(t, selection.WaitPlan)
			ctx := ContextWithSelectionProfitGate(t.Context(), selection)
			require.True(t, AccountRotationSticky(ctx, 48101))
			require.False(t, AccountRotationSticky(ctx, 48102), "proof belongs only to the bound account")
			require.True(t, HasAccountRotationTracking(ctx))
			cache.acquireResults[48101] = true
			result, err := svc.concurrencyService.AcquireAccountSlot(ctx, 48101, selection.WaitPlan.MaxConcurrency)
			require.NoError(t, err)
			require.True(t, result.Acquired)
			t.Cleanup(result.ReleaseFunc)
			cache.phases[48101] = "resting"
			result, err = svc.concurrencyService.AcquireAccountSlot(ctx, 48101, selection.WaitPlan.MaxConcurrency)
			require.ErrorIs(t, err, ErrAccountRotationUnavailable, "a valid old WaitPlan cannot reopen rest")
			require.Nil(t, result)
		})
	}
}

func TestOpenAIAccountRotationRestingStickyLookupDoesNotDeleteBinding(t *testing.T) {
	svc, cache := newOpenAIRotationTestService(t, true, true, true, AccountRotationModeSticky)
	cache.phases[48101] = "resting"
	require.NoError(t, svc.setStickySessionAccountID(t.Context(), nil, "old-session", 48101, time.Hour))
	ctx := svc.withAccountRotationSession(t.Context(), nil, "old-session")
	scheduler := newDefaultOpenAIAccountScheduler(svc, nil).(*defaultOpenAIAccountScheduler)
	selection, _, err := scheduler.selectBySessionHash(ctx, OpenAIAccountScheduleRequest{
		Platform: PlatformOpenAI, SessionHash: "old-session", RequestedModel: "gpt-5.1", RequiredTransport: OpenAIUpstreamTransportAny,
	})
	require.NoError(t, err)
	require.Nil(t, selection)
	account := svc.tryStickySessionHit(ctx, nil, PlatformOpenAI, "old-session", "gpt-5.1", nil, false, 0, "")
	require.Nil(t, account)
	owner, err := svc.getStickySessionAccountID(t.Context(), nil, "old-session")
	require.NoError(t, err)
	require.EqualValues(t, 48101, owner)
	require.Empty(t, svc.cache.(*schedulerTestGatewayCache).deletedSessions)
	require.Empty(t, cache.calls)
}

func TestOpenAIAccountRotationExpiryDuringReservationReselects(t *testing.T) {
	for _, path := range []struct {
		name            string
		advanced, batch bool
	}{{"advanced", true, true}, {"advanced_load_failure", true, true}, {"legacy_load", false, true}, {"legacy_priority", false, false}} {
		t.Run(path.name, func(t *testing.T) {
			svc, cache := newOpenAIRotationTestService(t, path.advanced, path.batch, true, AccountRotationModeAggressive)
			if path.name == "advanced_load_failure" {
				cache.loadBatchErr = errors.New("load snapshot unavailable")
			}
			cache.onAcquire = func(id int64) {
				if id == 48101 {
					cache.phases[id] = "draining"
				}
			}
			selection, err := selectOpenAIRotationTestAccount(svc, t.Context(), "")
			cleanupOpenAIRotationSelection(t, selection)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.EqualValues(t, 48102, selection.Account.ID)
			require.True(t, selection.Acquired)
			require.Nil(t, selection.WaitPlan, "a closed work window is not a full concurrency queue")
			require.GreaterOrEqual(t, len(cache.calls), 2)
			require.EqualValues(t, 48102, cache.calls[len(cache.calls)-1].id)
		})
	}
}

func TestOpenAIAccountRotationPreviousResponsePreservesOwnerBinding(t *testing.T) {
	for _, tc := range []struct {
		mode, phase string
		allowed     bool
	}{{AccountRotationModeSticky, "draining", true}, {AccountRotationModeSticky, "resting", false}, {AccountRotationModeAggressive, "draining", false}, {AccountRotationModeAggressive, "resting", false}} {
		t.Run(tc.mode+"/"+tc.phase, func(t *testing.T) {
			svc, cache := newOpenAIRotationTestService(t, true, true, true, tc.mode)
			cache.phases[48101] = tc.phase
			store := svc.getOpenAIWSStateStore()
			require.NoError(t, store.BindResponseAccount(t.Context(), 0, "resp_rotation", 48101, time.Hour))
			selection, err := svc.selectAccountByPreviousResponseIDForCapability(t.Context(), nil, "resp_rotation", "gpt-5.1", nil, OpenAIEndpointCapabilityResponses, false)
			cleanupOpenAIRotationSelection(t, selection)
			require.NoError(t, err)
			if tc.allowed {
				require.NotNil(t, selection)
				require.True(t, selection.Acquired)
				require.True(t, AccountRotationSticky(ContextWithSelectionProfitGate(t.Context(), selection), 48101))
				require.Len(t, cache.calls, 1)
				require.True(t, cache.calls[0].sticky)
			} else {
				require.Nil(t, selection, "previous_response_id cannot bypass rest or aggressive drain")
				require.Empty(t, cache.calls)
			}
			owner, err := store.GetResponseAccount(t.Context(), 0, "resp_rotation")
			require.NoError(t, err)
			require.EqualValues(t, 48101, owner, "rotation must preserve provider ownership")
		})
	}
}

func TestOpenAIAccountRotationWeightedFallbackCannotBypassRest(t *testing.T) {
	for _, tc := range []struct {
		mode, phase string
		allowed     bool
	}{{AccountRotationModeSticky, "draining", true}, {AccountRotationModeSticky, "resting", false}, {AccountRotationModeAggressive, "draining", false}, {AccountRotationModeAggressive, "resting", false}} {
		t.Run(tc.mode+"/"+tc.phase, func(t *testing.T) {
			svc, cache := newOpenAIRotationTestService(t, true, true, true, tc.mode)
			cache.phases[48101] = tc.phase
			scheduler := newDefaultOpenAIAccountScheduler(svc, nil).(*defaultOpenAIAccountScheduler)
			ctx := svc.concurrencyService.AccountRotationContext(WithAccountRotationSticky(t.Context(), 48101))
			selection, err := scheduler.tryFallbackToWeightedSticky(ctx, OpenAIAccountScheduleRequest{
				Platform: PlatformOpenAI, SessionHash: "old-session", StickyAccountID: 48101,
				StickyPreviousAccountID: 48101, StickyWeighted: true, PreserveStickyBinding: true,
				RequestedModel: "gpt-5.1", RequiredTransport: OpenAIUpstreamTransportAny,
			})
			cleanupOpenAIRotationSelection(t, selection)
			require.NoError(t, err)
			if tc.allowed {
				require.NotNil(t, selection)
				require.True(t, selection.Acquired)
				require.EqualValues(t, 48101, selection.Account.ID)
			} else {
				require.Nil(t, selection, "weighted fallback cannot return a closed account or a WaitPlan for it")
			}
		})
	}
}

func TestOpenAIAccountRotationUnlimitedConcurrencyUsesTrackedReservation(t *testing.T) {
	svc, cache := newOpenAIRotationTestService(t, true, true, true, AccountRotationModeAggressive)
	var releases []int64
	cache.releasedIDs = &releases
	result, err := svc.concurrencyService.AcquireAccountSlot(t.Context(), 48101, 0)
	require.NoError(t, err)
	require.True(t, result.Acquired)
	t.Cleanup(result.ReleaseFunc)
	require.NotEmpty(t, result.RequestID, "unlimited accounts must still own a tracked request")
	require.Len(t, cache.calls, 1)
	call := cache.calls[0]
	require.Zero(t, call.max)
	require.Equal(t, 300*60, call.policy.WorkSeconds)
	require.Equal(t, 10*60, call.policy.RestMinSeconds)
	require.Equal(t, 30*60, call.policy.RestMaxSeconds)
	require.Equal(t, AccountRotationModeAggressive, call.policy.Mode)
	require.NotEmpty(t, call.pool)
	result.ReleaseFunc()
	result.ReleaseFunc()
	require.Equal(t, []int64{48101}, releases)
	cache.phases[48101] = "resting"
	result, err = svc.concurrencyService.AcquireAccountSlot(WithAccountRotationSticky(t.Context(), 48101), 48101, 0)
	require.ErrorIs(t, err, ErrAccountRotationUnavailable)
	require.Nil(t, result)
}

func TestOpenAIAccountRotationCacheFailureDoesNotAdmit(t *testing.T) {
	svc, cache := newOpenAIRotationTestService(t, true, true, true, AccountRotationModeSticky)
	cache.checkErr = errors.New("rotation state unavailable")
	selection, err := selectOpenAIRotationTestAccount(svc, t.Context(), "new-session")
	cleanupOpenAIRotationSelection(t, selection)
	require.ErrorContains(t, err, "rotation state unavailable")
	require.Nil(t, selection)
	require.Empty(t, cache.calls)
}

func TestOpenAIAccountRotationDirectSchedulerDefaultsPlatform(t *testing.T) {
	svc, cache := newOpenAIRotationTestService(t, true, true, true, AccountRotationModeAggressive)
	cache.phases[48101] = "resting"
	selection, _, err := newDefaultOpenAIAccountScheduler(svc, nil).Select(t.Context(), OpenAIAccountScheduleRequest{
		RequestedModel: "gpt-5.1", RequiredTransport: OpenAIUpstreamTransportAny,
	})
	cleanupOpenAIRotationSelection(t, selection)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.True(t, selection.Acquired)
	require.EqualValues(t, 48102, selection.Account.ID, "empty platform must retain the OpenAI default and rotation filtering")
}

func TestOpenAIAccountRotationAdvancedExpiryOnFreshLoadRetryReselects(t *testing.T) {
	svc, cache := newOpenAIRotationTestService(t, true, true, true, AccountRotationModeAggressive)
	cache.acquireResults = map[int64]bool{48101: false}
	cache.onAcquire = func(id int64) {
		if id == 48101 && len(cache.calls) >= 2 {
			cache.phases[id] = "draining"
		}
	}
	selection, err := selectOpenAIRotationTestAccount(svc, t.Context(), "")
	cleanupOpenAIRotationSelection(t, selection)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.EqualValues(t, 48102, selection.Account.ID)
	require.True(t, selection.Acquired)
	require.Nil(t, selection.WaitPlan)
	require.GreaterOrEqual(t, len(cache.calls), 3, "first admission saw a full account; only the fresh retry saw drain")
}

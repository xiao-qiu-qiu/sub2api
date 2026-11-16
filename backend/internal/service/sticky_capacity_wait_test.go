//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenAIStickyCapacityWaitKeepsOwnerAcrossSchedulers(t *testing.T) {
	for _, path := range []string{"advanced", "weighted", "legacy batch", "legacy sequential", "previous weighted"} {
		for _, timeout := range []time.Duration{time.Second, 45 * time.Second} {
			t.Run(path+"/"+timeout.String(), func(t *testing.T) {
				group := int64(11)
				accounts := []Account{
					{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 100, GroupIDs: []int64{group}, Extra: map[string]any{"openai_apikey_responses_websockets_v2_enabled": true}},
					{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 10, Priority: 0, GroupIDs: []int64{group}},
				}
				cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:capacity-owner": 1}}
				cfg := newSchedulerTestOpenAIWSV2Config()
				cfg.Gateway.Scheduling = config.GatewaySchedulingConfig{
					LoadBatchEnabled: path != "legacy sequential", StickySessionWaitTimeout: timeout,
					StickySessionMaxWaiting: 2, FallbackWaitTimeout: 30 * time.Second, FallbackMaxWaiting: 100,
				}
				cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
				var probes []int64
				svc := &OpenAIGatewayService{
					accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cache: cache, cfg: cfg,
					concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
						acquireResults: map[int64]bool{1: false, 2: true}, acquiredIDs: &probes,
						waitCounts: map[int64]int{1: 999},
						loadMap:    map[int64]*AccountLoadInfo{1: {AccountID: 1, CurrentConcurrency: 1, LoadRate: 100, WaitingCount: 999}, 2: {AccountID: 2}},
					}),
				}
				var selection *AccountSelectionResult
				var err error
				if path == "legacy batch" || path == "legacy sequential" {
					selection, err = svc.SelectAccountWithLoadAwareness(t.Context(), &group, "capacity-owner", "gpt-test", nil)
				} else {
					req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, GroupID: &group, SessionHash: "capacity-owner", RequestedModel: "gpt-test", StickyWeighted: path == "weighted" || path == "previous weighted"}
					if path == "previous weighted" {
						req.PreviousResponseID, req.PreviousResponseCanMove = "resp_capacity_owner", true
						require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(t.Context(), group, req.PreviousResponseID, 1, time.Hour))
					}
					scheduler := newDefaultOpenAIAccountScheduler(svc, nil)
					selection, _, err = scheduler.Select(t.Context(), req)
				}
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.EqualValues(t, 1, selection.Account.ID, "an idle higher-priority backup must not take a busy owner's conversation")
				require.False(t, selection.Acquired)
				require.NotNil(t, selection.WaitPlan)
				require.Equal(t, stickySessionWaitTimeout(timeout), selection.WaitPlan.Timeout)
				require.GreaterOrEqual(t, selection.WaitPlan.Timeout, 10*time.Second)
				require.Equal(t, 2, selection.WaitPlan.MaxWaiting, "full queue admission stays bounded")
				require.Equal(t, []int64{1}, probes, "capacity must not trigger even a slot probe on the backup")
				require.EqualValues(t, 1, cache.sessionBindings["openai:capacity-owner"])
				require.Empty(t, cache.deletedSessions)
			})
		}
	}
}

func TestOpenAIStickyCapacityProtectionStillFailsOverHardFailures(t *testing.T) {
	for _, reason := range []string{"excluded", "disabled", "quota window", "policy cooldown"} {
		t.Run(reason, func(t *testing.T) {
			group := int64(11)
			accounts := []Account{
				{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{group}},
				{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{group}},
			}
			req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, GroupID: &group, SessionHash: "hard-failure", RequestedModel: "gpt-test", StickyWeighted: true}
			until := time.Now().Add(time.Minute)
			switch reason {
			case "excluded":
				req.ExcludedIDs = map[int64]struct{}{1: {}}
			case "disabled":
				accounts[0].Schedulable = false
			case "quota window":
				accounts[0].RateLimitResetAt = &until
			case "policy cooldown":
				accounts[0].TempUnschedulableUntil, accounts[0].TempUnschedulableReason = &until, "usage_policy"
			}
			svc := &OpenAIGatewayService{
				accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cfg: newSchedulerTestOpenAIWSV2Config(),
				cache:              &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:hard-failure": 1}},
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
			}
			scheduler := newDefaultOpenAIAccountScheduler(svc, nil)
			selection, _, err := scheduler.Select(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.EqualValues(t, 2, selection.Account.ID)
			require.True(t, selection.Acquired)
			t.Cleanup(selection.ReleaseFunc)
		})
	}
}

func TestOpenAIStickyCapacityWaitIgnoresEffortScopedSoftTTFT(t *testing.T) {
	group := int64(11)
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{group}},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{group}},
	}
	cfg := newSchedulerTestOpenAIWSV2Config()
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 1000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = .5
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cfg: cfg,
		cache:              &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:slow-effort": 1}},
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{1: false, 2: true}}),
	}
	stats := newOpenAIAccountRuntimeStats()
	req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, GroupID: &group, SessionHash: "slow-effort", RequestedModel: "gpt-test", ObservationEffort: "xhigh", ObservationEffortScoped: true}
	for range 5 {
		stats.attempts.report(priorityRequestAttemptKey(req, 1), true, time.Now(), 20000)
	}
	scheduler := newDefaultOpenAIAccountScheduler(svc, stats)
	selection, decision, err := scheduler.Select(t.Context(), req)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.EqualValues(t, 1, selection.Account.ID)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
}

func TestGatewayStickyFullQueueKeepsOwnerWithMinimumWait(t *testing.T) {
	for _, path := range []string{"batch", "sequential", "model route"} {
		t.Run(path, func(t *testing.T) {
			accounts := []Account{
				{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1},
				{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 10},
			}
			repo := &mockAccountRepoForPlatform{accounts: accounts, accountsByID: map[int64]*Account{1: &accounts[0], 2: &accounts[1]}}
			cache := &mockGatewayCacheForPlatform{sessionBindings: map[string]int64{"cache-owner": 1}}
			cfg := testConfig()
			cfg.Gateway.Scheduling = config.GatewaySchedulingConfig{
				LoadBatchEnabled: path != "sequential", StickySessionWaitTimeout: time.Second,
				StickySessionMaxWaiting: 1, FallbackWaitTimeout: time.Minute, FallbackMaxWaiting: 100,
			}
			svc := &GatewayService{accountRepo: repo, cache: cache, cfg: cfg,
				concurrencyService: NewConcurrencyService(&mockConcurrencyCache{acquireResults: map[int64]bool{1: false, 2: true}, waitCounts: map[int64]int{1: 999}})}
			var groupID *int64
			if path == "model route" {
				id := int64(11)
				groupID = &id
				svc.groupRepo = &mockGroupRepoForGateway{groups: map[int64]*Group{id: {
					ID: id, Platform: PlatformAnthropic, Status: StatusActive, Hydrated: true,
					ModelRoutingEnabled: true, ModelRouting: map[string][]int64{"claude-test": {1, 2}},
				}}}
			}
			selection, err := svc.SelectAccountWithLoadAwareness(t.Context(), groupID, "cache-owner", "claude-test", nil, "", 0)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.EqualValues(t, 1, selection.Account.ID)
			require.NotNil(t, selection.WaitPlan)
			require.Equal(t, 10*time.Second, selection.WaitPlan.Timeout)
			require.Equal(t, 1, selection.WaitPlan.MaxWaiting)
			require.EqualValues(t, 1, cache.sessionBindings["cache-owner"])
		})
	}
}

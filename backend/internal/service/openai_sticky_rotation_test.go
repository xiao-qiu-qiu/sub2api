//go:build unit

package service

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func newStickyRotationSoftEscapeFixture(t *testing.T, strategy string) (*OpenAIGatewayService, *defaultOpenAIAccountScheduler, *rotationHoldTestCache, OpenAIAccountScheduleRequest) {
	t.Helper()
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	group := int64(11)
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 10, GroupIDs: []int64{group}},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 10, GroupIDs: []int64{group}},
	}
	priority := DefaultPrioritySchedulingConfig()
	priority.TargetTTFTMs = 3000
	priority.Enabled = strategy == "latency" || strategy == "rebalance"
	reader := &priorityReaderStub{}
	if strategy == "latency" {
		badAt := time.Now().Add(-time.Minute)
		reader.signal = map[int64]PrioritySchedulingSignal{
			1: {Samples: 20, P90TTFTMs: 20000, LatestBadLatencyAt: &badAt, QualitySamples: 10, QualityPassed: 10},
			2: {Samples: 20, P90TTFTMs: 500, QualitySamples: 10, QualityPassed: 10},
		}
	}
	cache := newRotationHoldTestCache()
	if strategy == "rebalance" {
		accounts[0].Type, accounts[0].Concurrency, accounts[0].GroupIDs = AccountTypeOAuth, 30, []int64{3, 5, 11, 13, 15}
		accounts[1].Type, accounts[1].Concurrency, accounts[1].GroupIDs = AccountTypeOAuth, 20, []int64{11, 15}
		cache.loadMap = map[int64]*AccountLoadInfo{1: {AccountID: 1, CurrentConcurrency: 18}, 2: {AccountID: 2}}
	}
	if strategy == "full" {
		cache.full[1] = true
	}
	if strategy == "weighted" {
		cache.loadMap = map[int64]*AccountLoadInfo{1: {AccountID: 1, CurrentConcurrency: 10, LoadRate: 100}, 2: {AccountID: 2}}
	}
	svc := priorityGateway(priority, reader)
	svc.cfg = newSchedulerTestOpenAIWSV2Config()
	svc.cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
	svc.cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 1000
	svc.cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = .3
	svc.cfg.Gateway.Scheduling = config.GatewaySchedulingConfig{LoadBatchEnabled: true, StickySessionMaxWaiting: 3, StickySessionWaitTimeout: 45 * time.Second, FallbackMaxWaiting: 100, FallbackWaitTimeout: 30 * time.Second}
	svc.accountRepo = schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: accounts}}
	svc.cache = &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:rotation-soft": 1}}
	svc.concurrencyService = NewConcurrencyService(cache)
	settings := *DefaultAccountRotationSettings()
	settings.Enabled = true
	svc.concurrencyService.SetAccountRotation(openAIRotationSettingsStub{settings}, svc.accountRepo)
	stats := newOpenAIAccountRuntimeStats()
	if strategy == "ttft" {
		slow := 20000
		stats.report(1, true, &slow)
	}
	if strategy == "error_rate" {
		for range 5 {
			stats.report(1, false, nil)
		}
	}
	svc.openaiAccountStats = stats
	scheduler := &defaultOpenAIAccountScheduler{service: svc, stats: stats}
	req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, GroupID: &group, RequestedModel: "gpt-test", HistoryModel: "gpt-test", InboundEndpoint: "/v1/responses", UseUpstreamTokenCost: true, SessionHash: "rotation-soft", StickyAccountID: 1, StickyWeighted: strategy == "weighted"}
	if priority.Enabled {
		pool := []openAIAccountCandidateScore{{account: &accounts[0]}, {account: &accounts[1]}}
		require.Eventually(t, func() bool { return svc.priorityHistory(req, priority, pool).ready() }, time.Second, time.Millisecond)
	}
	return svc, scheduler, cache, req
}

func TestOpenAIStickyRotationWaitBypassesSoftEscapes(t *testing.T) {
	for _, strategy := range []string{"ttft", "error_rate", "latency", "rebalance", "full", "weighted"} {
		for _, state := range []AccountRotationState{
			{Phase: "draining", Hold: true, Reason: "scheduled"},
			{Phase: "resting", Hold: true, Reason: "manual"},
			{Phase: "working", Recovering: true, RetryAfterMS: 500},
		} {
			name := state.Reason
			if state.Recovering {
				name = "recovering"
			}
			t.Run(strategy+"/"+name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					svc, scheduler, cache, req := newStickyRotationSoftEscapeFixture(t, strategy)
					cache.setState(1, state)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					ctx = svc.withAccountRotationSession(ctx, req.GroupID, req.SessionHash)
					type outcome struct {
						selection *AccountSelectionResult
						escaped   bool
						err       error
					}
					done := make(chan outcome, 1)
					defer func() {
						cancel()
						synctest.Wait()
						select {
						case result := <-done:
							if result.selection != nil && result.selection.ReleaseFunc != nil {
								result.selection.ReleaseFunc()
							}
						default:
						}
					}()
					go func() {
						if req.StickyWeighted {
							selection, _, err := scheduler.Select(ctx, req)
							done <- outcome{selection: selection, err: err}
							return
						}
						selection, escaped, err := scheduler.selectBySessionHash(ctx, req)
						done <- outcome{selection, escaped, err}
					}()
					synctest.Wait()
					cache.assertCounts(t, 1, 0, 1)
					requireRotationHoldPending(t, done)
					cache.assertCounts(t, 2, 0, 0)
					time.Sleep(time.Minute)
					synctest.Wait()
					requireRotationHoldPending(t, done)
					cache.setState(1, AccountRotationState{Phase: "ready"})
					if strategy == "full" {
						time.Sleep(time.Second)
						synctest.Wait()
						requireRotationHoldPending(t, done)
						cache.mu.Lock()
						cache.full[1] = false
						cache.mu.Unlock()
					}
					time.Sleep(time.Second)
					result := <-done
					require.NoError(t, result.err)
					require.False(t, result.escaped)
					require.NotNil(t, result.selection)
					require.EqualValues(t, 1, result.selection.Account.ID)
					require.True(t, result.selection.Acquired)
					result.selection.ReleaseFunc()
					cache.assertCounts(t, 1, 0, 0)
					cache.mu.Lock()
					calls := append([]rotationHoldAcquireCall(nil), cache.calls...)
					cache.mu.Unlock()
					for _, call := range calls {
						require.EqualValues(t, 1, call.accountID)
						require.True(t, call.sticky)
					}
					bound, err := svc.getStickySessionAccountID(ctx, req.GroupID, req.SessionHash)
					require.NoError(t, err)
					require.EqualValues(t, 1, bound)
				})
			})
		}
	}
}

func TestOpenAIStickyRotationWaitKeepsHardFilters(t *testing.T) {
	for _, path := range []string{"advanced", "legacy_load", "legacy_priority"} {
		for _, filter := range []string{"disabled", "unhealthy", "group", "model", "excluded"} {
			t.Run(path+"/"+filter, func(t *testing.T) {
				svc, scheduler, cache, req := newStickyRotationSoftEscapeFixture(t, "ttft")
				accounts := svc.accountRepo.(schedulerGroupAwareOpenAIAccountRepo).accounts
				switch filter {
				case "disabled":
					accounts[0].Schedulable = false
				case "unhealthy":
					accounts[0].Status = StatusError
				case "group":
					accounts[0].GroupIDs = []int64{99}
				case "model":
					accounts[0].Credentials = map[string]any{"model_mapping": map[string]any{"other-model": "other-model"}}
				case "excluded":
					req.ExcludedIDs = map[int64]struct{}{1: {}}
				}
				cache.setState(1, AccountRotationState{Phase: "resting", Hold: true, Reason: "scheduled"})
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				if path == "advanced" {
					selected, _, err := scheduler.selectBySessionHash(ctx, req)
					require.NoError(t, err)
					require.Nil(t, selected)
				} else {
					svc.cfg.Gateway.Scheduling.LoadBatchEnabled = path == "legacy_load"
					selected, err := svc.selectAccountWithLoadAwareness(ctx, req.GroupID, req.Platform, req.SessionHash, req.RequestedModel, req.ExcludedIDs, false, "", true)
					require.NoError(t, err)
					require.NotNil(t, selected)
					require.EqualValues(t, 2, selected.Account.ID)
					selected.ReleaseFunc()
				}
				cache.assertCounts(t, 1, 0, 0)
			})
		}
	}
}

func TestOpenAIStickyRotationWaitBypassesTicketPriority(t *testing.T) {
	for _, path := range []string{"advanced", "legacy_load", "legacy_priority"} {
		for _, recovering := range []bool{false, true} {
			name := "hold"
			if recovering {
				name = "recovering"
			}
			t.Run(path+"/"+name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					svc, scheduler, cache, req := newStickyRotationSoftEscapeFixture(t, "")
					accounts := make([]Account, 2)
					for i := range accounts {
						account := ticketTestAccount(int64(i + 1))
						account.GroupIDs, account.Concurrency = []int64{*req.GroupID}, 10
						account.Priority = 1
						if i == 0 {
							account.Priority = 100
						}
						attachReadyCodexTicket(account, "gpt-6-astra")
						accounts[i] = *account
					}
					svc.accountRepo = schedulerTestOpenAIAccountRepo{accounts: accounts}
					svc.cfg.Gateway.OpenAICodexTicket = config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, TTLSeconds: 3600, FailClosed: true, Models: []string{"gpt-6-astra"}}
					req.RequestedModel, req.HistoryModel = "gpt-6-astra", "gpt-6-astra"
					require.True(t, svc.openAICodexTicketShouldYieldSticky(t.Context(), &accounts[0], req.GroupID, req.Platform, req.RequestedModel, false, nil), "fixture must have a real soft priority escape")
					state := AccountRotationState{Phase: "resting", Hold: true, Reason: "scheduled"}
					if recovering {
						state = AccountRotationState{Phase: "working", Recovering: true, RetryAfterMS: 500}
					}
					cache.setState(1, state)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					done := make(chan struct {
						selection *AccountSelectionResult
						err       error
					}, 1)
					defer func() {
						cancel()
						synctest.Wait()
						select {
						case result := <-done:
							if result.selection != nil && result.selection.ReleaseFunc != nil {
								result.selection.ReleaseFunc()
							}
						default:
						}
					}()
					go func() {
						var selection *AccountSelectionResult
						var err error
						if path == "advanced" {
							selection, _, err = scheduler.Select(ctx, req)
						} else {
							svc.cfg.Gateway.Scheduling.LoadBatchEnabled = path == "legacy_load"
							selection, err = svc.selectAccountWithLoadAwareness(ctx, req.GroupID, req.Platform, req.SessionHash, req.RequestedModel, nil, false, "", true)
						}
						done <- struct {
							selection *AccountSelectionResult
							err       error
						}{selection, err}
					}()
					synctest.Wait()
					cache.assertCounts(t, 1, 0, 1)
					requireRotationHoldPending(t, done)
					cache.setState(1, AccountRotationState{Phase: "ready"})
					time.Sleep(time.Second)
					result := <-done
					require.NoError(t, result.err)
					require.NotNil(t, result.selection)
					require.EqualValues(t, 1, result.selection.Account.ID)
					require.True(t, result.selection.Acquired)
					result.selection.ReleaseFunc()
					bound, err := svc.getStickySessionAccountID(ctx, req.GroupID, req.SessionHash)
					require.NoError(t, err)
					require.EqualValues(t, 1, bound)
				})
			})
		}
	}
}

func TestOpenAIStickyRotationSoftEscapeStillAppliesWithoutWait(t *testing.T) {
	for _, strategy := range []string{"ttft", "error_rate", "latency", "rebalance", "full"} {
		t.Run(strategy, func(t *testing.T) {
			_, scheduler, _, req := newStickyRotationSoftEscapeFixture(t, strategy)
			selection, escaped, err := scheduler.selectBySessionHash(t.Context(), req)
			require.NoError(t, err)
			require.Nil(t, selection)
			require.True(t, escaped)
		})
	}
}

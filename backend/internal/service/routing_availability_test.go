//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type routingAvailabilityTestCache struct {
	ConcurrencyCache
	AccountRotationHoldCache // mutating methods deliberately panic if called
	loads                    map[int64]*AccountLoadInfo
	states                   map[int64]*AccountRotationState
	err                      error
}

func (c *routingAvailabilityTestCache) ReadRoutingAccountLoads(_ context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	if c.err != nil {
		return nil, c.err
	}
	out := make(map[int64]*AccountLoadInfo, len(accounts))
	for _, account := range accounts {
		load, exists := c.loads[account.ID]
		if !exists {
			load = &AccountLoadInfo{AccountID: account.ID}
		}
		out[account.ID] = load
	}
	return out, nil
}
func (c *routingAvailabilityTestCache) PeekAccountRotation(_ context.Context, id int64, _ AccountRotationPolicy) (*AccountRotationState, error) {
	if c.err != nil {
		return nil, c.err
	}
	if state := c.states[id]; state != nil {
		return state, nil
	}
	return &AccountRotationState{Phase: "ready"}, nil
}

type routingAvailabilityTestRepo struct {
	schedulerTestOpenAIAccountRepo
	err      error
	groupIDs []int64
}

func (r *routingAvailabilityTestRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, id int64, platform string) ([]Account, error) {
	r.groupIDs = append(r.groupIDs, id)
	if r.err != nil {
		return nil, r.err
	}
	return r.schedulerTestOpenAIAccountRepo.ListSchedulableByGroupIDAndPlatform(ctx, id, platform)
}

type routingAvailabilitySnapshotCache struct {
	*openAISnapshotCacheStub
	buckets map[SchedulerBucket][]*Account
	read    []SchedulerBucket
}

func (c *routingAvailabilitySnapshotCache) GetSnapshot(_ context.Context, bucket SchedulerBucket) ([]*Account, bool, error) {
	c.read = append(c.read, bucket)
	accounts, hit := c.buckets[bucket]
	return accounts, hit, nil
}

func routingAvailabilityFixture() (*OpenAIGatewayService, *APIKey, *routingAvailabilityTestRepo, *routingAvailabilityTestCache) {
	group := &Group{ID: 42, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true}
	key := &APIKey{ID: 7, UserID: 9, GroupID: &group.ID, Group: group}
	repo := &routingAvailabilityTestRepo{schedulerTestOpenAIAccountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{{
		ID: 1, GroupIDs: []int64{group.ID}, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 2,
		Credentials: map[string]any{"api_key": "must-never-leak", "model_mapping": map[string]any{"gpt-5.4": "gpt-5.4"}},
	}}}}
	cache := &routingAvailabilityTestCache{loads: make(map[int64]*AccountLoadInfo), states: make(map[int64]*AccountRotationState)}
	concurrency := NewConcurrencyService(cache)
	return &OpenAIGatewayService{accountRepo: repo, concurrencyService: concurrency, cfg: &config.Config{}}, key, repo, cache
}

func TestRoutingAvailabilityEligibilityAndCapacity(t *testing.T) {
	cases := []struct {
		name, state string
		change      func(*OpenAIGatewayService, *APIKey, *routingAvailabilityTestRepo, *routingAvailabilityTestCache, *RoutingAvailabilityRequest)
	}{
		{"apikey_ready", "ready", nil},
		{"oauth_ready", "ready", func(_ *OpenAIGatewayService, _ *APIKey, r *routingAvailabilityTestRepo, _ *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			r.accounts[0].Type = AccountTypeOAuth
		}},
		{"no_accounts", "empty", func(_ *OpenAIGatewayService, _ *APIKey, r *routingAvailabilityTestRepo, _ *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			r.accounts = nil
		}},
		{"full", "busy", func(_ *OpenAIGatewayService, _ *APIKey, _ *routingAvailabilityTestRepo, c *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			c.loads[1] = &AccountLoadInfo{AccountID: 1, CurrentConcurrency: 2}
		}},
		{"unlimited", "ready", func(_ *OpenAIGatewayService, _ *APIKey, r *routingAvailabilityTestRepo, c *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			r.accounts[0].Concurrency = 0
			c.loads[1] = &AccountLoadInfo{CurrentConcurrency: 99}
		}},
		{"disabled", "empty", func(_ *OpenAIGatewayService, _ *APIKey, r *routingAvailabilityTestRepo, _ *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			r.accounts[0].Schedulable = false
		}},
		{"cooldown", "empty", func(_ *OpenAIGatewayService, _ *APIKey, r *routingAvailabilityTestRepo, _ *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			until := time.Now().Add(time.Hour)
			r.accounts[0].RateLimitResetAt = &until
		}},
		{"model_unsupported", "empty", func(_ *OpenAIGatewayService, _ *APIKey, _ *routingAvailabilityTestRepo, _ *routingAvailabilityTestCache, q *RoutingAvailabilityRequest) {
			q.Model = "gpt-unavailable"
		}},
		{"group_isolation", "empty", func(_ *OpenAIGatewayService, _ *APIKey, r *routingAvailabilityTestRepo, _ *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			r.accounts[0].GroupIDs = []int64{99}
		}},
		{"privacy", "empty", func(_ *OpenAIGatewayService, k *APIKey, _ *routingAvailabilityTestRepo, _ *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			k.Group.RequirePrivacySet = true
		}},
		{"compact_unsupported", "empty", func(_ *OpenAIGatewayService, _ *APIKey, r *routingAvailabilityTestRepo, _ *routingAvailabilityTestCache, q *RoutingAvailabilityRequest) {
			r.accounts[0].Extra = map[string]any{"openai_compact_supported": false}
			q.RequireCompact = true
		}},
		{"websocket_disabled", "empty", func(_ *OpenAIGatewayService, _ *APIKey, _ *routingAvailabilityTestRepo, _ *routingAvailabilityTestCache, q *RoutingAvailabilityRequest) {
			q.Transport = "websocket"
		}},
		{"platform_unknown", "unknown", func(_ *OpenAIGatewayService, k *APIKey, _ *routingAvailabilityTestRepo, _ *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			k.Group.Platform = PlatformAnthropic
		}},
		{"query_error", "unknown", func(_ *OpenAIGatewayService, _ *APIKey, r *routingAvailabilityTestRepo, _ *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			r.err = errors.New("private database error")
		}},
		{"load_error", "unknown", func(_ *OpenAIGatewayService, _ *APIKey, _ *routingAvailabilityTestRepo, c *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			c.err = errors.New("private redis error")
		}},
		{"load_missing", "unknown", func(_ *OpenAIGatewayService, _ *APIKey, _ *routingAvailabilityTestRepo, c *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			c.loads[1] = nil
		}},
		{"resting", "busy", func(s *OpenAIGatewayService, _ *APIKey, _ *routingAvailabilityTestRepo, c *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			s.concurrencyService.SetAccountRotation(openAIHandlerFreeRotationSettings{}, nil)
			c.states[1] = &AccountRotationState{Phase: "resting"}
		}},
		{"manual_hold", "busy", func(s *OpenAIGatewayService, _ *APIKey, _ *routingAvailabilityTestRepo, c *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			s.concurrencyService.SetAccountRotation(openAIHandlerFreeRotationSettings{}, nil)
			c.states[1] = &AccountRotationState{Phase: "resting", Hold: true}
		}},
		{"recovery_pacing", "busy", func(s *OpenAIGatewayService, _ *APIKey, _ *routingAvailabilityTestRepo, c *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			s.concurrencyService.SetAccountRotation(openAIHandlerFreeRotationSettings{}, nil)
			c.states[1] = &AccountRotationState{Phase: "working", Recovering: true, RetryAfterMS: 500}
		}},
		{"rest_start_needs_persistence", "unknown", func(s *OpenAIGatewayService, _ *APIKey, _ *routingAvailabilityTestRepo, c *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			s.concurrencyService.SetAccountRotation(openAIHandlerFreeRotationSettings{}, nil)
			c.err = errors.New("ROUTING_ROTATION_TRANSITION_REQUIRED")
		}},
		{"rpm_authority_error", "unknown", func(_ *OpenAIGatewayService, _ *APIKey, r *routingAvailabilityTestRepo, _ *routingAvailabilityTestCache, _ *RoutingAvailabilityRequest) {
			r.accounts[0].Type = AccountTypeOAuth
			r.accounts[0].Extra = map[string]any{"base_rpm": 10}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, key, repo, cache := routingAvailabilityFixture()
			request := RoutingAvailabilityRequest{Model: "gpt-5.4"}
			if tc.change != nil {
				tc.change(svc, key, repo, cache, &request)
			}
			before := svc.SnapshotOpenAIAccountSchedulerMetrics()
			got := svc.RoutingAvailability(t.Context(), key, request)
			assert.Equal(t, tc.state, got.State, got)
			assert.Equal(t, "routing.availability", got.Object)
			assert.Equal(t, request.Model, got.Model)
			assert.NotEmpty(t, got.Reason)
			assert.Greater(t, got.CheckedAt, int64(0))
			assert.Equal(t, before, svc.SnapshotOpenAIAccountSchedulerMetrics(), "a probe is not a scheduler selection")
		})
	}
}

type openAIHandlerFreeRotationSettings struct{}

func (openAIHandlerFreeRotationSettings) GetAccountRotationSettings(context.Context) (*AccountRotationSettings, error) {
	return DefaultAccountRotationSettings(), nil
}

func TestRoutingAvailabilityRotationReasonAndReadOnly(t *testing.T) {
	cases := []struct {
		name  string
		state AccountRotationState
	}{
		{"manual_draining", AccountRotationState{Phase: "draining", Reason: "manual", Hold: true}},
		{"manual_resting", AccountRotationState{Phase: "resting", Reason: "manual", Hold: true}},
		{"scheduled_hold_draining", AccountRotationState{Phase: "draining", Reason: "scheduled", Hold: true}},
		{"scheduled_hold_resting", AccountRotationState{Phase: "resting", Reason: "scheduled", Hold: true}},
		{"early_draining", AccountRotationState{Phase: "draining", Reason: "early", Hold: true}},
		{"early_resting", AccountRotationState{Phase: "resting", Reason: "early", Hold: true}},
		{"recovering", AccountRotationState{Phase: "working", Recovering: true, RetryAfterMS: 500}},
		{"ordinary_draining", AccountRotationState{Phase: "draining", Reason: "scheduled"}},
		{"ordinary_resting", AccountRotationState{Phase: "resting", Reason: "scheduled"}},
	}
	for _, mode := range []string{"aggressive", "sticky"} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				svc, key, repo, cache := routingAvailabilityFixture()
				settings := *DefaultAccountRotationSettings()
				settings.Enabled, settings.Mode = true, mode
				svc.concurrencyService.SetAccountRotation(openAIRotationSettingsStub{settings: settings}, nil)
				state := tc.state
				state.StartedAt, state.DrainAt, state.RestUntil = 100, 200, 300
				state.WaitingCount = 3
				original := state
				cache.states[1] = &state
				before := svc.SnapshotOpenAIAccountSchedulerMetrics()
				// Repeat observations against the same authority: peeking must not
				// start/finish a rest, consume recovery pacing, or reserve a slot.
				for range 2 {
					got := svc.RoutingAvailability(t.Context(), key, RoutingAvailabilityRequest{Model: "gpt-5.4"})
					assert.Equal(t, "busy", got.State, got)
					assert.Equal(t, "account_rotation", got.Reason)
					assert.Equal(t, 1, got.RetryAfterSeconds)
				}
				assert.Equal(t, original, *cache.states[1])
				assert.Equal(t, before, svc.SnapshotOpenAIAccountSchedulerMetrics())
				assert.Empty(t, cache.loads)
				assert.Nil(t, repo.accounts[0].LastUsedAt)
				assert.Nil(t, svc.openaiScheduler)
			})
		}
	}
}

func TestRoutingAvailabilityRotationRequiresEligibility(t *testing.T) {
	cases := []struct {
		name   string
		change func(*OpenAIGatewayService, *APIKey, *Account, *RoutingAvailabilityRequest)
	}{
		{"disabled", func(_ *OpenAIGatewayService, _ *APIKey, a *Account, _ *RoutingAvailabilityRequest) {
			a.Schedulable = false
		}},
		{"model_unsupported", func(_ *OpenAIGatewayService, _ *APIKey, a *Account, _ *RoutingAvailabilityRequest) {
			a.Credentials["model_mapping"] = map[string]any{"gpt-other": "gpt-other"}
		}},
		{"other_group", func(_ *OpenAIGatewayService, _ *APIKey, a *Account, _ *RoutingAvailabilityRequest) {
			a.GroupIDs = []int64{99}
		}},
		{"group_model_restricted", func(_ *OpenAIGatewayService, k *APIKey, a *Account, _ *RoutingAvailabilityRequest) {
			a.AccountGroups = []AccountGroup{{GroupID: *k.GroupID, AllowedModels: []string{"gpt-other"}}}
		}},
		{"privacy", func(_ *OpenAIGatewayService, k *APIKey, _ *Account, _ *RoutingAvailabilityRequest) {
			k.Group.RequirePrivacySet = true
		}},
		{"compact_unsupported", func(_ *OpenAIGatewayService, _ *APIKey, a *Account, q *RoutingAvailabilityRequest) {
			a.Extra = map[string]any{"openai_compact_supported": false}
			q.RequireCompact = true
		}},
		{"websocket_disabled", func(_ *OpenAIGatewayService, _ *APIKey, _ *Account, q *RoutingAvailabilityRequest) {
			q.Transport = "websocket"
		}},
		{"rate_limited", func(_ *OpenAIGatewayService, _ *APIKey, a *Account, _ *RoutingAvailabilityRequest) {
			until := time.Now().Add(time.Hour)
			a.RateLimitResetAt = &until
		}},
		{"model_runtime_blocked", func(s *OpenAIGatewayService, _ *APIKey, a *Account, q *RoutingAvailabilityRequest) {
			modelKey, _ := openAIAccountModelTransientKey(a.ID, q.Model)
			s.getOpenAIAccountModelTransientState().entries[modelKey] = openAIAccountModelTransientEntry{blockUntil: time.Now().Add(time.Hour)}
		}},
		{"rpm_exhausted", func(s *OpenAIGatewayService, _ *APIKey, a *Account, _ *RoutingAvailabilityRequest) {
			a.Type, a.Extra = AccountTypeOAuth, map[string]any{"base_rpm": 10}
			s.rpmCache = &groupCapacityRPMCacheStub{counts: map[int64]int{a.ID: 10}}
		}},
		{"fast_policy_blocked", func(s *OpenAIGatewayService, _ *APIKey, _ *Account, q *RoutingAvailabilityRequest) {
			s.settingService = newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{
				ServiceTier: OpenAIFastTierPriority, Action: BetaPolicyActionBlock, Scope: BetaPolicyScopeAll,
			}}}).settingService
			q.ServiceTier = OpenAIFastTierPriority
		}},
	}
	for _, rotation := range []AccountRotationState{
		{Phase: "resting", Reason: "manual", Hold: true},
		{Phase: "resting", Reason: "scheduled"},
		{Phase: "working", Recovering: true},
	} {
		for _, tc := range cases {
			t.Run(rotation.Phase+"/"+rotation.Reason+"/"+tc.name, func(t *testing.T) {
				svc, key, repo, cache := routingAvailabilityFixture()
				svc.concurrencyService.SetAccountRotation(openAIHandlerFreeRotationSettings{}, nil)
				state := rotation
				cache.states[1] = &state
				request := RoutingAvailabilityRequest{Model: "gpt-5.4"}
				tc.change(svc, key, &repo.accounts[0], &request)
				got := svc.RoutingAvailability(t.Context(), key, request)
				assert.Equal(t, "empty", got.State, got)
				assert.Equal(t, "no_eligible_accounts", got.Reason)
			})
		}
	}
}

func TestRoutingAvailabilityRotationMixedPool(t *testing.T) {
	cases := []struct {
		name                  string
		rotation              bool
		full                  bool
		restrictRotation      bool
		wantState, wantReason string
	}{
		{"ready_beats_rotation", true, false, false, "ready", "capacity_available"},
		{"rotation_beats_capacity_busy", true, true, false, "busy", "account_rotation"},
		{"capacity_busy_without_rotation", false, true, false, "busy", "capacity_busy"},
		{"ineligible_rotation_with_ready", true, false, true, "ready", "capacity_available"},
		{"ineligible_rotation_with_capacity_busy", true, true, true, "busy", "capacity_busy"},
	}
	for _, reverse := range []bool{false, true} {
		for _, tc := range cases {
			order := "rotation_first/"
			if reverse {
				order = "rotation_last/"
			}
			t.Run(order+tc.name, func(t *testing.T) {
				svc, key, repo, cache := routingAvailabilityFixture()
				svc.concurrencyService.SetAccountRotation(openAIHandlerFreeRotationSettings{}, nil)
				other := repo.accounts[0]
				other.ID = 2
				repo.accounts = append(repo.accounts, other)
				if tc.rotation {
					cache.states[1] = &AccountRotationState{Phase: "resting", Reason: "scheduled"}
				}
				if tc.restrictRotation {
					repo.accounts[0].AccountGroups = []AccountGroup{{GroupID: *key.GroupID, AllowedModels: []string{"gpt-other"}}}
				}
				if tc.full {
					cache.loads[1] = &AccountLoadInfo{CurrentConcurrency: 2}
					cache.loads[2] = &AccountLoadInfo{CurrentConcurrency: 2}
				}
				if reverse {
					repo.accounts[0], repo.accounts[1] = repo.accounts[1], repo.accounts[0]
				}
				got := svc.RoutingAvailability(t.Context(), key, RoutingAvailabilityRequest{Model: "gpt-5.4"})
				assert.Equal(t, tc.wantState, got.State, got)
				assert.Equal(t, tc.wantReason, got.Reason)
			})
		}
	}
}

func TestRoutingAvailabilityRecoversWithoutAdmission(t *testing.T) {
	svc, key, repo, cache := routingAvailabilityFixture()
	q := RoutingAvailabilityRequest{Model: "gpt-5.4"}
	cache.loads[1] = &AccountLoadInfo{CurrentConcurrency: 2}
	require.Equal(t, "busy", svc.RoutingAvailability(t.Context(), key, q).State)
	cache.loads[1].CurrentConcurrency = 1
	require.Equal(t, "ready", svc.RoutingAvailability(t.Context(), key, q).State)
	repo.accounts[0].Schedulable = false
	require.Equal(t, "empty", svc.RoutingAvailability(t.Context(), key, q).State)
	repo.accounts[0].Schedulable = true
	require.Equal(t, "ready", svc.RoutingAvailability(t.Context(), key, q).State)
	assert.Nil(t, repo.accounts[0].LastUsedAt)
	assert.Nil(t, repo.accounts[0].TempUnschedulableUntil)
	assert.Nil(t, svc.openaiScheduler)
}

func TestRoutingAvailabilityKeepsEmptyPrimaryGroupSeparateFromReadyBackupGroup(t *testing.T) {
	for _, source := range []string{"repo", "snapshot"} {
		t.Run(source, func(t *testing.T) {
			svc, primaryKey, repo, _ := routingAvailabilityFixture()
			svc.cfg.RunMode = config.RunModeStandard
			primaryGroup := &Group{ID: 2, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true}
			backupGroup := &Group{ID: 5, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true}
			primaryKey.GroupID, primaryKey.Group = &primaryGroup.ID, primaryGroup
			backupKey := &APIKey{ID: 8, UserID: primaryKey.UserID, GroupID: &backupGroup.ID, Group: backupGroup}
			backupAccount := repo.accounts[0]
			backupAccount.ID, backupAccount.GroupIDs = 2, []int64{backupGroup.ID}
			repo.accounts[0].GroupIDs, repo.accounts[0].Schedulable = []int64{primaryGroup.ID}, false
			repo.accounts = append(repo.accounts, backupAccount)
			primaryBucket := SchedulerBucket{GroupID: primaryGroup.ID, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}
			backupBucket := SchedulerBucket{GroupID: backupGroup.ID, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}
			var snapshot *routingAvailabilitySnapshotCache
			if source == "snapshot" {
				snapshot = &routingAvailabilitySnapshotCache{
					openAISnapshotCacheStub: &openAISnapshotCacheStub{accountsByID: map[int64]*Account{
						1: &repo.accounts[0], 2: &repo.accounts[1],
					}},
					buckets: map[SchedulerBucket][]*Account{
						primaryBucket: {&repo.accounts[0]},
						backupBucket:  {&repo.accounts[1]},
					},
				}
				svc.schedulerSnapshot = &SchedulerSnapshotService{cache: snapshot, cfg: svc.cfg}
				repo.err = errors.New("snapshot availability must not query the repository")
			}
			request := RoutingAvailabilityRequest{Model: "gpt-5.4"}
			primary := svc.RoutingAvailability(t.Context(), primaryKey, request)
			backup := svc.RoutingAvailability(t.Context(), backupKey, request)
			again := svc.RoutingAvailability(t.Context(), primaryKey, request)
			assert.Equal(t, "empty", primary.State, primary)
			assert.Equal(t, "ready", backup.State, backup)
			assert.Equal(t, "empty", again.State, "another group's ready result must not change primary availability")
			if source == "snapshot" {
				assert.Equal(t, []SchedulerBucket{primaryBucket, backupBucket, primaryBucket}, snapshot.read)
				assert.Empty(t, repo.groupIDs)
			} else {
				assert.Equal(t, []int64{primaryGroup.ID, backupGroup.ID, primaryGroup.ID}, repo.groupIDs)
			}
		})
	}
}

func TestRoutingAvailabilityChannelAndAccountModelMapping(t *testing.T) {
	svc, key, repo, _ := routingAvailabilityFixture()
	channel := Channel{ID: 1, Status: StatusActive, GroupIDs: []int64{42}, ModelMapping: map[string]map[string]string{PlatformOpenAI: {"public-model": "account-alias"}}}
	svc.channelService = newTestChannelService(makeStandardRepo(channel, map[int64]string{42: PlatformOpenAI}))
	repo.accounts[0].Credentials["model_mapping"] = map[string]any{"account-alias": "gpt-5.4"}
	result := svc.RoutingAvailability(t.Context(), key, RoutingAvailabilityRequest{Model: "public-model", ServiceTier: "priority"})
	assert.Equal(t, "ready", result.State)
	assert.Equal(t, "public-model", result.Model)
	other := int64(43)
	key.GroupID = &other
	key.Group = &Group{ID: other, Platform: PlatformOpenAI}
	assert.Equal(t, "empty", svc.RoutingAvailability(t.Context(), key, RoutingAvailabilityRequest{Model: "public-model"}).State)
}

func TestRoutingAvailabilityDoesNotTouchRuntimeCooldowns(t *testing.T) {
	svc, key, _, _ := routingAvailabilityFixture()
	until := time.Now().Add(-time.Minute)
	svc.openaiAccountRuntimeBlockUntil.Store(int64(1), until)
	svc.excelBPSCooldownUntil.Store(int64(1), until)
	modelKey, ok := openAIAccountModelTransientKey(1, "gpt-5.4")
	require.True(t, ok)
	original := openAIAccountModelTransientEntry{failureStreak: 1, lastFailure: time.Now().Add(-time.Minute), lastTouched: until, blockUntil: until}
	state := svc.getOpenAIAccountModelTransientState()
	state.entries[modelKey] = original
	proxyID := int64(8)
	// Account proxy quarantine checks may observe an expired entry, but must
	// preserve it for the ordinary scheduler's own housekeeping.
	circuit := svc.getOpenAIProxyStreamCircuit()
	circuit.entries[proxyID] = openAIProxyStreamCircuitEntry{blockedUntil: until}
	request := RoutingAvailabilityRequest{Model: "gpt-5.4"}
	require.Equal(t, "ready", svc.RoutingAvailability(t.Context(), key, request).State)
	value, found := svc.openaiAccountRuntimeBlockUntil.Load(int64(1))
	assert.True(t, found)
	assert.Equal(t, until, value)
	_, found = svc.excelBPSCooldownUntil.Load(int64(1))
	assert.True(t, found)
	assert.Equal(t, original, state.entries[modelKey])
	assert.Zero(t, svc.openaiAccountRuntimeBlockSequence.Load())
}

func TestRoutingAvailabilityMultiGroupIsUnknown(t *testing.T) {
	svc, key, repo, _ := routingAvailabilityFixture()
	key.GroupIDs = []int64{42, 43}
	repo.err = errors.New("must not query one selected group's pool")
	got := svc.RoutingAvailability(t.Context(), key, RoutingAvailabilityRequest{Model: "gpt-5.4"})
	assert.Equal(t, "unknown", got.State)
	assert.Equal(t, "multi_group_evaluation_not_supported", got.Reason)
}

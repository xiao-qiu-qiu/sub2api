//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type stickyMigrationCacheCall struct {
	groupID          int64
	primary, legacy  string
	expected, owner  int64
	expectedRevision string
	ttl, legacyTTL   time.Duration
	contextErr       error
	hasDeadline      bool
}

type stickyMigrationTestCache struct {
	GatewayCache
	mu                    sync.Mutex
	bindings              map[string]int64
	revisions             map[string]string
	revisionSequence      int
	calls                 []stickyMigrationCacheCall
	sets, deletes         int
	readErr, migrationErr error
}

func stickyMigrationTestKey(groupID int64, hash string) string {
	return fmt.Sprintf("%d:%s", groupID, hash)
}

func (c *stickyMigrationTestCache) GetSessionAccountID(_ context.Context, groupID int64, hash string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readErr != nil {
		return 0, c.readErr
	}
	owner, ok := c.bindings[stickyMigrationTestKey(groupID, hash)]
	if !ok {
		return 0, ErrStickySessionNotFound
	}
	return owner, nil
}

func (c *stickyMigrationTestCache) SetSessionAccountID(_ context.Context, groupID int64, hash string, owner int64, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sets++
	key := stickyMigrationTestKey(groupID, hash)
	c.bindings[key] = owner
	c.revisionSequence++
	if c.revisions == nil {
		c.revisions = make(map[string]string)
	}
	c.revisions[key] = fmt.Sprintf("revision-%d", c.revisionSequence)
	return nil
}

func (c *stickyMigrationTestCache) DeleteSessionAccountID(_ context.Context, groupID int64, hash string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes++
	delete(c.bindings, stickyMigrationTestKey(groupID, hash))
	delete(c.revisions, stickyMigrationTestKey(groupID, hash))
	return nil
}

func (c *stickyMigrationTestCache) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}

func (c *stickyMigrationTestCache) ReadSessionAccountBinding(_ context.Context, groupID int64, primary, legacy string) (int64, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readErr != nil {
		return 0, "", c.readErr
	}
	key := stickyMigrationTestKey(groupID, primary)
	owner, exists := c.bindings[key]
	if !exists && legacy != "" {
		key = stickyMigrationTestKey(groupID, legacy)
		owner, exists = c.bindings[key]
	}
	if !exists {
		return 0, "", ErrStickySessionNotFound
	}
	return owner, c.revisions[key], nil
}

func (c *stickyMigrationTestCache) CompareAndSwapSessionAccountID(ctx context.Context, groupID int64, primary, legacy string, expected int64, expectedRevision string, owner int64, ttl, legacyTTL time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, hasDeadline := ctx.Deadline()
	c.calls = append(c.calls, stickyMigrationCacheCall{groupID: groupID, primary: primary, legacy: legacy, expected: expected, owner: owner, expectedRevision: expectedRevision, ttl: ttl, legacyTTL: legacyTTL, contextErr: ctx.Err(), hasDeadline: hasDeadline})
	if c.migrationErr != nil {
		return false, c.migrationErr
	}
	primaryKey := stickyMigrationTestKey(groupID, primary)
	currentKey := primaryKey
	current, exists := c.bindings[primaryKey]
	if !exists && legacy != "" {
		currentKey = stickyMigrationTestKey(groupID, legacy)
		current = c.bindings[currentKey]
	}
	if current <= 0 || current != expected || c.revisions[currentKey] != expectedRevision {
		return false, nil
	}
	c.revisionSequence++
	if c.revisions == nil {
		c.revisions = make(map[string]string)
	}
	revision := fmt.Sprintf("revision-%d", c.revisionSequence)
	c.bindings[primaryKey] = owner
	c.revisions[primaryKey] = revision
	if legacy != "" {
		legacyKey := stickyMigrationTestKey(groupID, legacy)
		c.bindings[legacyKey] = owner
		c.revisions[legacyKey] = revision
	}
	return true, nil
}

func newStickyMigrationFixture(t *testing.T) (*OpenAIGatewayService, *defaultOpenAIAccountScheduler, *stickyMigrationTestCache, OpenAIAccountScheduleRequest) {
	t.Helper()
	svc, scheduler, _, req := newStickyRotationSoftEscapeFixture(t, "error_rate")
	cfg := svc.settingService.prioritySchedulingConfig.config
	cfg.Enabled = true
	cfg.StickyMigrationOnSuccess = true
	svc.settingService.prioritySchedulingConfig.config = cfg
	cache := &stickyMigrationTestCache{GatewayCache: &schedulerTestGatewayCache{}, bindings: map[string]int64{
		stickyMigrationTestKey(*req.GroupID, "openai:"+req.SessionHash): 1,
	}}
	svc.cache = cache
	return svc, scheduler, cache, req
}

func stickyMigrationTestContext(t *testing.T, svc *OpenAIGatewayService, ctx context.Context, req OpenAIAccountScheduleRequest) context.Context {
	t.Helper()
	req.stickyMigration = svc.prepareOpenAIStickyMigration(ctx, req)
	require.NotNil(t, req.stickyMigration)
	return ContextWithSelectionPriorityRequest(ctx, &AccountSelectionResult{priorityRequest: &req})
}

func requireStickyMigrationOwner(t *testing.T, cache *stickyMigrationTestCache, req OpenAIAccountScheduleRequest, owner int64) {
	t.Helper()
	got, err := cache.GetSessionAccountID(context.Background(), *req.GroupID, "openai:"+req.SessionHash)
	require.NoError(t, err)
	require.Equal(t, owner, got)
}

func TestOpenAIStickyMigrationDisabledPreservesAdmissionBinding(t *testing.T) {
	svc, scheduler, cache, req := newStickyMigrationFixture(t)
	svc.settingService.prioritySchedulingConfig.config.StickyMigrationOnSuccess = false
	selected, _, err := scheduler.Select(t.Context(), req)
	require.NoError(t, err)
	require.NotNil(t, selected)
	t.Cleanup(selected.ReleaseFunc)
	require.EqualValues(t, 2, selected.Account.ID)
	ctx := ContextWithSelectionPriorityRequest(t.Context(), selected)
	require.Nil(t, stickyMigrationFromContext(ctx))
	requireStickyMigrationOwner(t, cache, req, 1)
	require.NoError(t, svc.BindStickySessionAfterProfitAdmission(ctx, req.GroupID, req.SessionHash, selected.Account.ID))
	requireStickyMigrationOwner(t, cache, req, 2)
	require.Positive(t, cache.sets)
	require.False(t, svc.CompleteOpenAIStickyMigration(ctx, selected.Account, &OpenAIForwardResult{}, nil))
	require.Empty(t, cache.calls)
}

func TestOpenAIStickyMigrationScopesAndStrongAffinity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*OpenAIGatewayService, *OpenAIAccountScheduleRequest, *context.Context)
		want   bool
	}{
		{"soft-text", func(*OpenAIGatewayService, *OpenAIAccountScheduleRequest, *context.Context) {}, true},
		{"priority-disabled", func(s *OpenAIGatewayService, _ *OpenAIAccountScheduleRequest, _ *context.Context) {
			s.settingService.prioritySchedulingConfig.config.Enabled = false
		}, false},
		{"outside-group", func(s *OpenAIGatewayService, _ *OpenAIAccountScheduleRequest, _ *context.Context) {
			s.settingService.prioritySchedulingConfig.config.GroupIDs = []int64{99}
		}, false},
		{"outside-model", func(s *OpenAIGatewayService, _ *OpenAIAccountScheduleRequest, _ *context.Context) {
			s.settingService.prioritySchedulingConfig.config.Models = []string{"other-model"}
		}, false},
		{"public-model-scope", func(s *OpenAIGatewayService, r *OpenAIAccountScheduleRequest, _ *context.Context) {
			r.HistoryModel = "public-model"
			s.settingService.prioritySchedulingConfig.config.Models = []string{"public-model"}
		}, true},
		{"routing-model-alias", func(s *OpenAIGatewayService, r *OpenAIAccountScheduleRequest, _ *context.Context) {
			r.HistoryModel = "public-model"
			s.settingService.prioritySchedulingConfig.config.Models = []string{r.RequestedModel}
		}, true},
		{"other-platform", func(_ *OpenAIGatewayService, r *OpenAIAccountScheduleRequest, _ *context.Context) {
			r.Platform = PlatformAnthropic
		}, false},
		{"non-billable", func(_ *OpenAIGatewayService, r *OpenAIAccountScheduleRequest, _ *context.Context) {
			r.UseUpstreamTokenCost = false
		}, false},
		{"image", func(_ *OpenAIGatewayService, r *OpenAIAccountScheduleRequest, _ *context.Context) {
			r.RequiredImageCapability = OpenAIImagesCapability("generation")
		}, false},
		{"no-session", func(_ *OpenAIGatewayService, r *OpenAIAccountScheduleRequest, _ *context.Context) {
			r.SessionHash = " "
		}, false},
		{"previous-response", func(_ *OpenAIGatewayService, r *OpenAIAccountScheduleRequest, _ *context.Context) {
			r.PreviousResponseID = "resp_fixture"
		}, false},
		{"movable-previous-response", func(_ *OpenAIGatewayService, r *OpenAIAccountScheduleRequest, _ *context.Context) {
			r.PreviousResponseID = "resp_fixture"
			r.PreviousResponseCanMove = true
		}, false},
		{"guardian-parent", func(_ *OpenAIGatewayService, r *OpenAIAccountScheduleRequest, _ *context.Context) {
			r.GuardianParentAccountID = 1
		}, false},
		{"preserve-binding", func(_ *OpenAIGatewayService, r *OpenAIAccountScheduleRequest, _ *context.Context) {
			r.PreserveStickyBinding = true
		}, false},
		{"disable-escape", func(_ *OpenAIGatewayService, r *OpenAIAccountScheduleRequest, _ *context.Context) {
			r.DisableStickyEscape = true
		}, false},
		{"encrypted", func(_ *OpenAIGatewayService, r *OpenAIAccountScheduleRequest, _ *context.Context) {
			r.EncryptedReasoning = true
		}, false},
		{"availability-evaluation", func(_ *OpenAIGatewayService, r *OpenAIAccountScheduleRequest, _ *context.Context) {
			r.routingAvailability = &routingAvailabilityEvaluation{}
		}, false},
		{"manual-test", func(_ *OpenAIGatewayService, _ *OpenAIAccountScheduleRequest, c *context.Context) {
			*c = WithManualAccountTest(*c)
		}, false},
		{"unsupported-cache", func(s *OpenAIGatewayService, _ *OpenAIAccountScheduleRequest, _ *context.Context) {
			s.cache = &schedulerTestGatewayCache{}
		}, false},
		{"missing-cache", func(s *OpenAIGatewayService, _ *OpenAIAccountScheduleRequest, _ *context.Context) { s.cache = nil }, false},
		{"no-existing-owner", func(_ *OpenAIGatewayService, r *OpenAIAccountScheduleRequest, _ *context.Context) {
			r.StickyAccountID = 0
			r.SessionHash = "unbound"
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc, _, _, req := newStickyMigrationFixture(t)
			ctx := context.Background()
			test.mutate(svc, &req, &ctx)
			migration := svc.prepareOpenAIStickyMigration(ctx, req)
			require.Equal(t, test.want, migration != nil)
		})
	}
}

func TestOpenAIStickyMigrationDefersAdmissionWritesUntilSuccessfulCompletion(t *testing.T) {
	svc, scheduler, cache, req := newStickyMigrationFixture(t)
	req.StickyWeighted = true
	selected, _, err := scheduler.Select(t.Context(), req)
	require.NoError(t, err)
	require.NotNil(t, selected)
	require.EqualValues(t, 2, selected.Account.ID)
	t.Cleanup(selected.ReleaseFunc)
	ctx := ContextWithSelectionPriorityRequest(t.Context(), selected)
	require.NotNil(t, stickyMigrationFromContext(ctx), "the scheduler must carry the private migration token through its owned request")
	requireStickyMigrationOwner(t, cache, req, 1)
	require.NoError(t, svc.BindStickySession(ctx, req.GroupID, req.SessionHash, 2))
	require.NoError(t, svc.bindOpenAIStickySessionDuringSelection(ctx, req.GroupID, req.SessionHash, 2))
	require.NoError(t, svc.BindStickySessionAfterProfitAdmission(ctx, req.GroupID, req.SessionHash, 2))
	require.NoError(t, svc.setStickySessionAccountID(ctx, req.GroupID, req.SessionHash, 2, time.Hour))
	require.NoError(t, svc.deleteStickySessionAccountID(ctx, req.GroupID, req.SessionHash))
	requireStickyMigrationOwner(t, cache, req, 1)
	require.Zero(t, cache.sets)
	require.Zero(t, cache.deletes)

	require.True(t, svc.CompleteOpenAIStickyMigration(ctx, selected.Account, &OpenAIForwardResult{Stream: true, UpstreamTerminalEvent: "response.completed"}, nil))
	requireStickyMigrationOwner(t, cache, req, 2)
	require.Len(t, cache.calls, 1)
	require.NoError(t, svc.setStickySessionAccountID(ctx, req.GroupID, req.SessionHash, 1, time.Hour))
	require.NoError(t, svc.deleteStickySessionAccountID(ctx, req.GroupID, req.SessionHash))
	requireStickyMigrationOwner(t, cache, req, 2)
	require.Zero(t, cache.sets, "late admission/WS writes cannot bypass the completed migration")
	require.Zero(t, cache.deletes)

	// A fresh turn resolves the new owner. Weighted affinity must not redraw a
	// healthy session after its successful move.
	next := req
	next.StickyAccountID = 0
	next.StickyWeighted = true
	selectedNext, decision, err := scheduler.Select(t.Context(), next)
	require.NoError(t, err)
	require.NotNil(t, selectedNext)
	t.Cleanup(selectedNext.ReleaseFunc)
	require.EqualValues(t, 2, selectedNext.Account.ID)
	require.True(t, decision.StickySessionHit)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
}

func TestOpenAIStickyMigrationRejectsFailedOrUnfinishedResults(t *testing.T) {
	for _, test := range []struct {
		name    string
		result  *OpenAIForwardResult
		err     error
		cancel  bool
		account *Account
	}{
		{"forward-error", &OpenAIForwardResult{UpstreamTerminalEvent: "response.completed"}, errors.New("upstream failed"), false, &Account{ID: 2}},
		{"stream-failed", &OpenAIForwardResult{Stream: true, UpstreamTerminalEvent: "response.failed"}, nil, false, &Account{ID: 2}},
		{"stream-incomplete-event", &OpenAIForwardResult{Stream: true, UpstreamTerminalEvent: "response.incomplete"}, nil, false, &Account{ID: 2}},
		{"missing-stream-terminal", &OpenAIForwardResult{Stream: true}, nil, false, &Account{ID: 2}},
		{"missing-ws-terminal", &OpenAIForwardResult{OpenAIWSMode: true}, nil, false, &Account{ID: 2}},
		{"stream-read-incomplete", &OpenAIForwardResult{Stream: true, UpstreamTerminalEvent: "response.completed", streamReadIncomplete: true}, nil, false, &Account{ID: 2}},
		{"invalid-terminal", &OpenAIForwardResult{UpstreamTerminalEvent: "response.completed", stickyMigrationInvalidTerminal: true}, nil, false, &Account{ID: 2}},
		{"client-disconnected", &OpenAIForwardResult{UpstreamTerminalEvent: "response.completed", ClientDisconnect: true}, nil, false, &Account{ID: 2}},
		{"quality-interrupted", &OpenAIForwardResult{UpstreamTerminalEvent: "response.completed", QualityInterrupted: true}, nil, false, &Account{ID: 2}},
		{"cancelled-context", &OpenAIForwardResult{UpstreamTerminalEvent: "response.completed"}, nil, true, &Account{ID: 2}},
		{"nil-result", nil, nil, false, &Account{ID: 2}},
		{"nil-account", &OpenAIForwardResult{}, nil, false, nil},
		{"invalid-account", &OpenAIForwardResult{}, nil, false, &Account{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc, _, cache, req := newStickyMigrationFixture(t)
			ctx := stickyMigrationTestContext(t, svc, context.Background(), req)
			if test.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			require.False(t, svc.CompleteOpenAIStickyMigration(ctx, test.account, test.result, test.err))
			requireStickyMigrationOwner(t, cache, req, 1)
			require.Empty(t, cache.calls)
			require.Zero(t, cache.sets)
			require.False(t, stickyMigrationFromContext(ctx).completed.Load(), "a failed attempt must leave the original token available for failover")
		})
	}
	var svc *OpenAIGatewayService
	require.False(t, svc.CompleteOpenAIStickyMigration(nil, nil, nil, nil))
}

func TestOpenAIStickyMigrationSuccessfulTerminalForms(t *testing.T) {
	for _, result := range []OpenAIForwardResult{
		{},
		{Stream: true, UpstreamTerminalEvent: "response.completed"},
		{OpenAIWSMode: true, UpstreamTerminalEvent: "response.done"},
		{Stream: true, UpstreamTerminalEvent: "[DONE]"},
	} {
		t.Run(fmt.Sprintf("%s/stream=%t/ws=%t", result.UpstreamTerminalEvent, result.Stream, result.OpenAIWSMode), func(t *testing.T) {
			svc, _, cache, req := newStickyMigrationFixture(t)
			ctx := stickyMigrationTestContext(t, svc, context.Background(), req)
			require.True(t, svc.CompleteOpenAIStickyMigration(ctx, &Account{ID: 2}, &result, nil))
			requireStickyMigrationOwner(t, cache, req, 2)
			require.Len(t, cache.calls, 1)
			require.True(t, cache.calls[0].hasDeadline)
			require.NoError(t, cache.calls[0].contextErr)
		})
	}
}

func TestOpenAIStickyMigrationFailoverRetainsOriginalOwner(t *testing.T) {
	svc, scheduler, cache, req := newStickyMigrationFixture(t)
	stats := newOpenAIAccountRuntimeStats()
	svc.openaiAccountStats = stats
	scheduler.stats = stats
	first, _, err := scheduler.Select(t.Context(), req)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.EqualValues(t, 1, first.Account.ID)
	first.ReleaseFunc()
	ctx := ContextWithSelectionPriorityRequest(t.Context(), first)
	origin := stickyMigrationFromContext(ctx)
	require.NotNil(t, origin)
	require.False(t, svc.CompleteOpenAIStickyMigration(ctx, first.Account, &OpenAIForwardResult{Stream: true, UpstreamTerminalEvent: "response.failed"}, nil))
	// Even if the next scheduling request carries an attempt's account ID, its
	// migration token must preserve the owner observed by the first attempt.
	req.StickyAccountID = 2
	req.ExcludedIDs = map[int64]struct{}{1: {}}
	next, _, err := scheduler.Select(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, next)
	t.Cleanup(next.ReleaseFunc)
	nextCtx := ContextWithSelectionPriorityRequest(ctx, next)
	require.Same(t, origin, stickyMigrationFromContext(nextCtx))
	require.EqualValues(t, 1, stickyMigrationFromContext(nextCtx).ownerID)
	require.True(t, svc.CompleteOpenAIStickyMigration(nextCtx, next.Account, &OpenAIForwardResult{}, nil))
	require.Len(t, cache.calls, 1)
	require.EqualValues(t, 1, cache.calls[0].expected)
	require.EqualValues(t, 2, cache.calls[0].owner)
	requireStickyMigrationOwner(t, cache, req, 2)
}

func TestOpenAIStickyMigrationConcurrentRequestsAndLateOriginalOwner(t *testing.T) {
	svc, _, cache, req := newStickyMigrationFixture(t)
	ctxB := stickyMigrationTestContext(t, svc, context.Background(), req)
	ctxC := stickyMigrationTestContext(t, svc, context.Background(), req)
	ctxA := stickyMigrationTestContext(t, svc, context.Background(), req)
	start := make(chan struct{})
	results := make(chan bool, 2)
	for i, ctx := range []context.Context{ctxB, ctxC} {
		go func() {
			<-start
			results <- svc.CompleteOpenAIStickyMigration(ctx, &Account{ID: int64(i + 2)}, &OpenAIForwardResult{}, nil)
		}()
	}
	close(start)
	first, second := <-results, <-results
	require.NotEqual(t, first, second, "only one independent request can replace owner A")
	owner, err := cache.GetSessionAccountID(t.Context(), *req.GroupID, "openai:"+req.SessionHash)
	require.NoError(t, err)
	require.Contains(t, []int64{2, 3}, owner)
	require.Len(t, cache.calls, 2)
	require.False(t, svc.CompleteOpenAIStickyMigration(ctxA, &Account{ID: 1}, &OpenAIForwardResult{}, nil))
	require.False(t, svc.CompleteOpenAIStickyMigration(ctxA, &Account{ID: 4}, &OpenAIForwardResult{}, nil), "late owner A completion consumes its token without restoring A")
	require.NoError(t, svc.BindStickySessionAfterProfitAdmission(ctxA, req.GroupID, req.SessionHash, 1))
	requireStickyMigrationOwner(t, cache, req, owner)
	require.Len(t, cache.calls, 2)
	require.Zero(t, cache.sets)
}

func TestOpenAIStickyMigrationCacheFailureAndClearedBindingNeverSet(t *testing.T) {
	for _, cleared := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleared=%t", cleared), func(t *testing.T) {
			svc, _, cache, req := newStickyMigrationFixture(t)
			ctx := stickyMigrationTestContext(t, svc, context.Background(), req)
			if cleared {
				delete(cache.bindings, stickyMigrationTestKey(*req.GroupID, "openai:"+req.SessionHash))
			} else {
				cache.migrationErr = errors.New("cache unavailable")
			}
			require.False(t, svc.CompleteOpenAIStickyMigration(ctx, &Account{ID: 2}, &OpenAIForwardResult{}, nil))
			require.Len(t, cache.calls, 1)
			require.Zero(t, cache.sets, "CAS errors/misses must not degrade into SET")
			owner, err := cache.GetSessionAccountID(t.Context(), *req.GroupID, "openai:"+req.SessionHash)
			if cleared {
				require.ErrorIs(t, err, ErrStickySessionNotFound)
				require.Zero(t, owner)
			} else {
				require.NoError(t, err)
				require.EqualValues(t, 1, owner)
			}
		})
	}
}

func TestOpenAIStickyMigrationLegacyKeysAndTTLs(t *testing.T) {
	for _, test := range []struct {
		name          string
		read, write   bool
		legacyOnly    bool
		ttlSeconds    int
		wantLegacy    string
		wantLegacyTTL time.Duration
	}{
		{"disabled", false, false, false, 3600, "", 10 * time.Minute},
		{"read-only", true, false, false, 3600, "openai:old-hash", 10 * time.Minute},
		{"write-only", false, true, false, 3600, "openai:old-hash", 10 * time.Minute},
		{"both", true, true, false, 3600, "openai:old-hash", 10 * time.Minute},
		{"legacy-only-binding", true, false, true, 300, "openai:old-hash", 5 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc, _, cache, req := newStickyMigrationFixture(t)
			svc.cfg.Gateway.OpenAIWS.SessionHashReadOldFallback = test.read
			svc.cfg.Gateway.OpenAIWS.SessionHashDualWriteOld = test.write
			svc.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds = test.ttlSeconds
			ctx := withOpenAILegacySessionHash(t.Context(), "old-hash")
			cache.bindings[stickyMigrationTestKey(*req.GroupID, "openai:old-hash")] = 1
			if test.legacyOnly {
				delete(cache.bindings, stickyMigrationTestKey(*req.GroupID, "openai:"+req.SessionHash))
				req.StickyAccountID = 0
			}
			ctx = stickyMigrationTestContext(t, svc, ctx, req)
			require.True(t, svc.CompleteOpenAIStickyMigration(ctx, &Account{ID: 2}, &OpenAIForwardResult{}, nil))
			require.Len(t, cache.calls, 1)
			call := cache.calls[0]
			require.Equal(t, *req.GroupID, call.groupID)
			require.Equal(t, "openai:"+req.SessionHash, call.primary)
			require.Equal(t, test.wantLegacy, call.legacy)
			require.Equal(t, time.Duration(test.ttlSeconds)*time.Second, call.ttl)
			require.Equal(t, test.wantLegacyTTL, call.legacyTTL)
			requireStickyMigrationOwner(t, cache, req, 2)
			legacyOwner, err := cache.GetSessionAccountID(t.Context(), *req.GroupID, "openai:old-hash")
			require.NoError(t, err)
			if test.wantLegacy != "" {
				require.EqualValues(t, 2, legacyOwner)
			} else {
				require.EqualValues(t, 1, legacyOwner)
			}
		})
	}
}

func TestOpenAIStickyMigrationProfitVetoPreservesOriginalOwner(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("migration=%t", enabled), func(t *testing.T) {
			svc, scheduler, cache, req := newStickyMigrationFixture(t)
			svc.settingService.prioritySchedulingConfig.config.StickyMigrationOnSuccess = enabled
			accounts := svc.accountRepo.(schedulerGroupAwareOpenAIAccountRepo).accounts
			profitControlTestAccountWithRate(&accounts[0], .8)
			profitControlTestAccountWithRate(&accounts[1], .3)
			ctx := profitControlTestCtx(profitControlTestGroup(*req.GroupID, .5, 0))
			ctx, _ = svc.WithOpenAIRequestPricingContext(ctx, req.GroupID)
			selected, _, err := scheduler.Select(ctx, req)
			require.NoError(t, err)
			require.NotNil(t, selected)
			t.Cleanup(selected.ReleaseFunc)
			require.EqualValues(t, 2, selected.Account.ID, "the expensive owner cannot bypass the existing profit gate")
			ctx = ContextWithSelectionProfitGate(ctx, selected)
			ctx = ContextWithSelectionPriorityRequest(ctx, selected)
			require.True(t, gatewayProfitControlGateActive(ctx))
			_, veto, _ := svc.ProfitControlVetoLatest(ctx, selected.Account)
			require.False(t, veto)
			require.NoError(t, svc.BindStickySessionAfterProfitAdmission(ctx, req.GroupID, req.SessionHash, 2))
			requireStickyMigrationOwner(t, cache, req, 1)
			require.False(t, svc.CompleteOpenAIStickyMigration(ctx, selected.Account, &OpenAIForwardResult{}, nil))
			requireStickyMigrationOwner(t, cache, req, 1)
			require.Empty(t, cache.calls, "a profit veto borrows another account without transferring the original session")
		})
	}
}

func TestOpenAIStickyMigrationTerminalProfitVetoSurvivesFailover(t *testing.T) {
	svc, scheduler, cache, req := newStickyMigrationFixture(t)
	stats := newOpenAIAccountRuntimeStats()
	svc.openaiAccountStats, scheduler.stats = stats, stats
	accounts := svc.accountRepo.(schedulerGroupAwareOpenAIAccountRepo).accounts
	for i := range accounts {
		profitControlTestAccountWithRate(&accounts[i], .3)
	}
	ctx := profitControlTestCtx(profitControlTestGroup(*req.GroupID, .5, 0))
	ctx, _ = svc.WithOpenAIRequestPricingContext(ctx, req.GroupID)
	first, _, err := scheduler.Select(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.EqualValues(t, 1, first.Account.ID)
	ctx = ContextWithSelectionPriorityRequest(ContextWithSelectionProfitGate(ctx, first), first)
	// The rate changes after slot admission; the terminal gate observes the
	// newly expensive owner and the subsequent retry excludes it entirely.
	profitControlTestAccountWithRate(first.Account, .8)
	_, veto, reason := svc.ProfitControlVetoLatest(ctx, first.Account)
	require.True(t, veto)
	require.Equal(t, openAIProfitFilterReasonThreshold, reason)
	first.ReleaseFunc()
	req.ExcludedIDs = map[int64]struct{}{1: {}}
	second, _, err := scheduler.Select(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, second)
	t.Cleanup(second.ReleaseFunc)
	require.EqualValues(t, 2, second.Account.ID)
	ctx = ContextWithSelectionPriorityRequest(ContextWithSelectionProfitGate(ctx, second), second)
	require.NoError(t, svc.BindStickySessionAfterProfitAdmission(ctx, req.GroupID, req.SessionHash, 2))
	require.False(t, svc.CompleteOpenAIStickyMigration(ctx, second.Account, &OpenAIForwardResult{}, nil))
	requireStickyMigrationOwner(t, cache, req, 1)
	require.Empty(t, cache.calls)
	require.Zero(t, cache.sets)
}

func TestOpenAIStickyMigrationCapacityWaitWithEligibleProfitOwner(t *testing.T) {
	svc, scheduler, cache, req := newStickyMigrationFixture(t)
	stats := newOpenAIAccountRuntimeStats()
	svc.openaiAccountStats, scheduler.stats = stats, stats
	concurrency := svc.concurrencyService.cache.(*rotationHoldTestCache)
	concurrency.full[1] = true
	accounts := svc.accountRepo.(schedulerGroupAwareOpenAIAccountRepo).accounts
	for i := range accounts {
		profitControlTestAccountWithRate(&accounts[i], .3)
	}
	ctx := profitControlTestCtx(profitControlTestGroup(*req.GroupID, .5, 0))
	ctx, _ = svc.WithOpenAIRequestPricingContext(ctx, req.GroupID)
	selected, _, err := scheduler.Select(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, selected)
	require.EqualValues(t, 1, selected.Account.ID)
	require.NotNil(t, selected.WaitPlan)
	require.False(t, selected.Acquired)
	ctx = ContextWithSelectionPriorityRequest(ContextWithSelectionProfitGate(ctx, selected), selected)
	_, veto, _ := svc.ProfitControlVetoLatest(ctx, selected.Account)
	require.False(t, veto)
	require.False(t, svc.CompleteOpenAIStickyMigration(ctx, selected.Account, &OpenAIForwardResult{}, nil))
	requireStickyMigrationOwner(t, cache, req, 1)
	require.Empty(t, cache.calls, "capacity pressure cannot commit a cache-owner migration")
}

func TestOpenAIStickyMigrationRevisionSnapshotRejectsABA(t *testing.T) {
	for _, transition := range []string{"owner-cycle", "delete-recreate", "same-owner-write"} {
		t.Run(transition, func(t *testing.T) {
			svc, _, cache, req := newStickyMigrationFixture(t)
			require.NoError(t, cache.SetSessionAccountID(t.Context(), *req.GroupID, "openai:"+req.SessionHash, 1, time.Hour))
			ctx := stickyMigrationTestContext(t, svc, t.Context(), req)
			_, expectedRevision, err := cache.ReadSessionAccountBinding(t.Context(), *req.GroupID, "openai:"+req.SessionHash, "")
			require.NoError(t, err)
			switch transition {
			case "owner-cycle":
				require.NoError(t, cache.SetSessionAccountID(t.Context(), *req.GroupID, "openai:"+req.SessionHash, 2, time.Hour))
			case "delete-recreate":
				require.NoError(t, cache.DeleteSessionAccountID(t.Context(), *req.GroupID, "openai:"+req.SessionHash))
			}
			require.NoError(t, cache.SetSessionAccountID(t.Context(), *req.GroupID, "openai:"+req.SessionHash, 1, time.Hour))
			writesBefore := cache.sets
			require.False(t, svc.CompleteOpenAIStickyMigration(ctx, &Account{ID: 3}, &OpenAIForwardResult{}, nil))
			requireStickyMigrationOwner(t, cache, req, 1)
			require.Len(t, cache.calls, 1)
			require.Equal(t, expectedRevision, cache.calls[0].expectedRevision)
			require.Equal(t, writesBefore, cache.sets)
		})
	}
}

func TestOpenAIStickyMigrationUnavailableSnapshotDefersWithoutRecreating(t *testing.T) {
	for _, unavailable := range []string{"read-error", "deleted-after-owner-read"} {
		t.Run(unavailable, func(t *testing.T) {
			svc, _, cache, req := newStickyMigrationFixture(t)
			if unavailable == "read-error" {
				cache.readErr = errors.New("cache unavailable")
			} else {
				delete(cache.bindings, stickyMigrationTestKey(*req.GroupID, "openai:"+req.SessionHash))
			}
			ctx := stickyMigrationTestContext(t, svc, t.Context(), req)
			require.NoError(t, svc.BindStickySessionAfterProfitAdmission(ctx, req.GroupID, req.SessionHash, 2))
			require.NoError(t, svc.deleteStickySessionAccountID(ctx, req.GroupID, req.SessionHash))
			require.False(t, svc.CompleteOpenAIStickyMigration(ctx, &Account{ID: 2}, &OpenAIForwardResult{}, nil))
			require.Empty(t, cache.calls)
			require.Zero(t, cache.sets)
			require.Zero(t, cache.deletes)
		})
	}
}

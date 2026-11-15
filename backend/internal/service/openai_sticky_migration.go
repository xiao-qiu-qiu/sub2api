package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

type openAIStickyMigrationContextKey struct{}

// The original owner is immutable across attempts. Only a confirmed successful
// completion may replace it, and a later completion must still win Redis CAS.
type openAIStickyMigration struct {
	groupID             int64
	sessionHash         string
	legacyKey           string
	ownerID             int64
	revision            string
	snapshotUnavailable bool
	model               string
	historyModel        string
	completed           atomic.Bool
	preserveForProfit   atomic.Bool
}

func stickyMigrationFromContext(ctx context.Context) *openAIStickyMigration {
	if ctx == nil {
		return nil
	}
	if migration, captured := ctx.Value(openAIStickyMigrationContextKey{}).(*openAIStickyMigration); captured {
		return migration
	}
	if req, ok := ctx.Value(prioritySelectionRequestKey{}).(OpenAIAccountScheduleRequest); ok {
		return req.stickyMigration
	}
	return nil
}

func (m *openAIStickyMigration) matches(groupID *int64, sessionHash string) bool {
	return m != nil && m.groupID == derefGroupID(groupID) && m.sessionHash == strings.TrimSpace(sessionHash)
}

func deferOpenAIStickyBinding(ctx context.Context, groupID *int64, sessionHash string) bool {
	// Continue suppressing eager writes after completion too: an old WebSocket
	// connection must not overwrite a newer request's successful migration.
	return stickyMigrationFromContext(ctx).matches(groupID, sessionHash)
}

func preserveOpenAIStickyMigrationForProfit(ctx context.Context, account *Account) {
	if migration := stickyMigrationFromContext(ctx); migration != nil && account != nil && account.ID == migration.ownerID {
		// Profit-controlled fallback is intentionally temporary. Remember a
		// veto even if the next selection excludes this owner before rechecking.
		migration.preserveForProfit.Store(true)
	}
}

func (s *OpenAIGatewayService) prepareOpenAIStickyMigration(ctx context.Context, req OpenAIAccountScheduleRequest) *openAIStickyMigration {
	if s == nil || s.cache == nil || req.routingAvailability != nil || req.Platform != PlatformOpenAI ||
		!req.UseUpstreamTokenCost || req.RequiredImageCapability != "" || strings.TrimSpace(req.SessionHash) == "" ||
		strings.TrimSpace(req.PreviousResponseID) != "" || req.GuardianParentAccountID > 0 || req.PreserveStickyBinding ||
		req.DisableStickyEscape || req.EncryptedReasoning || isManualAccountTest(ctx) {
		return nil
	}
	cfg := s.prioritySchedulingRuntimeConfig()
	if !cfg.StickyMigrationOnSuccess || !priorityApplies(cfg, req) {
		return nil
	}
	cache, supported := s.cache.(StickySessionMigrationCache)
	if !supported {
		return nil
	}
	if prior := stickyMigrationFromContext(ctx); prior.matches(req.GroupID, req.SessionHash) && !prior.completed.Load() &&
		prior.model == req.RequestedModel && prior.historyModel == priorityHistoryModel(req) {
		return prior
	}
	legacyKey := ""
	if s.openAISessionHashReadOldFallbackEnabled() || s.openAISessionHashDualWriteOldEnabled() {
		legacyKey = s.openAILegacySessionCacheKey(ctx, req.SessionHash)
	}
	migration := &openAIStickyMigration{
		groupID: derefGroupID(req.GroupID), sessionHash: strings.TrimSpace(req.SessionHash), legacyKey: legacyKey,
		ownerID: req.StickyAccountID, model: req.RequestedModel, historyModel: priorityHistoryModel(req),
	}
	ownerID, revision, err := cache.ReadSessionAccountBinding(ctx, migration.groupID, s.openAISessionCacheKey(req.SessionHash), legacyKey)
	if err != nil || ownerID <= 0 {
		if errors.Is(err, ErrStickySessionNotFound) && req.StickyAccountID <= 0 {
			return nil // A genuinely new session retains normal initial binding.
		}
		// A failed snapshot or a binding cleared since the initial read must
		// never degrade into an eager SET or resurrect the cleared session.
		migration.snapshotUnavailable = true
		return migration
	}
	migration.ownerID, migration.revision = ownerID, revision
	return migration
}

// SucceededForStickyMigration requires completed generation, rather than merely
// a nil forwarding error. Billing can retain usage from incomplete responses.
func (r *OpenAIForwardResult) SucceededForStickyMigration() bool {
	if r == nil || r.ClientDisconnect || r.QualityInterrupted || r.streamReadIncomplete || r.stickyMigrationInvalidTerminal || r.stickyMigrationAwaitingDelivery {
		return false
	}
	switch r.UpstreamTerminalEvent {
	case "response.completed", "response.done", "[DONE]":
		return true
	case "":
		return !r.Stream && !r.OpenAIWSMode
	default:
		return false
	}
}

// CompleteOpenAIStickyMigration is called only at a forward/turn completion
// boundary. Failed attempts and disconnects keep the original binding. Cache
// failures never affect the successful response and never fall back to SET.
func (s *OpenAIGatewayService) CompleteOpenAIStickyMigration(ctx context.Context, account *Account, result *OpenAIForwardResult, forwardErr error) bool {
	migration := stickyMigrationFromContext(ctx)
	if s == nil || migration == nil || migration.snapshotUnavailable || migration.preserveForProfit.Load() || account == nil || account.ID <= 0 || forwardErr != nil ||
		ctx.Err() != nil || !result.SucceededForStickyMigration() {
		return false
	}
	cfg := s.prioritySchedulingRuntimeConfig()
	groupID := migration.groupID
	req := OpenAIAccountScheduleRequest{GroupID: &groupID, RequestedModel: migration.model, HistoryModel: migration.historyModel}
	if !cfg.StickyMigrationOnSuccess || !priorityApplies(cfg, req) ||
		s.preserveOAuthRecoveryStickyBinding(ctx, &groupID, migration.sessionHash, account.ID) ||
		s.preserveOpenAIAccountCooldownStickyBinding(ctx, &groupID, migration.sessionHash, account.ID) {
		return false
	}
	if !migration.completed.CompareAndSwap(false, true) || account.ID == migration.ownerID {
		return false
	}
	cache, ok := s.cache.(StickySessionMigrationCache)
	if !ok {
		return false
	}
	ttl := s.openAIWSSessionStickyTTL()
	// The request may finish immediately after delivering the terminal event.
	// Keep this bookkeeping bounded independently of connection teardown.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 500*time.Millisecond)
	defer cancel()
	moved, err := cache.CompareAndSwapSessionAccountID(writeCtx, migration.groupID,
		s.openAISessionCacheKey(migration.sessionHash), migration.legacyKey,
		migration.ownerID, migration.revision, account.ID, ttl, s.openAIStickyLegacyTTL(ttl))
	if err != nil {
		slog.Warn("openai_sticky_migration_failed", "group_id", migration.groupID, "from_account_id", migration.ownerID, "to_account_id", account.ID, "error", err)
		return false
	}
	if moved {
		slog.Info("openai_sticky_migration_completed", "group_id", migration.groupID, "from_account_id", migration.ownerID, "to_account_id", account.ID)
	}
	return moved
}

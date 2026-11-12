package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// OpenAIAccountCooldownWait is shared by all cooldown waits in one request.
const OpenAIAccountCooldownWait = 30 * time.Second

var ErrOpenAIAccountCooldownAdmissionInterrupted = errors.New("leave concurrency admission to wait for account cooldown")

type openAIAccountCooldownBudgetKey struct{}

type openAIAccountCooldownCandidate struct {
	accountID   int64
	groupID     *int64
	sessionHash string
	model       string
	transport   OpenAIUpstreamTransport
	capability  OpenAIEndpointCapability
	compact     bool
}

type openAIAccountCooldownBudget struct {
	mu              sync.Mutex
	deadline        time.Time
	pending         *Account
	model           string
	candidate       *openAIAccountCooldownCandidate
	waitedAccountID int64
	owner           *openAIAccountCooldownCandidate
}

// WithOpenAIAccountCooldownBudget keeps the same lazy deadline across retries.
func WithOpenAIAccountCooldownBudget(ctx context.Context) context.Context {
	if accountCooldownBudget(ctx) != nil {
		return ctx
	}
	return NewOpenAIAccountCooldownBudget(ctx)
}

// NewOpenAIAccountCooldownBudget starts a new logical WebSocket turn.
func NewOpenAIAccountCooldownBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, openAIAccountCooldownBudgetKey{}, &openAIAccountCooldownBudget{})
}

func accountCooldownBudget(ctx context.Context) *openAIAccountCooldownBudget {
	b, _ := ctx.Value(openAIAccountCooldownBudgetKey{}).(*openAIAccountCooldownBudget)
	return b
}

func (b *openAIAccountCooldownBudget) expired() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.deadline.IsZero() && !time.Now().Before(b.deadline)
}

func (s *OpenAIGatewayService) excelBPSCooldownRemaining(account *Account, model string) time.Duration {
	if s == nil || account == nil || !account.IsExcelBPSEnabledForModel(model) {
		return 0
	}
	value, ok := s.excelBPSCooldownUntil.Load(account.ID)
	if !ok {
		return 0
	}
	until, ok := value.(time.Time)
	if !ok {
		return 0
	}
	remaining := time.Until(until)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (s *OpenAIGatewayService) queueOpenAIAccountCooldown(ctx context.Context, account *Account, model string) bool {
	b := accountCooldownBudget(ctx)
	if b == nil || ctx.Err() != nil || s.excelBPSCooldownRemaining(account, model) <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deadline.IsZero() {
		b.deadline = time.Now().Add(OpenAIAccountCooldownWait)
	}
	if !time.Now().Before(b.deadline) {
		return false
	}
	b.pending, b.model = account, model
	b.waitedAccountID = account.ID
	if b.candidate != nil && b.candidate.model == model && (b.candidate.accountID == 0 || b.candidate.accountID == account.ID) {
		owner := *b.candidate
		owner.accountID = account.ID
		b.owner = &owner
	}
	return true
}

// Shared affinity can be changed by an older request whose budget has already
// expired. A younger waiter retains its own owner until its budget expires.
func openAIAccountCooldownStickyOwner(ctx context.Context, groupID *int64, sessionHash string) int64 {
	b := accountCooldownBudget(ctx)
	if b == nil || sessionHash == "" {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	owner := b.owner
	if owner == nil || owner.accountID != b.waitedAccountID || owner.sessionHash != sessionHash || derefGroupID(owner.groupID) != derefGroupID(groupID) ||
		b.deadline.IsZero() || !time.Now().Before(b.deadline) {
		return 0
	}
	return owner.accountID
}

// Keep a recovered owner out of soft rebalancing for the remainder of its
// request's wait budget. Normal eligibility and capacity checks still apply.
func (s *OpenAIGatewayService) openAIStickyWaitProtected(ctx context.Context, accountID int64) bool {
	if s.openAIStickyRotationWaiting(ctx, accountID) {
		return true
	}
	b := accountCooldownBudget(ctx)
	if b == nil || accountID <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.waitedAccountID == accountID && !b.deadline.IsZero() && time.Now().Before(b.deadline)
}

// QueueOpenAIAccountCooldown handles a replayable BPS 429 before failover.
// Ingress must release generation slots before calling the waiter.
func (s *OpenAIGatewayService) QueueOpenAIAccountCooldown(ctx context.Context, accountID int64, model string) bool {
	if s == nil || (s.accountRepo == nil && s.schedulerSnapshot == nil) || accountCooldownBudget(ctx) == nil {
		return false
	}
	account, err := s.getSchedulableAccount(ctx, accountID)
	if err != nil || account == nil || !account.IsSchedulable() {
		return false
	}
	return s.queueOpenAIAccountCooldown(ctx, account, model)
}

// WaitForOpenAIAccountCooldown parks only this request. Cooldown extensions
// share its original deadline; cancellation never changes another waiter or
// the account's cooldown. At the deadline normal selection may fail over.
func (s *OpenAIGatewayService) WaitForOpenAIAccountCooldown(ctx context.Context) (bool, error) {
	b := accountCooldownBudget(ctx)
	if b == nil {
		return false, nil
	}
	b.mu.Lock()
	account, model, deadline := b.pending, b.model, b.deadline
	b.mu.Unlock()
	if account == nil {
		return false, nil
	}
	defer func() {
		b.mu.Lock()
		if b.pending == account {
			b.pending = nil
		}
		b.mu.Unlock()
	}()
	for {
		if ctx.Err() != nil {
			return true, context.Cause(ctx)
		}
		remaining := min(s.excelBPSCooldownRemaining(account, model), time.Until(deadline))
		if remaining <= 0 {
			return true, nil
		}
		// Recheck local state so an early cooldown removal releases promptly.
		timer := time.NewTimer(min(remaining, 250*time.Millisecond))
		select {
		case <-ctx.Done():
			timer.Stop()
			return true, context.Cause(ctx)
		case <-timer.C:
		}
	}
}

// Capture the owner before selection can replace or delete its sticky key.
// The returned check is also used after selection to catch concurrent 429s.
func (s *OpenAIGatewayService) prepareOpenAIAccountCooldownSelection(ctx context.Context, groupID *int64, previousResponseID, sessionHash, model string, excluded map[int64]struct{}, transport OpenAIUpstreamTransport, capability OpenAIEndpointCapability, compact bool) func() bool {
	noop := func() bool { return false }
	b := accountCooldownBudget(ctx)
	if s == nil || b == nil {
		return noop
	}
	if b.expired() || (s.accountRepo == nil && s.schedulerSnapshot == nil) {
		return noop
	}
	candidate := &openAIAccountCooldownCandidate{groupID: groupID, sessionHash: sessionHash, model: model, transport: transport, capability: capability, compact: compact}
	b.mu.Lock()
	b.candidate = candidate
	b.mu.Unlock()
	var accountID int64
	if strings.TrimSpace(previousResponseID) != "" {
		if store := s.getOpenAIWSStateStore(); store != nil {
			accountID, _ = store.GetResponseAccount(ctx, derefGroupID(groupID), previousResponseID)
		}
	}
	if accountID <= 0 && sessionHash != "" {
		accountID, _ = s.getStickySessionAccountID(ctx, groupID, sessionHash)
	}
	if accountID <= 0 {
		return noop
	}
	if _, skip := excluded[accountID]; skip {
		return noop
	}
	b.mu.Lock()
	candidate.accountID = accountID
	b.mu.Unlock()
	return func() bool {
		b.mu.Lock()
		observed := b.pending != nil && b.pending.ID == candidate.accountID && time.Now().Before(b.deadline)
		b.mu.Unlock()
		if observed {
			// A sticky write already observed the race. Even if its cooldown has
			// just ended, reselect the owner instead of serving the replacement.
			return true
		}
		account := s.openAIAccountCooldownCandidate(ctx, candidate)
		if account == nil || !s.queueOpenAIAccountCooldown(ctx, account, model) {
			return false
		}
		if sessionHash != "" {
			_ = s.refreshStickySessionTTL(ctx, groupID, sessionHash, openaiStickySessionTTL)
		}
		return true
	}
}

func (s *OpenAIGatewayService) openAIAccountCooldownCandidate(ctx context.Context, candidate *openAIAccountCooldownCandidate) *Account {
	if candidate == nil || ctx.Err() != nil {
		return nil
	}
	// Healthy sticky requests must not add account/DB reads on either side of
	// selection. Only an active local cooldown needs the eligibility probe.
	value, ok := s.excelBPSCooldownUntil.Load(candidate.accountID)
	until, valid := value.(time.Time)
	if !ok || !valid || !time.Now().Before(until) {
		return nil
	}
	account, err := s.getSchedulableAccount(ctx, candidate.accountID)
	if err != nil || account == nil || s.excelBPSCooldownRemaining(account, candidate.model) <= 0 {
		return nil
	}
	// A manual stop, group removal, model veto or other runtime block must
	// still win. Ignore only the BPS 429 gate being waited on.
	if s.accountRepo != nil {
		account, err = s.accountRepo.GetByID(ctx, candidate.accountID)
	}
	if err != nil || account == nil || s.excelBPSCooldownRemaining(account, candidate.model) <= 0 || !account.IsSchedulable() || account.Platform != PlatformOpenAI ||
		!s.openAIAccountMatchesSchedulingGroup(account, candidate.groupID) ||
		!s.isOpenAIAccountTransportCompatible(account, candidate.transport, candidate.model) ||
		s.isOpenAIAccountRequestRuntimeBlockedWithoutBPSCooldown(account, candidate.model, candidate.compact) {
		return nil
	}
	checkCtx := s.withOpenAIQuotaAutoPauseContext(ctx)
	checkCtx = s.withOpenAIProfitControlGate(checkCtx, candidate.groupID)
	checkCtx = WithAccountRotationSticky(checkCtx, account.ID)
	scheduler := &defaultOpenAIAccountScheduler{service: s}
	compatible, _ := scheduler.accountRequestCompatibility(checkCtx, account, OpenAIAccountScheduleRequest{
		GroupID: candidate.groupID, Platform: PlatformOpenAI, RequestedModel: candidate.model,
		RequiredTransport: candidate.transport, RequiredCapability: candidate.capability,
		RequireCompact: candidate.compact, RequirePrivacySet: s.openAIGroupRequiresPrivacySet(ctx, candidate.groupID),
	}, true)
	if !compatible || (candidate.compact && openAICompactSupportTier(account) == 0) {
		return nil
	}
	return account
}

func (s *OpenAIGatewayService) preserveOpenAIAccountCooldownStickyBinding(ctx context.Context, groupID *int64, sessionHash string, replacementID int64) bool {
	b := accountCooldownBudget(ctx)
	if b == nil || b.expired() {
		return false
	}
	b.mu.Lock()
	candidate := b.candidate
	b.mu.Unlock()
	if candidate == nil || candidate.sessionHash != sessionHash || derefGroupID(candidate.groupID) != derefGroupID(groupID) {
		return false
	}
	if ownerID := openAIAccountCooldownStickyOwner(ctx, groupID, sessionHash); ownerID > 0 && s.cache != nil {
		// The request-local owner must not undo another request's legitimate
		// failover or delete its replacement when the old owner is invalid.
		currentID, err := s.cache.GetSessionAccountID(ctx, derefGroupID(groupID), s.openAISessionCacheKey(sessionHash))
		if err == nil && currentID > 0 && currentID != ownerID {
			return true
		}
	}
	if candidate.accountID == replacementID {
		return false
	}
	account := s.openAIAccountCooldownCandidate(ctx, candidate)
	return account != nil && s.queueOpenAIAccountCooldown(ctx, account, candidate.model)
}

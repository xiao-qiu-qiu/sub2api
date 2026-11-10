package service

import (
	"context"
	"errors"
	"strings"
	"time"
)

type oauthRecoveryStickyBinding struct {
	groupID int64
	hash    string
}

// This state belongs to one request/WS turn. A temporary backup must not
// replace the session binding whose credential is being repaired.
type oauthRecoveryRouting struct {
	hardBound        bool
	selectionStarted time.Time
	bindings         map[oauthRecoveryStickyBinding]int64
	excluded         map[int64]struct{}
}

func oauthRecoveryAccountExcluded(ctx context.Context, accountID int64) bool {
	b := recoveryBudget(ctx)
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, excluded := b.routing.excluded[accountID]
	return excluded
}

func withOAuthRecoveryExcludedAccounts(ctx context.Context, excluded map[int64]struct{}) map[int64]struct{} {
	b := recoveryBudget(ctx)
	if b == nil {
		return excluded
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.routing.excluded) == 0 {
		return excluded
	}
	combined := cloneExcludedAccountIDs(excluded)
	if combined == nil {
		combined = make(map[int64]struct{}, len(b.routing.excluded))
	}
	for id := range b.routing.excluded {
		combined[id] = struct{}{}
	}
	return combined
}

// PrepareOpenAIOAuthRecoveryFallback permits a safe, pre-output retry to use
// another account for this request only. The login task continues separately.
func PrepareOpenAIOAuthRecoveryFallback(ctx context.Context, err error) bool {
	if ctx.Err() != nil || (!errors.Is(err, ErrOAuthRecoveryTimeout) &&
		!errors.Is(err, ErrOAuthRecoveryFailed) && !errors.Is(err, ErrOAuthRecoveryQueueFull)) {
		return false
	}
	b := recoveryBudget(ctx)
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.routing.hardBound || len(b.observed) == 0 {
		return false
	}
	if b.routing.excluded == nil {
		b.routing.excluded = make(map[int64]struct{})
	}
	prepared := false
	for _, flight := range b.observed {
		if flight == nil || flight.accountID <= 0 {
			continue
		}
		b.routing.excluded[flight.accountID] = struct{}{}
		b.attempts[flight.accountID] = true
		prepared = true
	}
	if prepared {
		b.pending = nil
	}
	return prepared
}

func rememberOAuthRecoveryStickyBinding(ctx context.Context, groupID *int64, sessionHash string, accountID int64) {
	b := recoveryBudget(ctx)
	if b == nil || accountID <= 0 || sessionHash == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	key := oauthRecoveryStickyBinding{groupID: derefGroupID(groupID), hash: sessionHash}
	if b.routing.bindings == nil {
		b.routing.bindings = make(map[oauthRecoveryStickyBinding]int64)
	}
	// Once a binding has entered recovery, another selected account is only a
	// backup; retain the original owner across repeated selection attempts.
	previous := b.routing.bindings[key]
	if _, excluded := b.routing.excluded[previous]; previous > 0 && excluded {
		return
	}
	b.routing.bindings[key] = accountID
}

func (s *OpenAIGatewayService) preserveOAuthRecoveryStickyBinding(ctx context.Context, groupID *int64, sessionHash string, replacementID int64) bool {
	b := recoveryBudget(ctx)
	if b == nil {
		return false
	}
	b.mu.Lock()
	ownerID := b.routing.bindings[oauthRecoveryStickyBinding{groupID: derefGroupID(groupID), hash: sessionHash}]
	_, excluded := b.routing.excluded[ownerID]
	selectionStarted := b.routing.selectionStarted
	b.mu.Unlock()
	if ownerID <= 0 || ownerID == replacementID {
		return false
	}
	// Cover a 401 arriving after the selection precheck too: do not erase the
	// old binding while another request has just started its login task.
	if excluded {
		return true
	}
	if s == nil || s.oauthRecovery == nil || s.accountRepo == nil {
		return false
	}
	s.oauthRecovery.mu.Lock()
	knownRound := s.oauthRecovery.flights[ownerID] != nil
	s.oauthRecovery.mu.Unlock()
	if !knownRound {
		return false
	}
	account, err := s.getSchedulableAccount(ctx, ownerID)
	if err != nil || account == nil || !account.Schedulable || !s.openAIAccountMatchesSchedulingGroup(account, groupID) ||
		(account.Status != StatusActive && (account.Status != StatusError || account.ErrorMessage != ImmediateOAuthRecoveryErrorMessage)) {
		return false
	}
	// A round may finish between the precheck and sticky invalidation. Even
	// successful recovery changes the token, so token matching alone would
	// let the backup permanently take the newly recovered account's binding.
	rejectedToken := oauthRecoveryRejectedToken(ctx, account)
	s.oauthRecovery.mu.Lock()
	defer s.oauthRecovery.mu.Unlock()
	flight := s.oauthRecovery.flights[ownerID]
	return flight != nil && (flight.finished.IsZero() || flight.token == rejectedToken ||
		(!selectionStarted.IsZero() && !flight.finished.Before(selectionStarted)))
}

func (s *OpenAIGatewayService) queueBoundOpenAIOAuthRecovery(
	ctx context.Context,
	groupID *int64,
	previousResponseID, sessionHash, requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requireCompact, previousResponseCanMove bool,
) bool {
	if s == nil || s.oauthRecovery == nil || s.accountRepo == nil || recoveryBudget(ctx) == nil {
		return false
	}
	b := recoveryBudget(ctx)
	hardBound := strings.TrimSpace(previousResponseID) != "" && !previousResponseCanMove
	hardBound = hardBound || ctx.Value(openAIEncryptedReasoningAffinityKey{}) == true || preserveOpenAIGuardianParentBinding(ctx, sessionHash)
	b.mu.Lock()
	b.routing.hardBound = hardBound
	if b.routing.selectionStarted.IsZero() {
		b.routing.selectionStarted = time.Now()
	}
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
	if accountID <= 0 || oauthRecoveryAccountExcluded(ctx, accountID) {
		return false
	}
	if _, excluded := excludedIDs[accountID]; excluded {
		return false
	}
	account, err := s.getSchedulableAccount(ctx, accountID)
	if err != nil || account == nil || (account.Status == StatusActive && !s.oauthRecovery.active(accountID)) {
		return false
	}
	// A normal healthy session stays on the cached hot path. Only recovery
	// candidates need a fresh read before they are allowed to park a request.
	account, err = s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil || !oauth401CanRecover(account, nil) || !account.Schedulable ||
		!s.openAIAccountMatchesSchedulingGroup(account, groupID) {
		return false
	}
	// Only the temporary credential recovery gate may be ignored. Manual
	// stops, quality removal, quota, model and protocol restrictions still win.
	if account.Status != StatusActive && (account.Status != StatusError || account.ErrorMessage != ImmediateOAuthRecoveryErrorMessage) {
		return false
	}
	available := *account
	available.Status = StatusActive
	if !available.IsSchedulable() || !s.isOpenAIAccountTransportCompatible(&available, requiredTransport, requestedModel) {
		return false
	}
	checkCtx := s.withOpenAIQuotaAutoPauseContext(ctx)
	checkCtx = s.withOpenAIProfitControlGate(checkCtx, groupID)
	checkCtx = WithAccountRotationSticky(checkCtx, accountID)
	scheduler := &defaultOpenAIAccountScheduler{service: s}
	compatible, _ := scheduler.accountRequestCompatibility(checkCtx, &available, OpenAIAccountScheduleRequest{
		GroupID: groupID, Platform: PlatformOpenAI, RequestedModel: requestedModel,
		RequiredTransport: requiredTransport, RequiredCapability: requiredCapability,
		RequireCompact: requireCompact, RequirePrivacySet: s.openAIGroupRequiresPrivacySet(ctx, groupID),
	}, true)
	if !compatible || (requireCompact && openAICompactSupportTier(&available) == 0) {
		return false
	}
	if !s.QueueOpenAIOAuthRecoveryForAccount(ctx, accountID) {
		return false
	}
	if sessionHash != "" {
		_ = s.refreshStickySessionTTL(ctx, groupID, sessionHash, openaiStickySessionTTL)
	}
	return true
}

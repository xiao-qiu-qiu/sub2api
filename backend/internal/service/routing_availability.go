package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// RoutingAvailabilityRequest describes a Chat or Responses request before account-level
// model mapping. Model is still checked against the authenticated group's ACL.
type RoutingAvailabilityRequest struct {
	Model          string
	APIFormat      string
	ServiceTier    string
	Transport      string
	RequireCompact bool
}

func (r *RoutingAvailabilityRequest) Validate() error {
	r.Model = strings.TrimSpace(r.Model)
	if r.Model == "" || len(r.Model) > 512 {
		return errors.New("model is required and must be at most 512 bytes")
	}
	if r.APIFormat == "" {
		r.APIFormat = APIProtocolResponses
	}
	if r.APIFormat != APIProtocolResponses && r.APIFormat != APIProtocolChatCompletions {
		return errors.New("api_format must be responses or chat_completions")
	}
	if r.Transport == "" {
		r.Transport = "http"
	}
	if r.Transport != "http" && r.Transport != "websocket" {
		return errors.New("transport must be http or websocket")
	}
	if r.APIFormat == APIProtocolChatCompletions && (r.Transport != "http" || r.RequireCompact) {
		return errors.New("chat_completions requires transport=http and require_compact=false")
	}
	if r.ServiceTier == "" {
		r.ServiceTier = "default"
	}
	tier := normalizedOpenAIServiceTierValue(r.ServiceTier)
	if tier == "" {
		return &ErrInvalidOpenAIServiceTier{Value: boundInvalidOpenAIServiceTierValue(r.ServiceTier)}
	}
	r.ServiceTier = tier
	return nil
}

type RoutingAvailability struct {
	Object            string `json:"object"`
	Model             string `json:"model"`
	APIFormat         string `json:"api_format,omitempty"`
	State             string `json:"state"`
	Reason            string `json:"reason"`
	CheckedAt         int64  `json:"checked_at"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

// Optional cache capabilities keep this endpoint off slot acquisition and the
// mutating rotation/load APIs. Authorities without these capabilities fail unknown.
type routingAvailabilityCache interface {
	ReadRoutingAccountLoads(context.Context, []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error)
}

type routingAvailabilityEvaluation struct {
	cache           routingAvailabilityCache
	rotationWaiting map[int64]bool
	unsupported     bool
}

// RoutingAvailability only observes eligibility and live capacity. In
// particular it never calls Select, creates bindings, reserves RPM, or forwards.
func (s *OpenAIGatewayService) RoutingAvailability(ctx context.Context, key *APIKey, input RoutingAvailabilityRequest) (result RoutingAvailability) {
	result = RoutingAvailability{Object: "routing.availability", Model: strings.TrimSpace(input.Model), State: "unknown", Reason: "evaluation_unavailable", CheckedAt: time.Now().Unix(), RetryAfterSeconds: 3}
	if input.Validate() != nil {
		return result
	}
	result.APIFormat = input.APIFormat
	if key == nil || key.Group == nil || key.GroupID == nil || key.Group.ID != *key.GroupID {
		return result
	}
	// GET group selection intentionally skips generation billing eligibility.
	// It cannot predict which group a multi-group POST will ultimately choose.
	if len(key.ConfiguredGroupIDs()) > 1 {
		result.Reason = "multi_group_evaluation_not_supported"
		return result
	}
	if key.Group.Platform != PlatformOpenAI {
		result.Reason = "platform_not_supported"
		return result
	}
	if s == nil || s.concurrencyService == nil || (s.schedulerSnapshot == nil && s.accountRepo == nil) {
		return result
	}
	cache, ok := s.concurrencyService.cache.(routingAvailabilityCache)
	if !ok {
		result.Reason = "capacity_authority_unavailable"
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, ctxkey.Group, key.Group)
	ctx = context.WithValue(ctx, ctxkey.UserID, key.UserID)
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	// Install the same profit eligibility gate without recording a selection.
	if gate := s.resolveOpenAIProfitControlGate(ctx, key.GroupID); gate != nil {
		ctx = context.WithValue(ctx, openAIProfitControlGateCtxKey{}, gate)
	}
	mapping := ChannelMappingResult{MappedModel: input.Model}
	if s.channelService != nil {
		lookup, err := s.channelService.lookupGroupChannel(ctx, *key.GroupID)
		if err != nil {
			return result
		}
		if lookup != nil {
			mapping = resolveMapping(lookup, *key.GroupID, input.Model)
		}
	}
	if s.checkChannelPricingRestriction(ctx, key.GroupID, input.Model) {
		result.State, result.Reason = "empty", "model_restricted"
		return result
	}
	model := input.Model
	if mapping.Mapped {
		model = mapping.MappedModel
	}
	ctx = WithOpenAIForwardModel(ctx, model, input.RequireCompact)
	transport := OpenAIUpstreamTransportHTTPSSE
	if input.Transport == "websocket" {
		transport = OpenAIUpstreamTransportResponsesWebsocketV2Ingress
	}
	capability := OpenAIEndpointCapabilityResponses
	endpoint := "/v1/responses"
	if input.APIFormat == APIProtocolChatCompletions {
		capability = OpenAIEndpointCapabilityChatCompletions
		endpoint = "/v1/chat/completions"
		transport = OpenAIUpstreamTransportAny
	} else if input.RequireCompact {
		capability = OpenAIEndpointCapabilityResponsesCompact
	}
	evaluation := &routingAvailabilityEvaluation{cache: cache, rotationWaiting: make(map[int64]bool)}
	req := OpenAIAccountScheduleRequest{
		GroupID: key.GroupID, Platform: PlatformOpenAI, RequestedModel: model,
		HistoryModel: input.Model, InboundEndpoint: endpoint, UseUpstreamTokenCost: true,
		RequirePrivacySet: key.Group.RequirePrivacySet, RequiredTransport: transport,
		RequiredCapability: capability, RequireCompact: input.RequireCompact,
		routingAvailability: evaluation,
	}
	defer func() {
		// The receiving gateway owns the request's recovery budget. An upstream
		// router must send the request here instead of treating a login as an
		// empty pool or spending a separate capacity-wait budget first.
		if result.State != "empty" && result.State != "busy" {
			return
		}
		pending, readErr := s.routingOAuthRecoveryPending(ctx, key, req, input.ServiceTier)
		if readErr != nil {
			result.State, result.Reason = "unknown", "evaluation_unavailable"
			return
		}
		if pending {
			result.State, result.Reason, result.RetryAfterSeconds = "busy", "upstream_auth_recovery_pending", 1
		}
	}()
	scheduler := &defaultOpenAIAccountScheduler{service: s}
	ctx, accounts, loads, _, err := scheduler.loadOpenAIEligibleAccounts(ctx, req)
	if err != nil {
		if errors.Is(err, ErrNoAvailableAccounts) || errors.Is(err, ErrNoAvailableCompactAccounts) || errors.Is(err, ErrOpenAIRPMExhausted) {
			if evaluation.unsupported {
				result.Reason = "account_evaluation_not_supported"
				return result
			}
			result.State, result.Reason = "empty", "no_eligible_accounts"
		}
		return result
	}
	var fastPolicy *OpenAIFastPolicySettings
	if s.settingService != nil {
		fastPolicy, err = s.settingService.GetOpenAIFastPolicySettings(ctx)
		if err != nil {
			return result
		}
	}
	allowed := make(map[int64]struct{}, len(accounts))
	for _, account := range accounts {
		allowed[account.ID] = struct{}{}
	}
	cfg := s.prioritySchedulingRuntimeConfig()
	if priorityHardStopApplies(cfg, req) {
		pool := make([]openAIAccountCandidateScore, 0, len(accounts))
		for _, account := range accounts {
			pool = append(pool, openAIAccountCandidateScore{account: account, loadInfo: loads[account.ID], loadKnown: loads[account.ID] != nil})
		}
		history := s.priorityHistory(req, cfg, pool, true)
		if !history.ready() {
			result.Reason = "latency_history_unavailable"
			return result
		}
		allowed, _ = priorityLatencyAllowed(cfg, pool, history, time.Now())
	}
	busy, rotationWaiting := false, false
	for _, account := range accounts {
		if _, ok := allowed[account.ID]; !ok {
			continue
		}
		action, _ := evaluateOpenAIFastPolicyWithSettings(fastPolicy, key.UserID, account, model, input.ServiceTier)
		if action == BetaPolicyActionBlock {
			continue
		}
		load := loads[account.ID]
		if load == nil {
			result.Reason = "capacity_authority_unavailable"
			return result
		}
		if evaluation.rotationWaiting[account.ID] {
			rotationWaiting = true
			continue
		}
		if account.Concurrency > 0 && load.CurrentConcurrency >= account.Concurrency {
			busy = true
			continue
		}
		result.State, result.Reason, result.RetryAfterSeconds = "ready", "capacity_available", 0
		return result
	}
	if rotationWaiting {
		result.State, result.Reason, result.RetryAfterSeconds = "busy", "account_rotation", 1
		return result
	}
	if busy {
		result.State, result.Reason, result.RetryAfterSeconds = "busy", "capacity_busy", 1
		return result
	}
	if evaluation.unsupported {
		result.Reason = "account_evaluation_not_supported"
		return result
	}
	result.State, result.Reason = "empty", "no_eligible_accounts"
	return result
}

// Observe both local verification and durable login tasks without starting a
// login, verification probe, scheduler selection, or concurrency reservation.
func (s *OpenAIGatewayService) routingOAuthRecoveryPending(ctx context.Context, key *APIKey, req OpenAIAccountScheduleRequest, tier string) (bool, error) {
	if s.oauthRecovery == nil || s.accountRepo == nil {
		return false, nil
	}
	// Regional snapshots may intentionally omit otherwise valid accounts.
	// A central database query must never widen that assignment.
	if s.schedulerSnapshot != nil && s.schedulerSnapshot.localSnapshotReader != nil {
		return false, nil
	}
	accounts, err := s.accountRepo.ListAllWithFilters(ctx, PlatformOpenAI, "", "active,error", "", derefGroupID(req.GroupID), "")
	if err != nil {
		return false, err
	}
	ctx, err = s.withOpenAIRPMPrefetch(ctx, accounts)
	if err != nil {
		return false, err
	}
	var fastPolicy *OpenAIFastPolicySettings
	if s.settingService != nil {
		fastPolicy, err = s.settingService.GetOpenAIFastPolicySettings(ctx)
		if err != nil {
			return false, err
		}
	}
	var thresholds map[string]int
	if s.rateLimitService != nil && s.rateLimitService.settingService != nil {
		thresholds = s.rateLimitService.settingService.GetAccountSchedulingThresholds(ctx)
	}
	scheduler := &defaultOpenAIAccountScheduler{service: s}
	for i := range accounts {
		account := &accounts[i]
		if !oauth401CanRecover(account, nil) || account.IsCredentialShadow() || account.HasManagedOpenAIConnections() ||
			account.IsPrismBrowserEnabledForModel(req.RequestedModel) || !account.Schedulable ||
			!s.openAIAccountMatchesSchedulingGroup(account, req.GroupID) ||
			(account.Status != StatusActive && (account.Status != StatusError || account.ErrorMessage != ImmediateOAuthRecoveryErrorMessage)) {
			continue
		}
		candidate := *account
		candidate.Status = StatusActive
		if !candidate.IsSchedulableForModelWithContext(ctx, req.RequestedModel) ||
			!s.isOpenAIAccountTransportCompatible(&candidate, req.RequiredTransport, req.RequestedModel) ||
			(req.RequireCompact && openAICompactSupportTier(&candidate) == 0) {
			continue
		}
		if decision := EvaluateAccountSchedulingThreshold(&candidate, thresholds, time.Now()); decision.ShouldPause && decision.Until != nil && decision.Until.After(time.Now()) {
			continue
		}
		if compatible, _ := scheduler.accountRequestCompatibility(ctx, &candidate, req, true); !compatible {
			continue
		}
		if s.isExcelBPSCoolingDown(&candidate, req.RequestedModel, true) ||
			s.isExcelBPSModelCoolingDown(&candidate, req.RequestedModel, true) ||
			s.isOpenAIAccountModelRuntimeBlocked(&candidate, req.RequestedModel, true) {
			continue
		}
		if rpm, ok := accountRPMStateFromContext(ctx, &candidate); ok && candidate.CheckRPMSchedulability(rpm.Current) == WindowCostNotSchedulable {
			continue
		}
		if action, _ := evaluateOpenAIFastPolicyWithSettings(fastPolicy, key.UserID, &candidate, req.RequestedModel, tier); action == BetaPolicyActionBlock {
			continue
		}
		if s.oauthRecovery.active(account.ID) {
			return true, nil
		}
		task, err := s.oauthRecovery.guard.GetImmediateReauthTask(ctx, account.ID)
		if err != nil {
			return false, err
		}
		if task != nil && task.AccountID == account.ID && (task.Status == OpenAIOAuthReauthStatusQueued || task.Status == OpenAIOAuthReauthStatusRunning || task.Status == OpenAIOAuthReauthStatusCallbackProcessing) {
			return true, nil
		}
	}
	return false, nil
}

// Use the scheduler's group/platform candidate authority, then its shared
// eligibility pass. Threshold decisions are pure; ordinary selection persists
// them and advances rotation, which a capacity observer must never do.
func (s *OpenAIGatewayService) listRoutingAvailabilityAccounts(ctx context.Context, req OpenAIAccountScheduleRequest) ([]Account, error) {
	var accounts []Account
	var err error
	var snapshotHit bool
	if snapshot := s.schedulerSnapshot; snapshot != nil {
		bucket := snapshot.bucketFor(req.GroupID, req.Platform, snapshot.resolveMode(req.Platform, false))
		var projected []*Account
		var hit bool
		if snapshot.localSnapshotReader != nil {
			projected, hit, err = snapshot.localSnapshotReader.GetLocalSnapshot(ctx, bucket)
		} else if snapshot.cache != nil {
			projected, hit, err = snapshot.cache.GetSnapshot(ctx, bucket)
		}
		if err != nil {
			return nil, err
		}
		// Only a central Redis miss may fall back to a read-only candidate
		// query. Regional snapshots remain authoritative; never rebuild here.
		if !hit && (snapshot.localSnapshotReader != nil || snapshot.cache == nil) {
			return nil, ErrSchedulerCacheNotReady
		}
		snapshotHit = hit
		if hit {
			ids := make([]int64, 0, len(projected))
			for _, account := range projected {
				if account != nil {
					ids = append(ids, account.ID)
				}
			}
			full, readErr := snapshot.GetAccounts(ctx, ids)
			if readErr != nil {
				return nil, readErr
			}
			for _, id := range ids {
				account := full[id]
				if account == nil {
					return nil, ErrSchedulerCacheNotReady
				}
				accounts = append(accounts, *account)
			}
		}
	}
	if !snapshotHit {
		if s.accountRepo == nil {
			return nil, ErrSchedulerCacheNotReady
		}
		if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
			accounts, err = s.accountRepo.ListSchedulableByPlatform(ctx, req.Platform)
		} else if req.GroupID != nil {
			accounts, err = s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, *req.GroupID, req.Platform)
		} else {
			accounts, err = s.accountRepo.ListSchedulableUngroupedByPlatform(ctx, req.Platform)
		}
	}
	if err != nil {
		return nil, err
	}
	policy, err := s.concurrencyService.accountRotationPolicy(ctx)
	if err != nil {
		return nil, err
	}
	rotationReader, canPeek := s.concurrencyService.cache.(interface {
		PeekAccountRotation(context.Context, int64, AccountRotationPolicy) (*AccountRotationState, error)
	})
	if policy != nil && !canPeek {
		return nil, errors.New("read-only rotation authority is unavailable")
	}
	var thresholds map[string]int
	if s.rateLimitService != nil && s.rateLimitService.settingService != nil {
		thresholds = s.rateLimitService.settingService.GetAccountSchedulingThresholds(ctx)
	}
	filtered := make([]Account, 0, len(accounts))
	now := time.Now()
	for i := range accounts {
		account := &accounts[i]
		if !s.openAIAccountMatchesSchedulingGroup(account, req.GroupID) || !account.IsSchedulableForModelWithContext(ctx, req.RequestedModel) {
			continue
		}
		if account.Type != AccountTypeOAuth && account.Type != AccountTypeAPIKey || account.IsExcelOAuth() || account.HasManagedOpenAIConnections() || account.IsPrismBrowserEnabledForModel(req.RequestedModel) {
			req.routingAvailability.unsupported = true
			continue
		}
		decision := EvaluateAccountSchedulingThreshold(account, thresholds, now)
		if decision.ShouldPause && decision.Until != nil && decision.Until.After(now) {
			continue
		}
		if req.RequireCompact && openAICompactSupportTier(account) == 0 {
			continue
		}
		if policy != nil {
			state, err := rotationReader.PeekAccountRotation(ctx, account.ID, *policy)
			if err != nil {
				return nil, err
			}
			if state == nil {
				return nil, fmt.Errorf("rotation state unavailable")
			}
			if state.Hold || state.Recovering || !rotationStateAllows(state, policy.Mode, false) {
				// Keep rotation-blocked accounts through the remaining eligibility
				// gates. Only a fully eligible candidate may report rotation waiting.
				req.routingAvailability.rotationWaiting[account.ID] = true
			}
		}
		// Shadow parent lookups in the scheduler return a boolean. Read the
		// authority first so an infrastructure error cannot masquerade as empty.
		if parent := account.ParentAccountID; parent != nil && *parent > 0 {
			var parentAccount *Account
			if s.schedulerSnapshot != nil {
				parentAccount, err = s.schedulerSnapshot.GetAccount(ctx, *parent)
			} else {
				parentAccount, err = s.accountRepo.GetByID(ctx, *parent)
			}
			if err != nil {
				return nil, err
			}
			if parentAccount == nil {
				continue
			}
		}
		filtered = append(filtered, *account)
	}
	return filtered, nil
}

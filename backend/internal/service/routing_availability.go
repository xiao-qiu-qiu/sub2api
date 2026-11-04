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

// RoutingAvailabilityRequest describes a Responses request before account-level
// model mapping. Model is still checked against the authenticated group's ACL.
type RoutingAvailabilityRequest struct {
	Model          string
	ServiceTier    string
	Transport      string
	RequireCompact bool
}

func (r *RoutingAvailabilityRequest) Validate() error {
	r.Model = strings.TrimSpace(r.Model)
	if r.Model == "" || len(r.Model) > 512 {
		return errors.New("model is required and must be at most 512 bytes")
	}
	if r.Transport == "" {
		r.Transport = "http"
	}
	if r.Transport != "http" && r.Transport != "websocket" {
		return errors.New("transport must be http or websocket")
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
func (s *OpenAIGatewayService) RoutingAvailability(ctx context.Context, key *APIKey, input RoutingAvailabilityRequest) RoutingAvailability {
	result := RoutingAvailability{Object: "routing.availability", Model: strings.TrimSpace(input.Model), State: "unknown", Reason: "evaluation_unavailable", CheckedAt: time.Now().Unix(), RetryAfterSeconds: 3}
	if input.Validate() != nil || key == nil || key.Group == nil || key.GroupID == nil || key.Group.ID != *key.GroupID {
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
	if input.RequireCompact {
		capability = OpenAIEndpointCapabilityResponsesCompact
	}
	evaluation := &routingAvailabilityEvaluation{cache: cache, rotationWaiting: make(map[int64]bool)}
	req := OpenAIAccountScheduleRequest{
		GroupID: key.GroupID, Platform: PlatformOpenAI, RequestedModel: model,
		HistoryModel: input.Model, InboundEndpoint: "/v1/responses", UseUpstreamTokenCost: true,
		RequirePrivacySet: key.Group.RequirePrivacySet, RequiredTransport: transport,
		RequiredCapability: capability, RequireCompact: input.RequireCompact,
		routingAvailability: evaluation,
	}
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

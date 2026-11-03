package service

import "context"

// Confirm waiting on the concurrency authority before applying a sticky soft
// escape. This does not grant eligibility: callers still enforce request,
// account-health and policy admission checks before reserving a real slot.
func (s *OpenAIGatewayService) openAIStickyRotationWaiting(ctx context.Context, accountID int64) bool {
	if s == nil || s.concurrencyService == nil || accountID <= 0 {
		return false
	}
	policy, err := s.concurrencyService.accountRotationPolicy(ctx)
	if err != nil || policy == nil {
		return false
	}
	state, err := s.concurrencyService.cache.(AccountRotationCache).CheckAccountRotation(ctx, accountID, *policy)
	return err == nil && state != nil && (state.Hold || state.Recovering)
}

func (s *OpenAIGatewayService) withAccountRotationSession(ctx context.Context, groupID *int64, sessionHash string) context.Context {
	if s == nil {
		return ctx
	}
	ctx = s.concurrencyService.AccountRotationContext(ctx)
	if !HasAccountRotationTracking(ctx) || sessionHash == "" || s.cache == nil {
		return ctx
	}
	id, err := s.getStickySessionAccountID(ctx, groupID, sessionHash)
	if err == nil && id > 0 {
		return WithAccountRotationSticky(ctx, id)
	}
	return ctx
}

package service

import "context"

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

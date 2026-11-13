package service

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
)

// The structured provider code controls quarantine. Free-form messages only
// select a bounded reason label; they never enter the persistent account state.
func (s *OpenAIGatewayService) handleExcelBPSRequestRejected(ctx context.Context, account *Account, raw []byte) bool {
	if !gjson.ValidBytes(raw) {
		return false
	}
	reason := ""
	for _, path := range []string{"error", "response.error", "detail", "@this"} {
		detail := gjson.GetBytes(raw, path)
		if detail.Get("code").String() != "request_rejected" {
			continue
		}
		reason = "upstream_rejected"
		switch detail.Get("message").String() {
		case "This request was blocked by our usage policy.":
			reason = "usage_policy"
		case "Invalid request body.":
			reason = "invalid_request"
		}
		break
	}
	if reason == "" {
		return false
	}
	if s == nil || account == nil || !BPSRequestRejectionWritesAllowed(ctx) {
		return true
	}
	repo, ok := s.accountRepo.(AccountBPSRequestRejectionRepository)
	if !ok {
		return true
	}
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	changed, err := repo.MarkBPSRequestRejected(stateCtx, account, reason)
	if err != nil {
		logger.LegacyPrintf("service.openai_excel_bps", "request rejection cooldown persistence failed: account_id=%d error_type=%T", account.ID, err)
	} else if changed {
		logger.LegacyPrintf("service.openai_excel_bps", "account paused until explicit state reset after request_rejected: account_id=%d reason=%s", account.ID, reason)
	}
	return true
}

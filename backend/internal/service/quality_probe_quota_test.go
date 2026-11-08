//go:build unit

package service

import (
	"testing"
	"time"
)

func TestQualityProbeQuotaExhaustedOpenAI(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name  string
		extra map[string]any
		want  bool
	}{
		{"missing snapshot", nil, false},
		{"5h below limit", map[string]any{"codex_5h_used_percent": 99.99, "codex_5h_reset_at": future}, false},
		{"percentage is not a ratio", map[string]any{"codex_5h_used_percent": 1.0, "codex_5h_reset_at": future}, false},
		{"5h at limit", map[string]any{"codex_5h_used_percent": 100.0, "codex_5h_reset_at": future}, true},
		{"7d at limit", map[string]any{"codex_7d_used_percent": 100.0, "codex_7d_reset_at": future}, true},
		{"7d above limit", map[string]any{"codex_7d_used_percent": 101.0, "codex_7d_reset_at": future}, true},
		{"5h reset at now", map[string]any{"codex_5h_used_percent": 100.0, "codex_5h_reset_at": now.Format(time.RFC3339)}, false},
		{"7d already reset", map[string]any{"codex_7d_used_percent": 100.0, "codex_7d_reset_at": now.Add(-time.Second).Format(time.RFC3339)}, false},
		{"one reset window does not release another", map[string]any{"codex_5h_used_percent": 100.0, "codex_5h_reset_at": now.Format(time.RFC3339), "codex_7d_used_percent": 100.0, "codex_7d_reset_at": future}, true},
		{"relative reset pending", map[string]any{"codex_5h_used_percent": "100", "codex_usage_updated_at": now.Add(-time.Hour).Format(time.RFC3339), "codex_5h_reset_after_seconds": 7200}, true},
		{"relative reset at now", map[string]any{"codex_5h_used_percent": 100.0, "codex_usage_updated_at": now.Add(-time.Hour).Format(time.RFC3339), "codex_5h_reset_after_seconds": 3600}, false},
		{"absolute reset takes precedence", map[string]any{"codex_5h_used_percent": 100.0, "codex_5h_reset_at": now.Format(time.RFC3339), "codex_usage_updated_at": now.Format(time.RFC3339), "codex_5h_reset_after_seconds": 3600}, false},
		{"fresh snapshot without reset", map[string]any{"codex_5h_used_percent": 100.0, "codex_usage_updated_at": now.Add(-time.Hour).Format(time.RFC3339)}, true},
		{"stale boundary without reset", map[string]any{"codex_5h_used_percent": 100.0, "codex_usage_updated_at": now.Add(-2 * time.Hour).Format(time.RFC3339)}, false},
		{"stale snapshot with future reset", map[string]any{"codex_7d_used_percent": 100.0, "codex_7d_reset_at": future, "codex_usage_updated_at": now.Add(-3 * time.Hour).Format(time.RFC3339)}, true},
		{"missing sample time retains existing semantics", map[string]any{"codex_5h_used_percent": 100.0}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: tc.extra}
			if got := qualityProbeQuotaExhausted(account, now); got != tc.want {
				t.Fatalf("qualityProbeQuotaExhausted() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestQualityProbeQuotaExhaustedAccountBudget(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name        string
		accountType string
		extra       map[string]any
		want        bool
	}{
		{"total at limit", AccountTypeAPIKey, map[string]any{"quota_limit": 10.0, "quota_used": 10.0}, true},
		{"total below limit", AccountTypeAPIKey, map[string]any{"quota_limit": 10.0, "quota_used": 9.99}, false},
		{"unlimited total", AccountTypeAPIKey, map[string]any{"quota_limit": 0.0, "quota_used": 10.0}, false},
		{"daily at limit", AccountTypeAPIKey, map[string]any{"quota_daily_limit": 10.0, "quota_daily_used": 10.0, "quota_daily_start": now.Add(-time.Hour).Format(time.RFC3339)}, true},
		{"daily expired", AccountTypeAPIKey, map[string]any{"quota_daily_limit": 10.0, "quota_daily_used": 10.0, "quota_daily_start": now.Add(-25 * time.Hour).Format(time.RFC3339)}, false},
		{"weekly at limit", AccountTypeAPIKey, map[string]any{"quota_weekly_limit": 10.0, "quota_weekly_used": 10.0, "quota_weekly_start": now.Add(-time.Hour).Format(time.RFC3339)}, true},
		{"weekly expired", AccountTypeAPIKey, map[string]any{"quota_weekly_limit": 10.0, "quota_weekly_used": 10.0, "quota_weekly_start": now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)}, false},
		{"daily missing start", AccountTypeAPIKey, map[string]any{"quota_daily_limit": 10.0, "quota_daily_used": 10.0}, false},
		{"bedrock budget", AccountTypeBedrock, map[string]any{"quota_limit": 10.0, "quota_used": 10.0}, true},
		{"service account budget", AccountTypeServiceAccount, map[string]any{"quota_limit": 10.0, "quota_used": 10.0}, true},
		{"oauth ignores local budget fields", AccountTypeOAuth, map[string]any{"quota_limit": 10.0, "quota_used": 10.0}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{Platform: PlatformOpenAI, Type: tc.accountType, Extra: tc.extra}
			if got := qualityProbeQuotaExhausted(account, now); got != tc.want {
				t.Fatalf("qualityProbeQuotaExhausted() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestQualityProbeQuotaExhaustedIgnoresUnrelatedState(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	for _, tc := range []struct {
		name    string
		account *Account
	}{
		{"nil account", nil},
		{"disabled scheduling", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Schedulable: false}},
		{"quality isolation", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{QualityModelIsolationKey: map[string]any{"gpt-test": map[string]any{}}}}},
		{"temporary cooldown", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, TempUnschedulableUntil: &future}},
		{"429 cooldown alone", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, RateLimitResetAt: &future}},
		{"api key legacy codex snapshot", &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, RateLimitResetAt: &future, Extra: map[string]any{"codex_5h_used_percent": 100.0, "codex_7d_used_percent": 100.0, "codex_5h_reset_at": future.Format(time.RFC3339), "codex_7d_reset_at": future.Format(time.RFC3339)}}},
		{"mismatched oauth snapshot", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "account-a"}, Extra: map[string]any{"chatgpt_account_id": "account-b", "codex_5h_used_percent": 100.0, "codex_5h_reset_at": future.Format(time.RFC3339)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if qualityProbeQuotaExhausted(tc.account, now) {
				t.Fatal("unrelated account state must not establish quota exhaustion")
			}
		})
	}
}

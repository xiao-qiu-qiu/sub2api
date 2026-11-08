package service

import "time"

// qualityProbeQuotaExhausted checks quota signals without consulting scheduling
// or quality isolation state. RateLimitResetAt also carries generic 429 backoff,
// so it cannot establish quota exhaustion on its own.
func qualityProbeQuotaExhausted(account *Account, now time.Time) bool {
	if account == nil {
		return false
	}
	if account.SupportsAccountQuota() && account.IsQuotaExceeded() {
		return true
	}
	if !account.IsOpenAIOAuth() || !openAICodexSnapshotIdentityTrusted(account) {
		return false
	}
	for _, window := range []string{"5h", "7d"} {
		if utilization, ok := resolveOpenAIQuotaUtilization(account.Extra, window, now); ok && utilization >= 1 {
			return true
		}
	}
	return false
}

package service

import "time"

// A known exhausted OAuth window is unavailable until its observed reset,
// independently of the optional thresholds used to pause an account early.
// Managed projections inherit aggregate usage, not an individual grant's quota.
func openAIExhaustedQuotaWindow(account *Account, now time.Time) (window string, resetAt time.Time, exhausted bool) {
	if account == nil || !account.IsOpenAIOAuth() || !openAICodexSnapshotIdentityTrusted(account) ||
		account.HasManagedOpenAIConnections() {
		return "", time.Time{}, false
	}
	for _, candidate := range []string{"5h", "7d"} {
		used, source, ok := extractOpenAICodexUsageWindow(account.Extra, candidate)
		if !ok || used < 100 {
			continue
		}
		reset, ok := openAICodexWindowResetAt(account.Extra, source)
		if !ok || !reset.After(now) || !reset.After(resetAt) {
			continue
		}
		window, resetAt, exhausted = candidate, reset, true
	}
	return window, resetAt, exhausted
}

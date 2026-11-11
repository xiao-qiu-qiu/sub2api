package service

import (
	"context"
	"fmt"
	"time"
)

// QualityProbeAdmissionKey is a server-owned snapshot shared by all schedulers.
const QualityProbeAdmissionKey = "quality_probe_admission"

// QualityProbeAdmissionPending requires a completed, conclusive probe that
// started after the latest release. Merely finishing an old in-flight probe
// must not admit an account into a new quota/cooldown generation.
func (a *Account) QualityProbeAdmissionPending(now time.Time) bool {
	return a.QualityProbeAdmissionPendingWithContext(context.Background(), now)
}

// The context carries the same global quota defaults used by account selection.
func (a *Account) QualityProbeAdmissionPendingWithContext(ctx context.Context, now time.Time) bool {
	if a == nil {
		return false
	}
	state, _ := a.Extra[QualityProbeAdmissionKey].(map[string]any)
	if enabled, _ := state["enabled"].(bool); !enabled {
		return false
	}
	required := parseExtraTime(state["required_after"])
	advance := func(at time.Time) {
		if at.After(required) {
			required = at
		}
	}
	for _, until := range []*time.Time{a.RateLimitResetAt, a.TempUnschedulableUntil, a.OverloadUntil} {
		if until != nil {
			advance(*until)
		}
	}
	// Quota snapshots can expire without an account write or a 429. Keep the
	// elapsed boundary until a newer probe supplies evidence for this window.
	advance(openAIQuotaAutoPauseRecoveryBoundary(ctx, a, now))
	if a.SupportsAccountQuota() {
		for _, window := range []struct {
			name        string
			limit, used float64
			mode        string
			duration    time.Duration
		}{
			{"daily", a.GetQuotaDailyLimit(), a.GetQuotaDailyUsed(), a.GetQuotaDailyResetMode(), 24 * time.Hour},
			{"weekly", a.GetQuotaWeeklyLimit(), a.GetQuotaWeeklyUsed(), a.GetQuotaWeeklyResetMode(), 7 * 24 * time.Hour},
		} {
			if window.limit <= 0 || window.used < window.limit {
				continue
			}
			start := a.getExtraTime("quota_" + window.name + "_start")
			if start.IsZero() {
				continue
			}
			reset := start.Add(window.duration)
			if window.mode == "fixed" {
				tz, err := time.LoadLocation(a.GetQuotaResetTimezone())
				if err != nil {
					tz = time.UTC
				}
				reset = nextFixedDailyReset(a.GetQuotaDailyResetHour(), tz, start)
				if window.name == "weekly" {
					reset = nextFixedWeeklyReset(a.GetQuotaWeeklyResetDay(), a.GetQuotaWeeklyResetHour(), tz, start)
				}
			}
			advance(reset)
		}
	}
	if required.IsZero() {
		return false
	}
	started := parseExtraTime(state["checked_started_at"])
	finished := parseExtraTime(state["checked_finished_at"])
	return started.IsZero() || started.Before(required) || finished.Before(started) || finished.After(now)
}

// Retained snapshots establish a recovery boundary only for exhausted windows
// or windows that the configured scheduler would actually have paused. A
// below-threshold/disabled/credit-exempt window is not a recovery generation.
func openAIQuotaAutoPauseRecoveryBoundary(ctx context.Context, account *Account, now time.Time) time.Time {
	if account == nil || !account.IsOpenAIOAuth() || !openAICodexSnapshotIdentityTrusted(account) {
		return time.Time{}
	}
	var boundary time.Time
	for _, window := range []string{"5h", "7d"} {
		used, source, ok := extractOpenAICodexUsageWindow(account.Extra, window)
		if !ok || used <= 0 {
			continue
		}
		releasedAt, hasReset := openAICodexWindowResetAt(account.Extra, source)
		if !hasReset {
			observed, err := parseTime(fmt.Sprint(account.Extra["codex_usage_updated_at"]))
			if err != nil {
				continue
			}
			releasedAt = observed.Add(openAICodexAutoPauseStaleAfter)
		}
		// The active window belongs to the native quota gate, including its
		// reset-credit notifications. Only an elapsed release needs a new probe.
		if now.Before(releasedAt) {
			continue
		}
		// Test immediately before the release; the normal usage reader at now
		// intentionally discards both expired windows and stale snapshots.
		evaluatedAt := releasedAt.Add(-time.Nanosecond)
		five, seven := 0.0, 0.0
		if window == "5h" {
			five = used / 100
		} else {
			seven = used / 100
		}
		paused, _ := evaluateOpenAIQuotaAutoPause(ctx, account, evaluatedAt, five, window == "5h", seven, window == "7d")
		if (used >= 100 || paused) && releasedAt.After(boundary) {
			boundary = releasedAt
		}
	}
	return boundary
}

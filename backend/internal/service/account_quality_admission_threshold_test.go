package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func qualityThresholdAccount(now time.Time, window string, used float64, reset *time.Time) *Account {
	extra := map[string]any{
		QualityProbeAdmissionKey: map[string]any{
			"enabled":             true,
			"required_after":      now.Add(-6 * time.Hour).Format(time.RFC3339Nano),
			"checked_started_at":  now.Add(-5 * time.Hour).Format(time.RFC3339Nano),
			"checked_finished_at": now.Add(-4 * time.Hour).Format(time.RFC3339Nano),
		},
		"codex_" + window + "_used_percent": used,
		"codex_usage_updated_at":            now.Add(-time.Hour).Format(time.RFC3339Nano),
	}
	if reset != nil {
		extra["codex_"+window+"_reset_at"] = reset.Format(time.RFC3339Nano)
	}
	return &Account{ID: 7319101, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Extra: extra}
}

func qualityThresholdContext(five, seven float64) context.Context {
	return withOpenAIQuotaAutoPauseSettings(context.Background(), OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold5h: five, DefaultThreshold7d: seven})
}

func qualityThresholdChecked(account *Account, started, finished time.Time) {
	state := qualityAdmissionTestState(account)
	state["checked_started_at"] = started.Format(time.RFC3339Nano)
	state["checked_finished_at"] = finished.Format(time.RFC3339Nano)
}

func TestQualityAdmissionThresholdResetPolicy(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(-time.Minute)
	for _, window := range []string{"5h", "7d"} {
		t.Run(window, func(t *testing.T) {
			for _, tc := range []struct {
				name         string
				global, used float64
				patch        map[string]any
				want         bool
			}{
				{"global threshold", .9, 95, nil, true},
				{"inclusive threshold", .9, 90, nil, true},
				{"below threshold", .9, 89.9, nil, false},
				{"no configured pause", 0, 95, nil, false},
				{"higher account override", .9, 95, map[string]any{"auto_pause_" + window + "_threshold": .99}, false},
				{"lower account override", .99, 95, map[string]any{"auto_pause_" + window + "_threshold": .8}, true},
				{"zero override inherits", .9, 95, map[string]any{"auto_pause_" + window + "_threshold": 0}, true},
				{"string account override", .99, 95, map[string]any{"auto_pause_" + window + "_threshold": "0.8"}, true},
				{"explicit disable", .9, 95, map[string]any{"auto_pause_" + window + "_disabled": true}, false},
				{"string disable", .9, 95, map[string]any{"auto_pause_" + window + "_disabled": "true"}, false},
				{"numeric disable", .9, 95, map[string]any{"auto_pause_" + window + "_disabled": 1}, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					account := qualityThresholdAccount(now, window, tc.used, &reset)
					for key, value := range tc.patch {
						account.Extra[key] = value
					}
					ctx := qualityThresholdContext(tc.global, tc.global)
					require.Equal(t, tc.want, account.QualityProbeAdmissionPendingWithContext(ctx, now))
					if tc.want {
						qualityThresholdChecked(account, reset.Add(-time.Nanosecond), now)
						require.True(t, account.QualityProbeAdmissionPendingWithContext(ctx, now))
						qualityThresholdChecked(account, reset, now)
						require.False(t, account.QualityProbeAdmissionPendingWithContext(ctx, now))
					}
				})
			}
		})
	}
}

func TestQualityAdmissionThresholdStaleRelease(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, window := range []string{"5h", "7d"} {
		t.Run(window, func(t *testing.T) {
			boundary := now.Add(-time.Minute)
			account := qualityThresholdAccount(now, window, 95, nil)
			account.Extra["codex_usage_updated_at"] = boundary.Add(-openAICodexAutoPauseStaleAfter).Format(time.RFC3339Nano)
			ctx := qualityThresholdContext(.9, .9)
			_, before := resolveOpenAIQuotaUtilization(account.Extra, window, boundary.Add(-time.Nanosecond))
			_, at := resolveOpenAIQuotaUtilization(account.Extra, window, boundary)
			require.True(t, before)
			require.False(t, at)
			require.False(t, account.QualityProbeAdmissionPendingWithContext(ctx, boundary.Add(-time.Nanosecond)), "active snapshots stay owned by native quota policy")
			require.True(t, account.QualityProbeAdmissionPendingWithContext(ctx, boundary), "staleness cannot reopen traffic on old probe evidence")
			qualityThresholdChecked(account, boundary.Add(-time.Nanosecond), now)
			require.True(t, account.QualityProbeAdmissionPendingWithContext(ctx, now))
			qualityThresholdChecked(account, boundary, now)
			require.False(t, account.QualityProbeAdmissionPendingWithContext(ctx, now))
			account.Extra["auto_pause_"+window+"_disabled"] = true
			qualityThresholdChecked(account, now.Add(-5*time.Hour), now.Add(-4*time.Hour))
			require.False(t, account.QualityProbeAdmissionPendingWithContext(ctx, now))
			delete(account.Extra, "auto_pause_"+window+"_disabled")
			require.False(t, account.QualityProbeAdmissionPendingWithContext(qualityThresholdContext(.99, .99), now))
			require.False(t, account.QualityProbeAdmissionPendingWithContext(context.Background(), now))
		})
	}
}

func TestQualityAdmissionThresholdActiveWindowAndTimestampRules(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	ctx := qualityThresholdContext(.9, .9)
	future, past := now.Add(time.Hour), now.Add(-time.Minute)
	for _, used := range []float64{95, 100} {
		account := qualityThresholdAccount(now, "5h", used, &future)
		account.Extra["codex_usage_updated_at"] = now.Add(-3 * time.Hour).Format(time.RFC3339Nano)
		require.False(t, account.QualityProbeAdmissionPendingWithContext(ctx, now), "a future reset must not become a new admission gate")
		_, active := resolveOpenAIQuotaUtilization(account.Extra, "5h", now)
		require.True(t, active, "a future reset takes precedence over stale-after")
	}
	for _, observed := range []any{nil, "bad timestamp"} {
		account := qualityThresholdAccount(now, "5h", 95, nil)
		if observed == nil {
			delete(account.Extra, "codex_usage_updated_at")
		} else {
			account.Extra["codex_usage_updated_at"] = observed
		}
		require.False(t, account.QualityProbeAdmissionPendingWithContext(ctx, now), "unknown observation time cannot invent a release")
	}
	account := qualityThresholdAccount(now, "5h", 95, &past)
	account.Extra["codex_usage_updated_at"] = now.Format(time.RFC3339Nano)
	require.True(t, account.QualityProbeAdmissionPendingWithContext(ctx, now), "an explicit reset need not wait for snapshot staleness")
	account.Extra["codex_5h_window_minutes"] = 1440
	require.False(t, account.QualityProbeAdmissionPendingWithContext(ctx, now), "an image quota is not an account window")
	account = qualityThresholdAccount(now, "5h", 95, &past)
	account.Credentials = map[string]any{"chatgpt_account_id": "new-identity"}
	account.Extra["chatgpt_account_id"] = "old-identity"
	require.False(t, account.QualityProbeAdmissionPendingWithContext(ctx, now))
	account = qualityThresholdAccount(now, "5h", 95, &past)
	account.Type = AccountTypeAPIKey
	require.False(t, account.QualityProbeAdmissionPendingWithContext(ctx, now))
}

func TestQualityAdmissionThresholdRawWindowAndLatestBoundary(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	older, latest := now.Add(-2*time.Minute), now.Add(-time.Minute)
	account := qualityThresholdAccount(now, "5h", 95, &older)
	account.Extra["codex_7d_used_percent"] = 95.0
	account.Extra["codex_7d_reset_at"] = latest.Format(time.RFC3339Nano)
	ctx := qualityThresholdContext(.9, .9)
	qualityThresholdChecked(account, older, now)
	require.True(t, account.QualityProbeAdmissionPendingWithContext(ctx, now))
	qualityThresholdChecked(account, latest, now)
	require.False(t, account.QualityProbeAdmissionPendingWithContext(ctx, now))
	account.Extra["auto_pause_7d_disabled"] = true
	qualityThresholdChecked(account, older, now)
	require.False(t, account.QualityProbeAdmissionPendingWithContext(ctx, now), "a disabled second window cannot advance the boundary")

	account = qualityThresholdAccount(now, "5h", 95, nil)
	delete(account.Extra, "codex_5h_used_percent")
	account.Extra["codex_primary_used_percent"] = 95.0
	account.Extra["codex_primary_window_minutes"] = 10080
	account.Extra["codex_primary_reset_at"] = latest.Format(time.RFC3339Nano)
	require.True(t, account.QualityProbeAdmissionPendingWithContext(qualityThresholdContext(0, .9), now), "raw reset must follow the classified source")
	qualityThresholdChecked(account, latest, now)
	require.False(t, account.QualityProbeAdmissionPendingWithContext(qualityThresholdContext(0, .9), now))
}

func TestQualityAdmissionThresholdResetCreditExemptions(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(-10 * time.Second)
	for _, tc := range []struct {
		name                      string
		consume                   float64
		available, disabled, want bool
	}{
		{"fresh credit exempts ordinary pause", 1, true, false, false},
		{"unknown credit pauses", 1, false, false, true},
		{"consumption threshold still pauses", .9, true, false, true},
		{"ignored credit window retains ordinary pause", 0, true, false, true},
		{"disabled ordinary window", 1, false, true, false},
		{"consume gate independent of ordinary disable", .9, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := qualityThresholdAccount(now, "5h", 95, &reset)
			account.Extra[OpenAIAutoResetCreditEnabledExtraKey] = true
			account.Extra[OpenAIAutoResetCredit5hThresholdExtraKey] = tc.consume
			account.Extra["auto_pause_5h_disabled"] = tc.disabled
			if tc.available {
				account.Extra[OpenAIAutoResetCreditStateExtraKey] = OpenAIAutoResetCreditState{Status: OpenAIAutoResetStatusAvailable, AvailableCount: 1, CheckedAt: now.Add(-time.Minute).Format(time.RFC3339)}
			}
			require.Equal(t, tc.want, account.QualityProbeAdmissionPendingWithContext(qualityThresholdContext(.9, .9), now))
		})
	}
}

func TestQualityAdmissionThresholdRetainsAutoResetNotifier(t *testing.T) {
	for _, tc := range []struct {
		name    string
		consume float64
		reason  string
	}{
		{"consume threshold", .9, "quota_auto_reset_pending_5h"},
		{"unknown credit at ordinary threshold", 1, "quota_auto_reset_credit_check_5h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			reset := now.Add(time.Hour)
			account := qualityThresholdAccount(now, "5h", 95, &reset)
			account.ID = 7319199
			account.Extra[OpenAIAutoResetCreditEnabledExtraKey] = true
			account.Extra[OpenAIAutoResetCredit5hThresholdExtraKey] = tc.consume
			service := &OpenAIQuotaAutoResetService{ctx: context.Background(), queue: make(chan int64, 8)}
			setOpenAIAutoResetNotifier(service)
			t.Cleanup(func() { clearOpenAIAutoResetNotifier(service); openAIAutoResetSchedulerNotifiedAt.Delete(account.ID) })
			ctx := qualityThresholdContext(.8, .9)
			require.True(t, account.IsSchedulableWithContext(ctx), "admission must let the active quota window reach its native notifier")
			paused, decision := shouldAutoPauseOpenAIAccountByQuota(ctx, account)
			require.True(t, paused)
			require.Equal(t, tc.reason, decision.reason)
			select {
			case notified := <-service.queue:
				require.Equal(t, account.ID, notified)
			default:
				t.Fatal("the quality admission gate swallowed the auto-reset notification")
			}
		})
	}
}

func TestQualityAdmissionThresholdSchedulerGate(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(-time.Minute)
	for _, mode := range []string{"reset", "stale", "ordinary account", "fresh evidence"} {
		t.Run(mode, func(t *testing.T) {
			account := qualityThresholdAccount(now, "5h", 95, &reset)
			wantPending := true
			switch mode {
			case "stale":
				delete(account.Extra, "codex_5h_reset_at")
				account.Extra["codex_usage_updated_at"] = now.Add(-openAICodexAutoPauseStaleAfter - time.Minute).Format(time.RFC3339Nano)
			case "ordinary account":
				delete(account.Extra, QualityProbeAdmissionKey)
				wantPending = false
			case "fresh evidence":
				qualityThresholdChecked(account, reset, now)
				wantPending = false
			}
			ctx := qualityThresholdContext(.9, .9)
			paused, decision := shouldAutoPauseOpenAIAccountByQuota(ctx, account)
			require.Equal(t, wantPending, paused)
			reason := openAICompatibleAccountEligibilityFailureReasonBeforeProfit(ctx, account, nil, PlatformOpenAI, "", false, "")
			if wantPending {
				require.Equal(t, "quality_probe_admission_pending", decision.reason)
				require.Equal(t, "not_schedulable", reason)
			} else {
				require.Empty(t, reason)
			}
		})
	}
}

type qualityThresholdAdmissionReader struct {
	AccountRepository
	account *Account
}

func (r *qualityThresholdAdmissionReader) GetOpenAITurnAdmission(context.Context, int64) (*Account, *Account, error) {
	return r.account, nil, nil
}

func TestQualityAdmissionThresholdPreSendUsesGlobalDefaults(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(-time.Minute)
	for _, mode := range []string{"reset", "stale", "higher override", "disabled", "below threshold", "probe", "fresh evidence", "no quality rule"} {
		t.Run(mode, func(t *testing.T) {
			account := qualityThresholdAccount(now, "5h", 95, &reset)
			ctx := context.Background()
			wantAllowed := false
			switch mode {
			case "stale":
				delete(account.Extra, "codex_5h_reset_at")
				account.Extra["codex_usage_updated_at"] = now.Add(-openAICodexAutoPauseStaleAfter - time.Minute).Format(time.RFC3339Nano)
			case "higher override":
				account.Extra["auto_pause_5h_threshold"], wantAllowed = .99, true
			case "disabled":
				account.Extra["auto_pause_5h_disabled"], wantAllowed = true, true
			case "below threshold":
				account.Extra["codex_5h_used_percent"], wantAllowed = 89.0, true
			case "probe":
				ctx, wantAllowed = context.WithValue(ctx, qualityProbeContextKey{}, true), true
			case "fresh evidence":
				qualityThresholdChecked(account, reset, now)
				wantAllowed = true
			case "no quality rule":
				delete(account.Extra, QualityProbeAdmissionKey)
				wantAllowed = true
			}
			settings := &SettingService{}
			settings.SetOpenAIQuotaAutoPauseSettings(OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold5h: .9})
			gateway := &OpenAIGatewayService{accountRepo: &qualityThresholdAdmissionReader{account: account}, settingService: settings}
			latest, err := gateway.latestOpenAITurnAccountForGroup(ctx, account, 0, false)
			if wantAllowed {
				require.NoError(t, err)
				require.NotNil(t, latest)
			} else {
				require.True(t, IsOpenAITurnAdmissionError(err))
				require.Nil(t, latest)
			}
		})
	}
}

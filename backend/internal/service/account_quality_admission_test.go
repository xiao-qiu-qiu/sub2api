package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func qualityAdmissionTestAccount(required time.Time) *Account {
	state := map[string]any{"enabled": true}
	if !required.IsZero() {
		state["required_after"] = required.Format(time.RFC3339Nano)
	}
	return &Account{
		ID: 42001, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"chatgpt_account_id": "quality-admission-account"},
		Extra:       map[string]any{QualityProbeAdmissionKey: state},
	}
}

func qualityAdmissionTestState(account *Account) map[string]any {
	state, ok := account.Extra[QualityProbeAdmissionKey].(map[string]any)
	if !ok || state == nil {
		panic("quality admission test fixture must contain an admission state map")
	}
	return state
}

func qualityAdmissionTestChecked(account *Account, started, finished time.Time) {
	state := qualityAdmissionTestState(account)
	state["checked_started_at"] = started.Format(time.RFC3339Nano)
	state["checked_finished_at"] = finished.Format(time.RFC3339Nano)
}

func TestQualityProbeAdmissionPendingEvidence(t *testing.T) {
	require.Equal(t, "quality_probe_admission", QualityProbeAdmissionKey)
	now := time.Date(2026, 11, 12, 12, 0, 0, 123456789, time.UTC)
	boundary := now.Add(-time.Minute)
	for _, tc := range []struct {
		name   string
		mutate func(*Account)
		want   bool
	}{
		{"no admission configuration", func(a *Account) { a.Extra = nil }, false},
		{"empty admission configuration", func(a *Account) { a.Extra[QualityProbeAdmissionKey] = map[string]any{} }, false},
		{"explicitly disabled", func(a *Account) { qualityAdmissionTestState(a)["enabled"] = false }, false},
		{"enabled without any recovery boundary", func(a *Account) { delete(qualityAdmissionTestState(a), "required_after") }, false},
		{"required but never checked", func(*Account) {}, true},
		{"completed before recovery", func(a *Account) {
			qualityAdmissionTestChecked(a, boundary.Add(-time.Minute), boundary.Add(-time.Second))
		}, true},
		{"started before recovery and finished after recovery", func(a *Account) {
			qualityAdmissionTestChecked(a, boundary.Add(-time.Nanosecond), boundary.Add(time.Second))
		}, true},
		{"starts exactly at recovery", func(a *Account) { qualityAdmissionTestChecked(a, boundary, boundary.Add(time.Second)) }, false},
		{"finishes exactly now", func(a *Account) { qualityAdmissionTestChecked(a, boundary, now) }, false},
		{"finish precedes start", func(a *Account) { qualityAdmissionTestChecked(a, boundary.Add(time.Second), boundary) }, true},
		{"finish is in the future", func(a *Account) { qualityAdmissionTestChecked(a, boundary, now.Add(time.Nanosecond)) }, true},
		{"missing start", func(a *Account) {
			qualityAdmissionTestState(a)["checked_finished_at"] = now.Format(time.RFC3339Nano)
		}, true},
		{"missing finish", func(a *Account) {
			qualityAdmissionTestState(a)["checked_started_at"] = boundary.Format(time.RFC3339Nano)
		}, true},
		{"invalid start", func(a *Account) {
			qualityAdmissionTestChecked(a, boundary, now)
			qualityAdmissionTestState(a)["checked_started_at"] = "invalid"
		}, true},
		{"invalid finish", func(a *Account) {
			qualityAdmissionTestChecked(a, boundary, now)
			qualityAdmissionTestState(a)["checked_finished_at"] = "invalid"
		}, true},
		{"new recovery invalidates previous check", func(a *Account) {
			qualityAdmissionTestChecked(a, boundary, boundary.Add(time.Second))
			qualityAdmissionTestState(a)["required_after"] = boundary.Add(2 * time.Second).Format(time.RFC3339Nano)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := qualityAdmissionTestAccount(boundary)
			tc.mutate(account)
			require.Equal(t, tc.want, account.QualityProbeAdmissionPending(now))
		})
	}
}

func TestQualityProbeAdmissionPendingCooldownRecoveryBoundaries(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	boundary := now.Add(-time.Minute)
	for _, cooldown := range []struct {
		name string
		set  func(*Account, time.Time)
	}{
		{"rate limit", func(a *Account, at time.Time) { a.RateLimitResetAt = &at }},
		{"overload", func(a *Account, at time.Time) { a.OverloadUntil = &at }},
		{"temporary unschedulability", func(a *Account, at time.Time) { a.TempUnschedulableUntil = &at }},
	} {
		t.Run(cooldown.name, func(t *testing.T) {
			account := qualityAdmissionTestAccount(time.Time{})
			cooldown.set(account, boundary)
			require.True(t, account.QualityProbeAdmissionPending(now), "expiry must not automatically admit business traffic")
			qualityAdmissionTestChecked(account, boundary.Add(-time.Nanosecond), boundary.Add(time.Second))
			require.True(t, account.QualityProbeAdmissionPending(now), "a probe already running before recovery is not evidence")
			qualityAdmissionTestChecked(account, boundary, now)
			require.False(t, account.QualityProbeAdmissionPending(now))
		})
	}
	t.Run("latest boundary wins", func(t *testing.T) {
		account := qualityAdmissionTestAccount(boundary.Add(-time.Minute))
		older, latest := boundary.Add(-time.Second), boundary.Add(time.Second)
		account.RateLimitResetAt, account.OverloadUntil, account.TempUnschedulableUntil = &boundary, &older, &latest
		qualityAdmissionTestChecked(account, boundary, now)
		require.True(t, account.QualityProbeAdmissionPending(now))
		qualityAdmissionTestChecked(account, latest, now)
		require.False(t, account.QualityProbeAdmissionPending(now))
	})
}

func TestQualityProbeAdmissionPendingNaturalQuotaReset(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for _, window := range []string{"5h", "7d"} {
		t.Run(window, func(t *testing.T) {
			for _, relative := range []bool{false, true} {
				name := "absolute"
				if relative {
					name = "relative"
				}
				t.Run(name, func(t *testing.T) {
					reset := now.Add(-time.Minute)
					account := qualityAdmissionTestAccount(time.Time{})
					account.Extra["chatgpt_account_id"] = "quality-admission-account"
					account.Extra["codex_"+window+"_used_percent"] = 100.0
					account.Extra["codex_usage_updated_at"] = reset.Add(-time.Hour).Format(time.RFC3339Nano)
					if relative {
						account.Extra["codex_"+window+"_reset_after_seconds"] = 3600
					} else {
						account.Extra["codex_"+window+"_reset_at"] = reset.Format(time.RFC3339Nano)
					}
					require.True(t, account.QualityProbeAdmissionPending(now), "natural quota expiry must establish a recovery boundary")
					qualityAdmissionTestChecked(account, reset.Add(-time.Nanosecond), now)
					require.True(t, account.QualityProbeAdmissionPending(now))
					qualityAdmissionTestChecked(account, reset, now)
					require.False(t, account.QualityProbeAdmissionPending(now))
				})
			}
		})
	}
	t.Run("latest quota window wins", func(t *testing.T) {
		account := qualityAdmissionTestAccount(time.Time{})
		older, latest := now.Add(-2*time.Minute), now.Add(-time.Minute)
		account.Extra["chatgpt_account_id"] = "quality-admission-account"
		account.Extra["codex_5h_used_percent"], account.Extra["codex_7d_used_percent"] = 100.0, 100.0
		account.Extra["codex_5h_reset_at"], account.Extra["codex_7d_reset_at"] = older.Format(time.RFC3339Nano), latest.Format(time.RFC3339Nano)
		qualityAdmissionTestChecked(account, older, now)
		require.True(t, account.QualityProbeAdmissionPending(now))
		qualityAdmissionTestChecked(account, latest, now)
		require.False(t, account.QualityProbeAdmissionPending(now))
	})
	t.Run("untrusted or unexhausted snapshots do not require a check", func(t *testing.T) {
		for _, mode := range []string{"identity mismatch", "below limit", "api key"} {
			t.Run(mode, func(t *testing.T) {
				account := qualityAdmissionTestAccount(time.Time{})
				account.Extra["chatgpt_account_id"] = "quality-admission-account"
				account.Extra["codex_7d_used_percent"] = 100.0
				account.Extra["codex_7d_reset_at"] = now.Add(-time.Minute).Format(time.RFC3339Nano)
				switch mode {
				case "identity mismatch":
					account.Extra["chatgpt_account_id"] = "another-account"
				case "below limit":
					account.Extra["codex_7d_used_percent"] = 99.0
				case "api key":
					account.Type = AccountTypeAPIKey
				}
				require.False(t, account.QualityProbeAdmissionPending(now))
			})
		}
	})
}

func TestQualityProbeAdmissionPendingAccountBudgetReset(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, window, mode string
		start, reset       time.Time
	}{
		{"daily rolling", "daily", "rolling", now.Add(-25 * time.Hour), now.Add(-time.Hour)},
		{"weekly rolling", "weekly", "rolling", now.Add(-169 * time.Hour), now.Add(-time.Hour)},
		{"daily fixed", "daily", "fixed", now.Add(-25 * time.Hour), time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)},
		{"weekly fixed", "weekly", "fixed", now.Add(-8 * 24 * time.Hour), time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := qualityAdmissionTestAccount(time.Time{})
			account.Type = AccountTypeAPIKey
			account.Extra["quota_"+tc.window+"_limit"] = 10.0
			account.Extra["quota_"+tc.window+"_used"] = 10.0
			account.Extra["quota_"+tc.window+"_start"] = tc.start.Format(time.RFC3339Nano)
			account.Extra["quota_"+tc.window+"_reset_mode"] = tc.mode
			account.Extra["quota_"+tc.window+"_reset_hour"] = 6
			account.Extra["quota_weekly_reset_day"] = 1
			account.Extra["quota_reset_timezone"] = "UTC"
			require.True(t, account.QualityProbeAdmissionPending(now), "budget expiry must not bypass admission")
			qualityAdmissionTestChecked(account, tc.reset.Add(-time.Nanosecond), now)
			require.True(t, account.QualityProbeAdmissionPending(now))
			qualityAdmissionTestChecked(account, tc.reset, now)
			require.False(t, account.QualityProbeAdmissionPending(now))
			delete(qualityAdmissionTestState(account), "checked_started_at")
			account.Extra["quota_"+tc.window+"_used"] = 9.0
			require.False(t, account.QualityProbeAdmissionPending(now), "unexhausted budgets must not establish a reset boundary")
		})
	}
}

func TestQualityProbeAdmissionSchedulingContext(t *testing.T) {
	now := time.Now()
	account := qualityAdmissionTestAccount(now.Add(-time.Minute))
	probe := context.WithValue(context.Background(), qualityProbeContextKey{}, true)
	require.False(t, account.IsSchedulable())
	require.False(t, account.IsSchedulableWithContext(context.Background()))
	require.False(t, account.IsSchedulableWithContext(context.WithValue(context.Background(), qualityProbeContextKey{}, false)))
	require.False(t, account.IsSchedulableForModelWithContext(context.Background(), "gpt-6-astra"))
	require.True(t, account.IsSchedulableWithContext(probe))
	require.True(t, account.IsSchedulableForModelWithContext(probe, "gpt-6-astra"))
	qualityAdmissionTestChecked(account, now.Add(-30*time.Second), now.Add(-time.Second))
	require.True(t, account.IsSchedulable())
	require.True(t, account.IsSchedulableForModelWithContext(context.Background(), "gpt-6-astra"))
}

func TestQualityProbeAdmissionContextPreservesIndependentLimits(t *testing.T) {
	future, past := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	probe := context.WithValue(context.Background(), qualityProbeContextKey{}, true)
	for _, tc := range []struct {
		name   string
		mutate func(*Account)
	}{
		{"manual disable", func(a *Account) { a.Schedulable = false }},
		{"error status", func(a *Account) { a.Status = StatusError }},
		{"performance pause", func(a *Account) { a.PerformancePaused = true }},
		{"expired account", func(a *Account) { a.AutoPauseOnExpired, a.ExpiresAt = true, &past }},
		{"rate limit", func(a *Account) { a.RateLimitResetAt = &future }},
		{"overload", func(a *Account) { a.OverloadUntil = &future }},
		{"temporary unschedulability", func(a *Account) { a.TempUnschedulableUntil = &future }},
		{"account budget", func(a *Account) {
			a.Type = AccountTypeAPIKey
			a.Extra["quota_limit"], a.Extra["quota_used"] = 10.0, 10.0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := qualityAdmissionTestAccount(time.Now().Add(-time.Minute))
			tc.mutate(account)
			require.False(t, account.IsSchedulableWithContext(probe))
			require.False(t, account.IsSchedulableForModelWithContext(probe, "gpt-6-astra"))
			qualityAdmissionTestChecked(account, time.Now().Add(-30*time.Second), time.Now().Add(-time.Second))
			require.False(t, account.IsSchedulable(), "a finished quality check must preserve independent restrictions")
		})
	}
	t.Run("native model cooldown", func(t *testing.T) {
		account := qualityAdmissionTestAccount(time.Now().Add(-time.Minute))
		account.Extra["model_rate_limits"] = map[string]any{"gpt-6-astra": map[string]any{"reason": "upstream_429", "rate_limit_reset_at": future.Format(time.RFC3339Nano)}}
		require.True(t, account.IsSchedulableWithContext(probe))
		require.False(t, account.IsSchedulableForModelWithContext(probe, "gpt-6-astra"))
	})
}

func TestQualityJudgeCandidatesPendingAdmissionPreservesBaseLimits(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	self := qualityAdmissionTestAccount(past)
	self.GroupIDs = []int64{8}
	eligible := qualityAdmissionTestAccount(past)
	eligible.ID, eligible.GroupIDs = self.ID+1, []int64{8}
	eligible.RateLimitResetAt = &past
	eligible.Extra["codex_7d_used_percent"] = 100.0
	eligible.Extra["codex_7d_reset_at"] = past.Format(time.RFC3339Nano)
	require.True(t, eligible.QualityProbeAdmissionPending(now))
	require.False(t, eligible.IsSchedulable(), "the grader is still blocked for business traffic")
	accounts := []Account{*self, *eligible}
	for _, mutate := range []func(*Account){
		func(a *Account) { a.Schedulable = false },
		func(a *Account) { a.RateLimitResetAt = &future },
		func(a *Account) { a.TempUnschedulableUntil = &future },
		func(a *Account) { a.OverloadUntil = &future },
		func(a *Account) { a.Status = StatusError },
		func(a *Account) {
			a.Type = AccountTypeAPIKey
			a.Extra["quota_limit"], a.Extra["quota_used"] = 10.0, 10.0
		},
	} {
		account := qualityAdmissionTestAccount(past)
		account.ID, account.GroupIDs = self.ID+int64(len(accounts)), []int64{8}
		mutate(account)
		accounts = append(accounts, *account)
	}
	judge := &QualityJudgeConfig{GroupID: 8, ModelID: "gpt-6-astra"}
	candidates := qualityJudgeCandidates(accounts, self.ID, judge)
	require.Len(t, candidates, 1, "only admission may be bypassed; self, manual disable and unexpired base limits still exclude candidates")
	require.Equal(t, eligible.ID, candidates[0].ID)
	require.True(t, candidates[0].QualityProbeAdmissionPending(now), "choosing an internal grader must not clear its business gate")

	svc, cfg, repository, slots := judgeFixture()
	repository.accounts = accounts
	cfg.Quality.Judge.ModelID = judge.ModelID
	svc.request = func(_ context.Context, id int64, _, _ string) (string, error) {
		require.Equal(t, eligible.ID, id)
		return `{"verdict":"correct","reason":"equivalent"}`, nil
	}
	result := svc.Judge(context.Background(), self.ID, cfg, "21")
	require.Equal(t, "correct", result.Verdict, "a pool of pending accounts must still be able to grade recovery probes")
	require.Equal(t, eligible.ID, result.AccountID)
	require.Equal(t, []int64{eligible.ID}, slots.acquired)
	require.Equal(t, slots.acquired, slots.released)
}

func qualityAdmissionTestResult(started, finished time.Time, verdict string) *ScheduledTestResult {
	result := &ScheduledTestResult{Status: "success", StartedAt: started, FinishedAt: finished, QualityJudgment: &QualityJudgment{Verdict: verdict}}
	if verdict == "incorrect" {
		result.Status, result.ErrorMessage = "failed", "answer_mismatch"
	}
	return result
}

func TestSetQualityProbeFinishedAtAdmissionEvidence(t *testing.T) {
	now := time.Now().Add(-time.Minute)
	for _, verdict := range []string{"correct", "incorrect"} {
		t.Run(verdict, func(t *testing.T) {
			plan := multiModelQualityPlan(t, `["gpt-6-astra","gpt-6-sol"]`)
			results := []*ScheduledTestResult{
				qualityAdmissionTestResult(now.Add(2*time.Second), now.Add(5*time.Second), verdict),
				qualityAdmissionTestResult(now, now.Add(3*time.Second), verdict),
			}
			SetQualityProbeFinishedAt(plan, results)
			require.True(t, plan.QualityProbeAdmissionChecked, "a definite incorrect result is evidence too; existing quality policy owns isolation")
			require.NotNil(t, plan.QualityProbeStartedAt)
			require.NotNil(t, plan.QualityProbeFinishedAt)
			require.True(t, plan.QualityProbeStartedAt.Equal(now))
			require.True(t, plan.QualityProbeFinishedAt.Equal(now.Add(5*time.Second)))
		})
	}
}

func TestSetQualityProbeFinishedAtRejectsIncompleteAdmissionEvidence(t *testing.T) {
	now := time.Now().Add(-time.Minute)
	for _, mode := range []string{"unknown", "skipped", "missing judgment", "missing sample", "nil sample", "missing start", "missing finish", "reversed times", "future finish"} {
		t.Run(mode, func(t *testing.T) {
			plan := multiModelQualityPlan(t, `["gpt-6-astra","gpt-6-sol"]`)
			results := []*ScheduledTestResult{qualityAdmissionTestResult(now, now.Add(time.Second), "correct"), qualityAdmissionTestResult(now, now.Add(2*time.Second), "correct")}
			SetQualityProbeFinishedAt(plan, results)
			require.True(t, plan.QualityProbeAdmissionChecked)
			switch mode {
			case "unknown":
				results[1].QualityJudgment.Verdict = "unknown"
			case "skipped":
				results[1].Status, results[1].QualityJudgment = "skipped", nil
			case "missing judgment":
				results[1].QualityJudgment = nil
			case "missing sample":
				results = results[:1]
			case "nil sample":
				results[1] = nil
			case "missing start":
				results[1].StartedAt = time.Time{}
			case "missing finish":
				results[1].FinishedAt = time.Time{}
			case "reversed times":
				results[1].StartedAt = results[1].FinishedAt.Add(time.Second)
			case "future finish":
				results[1].FinishedAt = time.Now().Add(time.Hour)
			}
			SetQualityProbeFinishedAt(plan, results)
			require.False(t, plan.QualityProbeAdmissionChecked, "incomplete evidence must also clear a reused plan's previous admission flag")
		})
	}
	t.Run("empty results clear previous evidence", func(t *testing.T) {
		plan := multiModelQualityPlan(t, `["gpt-6-astra"]`)
		SetQualityProbeFinishedAt(plan, []*ScheduledTestResult{qualityAdmissionTestResult(now, now.Add(time.Second), "correct")})
		require.True(t, plan.QualityProbeAdmissionChecked)
		SetQualityProbeFinishedAt(plan, nil)
		require.False(t, plan.QualityProbeAdmissionChecked)
		require.Nil(t, plan.QualityProbeStartedAt)
		require.Nil(t, plan.QualityProbeFinishedAt)
	})
}

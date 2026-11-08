package service

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type qualityQuotaPlanRepo struct {
	pelicanPlanRepo
	outcomes []string
}

func (r *qualityQuotaPlanRepo) ApplyQualityOutcome(_ context.Context, _ *ScheduledTestPlan, _ time.Time, outcome string) (string, error) {
	r.outcomes = append(r.outcomes, outcome)
	return outcome, nil
}

func (r *qualityQuotaPlanRepo) FinishTriggeredQuality(ctx context.Context, plan *ScheduledTestPlan, until, finished, _ time.Time) error {
	return r.FinishPelican(ctx, plan.ID, until, finished)
}

type qualityQuotaCalls struct {
	catalog atomic.Int64
	answer  atomic.Int64
	judge   atomic.Int64
}

func qualityQuotaRunnerAccount() *Account {
	now := time.Now()
	reset := now.Add(time.Hour)
	account := stateProbeAccount()
	account.ID, account.Status, account.Schedulable = 42, StatusActive, true
	account.RateLimitResetAt = &reset
	account.Extra = map[string]any{
		"codex_usage_updated_at": now.Format(time.RFC3339Nano),
		"codex_5h_used_percent":  100.0,
		"codex_5h_reset_at":      reset.Format(time.RFC3339),
		"codex_7d_used_percent":  20.0,
		"codex_7d_reset_at":      now.Add(24 * time.Hour).Format(time.RFC3339),
	}
	return account
}

func newQualityQuotaRunner(account *Account) (*ScheduledTestRunnerService, *qualityQuotaPlanRepo, *pelicanResults, *qualityQuotaCalls) {
	plans, saved, calls := &qualityQuotaPlanRepo{}, &pelicanResults{}, &qualityQuotaCalls{}
	tests := &AccountTestService{accountRepo: &qualitySupportAccounts{account: account}}
	svc := NewScheduledTestService(plans, saved)
	svc.accountTests = tests
	svc.qualityModels = func(context.Context, int64) ([]string, error) {
		calls.catalog.Add(1)
		return []string{"gpt-6-astra", "gpt-6-sol", "gpt-6.1-sol"}, nil
	}
	runner := &ScheduledTestRunnerService{planRepo: plans, scheduledSvc: svc, accountTestSvc: tests}
	runner.runPelican = func(_ context.Context, _ int64, _ string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
		calls.answer.Add(1)
		now := time.Now()
		return &ScheduledTestResult{Status: "success", ResponseText: "21", StartedAt: now, FinishedAt: now, PelicanConfig: cfg}, nil
	}
	runner.judgeQuality = func(context.Context, int64, *PelicanTestConfig, string) *QualityJudgment {
		calls.judge.Add(1)
		return &QualityJudgment{Verdict: "correct"}
	}
	return runner, plans, saved, calls
}

func TestQualityQuotaExhaustedPlanSkipsBeforeCatalogAnswerAndJudge(t *testing.T) {
	for _, name := range []string{"scheduled", "fallback_account_service", "state_probe", "upstream_5xx", "two_fa_admission", "random_question"} {
		t.Run(name, func(t *testing.T) {
			runner, plans, saved, calls := newQualityQuotaRunner(qualityQuotaRunnerAccount())
			plan := multiModelQualityPlan(t, `["gpt-6-astra", "gpt-6-sol"]`)
			plan.PelicanConfig.ParallelCount = 3
			wantOutcomes := []string(nil)
			switch name {
			case "fallback_account_service":
				runner.accountTestSvc = nil
			case "state_probe":
				plan.PelicanConfig = stateProbePlanConfig()
				plan.PelicanConfig.Quality = &QualityPolicy{Action: "disable_scheduling"}
			case "upstream_5xx":
				plan.TriggerSource = quality5xxSource
				plan.PelicanConfig.Quality.TriggerOnUpstream5xx = true
			case "two_fa_admission":
				policy := DefaultOpenAITwoFAAdmissionConfig()
				policy.Enabled, policy.IsolationGroupID = true, 44
				plan.ModelID = policy.ModelID
				plan.PelicanConfig = &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt,
					ReasoningEffort: policy.ReasoningEffort, ParallelCount: 1,
					Quality: &QualityPolicy{Action: QualityActionOpenAITwoFAAdmission, ExpectedAnswer: "21", Admission: &policy}}
				wantOutcomes = []string{"inconclusive"}
			case "random_question":
				// No shared-batch repository is wired: quota must take precedence
				// over question generation and its otherwise-failing setup.
				plan.PelicanConfig.QuestionKind = QualityRandomQuestionKind
			}

			require.True(t, runner.runOnePlan(t.Context(), plan))
			require.Zero(t, calls.catalog.Load())
			require.Zero(t, calls.answer.Load())
			require.Zero(t, calls.judge.Load())
			require.Equal(t, wantOutcomes, plans.outcomes)
			require.True(t, plans.finished, "skipped plans must release their lease")
			models := qualityPlanModels(plan)
			require.Len(t, saved.results, len(models)*plan.PelicanConfig.ParallelCount)
			counts := map[string]int{}
			for _, result := range saved.results {
				require.Equal(t, "skipped", result.Status)
				require.Equal(t, "account_quota_exhausted", result.ErrorMessage)
				require.Empty(t, result.ResponseText)
				require.Nil(t, result.QualityJudgment)
				require.Equal(t, "inconclusive", result.QualityAction)
				require.Equal(t, plan.ID, result.PlanID)
				require.NotEmpty(t, result.QualityRoundID)
				require.Equal(t, saved.results[0].QualityRoundID, result.QualityRoundID)
				counts[result.PelicanConfig.ModelID]++
			}
			for _, model := range models {
				require.Equal(t, plan.PelicanConfig.ParallelCount, counts[model])
			}
			require.Equal(t, "inconclusive", qualityRoundOutcome(saved.results))
		})
	}
}

func TestQualityQuotaExhaustedRecoveryAppliesInconclusiveToResetConfirmation(t *testing.T) {
	for _, onPassed := range []bool{false, true} {
		name := "restore_after_failure"
		if onPassed {
			name = "action_on_passed"
		}
		t.Run(name, func(t *testing.T) {
			runner, plans, saved, calls := newQualityQuotaRunner(qualityQuotaRunnerAccount())
			plan := multiModelQualityPlan(t, `["gpt-6-astra", "gpt-6-sol"]`)
			plan.PelicanConfig.ParallelCount = 2
			q := plan.PelicanConfig.Quality
			q.AutoRestore, q.RecoveryProbeEnabled = true, true
			if onPassed {
				q.Action, q.ActionOn = QualityActionEnableScheduling, "passed"
			}
			require.True(t, runner.runOnePlan(t.Context(), plan))
			// The repository resets its first-pass confirmation on inconclusive;
			// silently finishing an all-skipped round would leave it pending.
			require.Equal(t, []string{"inconclusive"}, plans.outcomes)
			require.True(t, plans.finished)
			require.Len(t, saved.results, 4)
			for _, result := range saved.results {
				require.Equal(t, "skipped", result.Status)
				require.Equal(t, "account_quota_exhausted", result.ErrorMessage)
				require.Nil(t, result.QualityJudgment)
			}
			require.Zero(t, calls.catalog.Load())
			require.Zero(t, calls.answer.Load())
			require.Zero(t, calls.judge.Load())
		})
	}
}

func TestQualityQuotaPlanResumesWhenUsageFallsOrWindowResets(t *testing.T) {
	for _, name := range []string{"below_limit", "window_reset"} {
		t.Run(name, func(t *testing.T) {
			account := qualityQuotaRunnerAccount()
			runner, plans, saved, calls := newQualityQuotaRunner(account)
			plan := multiModelQualityPlan(t, `["gpt-6-astra", "gpt-6-sol"]`)
			plan.PelicanConfig.ParallelCount = 2
			require.True(t, runner.runOnePlan(t.Context(), plan))
			require.Zero(t, calls.answer.Load())
			require.Len(t, saved.results, 4)

			account.RateLimitResetAt = nil
			if name == "below_limit" {
				account.Extra["codex_5h_used_percent"] = 30.0
			} else {
				past := time.Now().Add(-time.Minute)
				account.RateLimitResetAt = &past
				account.Extra["codex_5h_reset_at"] = past.Format(time.RFC3339)
			}
			// Quarantined accounts still need recovery probes; the manual
			// scheduling switch must not substitute for checking quota evidence.
			account.Schedulable = false
			plans.claimed, plans.finished = false, false
			require.True(t, runner.runOnePlan(t.Context(), plan))
			require.EqualValues(t, 1, calls.catalog.Load())
			require.EqualValues(t, 4, calls.answer.Load())
			require.EqualValues(t, 4, calls.judge.Load())
			require.Equal(t, []string{"passed"}, plans.outcomes)
			require.True(t, plans.finished)
			require.Len(t, saved.results, 8)
			for _, result := range saved.results[4:] {
				require.Equal(t, "success", result.Status)
				require.Empty(t, result.ErrorMessage)
				require.Equal(t, "correct", result.QualityJudgment.Verdict)
			}
		})
	}
}

func TestQualityQuotaAnswerRechecksBeforeRetry(t *testing.T) {
	account := qualityQuotaRunnerAccount()
	account.RateLimitResetAt = nil
	account.Extra["codex_5h_used_percent"] = 30.0
	runner, _, _, calls := newQualityQuotaRunner(account)
	runner.runPelican = func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
		calls.answer.Add(1)
		reset := time.Now().Add(time.Hour)
		account.RateLimitResetAt = &reset
		account.Extra["codex_5h_used_percent"] = 100.0
		return &ScheduledTestResult{Status: "failed", ErrorMessage: "API returned 429: quota exhausted"}, nil
	}
	result := runner.runPelicanSample(t.Context(), qualityAnswerRetryPlan())
	require.EqualValues(t, 1, calls.answer.Load(), "the retry must reload quota before another upstream call")
	require.Zero(t, calls.judge.Load())
	require.Equal(t, "skipped", result.Status)
	require.Equal(t, "account_quota_exhausted", result.ErrorMessage)
	require.Nil(t, result.QualityJudgment)
	require.Equal(t, "inconclusive", qualityRoundOutcome([]*ScheduledTestResult{result}))
}

func TestQualityQuotaBackgroundSkipsBeforeTransport(t *testing.T) {
	for _, kind := range []string{"candy", OpenAICodexStateProbeQuestionKind} {
		t.Run(kind, func(t *testing.T) {
			account := qualityQuotaRunnerAccount()
			upstream := &stateProbeUpstream{}
			tests := &AccountTestService{accountRepo: &qualitySupportAccounts{account: account}, httpUpstream: upstream,
				openaiGatewayService: &OpenAIGatewayService{httpUpstream: upstream}}
			cfg := &PelicanTestConfig{QuestionKind: kind, Prompt: CandyPrompt, ParallelCount: 1,
				Quality: &QualityPolicy{Action: "disable_scheduling", ExpectedAnswer: "21"}}
			result, err := tests.RunPelicanBackground(t.Context(), account.ID, "gpt-6-astra", cfg)
			require.NoError(t, err)
			require.Equal(t, "skipped", result.Status)
			require.Equal(t, "account_quota_exhausted", result.ErrorMessage)
			require.Nil(t, result.QualityJudgment)
			require.Empty(t, upstream.calls)
		})
	}
}

func TestQualityQuotaManualStateProbeSkipsAndResumesAfterReset(t *testing.T) {
	account := qualityQuotaRunnerAccount()
	result, upstream := runStateProbe(t, account)
	require.Empty(t, upstream.calls)
	require.Equal(t, OpenAICodexStateInconclusive, result.Verdict)
	require.Equal(t, OpenAICodexStateFailureRateLimited, result.Failure)
	require.Contains(t, result.Reason, "账号额度已用尽")
	require.False(t, result.Minted)

	past := time.Now().Add(-time.Minute)
	account.RateLimitResetAt = &past
	account.Extra["codex_5h_reset_at"] = past.Format(time.RFC3339)
	result, upstream = runStateProbe(t, account, stateProbeMint("ticket-1"), stateProbeReply{status: http.StatusOK, body: stateProbeCompletedStream})
	require.Len(t, upstream.calls, 2)
	require.Equal(t, OpenAICodexStateHealthy, result.Verdict)
	require.Empty(t, result.Failure)
}

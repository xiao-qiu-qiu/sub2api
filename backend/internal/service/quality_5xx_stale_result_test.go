package service

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/qualityqueue"
	"github.com/Wei-Shaw/sub2api/internal/pkg/qualityqueue/queuetest"
	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

// Capture the runner/repository boundary, including the original claim's lease
// and rule version. Database transaction semantics belong to repository tests.
type quality5xxStaleResultRepo struct {
	ScheduledTestPlanRepository
	plan           *ScheduledTestPlan
	claimAllowed   bool
	ownsBPS        bool
	claims         int
	finishes       int
	lease          time.Time
	claimedVersion time.Time
	outcomes       []string
	modelOutcomes  []map[string]string
	applied        int
}

func (r *quality5xxStaleResultRepo) GetByID(context.Context, int64) (*ScheduledTestPlan, error) {
	p := *r.plan
	p.PelicanConfig = cloneQualityPelicanConfig(r.plan.PelicanConfig)
	return &p, nil
}

func (r *quality5xxStaleResultRepo) ListByAccountID(ctx context.Context, id int64) ([]*ScheduledTestPlan, error) {
	p, err := r.GetByID(ctx, id)
	return []*ScheduledTestPlan{p}, err
}

func (r *quality5xxStaleResultRepo) ClaimPelican(_ context.Context, p *ScheduledTestPlan, now, until, _ time.Time) (bool, error) {
	r.claims++
	if !r.claimAllowed || !r.plan.Enabled || !r.plan.UpdatedAt.Equal(p.UpdatedAt) ||
		(r.plan.RunningUntil != nil && r.plan.RunningUntil.After(now)) {
		return false, nil
	}
	r.lease, r.claimedVersion = until, p.UpdatedAt
	r.plan.RunningUntil = &until
	// Like the real repository, inject ownership metadata after loading JSON.
	p.PelicanConfig.BPSRecoveryPending = r.ownsBPS
	return true, nil
}

func (r *quality5xxStaleResultRepo) ApplyQualityOutcome(_ context.Context, p *ScheduledTestPlan, until time.Time, outcome string) (string, error) {
	r.outcomes = append(r.outcomes, outcome)
	r.modelOutcomes = append(r.modelOutcomes, maps.Clone(p.QualityModelOutcomes))
	if !r.plan.Enabled || !r.plan.UpdatedAt.Equal(p.UpdatedAt) || !r.claimedVersion.Equal(p.UpdatedAt) ||
		!r.lease.Equal(until) || !r.lease.After(time.Now()) {
		return "stale_run", nil
	}
	r.applied++
	switch outcome {
	case "failed":
		return "scheduling_disabled", nil
	case "passed":
		return "restored", nil
	default:
		return "inconclusive", nil
	}
}

func (r *quality5xxStaleResultRepo) FinishTriggeredQuality(_ context.Context, _ *ScheduledTestPlan, until, finished, _ time.Time) error {
	r.finishes++
	if r.lease.Equal(until) {
		r.plan.RunningUntil = nil
		r.plan.LastRunAt = &finished
	}
	return nil
}

type quality5xxStaleResultHarness struct {
	runner  *ScheduledTestRunnerService
	repo    *quality5xxStaleResultRepo
	saved   *pelicanResults
	queue   qualityqueue.Queue
	signal  qualityqueue.Signal
	account *Account
}

func newQuality5xxStaleResultHarness(t *testing.T, plan *ScheduledTestPlan) *quality5xxStaleResultHarness {
	t.Helper()
	mr := miniredis.RunT(t)
	client := queuetest.NewClient(mr.Addr())
	t.Cleanup(func() { _ = client.Close() })
	queue := qualityqueue.NewRedis(client)
	t.Cleanup(func() { _ = queue.Close() })
	plan.UpdatedAt = time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	// Model cooldowns already have immediate protection; do not enter the
	// separate legacy "pending" fallback before the probe under test.
	plan.Quality5xxEpisode = 1
	repo := &quality5xxStaleResultRepo{plan: plan, claimAllowed: true}
	saved := &pelicanResults{}
	account := &Account{ID: plan.AccountID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	runner := &ScheduledTestRunnerService{
		planRepo:       repo,
		scheduledSvc:   NewScheduledTestService(repo, saved),
		accountTestSvc: &AccountTestService{accountRepo: &stateProbeAccountRepo{account: account}},
		qualityTrigger: &quality5xxTrigger{queue: queue},
	}
	signal := qualityqueue.Signal{AccountID: plan.AccountID, ObservedAt: time.Now().Add(-time.Minute).Truncate(time.Millisecond)}
	require.NoError(t, queue.Enqueue(context.Background(), signal.AccountID, signal.ObservedAt))
	return &quality5xxStaleResultHarness{runner: runner, repo: repo, saved: saved, queue: queue, signal: signal, account: account}
}

func quality5xxStaleStatePlan() *ScheduledTestPlan {
	plan := pelicanPlan()
	plan.PelicanConfig = stateProbePlanConfig()
	plan.PelicanConfig.Quality = &QualityPolicy{Action: "disable_scheduling", TriggerOnUpstream5xx: true, AutoRestore: true}
	return plan
}

func (h *quality5xxStaleResultHarness) run(ctx context.Context) error {
	return h.runner.runQualityTriggeredAccount(ctx, h.signal.AccountID, h.signal.ObservedAt)
}

// Reuse the original timestamp to exercise the real Redis monotonic score/CAS
// fence without sleeps, including independent failures in the same millisecond.
func (h *quality5xxStaleResultHarness) newerSignal(ctx context.Context) error {
	return h.queue.Enqueue(ctx, h.signal.AccountID, h.signal.ObservedAt)
}

func TestQuality5xxStaleStateProbeResult(t *testing.T) {
	for _, tc := range []struct {
		name, status, verdict, message, outcome, action string
		newSignal                                       bool
	}{
		{"fresh_degraded", "failed", "incorrect", openAICodexStateDegradedError, "failed", "scheduling_disabled", false},
		{"stale_degraded", "failed", "incorrect", openAICodexStateDegradedError, "failed", "scheduling_disabled", true},
		{"fresh_healthy", "success", "correct", "", "passed", "restored", false},
		{"stale_healthy", "success", "correct", "", "", "stale_run", true},
		{"fresh_unknown", "failed", "unknown", "state_probe_inconclusive: timeout", "inconclusive", "inconclusive", false},
		{"stale_unknown", "failed", "unknown", "state_probe_inconclusive: timeout", "", "stale_run", true},
		{"stale_transport_error", "failed", "", "upstream_503", "", "stale_run", true},
		{"stale_unknown_with_degraded_marker", "failed", "unknown", openAICodexStateDegradedError, "", "stale_run", true},
		{"stale_incorrect_without_degraded_evidence", "failed", "incorrect", "upstream_503", "", "stale_run", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newQuality5xxStaleResultHarness(t, quality5xxStaleStatePlan())
			h.runner.runPelican = func(ctx context.Context, _ int64, _ string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
				if tc.newSignal {
					if err := h.newerSignal(ctx); err != nil {
						return nil, err
					}
				}
				result := &ScheduledTestResult{Status: tc.status, ErrorMessage: tc.message, PelicanConfig: cfg}
				if tc.verdict != "" {
					result.QualityJudgment = &QualityJudgment{Verdict: tc.verdict}
				}
				return result, nil
			}
			require.NoError(t, h.run(context.Background()))
			require.Equal(t, 1, h.repo.claims)
			require.Equal(t, 1, h.repo.finishes)
			if tc.outcome == "" {
				require.Empty(t, h.repo.outcomes, "stale recovery/unknown evidence must not reach the action repository")
			} else {
				require.Equal(t, []string{tc.outcome}, h.repo.outcomes)
			}
			require.Len(t, h.saved.results, 1)
			result := h.saved.results[0]
			require.Equal(t, tc.action, result.QualityAction)
			require.Equal(t, tc.status, result.Status, "signal freshness must not rewrite the probe verdict")
			require.Equal(t, tc.message, result.ErrorMessage)
			if tc.verdict != "" {
				require.Equal(t, tc.verdict, result.QualityJudgment.Verdict)
			}
			require.Equal(t, OpenAICodexStateProbeQuestionKind, result.PelicanConfig.QuestionKind)
			require.Equal(t, quality5xxSource, result.PelicanConfig.TriggerSource)
			require.NotEmpty(t, result.QualityRoundID)
			require.Equal(t, !tc.newSignal, h.runner.qualityTrigger.signalIsCurrent(h.signal.AccountID, h.signal.ObservedAt))
		})
	}
}

func TestQuality5xxStaleMixedModelResult(t *testing.T) {
	for _, action := range []string{QualityActionRemoveModel, QualityActionIsolateModels} {
		t.Run(action, func(t *testing.T) {
			plan := multiModelQualityPlan(t, `["failed-model","passed-model","unknown-model","skipped-model"]`)
			plan.PelicanConfig.Quality = &QualityPolicy{
				Action: action, TriggerOnUpstream5xx: true, AutoRestore: true, ExpectedAnswer: "21",
				RemoveGroupIDs: []int64{7}, RemoveModels: []string{"failed-model", "passed-model", "unknown-model", "skipped-model"},
			}
			h := newQuality5xxStaleResultHarness(t, plan)
			h.runner.runPelican = func(ctx context.Context, _ int64, model string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
				switch model {
				case "failed-model":
					if err := h.newerSignal(ctx); err != nil {
						return nil, err
					}
					return &ScheduledTestResult{Status: "success", ResponseText: "17", PelicanConfig: cfg}, nil
				case "passed-model":
					return &ScheduledTestResult{Status: "success", ResponseText: "21", PelicanConfig: cfg}, nil
				case "unknown-model":
					return &ScheduledTestResult{Status: "failed", ErrorMessage: "upstream_503", QualityJudgment: &QualityJudgment{Verdict: "unknown"}, PelicanConfig: cfg}, nil
				default:
					return &ScheduledTestResult{Status: "skipped", ErrorMessage: "model_unavailable", PelicanConfig: cfg}, nil
				}
			}
			require.NoError(t, h.run(context.Background()))
			want := map[string]string{"failed-model": "failed", "passed-model": "inconclusive", "unknown-model": "inconclusive", "skipped-model": "skipped"}
			require.Equal(t, []string{"failed"}, h.repo.outcomes)
			require.Equal(t, []map[string]string{want}, h.repo.modelOutcomes, "suppress passed models before the repository can restore them")
			require.Len(t, h.saved.results, 4)
			for _, result := range h.saved.results {
				require.Equal(t, want, result.PelicanConfig.QualityModelOutcomes)
				require.Equal(t, "scheduling_disabled", result.QualityAction)
				if result.PelicanConfig.ModelID == "passed-model" {
					require.Equal(t, "success", result.Status)
					require.Equal(t, "correct", result.QualityJudgment.Verdict)
				}
			}
		})
	}
}

func TestQuality5xxStalePassedModelsCannotRestore(t *testing.T) {
	plan := stateProbeCooldownPlan(t)
	plan.PelicanConfig.Quality.TriggerOnUpstream5xx = true
	h := newQuality5xxStaleResultHarness(t, plan)
	h.runner.runPelican = func(ctx context.Context, _ int64, _ string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
		if err := h.newerSignal(ctx); err != nil {
			return nil, err
		}
		return &ScheduledTestResult{Status: "success", QualityJudgment: &QualityJudgment{Verdict: "correct"}, PelicanConfig: cfg}, nil
	}
	require.NoError(t, h.run(context.Background()))
	require.Empty(t, h.repo.outcomes)
	require.Len(t, h.saved.results, 1)
	result := h.saved.results[0]
	require.Equal(t, "stale_run", result.QualityAction)
	require.Len(t, result.PelicanConfig.QualityModelOutcomes, len(plan.PelicanConfig.Quality.RemoveModels))
	for _, model := range plan.PelicanConfig.Quality.RemoveModels {
		require.Equal(t, "inconclusive", result.PelicanConfig.QualityModelOutcomes[model], "state probe recovery must be suppressed for every cooldown target")
	}
}

func TestQuality5xxStaleFailedResultKeepsTriggeredQualityGate(t *testing.T) {
	for _, recoveryPending := range []bool{false, true} {
		for _, stale := range []bool{false, true} {
			name := "bps_action"
			if recoveryPending {
				name = "bps_recovery_pending"
			}
			if stale {
				name += "/new_signal"
			}
			t.Run(name, func(t *testing.T) {
				plan := quality5xxStaleStatePlan()
				if !recoveryPending {
					plan.PelicanConfig.Quality.Action = QualityActionEnableBPS
					plan.PelicanConfig.Quality.BPS = &QualityBPSPolicy{FailureThreshold: 1, AllModels: true}
				}
				h := newQuality5xxStaleResultHarness(t, plan)
				h.repo.ownsBPS = recoveryPending
				h.account.Extra = map[string]any{"openai_excel_bps": true}
				h.runner.runPelican = func(ctx context.Context, _ int64, _ string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
					if stale {
						if err := h.newerSignal(ctx); err != nil {
							return nil, err
						}
					}
					require.Equal(t, OpenAICodexStateProbeQuestionKind, cfg.QuestionKind)
					return &ScheduledTestResult{Status: "failed", ErrorMessage: openAICodexStateDegradedError,
						QualityJudgment: &QualityJudgment{Verdict: "incorrect"}, PelicanConfig: cfg}, nil
				}
				require.NoError(t, h.run(context.Background()))
				require.Equal(t, []string{"failed"}, h.repo.outcomes, "the BPS owner still checks its native recovery route")
				require.Len(t, h.saved.results, 1)
				result := h.saved.results[0]
				require.Equal(t, OpenAICodexStateProbeQuestionKind, result.PelicanConfig.QuestionKind)
				require.Equal(t, "incorrect", result.QualityJudgment.Verdict)
				wantAction := "scheduling_disabled"
				require.Equal(t, wantAction, result.QualityAction)
			})
		}
	}
}

func TestQuality5xxStaleFailedResultStillUsesRepositoryFences(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*quality5xxStaleResultRepo)
	}{
		{"rule_edited", func(r *quality5xxStaleResultRepo) { r.plan.UpdatedAt = r.plan.UpdatedAt.Add(time.Second) }},
		{"rule_paused", func(r *quality5xxStaleResultRepo) { r.plan.Enabled = false }},
		{"lease_replaced", func(r *quality5xxStaleResultRepo) { r.lease = r.lease.Add(time.Second) }},
		{"lease_expired", func(r *quality5xxStaleResultRepo) { r.lease = time.Now().Add(-time.Second) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newQuality5xxStaleResultHarness(t, quality5xxStaleStatePlan())
			h.runner.runPelican = func(ctx context.Context, _ int64, _ string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
				tc.change(h.repo)
				if err := h.newerSignal(ctx); err != nil {
					return nil, err
				}
				return &ScheduledTestResult{Status: "failed", ErrorMessage: openAICodexStateDegradedError, QualityJudgment: &QualityJudgment{Verdict: "incorrect"}, PelicanConfig: cfg}, nil
			}
			require.NoError(t, h.run(context.Background()))
			require.Equal(t, []string{"failed"}, h.repo.outcomes, "confirmed degradation still has to pass the repository's lease/version check")
			require.Zero(t, h.repo.applied)
			require.Len(t, h.saved.results, 1)
			require.Equal(t, "stale_run", h.saved.results[0].QualityAction)
			require.Empty(t, h.saved.results[0].PelicanConfig.QualityModelActions)
		})
	}
}

func TestQuality5xxStaleCompletionDoesNotConsumeNewPendingSignal(t *testing.T) {
	h := newQuality5xxStaleResultHarness(t, quality5xxStaleStatePlan())
	calls := 0
	h.runner.runPelican = func(ctx context.Context, _ int64, _ string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
		calls++
		if calls == 1 {
			if err := h.newerSignal(ctx); err != nil {
				return nil, err
			}
		}
		return &ScheduledTestResult{Status: "success", QualityJudgment: &QualityJudgment{Verdict: "correct"}, PelicanConfig: cfg}, nil
	}
	ctx := context.Background()
	require.NoError(t, h.run(ctx))
	require.Len(t, h.saved.results, 1)
	require.Equal(t, "stale_run", h.saved.results[0].QualityAction)
	newObservedAt, err := h.queue.Score(ctx, h.signal.AccountID)
	require.NoError(t, err)
	require.True(t, newObservedAt.After(h.signal.ObservedAt))
	require.NotNil(t, h.repo.plan.LastRunAt)
	require.True(t, h.repo.plan.LastRunAt.After(newObservedAt), "the old probe finished after the new 5xx was observed")
	require.NoError(t, h.queue.Acknowledge(ctx, h.signal))
	pending, err := h.queue.Pending(ctx, time.Now().Add(-20*time.Minute))
	require.NoError(t, err)
	require.Equal(t, []qualityqueue.Signal{{AccountID: h.signal.AccountID, ObservedAt: newObservedAt}}, pending, "old acknowledgment must retain the new episode")

	require.NoError(t, h.runner.runQualityTriggeredAccount(ctx, h.signal.AccountID, newObservedAt))
	require.Equal(t, 2, calls, "LastRunAt is a completion timestamp, not evidence that the newer signal was tested")
	require.Equal(t, 2, h.repo.claims)
	require.Equal(t, []string{"passed"}, h.repo.outcomes, "only a fresh second probe may restore")
	require.Len(t, h.saved.results, 2)
	require.Equal(t, "restored", h.saved.results[1].QualityAction)
	require.NoError(t, h.queue.Acknowledge(ctx, pending[0]))
	require.NoError(t, h.queue.Acknowledge(ctx, pending[0]))
	pending, err = h.queue.Pending(ctx, time.Now().Add(-20*time.Minute))
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestQuality5xxStaleRecentCompletionDoesNotHideClaimConflict(t *testing.T) {
	for _, activeLease := range []bool{false, true} {
		name := "claim_rejected"
		if activeLease {
			name = "active_lease"
		}
		t.Run(name, func(t *testing.T) {
			h := newQuality5xxStaleResultHarness(t, quality5xxStaleStatePlan())
			completed := h.signal.ObservedAt.Add(time.Second)
			h.repo.plan.LastRunAt = &completed
			h.repo.claimAllowed = false
			if activeLease {
				until := time.Now().Add(time.Minute)
				h.repo.plan.RunningUntil = &until
			}
			require.Error(t, h.run(context.Background()), "a newer pending signal must survive a busy lease or failed claim even with a later LastRunAt")
			wantClaims := 1
			if activeLease {
				wantClaims = 0
			}
			require.Equal(t, wantClaims, h.repo.claims)
			require.Empty(t, h.repo.outcomes)
			require.Empty(t, h.saved.results)
			observedAt, err := h.queue.Score(context.Background(), h.signal.AccountID)
			require.NoError(t, err)
			require.Equal(t, h.signal.ObservedAt, observedAt)
		})
	}
}

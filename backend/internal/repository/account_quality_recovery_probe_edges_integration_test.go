//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func pendingRecoveryFixture(t *testing.T) *qualityMoveGroupsFixture {
	t.Helper()
	f := newQualityMoveGroupsFixture(t, true)
	enableRecoveryProbe(t, f.qualityBPSFixture)
	require.Equal(t, "groups_moved", f.apply("failed"))
	claimRecoveryProbe(t, f, false)
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
	return f
}

func requireRecoveryScheduleMatchesState(t *testing.T, f *qualityMoveGroupsFixture) {
	t.Helper()
	stored, err := f.plans.GetByID(context.Background(), f.plan.ID)
	require.NoError(t, err)
	require.WithinDuration(t, *stored.NextRunAt, f.state().RecoveryProbe.NextProbeAt, time.Microsecond)
}

func TestQualityRecoveryProbeManualEarlyAndDueConfirmation(t *testing.T) {
	f := pendingRecoveryFixture(t)
	first := f.state().RecoveryProbe
	quarantined := f.memberships()
	require.NoError(t, f.plans.FinishPelican(context.Background(), f.plan.ID, f.until, time.Now()))
	f.claim() // Exercise the real manual-trigger API, including its version fence.
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
	require.Equal(t, first.FirstPassedAt, f.state().RecoveryProbe.FirstPassedAt)
	require.True(t, first.NextProbeAt.Equal(f.state().RecoveryProbe.NextProbeAt), "an early manual pass must keep the existing confirmation deadline")
	require.Equal(t, quarantined, f.memberships())
	requireRecoveryScheduleMatchesState(t, f)
	// Once due, another manual probe may supply the delayed confirmation.
	require.NoError(t, f.plans.FinishPelican(context.Background(), f.plan.ID, f.until, time.Now()))
	state := f.state()
	aged := time.Now().Add(-6 * time.Minute)
	state.RecoveryProbe.FirstPassedAt = &aged
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(context.Background(), `UPDATE account_quality_states SET state=$2 WHERE plan_id=$1`, f.plan.ID, raw)
	require.NoError(t, err)
	f.claim()
	require.Equal(t, "restored", f.apply("passed"))
}

func TestQualityRecoveryProbeCrashWithoutOutcomeBreaksConfirmation(t *testing.T) {
	f := pendingRecoveryFixture(t)
	claimRecoveryProbe(t, f, true)
	require.False(t, f.state().RecoveryProbe.PendingLease.IsZero())
	// Neither ApplyQualityOutcome nor a result write happened before the crash.
	_, err := integrationDB.ExecContext(context.Background(), `UPDATE scheduled_test_plans SET running_until=clock_timestamp()-interval '1 second' WHERE id=$1`, f.plan.ID)
	require.NoError(t, err)
	claimRecoveryProbe(t, f, false)
	require.Nil(t, f.state().RecoveryProbe.FirstPassedAt)
	requireRecoveryScheduleMatchesState(t, f)
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"), "a crash is an unknown gap, so the next pass starts a new confirmation")
}

func TestQualityRecoveryProbeRestoreConflictRollsBackAccountAndSchedulesRetry(t *testing.T) {
	f := pendingRecoveryFixture(t)
	claimRecoveryProbe(t, f, true)
	// Force a late source conflict after the first original group was reinserted.
	f.exec(`INSERT INTO account_groups(account_id,group_id,priority) VALUES($1,$2,99)`, f.sourcePeer)
	beforeMemberships := f.memberships()
	beforeOwnership := f.state()
	var beforeAccount, afterAccount string
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `SELECT to_jsonb(a)::text FROM accounts a WHERE id=$1`, f.account).Scan(&beforeAccount))
	require.Equal(t, "restore_conflict", f.apply("passed"))
	require.Equal(t, beforeMemberships, f.memberships(), "partial source inserts must be rolled back")
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `SELECT to_jsonb(a)::text FROM accounts a WHERE id=$1`, f.account).Scan(&afterAccount))
	require.JSONEq(t, beforeAccount, afterAccount)
	state := f.state()
	require.Equal(t, beforeOwnership.Removed, state.Removed)
	require.Equal(t, beforeOwnership.Added, state.Added)
	require.True(t, state.RecoveryProbe.PendingLease.IsZero())
	require.WithinDuration(t, time.Now().Add(time.Hour), state.RecoveryProbe.NextProbeAt, 3*time.Second)
	requireRecoveryScheduleMatchesState(t, f)
	require.NotNil(t, f.plan.QualityRestoreDiagnostic)
}

func TestQualityRecoveryProbe5xxInvalidatesRunningConfirmation(t *testing.T) {
	f := newQualityMoveGroupsFixture(t, true)
	f.plan.PelicanConfig.Quality.TriggerOnUpstream5xx = true
	enableRecoveryProbe(t, f.qualityBPSFixture)
	require.Equal(t, "groups_moved", f.apply("failed"))
	claimRecoveryProbe(t, f, false)
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
	claimRecoveryProbe(t, f, true)
	oldEpisode := f.plan.Quality5xxEpisode
	require.NoError(t, (&accountRepository{sql: integrationDB}).ApplyQuality5xx(context.Background(), f.account))
	state := f.state()
	require.Greater(t, state.Quality5xxEpisode, oldEpisode)
	require.Nil(t, state.RecoveryProbe.FirstPassedAt)
	require.Equal(t, "stale_run", f.apply("passed"), "a probe started before the new real 5xx must not restore")
	claimRecoveryProbe(t, f, false)
	require.Equal(t, state.Quality5xxEpisode, f.plan.Quality5xxEpisode)
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
}

func TestQualityRecoveryProbeQueued5xxPassStartsFreshConfirmation(t *testing.T) {
	f := newQualityMoveGroupsFixture(t, true)
	f.plan.PelicanConfig.Quality.TriggerOnUpstream5xx = true
	enableRecoveryProbe(t, f.qualityBPSFixture)
	require.Equal(t, "groups_moved", f.apply("failed"))
	claimRecoveryProbe(t, f, false)
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
	ctx := context.Background()
	require.NoError(t, f.plans.FinishPelican(ctx, f.plan.ID, f.until, time.Now()))
	state := f.state()
	first := time.Now().Add(-6 * time.Minute)
	state.RecoveryProbe.FirstPassedAt = &first
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `UPDATE account_quality_states SET state=$2 WHERE plan_id=$1`, f.plan.ID, raw)
	require.NoError(t, err)
	plan, err := f.plans.GetByID(ctx, f.plan.ID)
	require.NoError(t, err)
	plan.TriggerSource = "upstream_5xx"
	now := time.Now().Add(time.Second).Truncate(time.Microsecond)
	f.until = now.Add(15 * time.Minute)
	claimed, err := f.plans.ClaimPelican(ctx, plan, now, f.until, *plan.NextRunAt)
	require.NoError(t, err)
	require.True(t, claimed)
	f.plan = plan
	require.Nil(t, f.state().RecoveryProbe.FirstPassedAt)
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"), "a 5xx-triggered pass cannot reuse the pre-signal first pass")
	requireRecoveryScheduleMatchesState(t, f)
}

func TestQualityRecoveryProbeDisabledAfter5xxKeepsLegacyRecovery(t *testing.T) {
	f := newQualityMoveGroupsFixture(t, true)
	f.plan.PelicanConfig.Quality.TriggerOnUpstream5xx = true
	enableRecoveryProbe(t, f.qualityBPSFixture)
	require.Equal(t, "groups_moved", f.apply("failed"))
	require.NoError(t, (&accountRepository{sql: integrationDB}).ApplyQuality5xx(context.Background(), f.account))
	require.Positive(t, f.state().Quality5xxEpisode)
	plan, err := f.plans.GetByID(context.Background(), f.plan.ID)
	require.NoError(t, err)
	plan.PelicanConfig.Quality.RecoveryProbeEnabled = false
	f.plan, err = f.plans.Update(context.Background(), plan)
	require.NoError(t, err)
	require.Zero(t, f.state().Quality5xxEpisode)
	claimRecoveryProbe(t, f, false)
	require.Equal(t, "restored", f.apply("passed"))
}

func TestQualityRecoveryProbeSharedBatchConflictRetainsRetry(t *testing.T) {
	ctx := context.Background()
	f := newQualityMoveGroupsFixture(t, true)
	f.plan.PelicanConfig.QuestionKind = service.QualityRandomQuestionKind
	enableRecoveryProbe(t, f.qualityBPSFixture)
	// Seed the existing ownership and first normal result. The final confirmation
	// below goes through real collection, restart and shared-batch finalization.
	f.plan.QualityBatchEvaluated = true
	require.Equal(t, "groups_moved", f.apply("failed"))
	claimRecoveryProbe(t, f, false)
	f.plan.QualityBatchEvaluated = true
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
	claimRecoveryProbe(t, f, true)
	f.exec(`INSERT INTO account_groups(account_id,group_id,priority) VALUES($1,$2,99)`, f.sourcePeer)
	before := f.memberships()
	now := time.Now().UTC().Truncate(time.Microsecond)
	batch := &service.QualitySharedBatch{ID: "recovery-conflict-" + uuid.NewString(), StartedAt: now.Add(-time.Minute),
		StartDeadline: now.Add(time.Minute), ClosesAt: now.Add(11 * time.Minute),
		Questions: []service.QualityBatchQuestion{{Prompt: "Synthetic", Expected: "42", Question: &service.QualityRandomQuestion{ID: "fixture", Family: "paths", Version: "fixture"}}}}
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM quality_probe_batches WHERE id=$1`, batch.ID)
	})
	repo := f.plans.(*scheduledTestPlanRepository)
	_, err := repo.StartQualityBatch(ctx, batch, f.plan, f.until)
	require.NoError(t, err)
	cfg, policy := *f.plan.PelicanConfig, *f.plan.PelicanConfig.Quality
	cfg.Quality = &policy
	cfg.ModelID, cfg.Prompt, policy.ExpectedAnswer, cfg.RandomQuestion = f.plan.ModelID, "Synthetic", "42", batch.Questions[0].Question
	cfg.QualityBatch = &service.QualityBatchEvidence{ID: batch.ID, StartedAt: batch.StartedAt, StartDeadline: batch.StartDeadline, ClosesAt: batch.ClosesAt,
		Route: "synthetic", MappedModel: f.plan.ModelID}
	result := &service.ScheduledTestResult{PlanID: f.plan.ID, Status: "success", ResponseText: "42", StartedAt: now, FinishedAt: now.Add(time.Millisecond),
		QualityRoundID: f.until.Format(time.RFC3339Nano), PelicanConfig: &cfg, QualityJudgment: &service.QualityJudgment{Verdict: "correct"}}
	require.NoError(t, repo.CollectQualityBatch(ctx, &service.QualityBatchRun{Apply: true, Plan: f.plan, Until: f.until, Results: []*service.ScheduledTestResult{result}}))
	_, err = integrationDB.ExecContext(ctx, `UPDATE quality_probe_batches SET start_deadline=clock_timestamp()-interval '1 second' WHERE id=$1`, batch.ID)
	require.NoError(t, err)
	restarted := NewScheduledTestPlanRepository(integrationDB).(*scheduledTestPlanRepository)
	require.NoError(t, restarted.FinalizeQualityBatches(ctx))
	var action, status string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT quality_action FROM scheduled_test_results WHERE plan_id=$1`, f.plan.ID).Scan(&action))
	require.Equal(t, "restore_conflict", action)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM quality_probe_batch_runs WHERE batch_id=$1 AND plan_id=$2`, batch.ID, f.plan.ID).Scan(&status))
	require.Equal(t, "finalized", status)
	require.Equal(t, before, f.memberships())
	require.True(t, f.state().RecoveryProbe.PendingLease.IsZero())
	require.WithinDuration(t, time.Now().Add(time.Hour), f.state().RecoveryProbe.NextProbeAt, 3*time.Second)
	requireRecoveryScheduleMatchesState(t, f)
	stored, err := f.plans.GetByID(ctx, f.plan.ID)
	require.NoError(t, err)
	require.Nil(t, stored.RunningUntil)
}

func TestQualityRecoveryProbe5xxOnlyEnabledOwnedRules(t *testing.T) {
	for _, scenario := range []string{"paused", "no_ownership", "trigger_disabled", "other_scope"} {
		t.Run(scenario, func(t *testing.T) {
			f := newQualityMoveGroupsFixture(t, true)
			f.plan.PelicanConfig.Quality.TriggerOnUpstream5xx = true
			enableRecoveryProbe(t, f.qualityBPSFixture)
			if scenario != "no_ownership" {
				require.Equal(t, "groups_moved", f.apply("failed"))
			}
			switch scenario {
			case "paused":
				f.exec(`UPDATE scheduled_test_plans SET enabled=false WHERE account_id=$1`)
			case "trigger_disabled":
				f.exec(`UPDATE scheduled_test_plans SET pelican_config=jsonb_set(pelican_config,'{quality,trigger_on_upstream_5xx}','false') WHERE account_id=$1`)
			case "other_scope":
				_, err := integrationDB.ExecContext(context.Background(), `UPDATE account_quality_states SET state=jsonb_set(state,'{action}','"enable_bps"') WHERE plan_id=$1`, f.plan.ID)
				require.NoError(t, err)
			}
			before := f.snapshot()
			require.NoError(t, (&accountRepository{sql: integrationDB}).ApplyQuality5xx(context.Background(), f.account))
			require.JSONEq(t, before, f.snapshot())
			// Simulate an outer candidate lookup that raced with the pause/edit:
			// the locked inner operation must recheck eligibility too.
			applied, err := f.plans.(*scheduledTestPlanRepository).applyImmediateQuality5xx(context.Background(), f.plan.ID, f.account)
			require.NoError(t, err)
			require.False(t, applied)
			require.JSONEq(t, before, f.snapshot())
		})
	}
}

package repository

import (
	"context"
	"database/sql"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Called only after the outcome/action is accepted, in the same transaction.
// The caller already fences the plan version and lease; failures never publish
// admission independently from their committed isolation/group action.
func persistQualityProbeAdmissionCheck(ctx context.Context, tx *sql.Tx, plan *service.ScheduledTestPlan, outcome, action string) error {
	if plan == nil || !plan.QualityProbeAdmissionChecked || plan.QualityProbeStartedAt == nil || plan.QualityProbeFinishedAt == nil ||
		plan.PelicanConfig == nil || plan.PelicanConfig.Quality == nil ||
		(outcome != "passed" && outcome != "failed") {
		return nil
	}
	q := plan.PelicanConfig.Quality
	if plan.PelicanConfig.TestChannel == "bps" || q.RunsOnPassed() || q.Action == service.QualityActionEnableBPS || q.Action == service.QualityActionOpenAITwoFAAdmission ||
		(plan.PelicanConfig.QuestionKind == service.QualityRandomQuestionKind && !plan.QualityBatchEvaluated) {
		return nil
	}
	switch action {
	case "pending", "comparison_pending", "inconclusive", "stale_run", "action_error", "action_conflict", "restore_conflict", "account_deleted":
		return nil
	}
	started, finished := *plan.QualityProbeStartedAt, *plan.QualityProbeFinishedAt
	if started.IsZero() || finished.IsZero() || finished.Before(started) {
		return nil
	}
	// Observation-only rules do not otherwise lock the account. Maintain the
	// shared plan -> account -> admission lock order used by rule triggers.
	if _, err := tx.ExecContext(ctx, `SELECT id FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, plan.AccountID); err != nil {
		return err
	}
	if plan.QualityTemplateID != 0 {
		matches, err := qualityTemplatePlanMatchesTx(ctx, tx, plan.ID, plan.AccountID)
		if err != nil || !matches {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE account_quality_probe_admissions
 SET checked_started_at=$2, checked_finished_at=$3
 WHERE account_id=$1 AND enabled AND (required_after IS NULL OR required_after <= $2)
 AND $3 >= $2 AND $3 <= clock_timestamp()
 AND (checked_started_at IS NULL OR checked_started_at < $2
      OR (checked_started_at=$2 AND checked_finished_at < $3))
 AND EXISTS (SELECT 1 FROM scheduled_test_plans WHERE id=$4 AND account_id=$1
     AND enabled AND NOT scope_suspended AND quality_probe_admission_main_rule(pelican_config))`, plan.AccountID, started, finished, plan.ID)
	return err
}

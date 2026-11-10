package repository

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Only a committed, newly applied quarantine interrupts traffic. In particular,
// a recovery rule also returns groups_moved, and repeated failures in the
// quarantine pool must leave that pool's newly admitted requests alone.
func shouldInterruptQualityRequests(plan *service.ScheduledTestPlan, outcome, action string) bool {
	if plan == nil || plan.PelicanConfig == nil || plan.PelicanConfig.Quality == nil ||
		plan.PelicanConfig.Quality.RunsOnPassed() || outcome != "failed" {
		return false
	}
	switch action {
	case "groups_moved", "groups_removed", "scheduling_disabled":
		return true
	default:
		return false
	}
}

func persistQualityRequestWindow(ctx context.Context, tx *sql.Tx, plan *service.ScheduledTestPlan, outcome, action string) error {
	if plan.PelicanConfig == nil || plan.PelicanConfig.Quality == nil || plan.PelicanConfig.Quality.RunsOnPassed() {
		return nil
	}
	if outcome == "passed" && plan.QualityProbeFinishedAt != nil && !plan.QualityProbeFinishedAt.IsZero() {
		_, err := tx.ExecContext(ctx, `INSERT INTO account_quality_probe_checkpoints(plan_id,account_id,plan_version,healthy_at)
 VALUES($1,$2,$3,$4) ON CONFLICT(plan_id) DO UPDATE SET plan_version=EXCLUDED.plan_version,
 healthy_at=CASE WHEN account_quality_probe_checkpoints.plan_version=EXCLUDED.plan_version
 THEN GREATEST(account_quality_probe_checkpoints.healthy_at,EXCLUDED.healthy_at) ELSE EXCLUDED.healthy_at END`,
			plan.ID, plan.AccountID, plan.UpdatedAt, *plan.QualityProbeFinishedAt)
		return err
	}
	if !shouldInterruptQualityRequests(plan, outcome, action) {
		return nil
	}
	end := time.Now()
	if plan.QualityProbeFinishedAt != nil && !plan.QualityProbeFinishedAt.IsZero() {
		end = *plan.QualityProbeFinishedAt
	}
	end = end.UTC().Truncate(time.Microsecond)
	var healthy sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT healthy_at FROM account_quality_probe_checkpoints
 WHERE plan_id=$1 AND account_id=$2 AND plan_version=$3 AND healthy_at<$4`, plan.ID, plan.AccountID, plan.UpdatedAt, end).Scan(&healthy)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if !healthy.Valid {
		// Upgrade fallback: only whole, explicitly successful prior rounds count.
		// Never infer a healthy boundary from an unknown/skipped/latest-run time.
		err = tx.QueryRowContext(ctx, `SELECT MAX(finished) FROM (
 SELECT MAX(finished_at) AS finished FROM scheduled_test_results
 WHERE plan_id=$1 AND finished_at<$2 AND started_at>=$3
 AND quality_action NOT IN ('','stale_run','action_error','action_conflict','comparison_pending')
 GROUP BY COALESCE(NULLIF(quality_round_id,''),'legacy:'||id::text)
 HAVING BOOL_AND(status='success' AND COALESCE(quality_judgment->>'verdict','')='correct')
 AND COUNT(*) >= MAX(GREATEST(COALESCE((pelican_config->>'parallel_count')::int,1),1)
 * GREATEST(COALESCE(jsonb_array_length(pelican_config->'model_ids'),1),1))) rounds`, plan.ID, end, plan.UpdatedAt).Scan(&healthy)
		if err != nil {
			return err
		}
	}
	start := end.Add(-time.Minute)
	if healthy.Valid {
		start = healthy.Time
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO account_quality_request_windows(account_id,plan_id,window_start,window_end)
 VALUES($1,$2,$3,$4) ON CONFLICT(plan_id,window_end) DO NOTHING`, plan.AccountID, plan.ID, start, end)
	if err == nil {
		plan.QualityRequestWindowStart, plan.QualityRequestWindowEnd = &start, &end
	}
	return err
}

func interruptQualityRequests(ctx context.Context, plan *service.ScheduledTestPlan, outcome, action string) {
	if !shouldInterruptQualityRequests(plan, outcome, action) || plan.QualityRequestWindowStart == nil || plan.QualityRequestWindowEnd == nil {
		return
	}
	// Completion of the probe or its save deadline must not suppress a committed
	// quarantine's stop signal. Local cancellation happens before Redis I/O.
	publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	count, err := accountrequest.Default.InterruptWindow(publishCtx, plan.AccountID, *plan.QualityRequestWindowStart, *plan.QualityRequestWindowEnd)
	if err != nil {
		slog.Warn("quality request interruption broadcast failed", "account_id", plan.AccountID, "plan_id", plan.ID, "error", err)
	}
	slog.Info("quality quarantine interrupted account requests", "account_id", plan.AccountID, "plan_id", plan.ID, "action", action,
		"window_start", plan.QualityRequestWindowStart, "window_end", plan.QualityRequestWindowEnd, "local_requests", count)
}

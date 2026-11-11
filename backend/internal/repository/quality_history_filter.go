package repository

import (
	"context"
	"encoding/json"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// Filter at the input to the window functions, before round grouping and LIMIT.
func (r *scheduledTestResultRepository) ListFilteredQualityHistory(ctx context.Context, beforeID int64, limit int, filter service.QualityHistoryFilter) ([]*service.QualityHistoryResult, error) {
	rows, err := r.db.QueryContext(ctx, `WITH rounds AS (
 SELECT r.*, a.id AS account_id,a.name AS account_name, COALESCE(p.source_template_id,0) AS source_template_id,
 row_number() OVER round_window AS row_in_round,
 count(*) FILTER (WHERE r.status='success') OVER round_window AS passed_count,
 count(*) FILTER (WHERE r.status='skipped') OVER round_window AS skipped_count,
 GREATEST(count(*) OVER round_window,COALESCE((r.pelican_config->>'parallel_count')::int,0)) AS total_count,
 array_agg(r.id) OVER round_window AS result_ids,
 min(r.started_at) OVER round_window AS round_started_at,
 max(r.finished_at) OVER round_window AS round_finished_at,
 bool_and(r.status='success') OVER round_window AS all_passed,
 bool_or(r.error_message='answer_mismatch') OVER round_window AS any_wrong
 FROM scheduled_test_results r JOIN scheduled_test_plans p ON p.id=r.plan_id JOIN accounts a ON a.id=p.account_id
 WHERE r.pelican_config->'quality' IS NOT NULL AND a.deleted_at IS NULL
 AND ($3::bigint=0 OR p.source_template_id=$3)
 AND ($4::bigint=0 OR r.plan_id=$4)
 AND (NOT $5::boolean OR NOT EXISTS (SELECT 1 FROM quality_rule_template_accounts l WHERE l.plan_id=p.id))
 WINDOW round_window AS (PARTITION BY r.plan_id,COALESCE(NULLIF(r.quality_round_id,''),r.id::text) ORDER BY r.id DESC ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)
 ) SELECT id,plan_id,CASE WHEN all_passed THEN 'success' ELSE 'failed' END,
 CASE WHEN any_wrong THEN 'answer_mismatch' ELSE error_message END,
 latency_ms,round_started_at,round_finished_at,created_at,pelican_config,quality_action,quality_judgment,account_id,account_name,passed_count,total_count,skipped_count,result_ids,source_template_id
 FROM rounds WHERE row_in_round=1 AND ($1::bigint=0 OR id<$1) ORDER BY id DESC LIMIT $2`, beforeID, limit, filter.TemplateID, filter.PlanID, filter.Unassigned)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]*service.QualityHistoryResult, 0)
	for rows.Next() {
		item := &service.QualityHistoryResult{}
		var cfg, judgment []byte
		if err := rows.Scan(&item.ID, &item.PlanID, &item.Status, &item.ErrorMessage, &item.LatencyMs, &item.StartedAt, &item.FinishedAt, &item.CreatedAt, &cfg, &item.QualityAction, &judgment, &item.AccountID, &item.AccountName, &item.PassedCount, &item.TotalCount, &item.SkippedCount, pq.Array(&item.ResultIDs), &item.SourceTemplateID); err != nil {
			return nil, err
		}
		if len(cfg) > 0 {
			if err := json.Unmarshal(cfg, &item.PelicanConfig); err != nil {
				return nil, err
			}
		}
		if len(judgment) > 0 {
			if err := json.Unmarshal(judgment, &item.QualityJudgment); err != nil {
				return nil, err
			}
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

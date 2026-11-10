package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// enrichUsageQualityWindows joins only the current page's real request starts.
// Reading the immutable windows also covers logs saved before detection and logs
// written after detection; no update of usage_logs is needed or raced against.
func (r *usageLogRepository) enrichUsageQualityWindows(ctx context.Context, logs []service.UsageLog) (err error) {
	var values []string
	var args []any
	for i := range logs {
		logs[i].QualityWindow = nil
		if logs[i].RequestStartedAt == nil {
			continue
		}
		values = append(values, fmt.Sprintf("($%d::integer, $%d::bigint, $%d::timestamptz)", len(args)+1, len(args)+2, len(args)+3))
		args = append(args, i, logs[i].AccountID, *logs[i].RequestStartedAt)
	}
	if len(values) == 0 {
		return nil
	}
	query := `WITH page (position, account_id, request_started_at) AS (VALUES ` + strings.Join(values, ",") + `)
		SELECT page.position, matched.window_start, matched.window_end
		FROM page
		CROSS JOIN LATERAL (
			SELECT w.window_start, w.window_end
			FROM account_quality_request_windows w
			WHERE w.account_id = page.account_id
			  AND page.request_started_at > w.window_start
			  AND page.request_started_at <= w.window_end
			ORDER BY w.window_end DESC, w.window_start DESC, w.id DESC
			LIMIT 1
		) matched`
	rows, err := r.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
	}()
	for rows.Next() {
		var position int
		var evidence service.QualityWindowEvidence
		if err := rows.Scan(&position, &evidence.StartAt, &evidence.EndAt); err != nil {
			return err
		}
		evidence.Interrupted = logs[position].QualityInterrupted
		logs[position].QualityWindow = &evidence
	}
	return rows.Err()
}

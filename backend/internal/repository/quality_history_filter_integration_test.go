//go:build integration

package repository

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestQualityHistoryFiltersBeforeRoundsAndPagination(t *testing.T) {
	f := newQualityReincludeFixture(t)
	updated, err := f.svc.UpdateQualityTemplate(f.ctx, f.tpl, f.account)
	require.NoError(t, err)
	tpl := updated.Template
	plan, err := f.plans.GetByID(f.ctx, tpl.PlanIDs[0])
	require.NoError(t, err)
	repo := NewScheduledTestResultRepository(integrationDB)
	history := repo.(service.FilteredQualityHistoryRepository)
	cfg := *plan.PelicanConfig
	cfg.ModelIDs = []string{"model-one", "model-two"}
	cfg.ParallelCount = 2
	for _, status := range []string{"success", "success", "success", "skipped"} {
		_, err := repo.Create(f.ctx, &service.ScheduledTestResult{PlanID: plan.ID, Status: status, QualityRoundID: "shared-round", StartedAt: time.Now(), FinishedAt: time.Now(), PelicanConfig: &cfg})
		require.NoError(t, err)
	}
	empty, err := f.svc.ListFilteredQualityHistory(f.ctx, 0, service.QualityHistoryFilter{})
	require.NoError(t, err)
	require.Len(t, empty.Items, 1)
	require.Equal(t, tpl.ID, empty.Items[0].SourceTemplateID)
	scoped, err := f.svc.ListFilteredQualityHistory(f.ctx, 0, service.QualityHistoryFilter{TemplateID: tpl.ID})
	require.NoError(t, err)
	require.Equal(t, scoped.Items[0].SourceTemplateID, empty.Items[0].SourceTemplateID)
	// A manual rule produces enough newer rows to crowd the global first page.
	manual, _ := f.prepare(f.ctx, service.QualityTemplateAccount{ID: f.account})
	noisy, err := f.plans.Create(f.ctx, manual)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(f.ctx, `INSERT INTO scheduled_test_results(plan_id,status,response_text,error_message,latency_ms,started_at,finished_at,pelican_config,quality_round_id)
 SELECT $1,'success','','',1,NOW(),NOW(),$2::jsonb,'noise-'||g FROM generate_series(1,130) g`, noisy.ID, marshalPelicanConfig(noisy.PelicanConfig))
	require.NoError(t, err)
	page, err := f.svc.ListFilteredQualityHistory(f.ctx, 0, service.QualityHistoryFilter{TemplateID: tpl.ID})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Zero(t, page.NextCursor)
	round := page.Items[0]
	require.Equal(t, plan.ID, round.PlanID)
	require.Equal(t, tpl.ID, round.SourceTemplateID)
	require.Len(t, round.ResultIDs, 4)
	require.Equal(t, 3, round.PassedCount)
	require.Equal(t, 1, round.SkippedCount)
	require.Equal(t, 3, round.TotalCount)
	require.Equal(t, "success", round.Status)
	// A cursor between individual samples must not cut up the aggregate.
	rows, err := history.ListFilteredQualityHistory(f.ctx, round.ID, 100, service.QualityHistoryFilter{TemplateID: tpl.ID})
	require.NoError(t, err)
	require.Empty(t, rows)
	mismatch, err := history.ListFilteredQualityHistory(f.ctx, 0, 100, service.QualityHistoryFilter{TemplateID: tpl.ID, PlanID: noisy.ID})
	require.NoError(t, err)
	require.Empty(t, mismatch)
	first, err := f.svc.ListFilteredQualityHistory(f.ctx, 0, service.QualityHistoryFilter{Unassigned: true, PlanID: noisy.ID})
	require.NoError(t, err)
	require.Len(t, first.Items, 100)
	require.NotZero(t, first.NextCursor)
	second, err := f.svc.ListFilteredQualityHistory(f.ctx, first.NextCursor, service.QualityHistoryFilter{Unassigned: true, PlanID: noisy.ID})
	require.NoError(t, err)
	require.Len(t, second.Items, 30)
	require.Zero(t, second.NextCursor)
	require.Less(t, second.Items[0].ID, first.NextCursor)
	// Legacy no-filter callers retain the global feed.
	global, err := f.svc.ListQualityHistory(f.ctx, 0)
	require.NoError(t, err)
	require.Len(t, global.Items, 100)
	for _, item := range global.Items {
		require.Equal(t, noisy.ID, item.PlanID)
	}
}

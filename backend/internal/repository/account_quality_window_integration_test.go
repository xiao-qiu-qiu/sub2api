//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func beginQualityRequestAt(t *testing.T, accountID int64, started time.Time) context.Context {
	t.Helper()
	ctx, release := accountrequest.Default.Track(accountrequest.WithStartedAt(context.Background(), started), accountID)
	t.Cleanup(release)
	return ctx
}

func TestQualityWindowPreservesRequestsBeforeHealthyProbe(t *testing.T) {
	f := newQualityMoveGroupsFixture(t, true)
	end := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Microsecond)
	healthy := end.Add(-time.Minute)
	f.plan.QualityProbeFinishedAt = &healthy
	require.Equal(t, "passed", f.apply("passed"))
	old := beginQualityRequestAt(t, f.account, healthy.Add(-time.Second))
	edge := beginQualityRequestAt(t, f.account, healthy)
	inside := beginQualityRequestAt(t, f.account, healthy.Add(time.Second))
	last := beginQualityRequestAt(t, f.account, end)
	afterProbe := beginQualityRequestAt(t, f.account, end.Add(time.Microsecond))
	f.plan.QualityProbeFinishedAt = &end
	require.Equal(t, "groups_moved", f.apply("failed"))
	requireQualityRequestActive(t, old)
	requireQualityRequestActive(t, edge)
	requireQualityRequestInterrupted(t, inside)
	requireQualityRequestInterrupted(t, last)
	requireQualityRequestActive(t, afterProbe)
	var start, finish time.Time
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT window_start,window_end FROM account_quality_request_windows WHERE plan_id=$1`, f.plan.ID).Scan(&start, &finish))
	require.True(t, healthy.Equal(start))
	require.True(t, end.Equal(finish), "transaction and template-sync delays must not extend the probe window")
	fresh := beginQualityRequestAt(t, f.account, time.Now())
	require.Equal(t, "already_quarantined", f.apply("failed"))
	requireQualityRequestActive(t, fresh)
}

func TestQualityWindowNoHealthyProbeUsesOneMinute(t *testing.T) {
	f := newQualityMoveGroupsFixture(t, true)
	end := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
	f.plan.QualityProbeFinishedAt = &end
	old := beginQualityRequestAt(t, f.account, end.Add(-61*time.Second))
	inside := beginQualityRequestAt(t, f.account, end.Add(-30*time.Second))
	require.Equal(t, "groups_moved", f.apply("failed"))
	requireQualityRequestActive(t, old)
	requireQualityRequestInterrupted(t, inside)
	require.Equal(t, end.Add(-time.Minute), *f.plan.QualityRequestWindowStart)
}

func TestQualityWindowUnknownDoesNotAdvanceHealthyCheckpoint(t *testing.T) {
	f := newQualityMoveGroupsFixture(t, true)
	end := time.Now().UTC().Truncate(time.Microsecond)
	healthy := end.Add(-2 * time.Minute)
	f.plan.QualityProbeFinishedAt = &healthy
	require.Equal(t, "passed", f.apply("passed"))
	unknown := end.Add(-time.Minute)
	f.plan.QualityProbeFinishedAt = &unknown
	require.Equal(t, "inconclusive", f.apply("inconclusive"))
	inside := beginQualityRequestAt(t, f.account, healthy.Add(30*time.Second))
	f.plan.QualityProbeFinishedAt = &end
	require.Equal(t, "groups_moved", f.apply("failed"))
	requireQualityRequestInterrupted(t, inside)
	require.Equal(t, healthy, *f.plan.QualityRequestWindowStart)
}

func TestQualityWindowLoadsPreviousSuccessfulRoundOnUpgrade(t *testing.T) {
	f := newQualityMoveGroupsFixture(t, true)
	healthy := f.plan.UpdatedAt.Add(time.Second).UTC().Truncate(time.Microsecond)
	end := healthy.Add(2 * time.Minute)
	_, err := NewScheduledTestResultRepository(integrationDB).Create(t.Context(), &service.ScheduledTestResult{
		PlanID: f.plan.ID, Status: "success", StartedAt: healthy.Add(-time.Millisecond), FinishedAt: healthy,
		PelicanConfig: f.plan.PelicanConfig, QualityJudgment: &service.QualityJudgment{Verdict: "correct"}, QualityAction: "passed", QualityRoundID: "old-healthy",
	})
	require.NoError(t, err)
	old := beginQualityRequestAt(t, f.account, healthy.Add(-time.Second))
	inside := beginQualityRequestAt(t, f.account, healthy.Add(time.Second))
	f.plan.QualityProbeFinishedAt = &end
	require.Equal(t, "groups_moved", f.apply("failed"))
	requireQualityRequestActive(t, old)
	requireQualityRequestInterrupted(t, inside)
	require.True(t, healthy.Equal(*f.plan.QualityRequestWindowStart))
}

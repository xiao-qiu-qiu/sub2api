package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQualityOverviewRoundStatus(t *testing.T) {
	for _, tc := range []struct {
		name  string
		round QualityOverviewRoundEvidence
		want  string
	}{
		{"passed", QualityOverviewRoundEvidence{Complete: true, Samples: 1, ExpectedSamples: 1, Passed: 1}, "healthy"},
		{"wrong answer", QualityOverviewRoundEvidence{Complete: true, Samples: 1, ExpectedSamples: 1, Failed: 1}, "degraded"},
		{"HTTP failure is inconclusive", QualityOverviewRoundEvidence{Complete: true, Samples: 1, ExpectedSamples: 1}, "unknown"},
		{"no passing peer", QualityOverviewRoundEvidence{Complete: true, Samples: 1, ExpectedSamples: 1}, "unknown"},
		{"uncommitted comparison", QualityOverviewRoundEvidence{Samples: 1, ExpectedSamples: 1, Passed: 1}, "unknown"},
		{"partial multi model", QualityOverviewRoundEvidence{Complete: true, Samples: 1, ExpectedSamples: 2, Passed: 1}, "unknown"},
		{"partial wrong answer", QualityOverviewRoundEvidence{Complete: true, Samples: 1, ExpectedSamples: 2, Failed: 1}, "unknown"},
		{"all models passed", QualityOverviewRoundEvidence{Complete: true, Samples: 4, ExpectedSamples: 4, Passed: 4}, "healthy"},
		{"one model wrong", QualityOverviewRoundEvidence{Complete: true, Samples: 2, ExpectedSamples: 2, Passed: 1, Failed: 1}, "degraded"},
		{"wrong with transport error", QualityOverviewRoundEvidence{Complete: true, Samples: 2, ExpectedSamples: 2, Failed: 1}, "degraded"},
		{"passed with transport error", QualityOverviewRoundEvidence{Complete: true, Samples: 2, ExpectedSamples: 2, Passed: 1}, "unknown"},
		{"unsupported model skipped", QualityOverviewRoundEvidence{Complete: true, Samples: 2, ExpectedSamples: 2, Passed: 1, Skipped: 1}, "healthy"},
		{"all skipped", QualityOverviewRoundEvidence{Complete: true, Samples: 2, ExpectedSamples: 2, Skipped: 2}, "unknown"},
		{"missing route", QualityOverviewRoundEvidence{Samples: 1, ExpectedSamples: 1, Passed: 1}, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) { require.Equal(t, tc.want, qualityOverviewRoundStatus(tc.round)) })
	}
}

type overviewResultsStub struct {
	ScheduledTestResultRepository
	filter  QualityOverviewFilter
	records []QualityOverviewAccountEvidence
	total   int64
}

func (r *overviewResultsStub) ReadQualityOverview(_ context.Context, f QualityOverviewFilter) ([]QualityOverviewAccountEvidence, int64, error) {
	r.filter = f
	return r.records, r.total, nil
}

func TestQualityOverviewPageRatesAndLatest(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	round := func(id int64, passed, failed int) QualityOverviewRoundEvidence {
		return QualityOverviewRoundEvidence{QualityOverviewHistory: QualityOverviewHistory{ID: id, StartedAt: now.Add(time.Duration(id) * time.Minute)},
			Complete: true, Samples: 1, ExpectedSamples: 1, Passed: passed, Failed: failed}
	}
	repo := &overviewResultsStub{total: 42, records: []QualityOverviewAccountEvidence{
		{AccountID: 1, Rounds: []QualityOverviewRoundEvidence{round(1, 1, 0), round(2, 0, 1), round(3, 0, 0)}},
		{AccountID: 2},
		{AccountID: 3, Rounds: []QualityOverviewRoundEvidence{round(4, 0, 0)}},
	}}
	page, err := NewScheduledTestService(nil, repo).AccountQualityOverview(t.Context(), QualityOverviewFilter{Page: 2, PageSize: 3})
	require.NoError(t, err)
	require.EqualValues(t, 42, page.Total)
	require.Equal(t, 2, page.Page)
	require.Equal(t, 3, page.PageSize)
	require.Equal(t, "account", repo.filter.TestChannel)
	require.Equal(t, "all", repo.filter.ProbeScope)
	require.Equal(t, "unknown", page.Items[0].Status, "latest unknown must not reuse an older pass")
	require.Equal(t, 50.0, *page.Items[0].HealthyRate)
	require.Equal(t, now.Add(3*time.Minute), *page.Items[0].LastCheckedAt)
	require.Equal(t, []int64{1, 2, 3}, []int64{page.Items[0].History[0].ID, page.Items[0].History[1].ID, page.Items[0].History[2].ID})
	require.Equal(t, "untested", page.Items[1].Status)
	require.Nil(t, page.Items[1].HealthyRate)
	require.Nil(t, page.Items[1].LastCheckedAt)
	require.Nil(t, page.Items[2].HealthyRate)
	raw, err := json.Marshal(page.Items[1])
	require.NoError(t, err)
	require.Contains(t, string(raw), `"groups":[]`)
	require.Contains(t, string(raw), `"plans":[]`)
	require.Contains(t, string(raw), `"history":[]`)
	require.Contains(t, string(raw), `"healthy_rate":null`)
}

func TestQualityOverviewFilterBounds(t *testing.T) {
	valid := QualityOverviewFilter{}
	require.NoError(t, valid.Normalize())
	require.Equal(t, 20, valid.PageSize)
	require.Equal(t, 1, valid.Page)
	require.Equal(t, "all", valid.ProbeScope)
	for _, scope := range []string{"all", "main", "recovery"} {
		f := QualityOverviewFilter{ProbeScope: scope}
		require.NoError(t, f.Normalize())
		require.Equal(t, scope, f.ProbeScope)
	}
	negative := int64(-1)
	for _, f := range []QualityOverviewFilter{{Page: -1}, {Page: 1000001}, {PageSize: 101}, {PageSize: -2}, {TestChannel: "all"}, {ProbeScope: "quarantine"}, {ProbeScope: "MAIN"}, {GroupID: &negative}, {Search: string(make([]byte, 101))}} {
		require.Error(t, f.Normalize())
	}
}

func TestQualityOverviewPassesScopeAndMarksPlanActionOn(t *testing.T) {
	repo := &overviewResultsStub{total: 1, records: []QualityOverviewAccountEvidence{{
		AccountID: 9, Plans: []QualityOverviewPlan{{ID: 1}, {ID: 2, ActionOn: "passed"}},
	}}}
	page, err := NewScheduledTestService(nil, repo).AccountQualityOverview(t.Context(), QualityOverviewFilter{ProbeScope: "recovery", TestChannel: "bps"})
	require.NoError(t, err)
	require.Equal(t, "recovery", repo.filter.ProbeScope)
	require.Equal(t, "bps", repo.filter.TestChannel)
	require.Equal(t, "failed", page.Items[0].Plans[0].ActionOn)
	require.Equal(t, "passed", page.Items[0].Plans[1].ActionOn)
	raw, err := json.Marshal(page.Items[0].Plans)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"action_on":"failed"`)
	require.Contains(t, string(raw), `"action_on":"passed"`)
}

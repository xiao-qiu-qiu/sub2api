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
	require.Equal(t, 2, page.Items[0].EvaluatedRounds)
	require.Equal(t, 3, page.Items[0].TotalRounds)
	require.Equal(t, now.Add(3*time.Minute), *page.Items[0].LastCheckedAt)
	require.Equal(t, []int64{1, 2, 3}, []int64{page.Items[0].History[0].ID, page.Items[0].History[1].ID, page.Items[0].History[2].ID})
	require.Equal(t, "untested", page.Items[1].Status)
	require.Nil(t, page.Items[1].HealthyRate)
	require.Nil(t, page.Items[1].LastCheckedAt)
	require.Nil(t, page.Items[2].HealthyRate)
	require.Zero(t, page.Items[1].EvaluatedRounds)
	require.Zero(t, page.Items[1].TotalRounds)
	require.Zero(t, page.Items[2].EvaluatedRounds)
	require.Equal(t, 1, page.Items[2].TotalRounds)
	raw, err := json.Marshal(page.Items[1])
	require.NoError(t, err)
	require.Contains(t, string(raw), `"groups":[]`)
	require.Contains(t, string(raw), `"plans":[]`)
	require.Contains(t, string(raw), `"history":[]`)
	require.Contains(t, string(raw), `"healthy_rate":null`)
	require.Contains(t, string(raw), `"evaluated_rounds":0`)
	require.Contains(t, string(raw), `"total_rounds":0`)
}

func TestQualityOverviewRoundReasons(t *testing.T) {
	for _, tc := range []struct {
		name  string
		round QualityOverviewRoundEvidence
		want  []string
	}{
		{"quota skipped", QualityOverviewRoundEvidence{Complete: true, Samples: 1, ExpectedSamples: 1, Skipped: 1, QuotaExhausted: true}, []string{"quota_exhausted"}},
		{"quota without committed action", QualityOverviewRoundEvidence{Samples: 1, ExpectedSamples: 1, Skipped: 1, QuotaExhausted: true}, []string{"quota_exhausted"}},
		{"Astra passed Sol limited", QualityOverviewRoundEvidence{Complete: true, Samples: 2, ExpectedSamples: 2, Passed: 1, RateLimited: true}, []string{"rate_limited"}},
		{"both passed stale", QualityOverviewRoundEvidence{Samples: 2, ExpectedSamples: 2, Passed: 2, StaleResult: true}, []string{"stale_result"}},
		{"earlier stale latest observed", QualityOverviewRoundEvidence{QualityOverviewHistory: QualityOverviewHistory{QualityAction: "observed"}, Samples: 2, ExpectedSamples: 2, Passed: 2, StaleResult: true}, []string{"stale_result"}},
		{"specific reasons ordered", QualityOverviewRoundEvidence{QualityOverviewHistory: QualityOverviewHistory{RouteEvidenceMissing: true}, Samples: 4, ExpectedSamples: 4, StaleResult: true, QuotaExhausted: true, RateLimited: true, ComparisonPending: true}, []string{"stale_result", "quota_exhausted", "rate_limited", "route_unverified", "comparison_pending"}},
		{"missing route", QualityOverviewRoundEvidence{QualityOverviewHistory: QualityOverviewHistory{RouteEvidenceMissing: true}, Samples: 1, ExpectedSamples: 1, Passed: 1}, []string{"route_unverified"}},
		{"pending comparison", QualityOverviewRoundEvidence{Samples: 1, ExpectedSamples: 1, Passed: 1, ComparisonPending: true}, []string{"comparison_pending"}},
		{"partial round", QualityOverviewRoundEvidence{Complete: true, Samples: 1, ExpectedSamples: 2, Passed: 1}, []string{"incomplete_round"}},
		{"invalid structure", QualityOverviewRoundEvidence{Samples: 2, ExpectedSamples: 2, Passed: 2, IncompleteRound: true}, []string{"incomplete_round"}},
		{"independent missing sample and stale", QualityOverviewRoundEvidence{Samples: 1, ExpectedSamples: 2, Passed: 1, StaleResult: true}, []string{"stale_result", "incomplete_round"}},
		{"independent failure and rate limit", QualityOverviewRoundEvidence{Complete: true, Samples: 2, ExpectedSamples: 2, RateLimited: true, ProbeFailed: true}, []string{"rate_limited", "probe_failed"}},
		{"HTTP 503", QualityOverviewRoundEvidence{Complete: true, Samples: 1, ExpectedSamples: 1, ProbeFailed: true}, []string{"probe_failed"}},
		{"generic terminal error", QualityOverviewRoundEvidence{Complete: true, Samples: 1, ExpectedSamples: 1, ProbeFailed: true}, []string{"probe_failed"}},
		{"skipped", QualityOverviewRoundEvidence{Complete: true, Samples: 1, ExpectedSamples: 1, Skipped: 1, UnexplainedSkipped: true}, []string{"skipped"}},
		{"skipped without committed action", QualityOverviewRoundEvidence{Samples: 1, ExpectedSamples: 1, Skipped: 1, UnexplainedSkipped: true}, []string{"skipped"}},
		{"quota and independent skip", QualityOverviewRoundEvidence{Complete: true, Samples: 2, ExpectedSamples: 2, Skipped: 2, QuotaExhausted: true, UnexplainedSkipped: true}, []string{"quota_exhausted", "skipped"}},
		{"no passing peer", QualityOverviewRoundEvidence{Complete: true, Samples: 1, ExpectedSamples: 1}, []string{"inconclusive"}},
		{"healthy omits reasons", QualityOverviewRoundEvidence{Complete: true, Samples: 1, ExpectedSamples: 1, Passed: 1, RateLimited: true}, nil},
		{"degraded omits reasons", QualityOverviewRoundEvidence{Complete: true, Samples: 1, ExpectedSamples: 1, Failed: 1, RateLimited: true}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := qualityOverviewRoundStatus(tc.round)
			require.Equal(t, tc.want, qualityOverviewRoundReasons(tc.round, status))
		})
	}
}

func TestQualityOverviewReasonsPrivacyAndJSON(t *testing.T) {
	// Even an unexpected repository-supplied list must be replaced by the
	// service's fixed enums, including on healthy/degraded history.
	rounds := []QualityOverviewRoundEvidence{
		{Complete: true, Samples: 1, ExpectedSamples: 1, Passed: 1},
		{Complete: true, Samples: 1, ExpectedSamples: 1, Failed: 1},
		{Complete: true, Samples: 1, ExpectedSamples: 1, ProbeFailed: true},
	}
	for i := range rounds {
		rounds[i].ReasonCodes = []string{"PRIVATE_ERROR", "PRIVATE_ANSWER", "PRIVATE_IDENTITY", "PRIVATE_TOKEN"}
	}
	repo := &overviewResultsStub{records: []QualityOverviewAccountEvidence{{Rounds: rounds}}}
	page, err := NewScheduledTestService(nil, repo).AccountQualityOverview(t.Context(), QualityOverviewFilter{})
	require.NoError(t, err)
	require.Equal(t, 2, page.Items[0].EvaluatedRounds)
	require.Equal(t, 3, page.Items[0].TotalRounds)
	require.Nil(t, page.Items[0].History[0].ReasonCodes)
	require.Nil(t, page.Items[0].History[1].ReasonCodes)
	require.Equal(t, []string{"probe_failed"}, page.Items[0].History[2].ReasonCodes)
	raw, err := json.Marshal(page)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "PRIVATE")
	require.NotContains(t, string(raw), `"probe_failed":`)
	for _, history := range page.Items[0].History[:2] {
		raw, err := json.Marshal(history)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "reason_codes")
	}
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

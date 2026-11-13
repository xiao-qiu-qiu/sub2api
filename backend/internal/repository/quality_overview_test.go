package repository

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestQualityOverviewSingleBoundedRead(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	group := int64(23)
	mock.ExpectQuery(qualityOverviewSQL).WithArgs(`_a%\`, `%\_a\%\\%`, group, 20, int64(20), "account", "all").
		WillReturnRows(sqlmock.NewRows([]string{"total", "items"}).AddRow(28, `[{"account_id":4,"groups":[],"plans":[],"rounds":[]}]`))
	repo, ok := NewScheduledTestResultRepository(db).(service.QualityOverviewRepository)
	require.True(t, ok)
	items, total, err := repo.ReadQualityOverview(t.Context(), service.QualityOverviewFilter{Page: 2, PageSize: 20, Search: `_a%\`, GroupID: &group})
	require.NoError(t, err)
	require.EqualValues(t, 28, total)
	require.EqualValues(t, 4, items[0].AccountID)
	require.NoError(t, mock.ExpectationsWereMet(), "one query must include groups, plans and histories")
}

func TestQualityOverviewEmptyPageRetainsTotal(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectQuery("WITH eligible_plans AS NOT MATERIALIZED").WithArgs("", "%%", nil, 20, int64(180), "bps", "recovery").
		WillReturnRows(sqlmock.NewRows([]string{"total", "items"}).AddRow(3, `[]`))
	repo, ok := NewScheduledTestResultRepository(db).(service.QualityOverviewRepository)
	require.True(t, ok)
	items, total, err := repo.ReadQualityOverview(t.Context(), service.QualityOverviewFilter{Page: 10, TestChannel: "bps", ProbeScope: "recovery"})
	require.NoError(t, err)
	require.Empty(t, items)
	require.EqualValues(t, 3, total)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestQualityOverviewAggregatedReasonFacts(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	// The single bounded read decodes facts, while the service owns the public
	// reason enum and denominator. No raw error field is needed by the service.
	mock.ExpectQuery(qualityOverviewSQL).WithArgs("", "%%", nil, 20, int64(0), "account", "all").
		WillReturnRows(sqlmock.NewRows([]string{"total", "items"}).AddRow(1, `[{"account_id":4,"rounds":[
		 {"samples":1,"expected_samples":1,"passed":1,"complete":true},
		 {"samples":5,"expected_samples":5,"complete":false,"stale_result":true,"quota_exhausted":true,
		  "rate_limited":true,"route_evidence_missing":true,"comparison_pending":true}]}]`))
	page, err := service.NewScheduledTestService(nil, NewScheduledTestResultRepository(db)).AccountQualityOverview(t.Context(), service.QualityOverviewFilter{})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	item := page.Items[0]
	require.Equal(t, "unknown", item.Status)
	require.Equal(t, 100.0, *item.HealthyRate)
	require.Equal(t, 1, item.EvaluatedRounds)
	require.Equal(t, 2, item.TotalRounds)
	require.Empty(t, item.History[0].ReasonCodes)
	require.Equal(t, []string{"stale_result", "quota_exhausted", "rate_limited", "route_unverified", "comparison_pending"}, item.History[1].ReasonCodes)
	require.NoError(t, mock.ExpectationsWereMet())
}

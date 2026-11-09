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

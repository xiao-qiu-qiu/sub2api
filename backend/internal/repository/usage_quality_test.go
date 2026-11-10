package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageQualityEnrichBatchesPageAndSkipsUnknownStarts(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	logs := []service.UsageLog{
		{AccountID: 11, RequestStartedAt: &end, QualityInterrupted: true},
		{AccountID: 12, CreatedAt: end, QualityWindow: &service.QualityWindowEvidence{}},
		{AccountID: 13, RequestStartedAt: &end},
	}
	mock.ExpectQuery(`(?s)WITH page .*account_quality_request_windows.*page.request_started_at > w.window_start.*page.request_started_at <= w.window_end.*ORDER BY w.window_end DESC, w.window_start DESC, w.id DESC`).
		WithArgs(0, int64(11), end, 2, int64(13), end).
		WillReturnRows(sqlmock.NewRows([]string{"position", "window_start", "window_end"}).
			AddRow(0, start, end).AddRow(2, start, end)).RowsWillBeClosed()
	require.NoError(t, repo.enrichUsageQualityWindows(context.Background(), logs))
	require.Equal(t, &service.QualityWindowEvidence{StartAt: start, EndAt: end, Interrupted: true}, logs[0].QualityWindow)
	require.Nil(t, logs[1].QualityWindow)
	require.False(t, logs[2].QualityWindow.Interrupted)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageQualityEnrichNoQueryWithoutRealStarts(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}
	duration := 1000
	logs := []service.UsageLog{{CreatedAt: time.Now(), DurationMs: &duration, QualityInterrupted: true}}
	require.NoError(t, repo.enrichUsageQualityWindows(context.Background(), logs))
	require.Nil(t, logs[0].QualityWindow)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageQualityEnrichReturnsReadErrors(t *testing.T) {
	for _, failure := range []string{"query", "scan", "rows", "close"} {
		t.Run(failure, func(t *testing.T) {
			db, mock := newSQLMock(t)
			repo := &usageLogRepository{sql: db}
			now := time.Now().UTC()
			logs := []service.UsageLog{{AccountID: 1, RequestStartedAt: &now}}
			expectation := mock.ExpectQuery("WITH page")
			sentinel := errors.New("quality window read failed")
			if failure == "query" {
				expectation.WillReturnError(sentinel)
			} else {
				rows := sqlmock.NewRows([]string{"position", "window_start", "window_end"})
				switch failure {
				case "scan":
					rows.AddRow(0, "invalid timestamp", now)
				case "rows":
					rows.AddRow(0, now, now).RowError(0, sentinel)
				case "close":
					rows.CloseError(sentinel)
				}
				expectation.WillReturnRows(rows).RowsWillBeClosed()
			}
			require.Error(t, repo.enrichUsageQualityWindows(context.Background(), logs))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

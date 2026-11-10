//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUsageQualityPersistencePaths(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	user := mustCreateUser(t, client, &service.User{Email: "quality-usage-" + uuid.NewString() + "@example.com"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-" + uuid.NewString(), Name: "quality-usage"})
	account := mustCreateAccount(t, client, &service.Account{Name: "quality-usage-" + uuid.NewString()})
	repo := newUsageLogRepositoryWithSQL(client, integrationDB)
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for _, path := range []string{"single", "batch", "best_effort", "single_fallback", "historical"} {
		t.Run(path, func(t *testing.T) {
			log := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID,
				RequestID: uuid.NewString(), Model: "quality-fixture", RequestStartedAt: &start, QualityInterrupted: true}
			if path == "historical" {
				log.RequestStartedAt = nil
				log.QualityInterrupted = false
			}
			switch path {
			case "single", "historical":
				inserted, err := repo.createSingle(ctx, integrationDB, log)
				require.NoError(t, err)
				require.True(t, inserted)
			case "batch":
				inserted, err := repo.Create(ctx, log)
				require.NoError(t, err)
				require.True(t, inserted)
			case "best_effort":
				require.NoError(t, repo.CreateBestEffort(ctx, log))
			case "single_fallback":
				require.NoError(t, execUsageLogInsertNoResult(ctx, integrationDB, prepareUsageLogInsert(log)))
			}
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT id FROM usage_logs WHERE request_id=$1 AND api_key_id=$2`, log.RequestID, key.ID).Scan(&log.ID))
			saved, err := repo.GetByID(ctx, log.ID)
			require.NoError(t, err)
			require.Equal(t, log.RequestStartedAt, saved.RequestStartedAt)
			require.Equal(t, log.QualityInterrupted, saved.QualityInterrupted)
			require.Nil(t, saved.QualityWindow)
		})
	}
}

func TestUsageQualityWindowsReadExistingAndDelayedLogs(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	user := mustCreateUser(t, client, &service.User{Email: "quality-window-" + uuid.NewString() + "@example.com"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-" + uuid.NewString(), Name: "quality-window"})
	account := mustCreateAccount(t, client, &service.Account{Name: "quality-window-" + uuid.NewString()})
	other := mustCreateAccount(t, client, &service.Account{Name: "quality-window-other-" + uuid.NewString()})
	repo := newUsageLogRepositoryWithSQL(client, integrationDB)
	start := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	end := start.Add(10 * time.Minute)
	newStart := start.Add(2 * time.Minute)
	newEnd := end.Add(2 * time.Minute)
	create := func(accountID int64, offset time.Duration, historical, interrupted bool) *service.UsageLog {
		t.Helper()
		observed := start.Add(offset)
		duration := 60000
		log := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: accountID,
			RequestID: uuid.NewString(), Model: "quality-fixture", RequestStartedAt: &observed,
			QualityInterrupted: interrupted, DurationMs: &duration, CreatedAt: newEnd.Add(time.Hour)}
		if historical {
			log.RequestStartedAt = nil
		}
		inserted, err := repo.Create(ctx, log)
		require.NoError(t, err)
		require.True(t, inserted)
		return log
	}
	// A completed log exists before any quality window is detected.
	existing := create(account.ID, time.Minute, false, false)
	before, err := repo.GetByID(ctx, existing.ID)
	require.NoError(t, err)
	require.Nil(t, before.QualityWindow)
	for i, window := range [][2]time.Time{{start, end}, {newStart, newEnd}} {
		_, err := integrationDB.ExecContext(ctx, `INSERT INTO account_quality_request_windows(account_id,plan_id,window_start,window_end) VALUES($1,$2,$3,$4)`, account.ID, account.ID*10+int64(i), window[0], window[1])
		require.NoError(t, err)
	}
	// Delayed usage is saved after the windows, with a log time outside both.
	cases := []struct {
		name                    string
		offset                  time.Duration
		historical, interrupted bool
		window                  int
	}{
		{"lower_exclusive", 0, false, false, 0},
		{"inside_first", time.Minute, false, false, 1},
		{"new_lower_exclusive", 2 * time.Minute, false, false, 1},
		{"overlap_latest", 3 * time.Minute, false, true, 2},
		{"upper_inclusive", 12 * time.Minute, false, false, 2},
		{"outside", 13 * time.Minute, false, true, 0},
		{"historical_no_estimate", 3 * time.Minute, true, true, 0},
	}
	expected := map[int64]*service.QualityWindowEvidence{existing.ID: {StartAt: start, EndAt: end}}
	for _, tc := range cases {
		log := create(account.ID, tc.offset, tc.historical, tc.interrupted)
		var evidence *service.QualityWindowEvidence
		if tc.window == 1 {
			evidence = &service.QualityWindowEvidence{StartAt: start, EndAt: end, Interrupted: tc.interrupted}
		} else if tc.window == 2 {
			evidence = &service.QualityWindowEvidence{StartAt: newStart, EndAt: newEnd, Interrupted: tc.interrupted}
		}
		expected[log.ID] = evidence
		saved, err := repo.GetByID(ctx, log.ID)
		require.NoError(t, err, tc.name)
		require.Equal(t, evidence, saved.QualityWindow, tc.name)
	}
	logs, page, err := repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 20}, UsageLogFilters{AccountID: account.ID})
	require.NoError(t, err)
	require.EqualValues(t, len(expected), page.Total)
	require.Len(t, logs, len(expected))
	for _, log := range logs {
		require.Equal(t, expected[log.ID], log.QualityWindow, fmt.Sprintf("usage %d", log.ID))
	}
	// Unclosed parent rows block enrichment with a single available connection.
	readDB, err := sql.Open("postgres", integrationPostgresDSN)
	require.NoError(t, err)
	defer readDB.Close()
	readDB.SetMaxOpenConns(1)
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	readRepo := newUsageLogRepositoryWithSQL(client, readDB)
	single, err := readRepo.GetByID(readCtx, existing.ID)
	require.NoError(t, err)
	require.Equal(t, expected[existing.ID], single.QualityWindow)
	pageLogs, _, err := readRepo.ListWithFilters(readCtx, pagination.PaginationParams{Page: 1, PageSize: 2}, UsageLogFilters{AccountID: account.ID})
	require.NoError(t, err)
	require.Len(t, pageLogs, 2)
	for _, log := range pageLogs {
		require.Equal(t, expected[log.ID], log.QualityWindow)
	}
	tx, err := readDB.BeginTx(readCtx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	txRepo := newUsageLogRepositoryWithSQL(client, tx)
	single, err = txRepo.GetByID(readCtx, existing.ID)
	require.NoError(t, err)
	require.Equal(t, expected[existing.ID], single.QualityWindow)
	require.NoError(t, tx.Rollback())
	otherLog := create(other.ID, 3*time.Minute, false, true)
	saved, err := repo.GetByID(ctx, otherLog.ID)
	require.NoError(t, err)
	require.Nil(t, saved.QualityWindow, "windows from another account must never match")
}

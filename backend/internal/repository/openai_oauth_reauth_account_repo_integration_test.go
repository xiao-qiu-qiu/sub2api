//go:build integration

package repository

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func immediateReauthApplyFixture(t *testing.T) (*accountRepository, *service.OpenAIOAuthReauthTaskRecord, map[string]any) {
	t.Helper()
	guard, id, snapshot, rules := immediateReauthRepositoryFixture(t)
	task, err := guard.RequestImmediateReauth(t.Context(), id, snapshot, "synthetic-hash", rules, time.Now().UTC())
	require.NoError(t, err)
	require.NotNil(t, task)
	_, err = integrationDB.ExecContext(t.Context(), `UPDATE openai_oauth_reauth_tasks
        SET status='callback_processing',stage='applying_credentials',worker_id='apply-fixture-worker' WHERE id=$1`, task.ID)
	require.NoError(t, err)
	return newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil), task, snapshot
}

func TestImmediateReauthApplyPreservesManualControlsAfterQueue(t *testing.T) {
	var version string
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SHOW server_version`).Scan(&version))
	t.Logf("OAuth recovery integration environment: PostgreSQL %s; isolated testcontainers database; migrations applied", version)
	for _, test := range []struct {
		name        string
		status      string
		schedulable bool
		message     string
		action      func(*testing.T, *accountRepository, int64)
	}{
		{name: "unmodified", status: service.StatusActive, schedulable: true},
		{name: "manual_stop", status: service.StatusActive, action: func(t *testing.T, repo *accountRepository, id int64) {
			require.NoError(t, repo.SetSchedulable(t.Context(), id, false))
		}},
		{name: "manual_stop_then_clear_error", status: service.StatusActive, action: func(t *testing.T, repo *accountRepository, id int64) {
			require.NoError(t, repo.SetSchedulable(t.Context(), id, false))
			require.NoError(t, repo.ClearError(t.Context(), id))
		}},
		{name: "manual_disabled_status", status: service.StatusDisabled, schedulable: true, action: func(t *testing.T, repo *accountRepository, id int64) {
			account, err := repo.GetByID(t.Context(), id)
			require.NoError(t, err)
			account.Status, account.ErrorMessage = service.StatusDisabled, ""
			require.NoError(t, repo.Update(t.Context(), account))
		}},
		{name: "manual_disabled_and_stopped", status: service.StatusDisabled, action: func(t *testing.T, repo *accountRepository, id int64) {
			require.NoError(t, repo.SetSchedulable(t.Context(), id, false))
			account, err := repo.GetByID(t.Context(), id)
			require.NoError(t, err)
			account.Status, account.ErrorMessage = service.StatusDisabled, ""
			require.NoError(t, repo.Update(t.Context(), account))
		}},
		{name: "manual_stop_then_full_edit_clears_message", status: service.StatusError, action: func(t *testing.T, repo *accountRepository, id int64) {
			require.NoError(t, repo.SetSchedulable(t.Context(), id, false))
			account, err := repo.GetByID(t.Context(), id)
			require.NoError(t, err)
			account.Name, account.ErrorMessage = "edited during recovery", ""
			// A stale/full save may omit or attempt to replace server-owned keys.
			account.Extra = map[string]any{"ordinary_setting": true, immediateOAuthRecoveryTaskExtraKey: "forged"}
			require.NoError(t, repo.Update(t.Context(), account))
		}},
		{name: "manual_error_replaced", status: service.StatusError, message: "manual quarantine", action: func(t *testing.T, repo *accountRepository, id int64) {
			require.NoError(t, repo.SetError(t.Context(), id, "manual quarantine"))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo, task, snapshot := immediateReauthApplyFixture(t)
			if test.action != nil {
				test.action(t, repo, task.AccountID)
			}
			before, err := repo.GetByID(t.Context(), task.AccountID)
			require.NoError(t, err)
			require.Equal(t, strconv.FormatInt(task.ID, 10), before.Extra[immediateOAuthRecoveryTaskExtraKey], "admin edits must retain committed task provenance")
			applied, err := repo.ApplyOpenAIOAuthReauth(t.Context(), task.ID, "apply-fixture-worker", task.AccountID, snapshot,
				map[string]any{"access_token": "recovered-access", "refresh_token": "recovered-refresh"},
				map[string]any{immediateOAuthRecoveryTaskExtraKey: "worker-forged", "worker_field": "merged"})
			require.NoError(t, err)
			require.True(t, applied)
			after, err := repo.GetByID(t.Context(), task.AccountID)
			require.NoError(t, err)
			require.Equal(t, "recovered-access", after.GetCredential("access_token"))
			require.Equal(t, test.status, after.Status)
			require.Equal(t, test.schedulable, after.Schedulable)
			require.Equal(t, test.message, after.ErrorMessage)
			require.NotContains(t, after.Extra, immediateOAuthRecoveryTaskExtraKey)
			require.Equal(t, "merged", after.Extra["worker_field"])
			finished, err := (&openAIOAuthReauthRepository{db: integrationDB}).GetTask(t.Context(), task.ID)
			require.NoError(t, err)
			require.Equal(t, service.OpenAIOAuthReauthStatusSucceeded, finished.Status)
		})
	}
}

func TestImmediateReauthApplyPreservesServerProvenanceAcrossExtraUpdates(t *testing.T) {
	repo, task, _ := immediateReauthApplyFixture(t)
	ctx := t.Context()
	require.NoError(t, repo.UpdateExtra(ctx, task.AccountID, map[string]any{immediateOAuthRecoveryTaskExtraKey: nil, "ordinary_partial": true}))
	_, err := repo.BulkUpdate(ctx, []int64{task.AccountID}, service.AccountBulkUpdate{Extra: map[string]any{
		immediateOAuthRecoveryTaskExtraKey: "forged-bulk", "ordinary_bulk": true,
	}})
	require.NoError(t, err)
	after, err := repo.GetByID(ctx, task.AccountID)
	require.NoError(t, err)
	require.Equal(t, strconv.FormatInt(task.ID, 10), after.Extra[immediateOAuthRecoveryTaskExtraKey])
	require.Equal(t, true, after.Extra["ordinary_partial"])
	require.Equal(t, true, after.Extra["ordinary_bulk"])
}

func TestImmediateReauthApplyDoesNotReviveStaleMarkerForManualTask(t *testing.T) {
	ctx := t.Context()
	_, id, snapshot, _ := immediateReauthRepositoryFixture(t)
	tasks := &openAIOAuthReauthRepository{db: integrationDB}
	task, err := tasks.CreateTask(ctx, id, "synthetic-hash")
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `UPDATE openai_oauth_reauth_tasks
        SET status='callback_processing',stage='applying_credentials',worker_id='apply-fixture-worker' WHERE id=$1`, task.ID)
	require.NoError(t, err)
	repo := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	require.NoError(t, repo.SetError(ctx, id, "prior authentication failure"))
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(extra,'{`+immediateOAuthRecoveryTaskExtraKey+`}',to_jsonb($2::text),TRUE) WHERE id=$1`,
		id, task.ID-1)
	require.NoError(t, err)
	applied, err := repo.ApplyOpenAIOAuthReauth(ctx, task.ID, "apply-fixture-worker", id, snapshot, map[string]any{"access_token": "manual-recovered"}, nil)
	require.NoError(t, err)
	require.True(t, applied)
	after, err := repo.GetByID(ctx, id)
	require.NoError(t, err)
	require.Equal(t, service.StatusActive, after.Status)
	require.True(t, after.Schedulable, "an old event marker must not change a newer explicit re-login's recovery behavior")
	require.NotContains(t, after.Extra, immediateOAuthRecoveryTaskExtraKey)
}

func TestImmediateReauthApplyManualDisableWinsAccountLockRace(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	repo, task, snapshot := immediateReauthApplyFixture(t)
	adminTx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = adminTx.Rollback() }()
	_, err = adminTx.ExecContext(ctx, `UPDATE accounts SET status='disabled',schedulable=FALSE,error_message='' WHERE id=$1`, task.AccountID)
	require.NoError(t, err)
	type result struct {
		applied bool
		err     error
	}
	finished := make(chan result, 1)
	go func() {
		applied, err := repo.ApplyOpenAIOAuthReauth(ctx, task.ID, "apply-fixture-worker", task.AccountID,
			snapshot, map[string]any{"access_token": "race-recovered"}, nil)
		finished <- result{applied, err}
	}()
	// Prove the worker statement is already running and waiting on the admin's
	// account row lock, rather than merely invoking the two writes sequentially.
	require.Eventually(t, func() bool {
		var waiting bool
		err := integrationDB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
            WHERE datname=current_database() AND pid<>pg_backend_pid()
              AND query LIKE '%WITH locked_task AS%' AND wait_event_type='Lock')`).Scan(&waiting)
		return err == nil && waiting
	}, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, adminTx.Commit())
	select {
	case result := <-finished:
		require.NoError(t, result.err)
		require.True(t, result.applied)
	case <-ctx.Done():
		t.Fatal("worker did not finish after the admin transaction committed")
	}
	after, err := repo.GetByID(ctx, task.AccountID)
	require.NoError(t, err)
	require.Equal(t, service.StatusDisabled, after.Status)
	require.False(t, after.Schedulable)
	require.Empty(t, after.ErrorMessage)
	require.Equal(t, "race-recovered", after.GetCredential("access_token"))
}

func TestImmediateReauthApplyCredentialCASStillRejectsStaleCallback(t *testing.T) {
	repo, task, snapshot := immediateReauthApplyFixture(t)
	_, err := integrationDB.ExecContext(t.Context(), `UPDATE accounts SET credentials='{"access_token":"newer-refresh"}' WHERE id=$1`, task.AccountID)
	require.NoError(t, err)
	applied, err := repo.ApplyOpenAIOAuthReauth(t.Context(), task.ID, "apply-fixture-worker", task.AccountID, snapshot,
		map[string]any{"access_token": "stale-worker"}, nil)
	require.NoError(t, err)
	require.False(t, applied)
	after, err := repo.GetByID(t.Context(), task.AccountID)
	require.NoError(t, err)
	require.Equal(t, "newer-refresh", after.GetCredential("access_token"))
	require.Equal(t, strconv.FormatInt(task.ID, 10), after.Extra[immediateOAuthRecoveryTaskExtraKey])
}

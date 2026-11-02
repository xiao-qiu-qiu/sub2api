//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func immediateReauthRepositoryFixture(t *testing.T) (*accountTokenGuardV2Repository, int64, map[string]any, service.AccountTokenGuardV2Rules) {
	t.Helper()
	guard, _, id, rules := newBudgetFixture(t)
	// A live inspection lease and a future due time must not gate a 401 event.
	_, err := integrationDB.ExecContext(context.Background(), `UPDATE account_token_guard_v2_accounts
        SET enabled=FALSE,next_probe_at=NOW()+INTERVAL '1 hour',fail_streak=0 WHERE account_id=$1`, id)
	require.NoError(t, err)
	return guard, id, map[string]any{"access_token": "synthetic"}, rules
}

func TestImmediateReauthRepositoryConcurrentReuseAndLate401CAS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	guard, id, snapshot, rules := immediateReauthRepositoryFixture(t)
	var wg sync.WaitGroup
	ids, errs := make(chan int64, 8), make(chan error, 8)
	now := time.Now().UTC()
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other := &accountTokenGuardV2Repository{db: integrationDB}
			task, err := other.RequestImmediateReauth(ctx, id, snapshot, "synthetic-hash", rules, now)
			errs <- err
			if task != nil {
				ids <- task.ID
			}
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var first int64
	count := 0
	for taskID := range ids {
		count++
		if first == 0 {
			first = taskID
		}
		require.Equal(t, first, taskID, "all instances must wait on the same task")
	}
	require.Equal(t, 8, count)
	var tasks, outbox int
	var status, message string
	var schedulable bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM openai_oauth_reauth_tasks WHERE account_id=$1`, id).Scan(&tasks))
	require.Equal(t, 1, tasks)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status,error_message,schedulable FROM accounts WHERE id=$1`, id).Scan(&status, &message, &schedulable))
	require.Equal(t, service.StatusError, status)
	require.Equal(t, service.ImmediateOAuthRecoveryErrorMessage, message)
	require.True(t, schedulable, "the auth error must not impersonate a manual scheduling switch")
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM scheduler_outbox WHERE account_id=$1`, id).Scan(&outbox))
	require.Equal(t, 1, outbox, "idempotent reuse must not flood the scheduler outbox")
	stored, err := guard.GetAccount(ctx, id)
	require.NoError(t, err)
	require.False(t, stored.Enabled)
	require.Equal(t, "budget-owner", stored.LeaseOwner, "business events must not steal inspection leases")
	active, err := (&accountTokenGuardV2Repository{db: integrationDB}).GetImmediateReauthTask(ctx, id)
	require.NoError(t, err)
	require.Equal(t, first, active.ID)
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials='{"access_token":"rotated"}',status='active',error_message='' WHERE id=$1`, id)
	require.NoError(t, err)
	late, err := guard.RequestImmediateReauth(ctx, id, snapshot, "synthetic-hash", rules, now)
	require.NoError(t, err)
	require.Nil(t, late)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status,error_message FROM accounts WHERE id=$1`, id).Scan(&status, &message))
	require.Equal(t, service.StatusActive, status)
	require.Empty(t, message, "old-token 401 must not mark refreshed credentials as error")
	off := false
	require.NoError(t, guard.UpdateSwitches(ctx, id, nil, &off))
	active, err = guard.GetImmediateReauthTask(ctx, id)
	require.NoError(t, err)
	require.Nil(t, active)
}

func TestImmediateReauthRepositoryRetainsFailureCooldownAndBudget(t *testing.T) {
	ctx := context.Background()
	guard, id, snapshot, rules := immediateReauthRepositoryFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := 0; i < rules.ReloginMaxFailures; i++ {
		task, err := guard.RequestImmediateReauth(ctx, id, snapshot, "synthetic-hash", rules, now)
		require.NoError(t, err)
		require.NotNil(t, task)
		finishBudgetTask(t, task.ID, service.OpenAIOAuthReauthStatusFailed, now)
		_, err = guard.RequestImmediateReauth(ctx, id, snapshot, "synthetic-hash", rules, now.Add(time.Second))
		require.ErrorIs(t, err, service.ErrOAuthRecoveryUnavailable)
		stored, err := guard.GetAccount(ctx, id)
		require.NoError(t, err)
		require.Equal(t, i+1, stored.ReloginFailStreak)
		require.Equal(t, now.Add(service.ReloginBackoff(rules, i+1)), *stored.CooldownUntil)
		now = stored.CooldownUntil.Add(time.Second)
	}
	_, err := (&accountTokenGuardV2Repository{db: integrationDB}).RequestImmediateReauth(ctx, id, snapshot, "synthetic-hash", rules, now.Add(24*time.Hour))
	require.ErrorIs(t, err, service.ErrOAuthRecoveryUnavailable, "process restart or expired cooldown must not reset exhaustion")
	stored, err := guard.GetAccount(ctx, id)
	require.NoError(t, err)
	require.True(t, stored.AutoReloginExhausted)
}

func TestImmediateReauthRepositoryLate401RollsBackTaskReservationAndAccounting(t *testing.T) {
	ctx := context.Background()
	guard, id, snapshot, rules := immediateReauthRepositoryFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials='{"access_token":"already-refreshed"}' WHERE id=$1`, id)
	require.NoError(t, err)
	task, err := guard.RequestImmediateReauth(ctx, id, snapshot, "old-hash", rules, now)
	require.NoError(t, err)
	require.Nil(t, task)
	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM openai_oauth_reauth_tasks WHERE account_id=$1`, id).Scan(&count))
	require.Zero(t, count, "the task reservation must roll back on a credential CAS miss")
	// A stale 401 must not reconcile an old failed task against newer credentials.
	tasks := &openAIOAuthReauthRepository{db: integrationDB}
	oldTask, err := tasks.CreateTask(ctx, id, "old-hash")
	require.NoError(t, err)
	finishBudgetTask(t, oldTask.ID, service.OpenAIOAuthReauthStatusFailed, now)
	task, err = guard.RequestImmediateReauth(ctx, id, snapshot, "old-hash", rules, now.Add(time.Second))
	require.NoError(t, err)
	require.Nil(t, task)
	stored, err := guard.GetAccount(ctx, id)
	require.NoError(t, err)
	require.Zero(t, stored.ReloginFailStreak)
	require.Less(t, stored.LastCountedTaskID, oldTask.ID)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM scheduler_outbox WHERE account_id=$1`, id).Scan(&count))
	require.Zero(t, count)
}

func TestImmediateReauthRepositoryFreshVerificationAccountsSuccessAndRejectsStaleProbe(t *testing.T) {
	for _, state := range []string{service.AccountTokenGuardV2ProbeOK, service.AccountTokenGuardV2ProbeAuth, service.AccountTokenGuardV2ProbeTransient, "changed"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			guard, id, snapshot, rules := immediateReauthRepositoryFixture(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			task, err := guard.RequestImmediateReauth(ctx, id, snapshot, "synthetic-hash", rules, now)
			require.NoError(t, err)
			finishBudgetTask(t, task.ID, service.OpenAIOAuthReauthStatusSucceeded, now)
			_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials='{"access_token":"recovered"}',status='active',error_message='' WHERE id=$1`, id)
			require.NoError(t, err)
			newCredentials := map[string]any{"access_token": "recovered"}
			probeState := state
			if state == "changed" {
				probeState = service.AccountTokenGuardV2ProbeOK
				newCredentials = snapshot
			}
			result := service.AccountTokenGuardV2ProbeCompletion{ProbeState: probeState, ProbeDetail: "synthetic probe",
				LastProbeAt: now.Add(time.Second), NextProbeAt: now.Add(time.Hour)}
			err = guard.CompleteImmediateReauthVerification(ctx, id, newCredentials, result, rules)
			stored, readErr := guard.GetAccount(ctx, id)
			require.NoError(t, readErr)
			switch state {
			case service.AccountTokenGuardV2ProbeOK:
				require.NoError(t, err)
				require.Equal(t, task.ID, stored.LastCountedTaskID)
				require.Nil(t, stored.CooldownUntil)
				require.Zero(t, stored.ReloginFailStreak)
				// A real new-token 401 immediately after verified success gets a new task.
				newTask, err := guard.RequestImmediateReauth(ctx, id, newCredentials, "new-hash", rules, now.Add(2*time.Second))
				require.NoError(t, err)
				require.Greater(t, newTask.ID, task.ID)
			case service.AccountTokenGuardV2ProbeAuth:
				require.NoError(t, err)
				require.Equal(t, 1, stored.ReloginFailStreak)
				require.NotNil(t, stored.CooldownUntil)
				_, err = guard.RequestImmediateReauth(ctx, id, newCredentials, "new-hash", rules, now.Add(2*time.Second))
				require.ErrorIs(t, err, service.ErrOAuthRecoveryUnavailable)
			case service.AccountTokenGuardV2ProbeTransient:
				require.Equal(t, "TOKEN_GUARD_V2_RECOVERY_TASK_PENDING", infraerrors.Reason(err))
				require.Less(t, stored.LastCountedTaskID, task.ID)
				require.NotNil(t, stored.CooldownUntil)
			case "changed":
				require.Equal(t, "TOKEN_GUARD_V2_RECOVERY_CREDENTIALS_CHANGED", infraerrors.Reason(err))
				require.Less(t, stored.LastCountedTaskID, task.ID, "stale verification must roll back all budget changes")
			}
			require.Equal(t, "budget-owner", stored.LeaseOwner)
		})
	}
}

func TestImmediateReauthRepositoryVerifiedSuccessInsertionCooldownDoesNotGateNew401(t *testing.T) {
	ctx := context.Background()
	guard, id, snapshot, rules := immediateReauthRepositoryFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	task, err := guard.RequestImmediateReauth(ctx, id, snapshot, "synthetic-hash", rules, now)
	require.NoError(t, err)
	finished := task.CreatedAt.Add(time.Second)
	finishBudgetTask(t, task.ID, service.OpenAIOAuthReauthStatusSucceeded, finished)
	_, err = integrationDB.ExecContext(ctx, `UPDATE account_token_guard_v2_accounts SET
        last_counted_task_id=$2,probe_state='ok',last_probe_at=$3,last_reauth_at=$4,cooldown_until=$5 WHERE account_id=$1`,
		id, task.ID, finished, task.CreatedAt.Add(-time.Millisecond), task.CreatedAt.Add(time.Minute-time.Millisecond))
	require.NoError(t, err)
	newTask, err := guard.RequestImmediateReauth(ctx, id, snapshot, "new-hash", rules, finished.Add(time.Second))
	require.NoError(t, err)
	require.Greater(t, newTask.ID, task.ID)
}

func TestImmediateReauthRepositoryExplicitResumeCooldownAfterSuccessStillGates(t *testing.T) {
	ctx := context.Background()
	guard, id, snapshot, rules := immediateReauthRepositoryFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	task, err := guard.RequestImmediateReauth(ctx, id, snapshot, "synthetic-hash", rules, now)
	require.NoError(t, err)
	finished := task.CreatedAt.Add(time.Second)
	finishBudgetTask(t, task.ID, service.OpenAIOAuthReauthStatusSucceeded, finished)
	_, err = integrationDB.ExecContext(ctx, `UPDATE account_token_guard_v2_accounts SET
        last_counted_task_id=$2,probe_state='ok',last_probe_at=$3,last_reauth_at=$4,
        cooldown_until=$5,lease_owner=NULL,lease_until=NULL WHERE account_id=$1`,
		id, task.ID, finished, task.CreatedAt, finished)
	require.NoError(t, err)
	resumeAt := finished.Add(time.Second)
	require.NoError(t, guard.ResumeReloginBudget(ctx, id, rules, resumeAt))
	_, err = guard.RequestImmediateReauth(ctx, id, snapshot, "new-hash", rules, resumeAt.Add(time.Second))
	require.ErrorIs(t, err, service.ErrOAuthRecoveryUnavailable, "explicit resume cooldown must not be mistaken for the original successful task cooldown")
}

func TestImmediateReauthRepositoryPreservesManualControlsAndOtherTaskProfiles(t *testing.T) {
	for _, name := range []string{"auto disabled", "manual stop", "disabled", "excel task"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			guard, id, snapshot, rules := immediateReauthRepositoryFixture(t)
			var err error
			switch name {
			case "auto disabled":
				_, err = integrationDB.ExecContext(ctx, `UPDATE account_token_guard_v2_accounts SET auto_relogin_enabled=FALSE WHERE account_id=$1`, id)
			case "manual stop":
				_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET schedulable=FALSE WHERE id=$1`, id)
			case "disabled":
				_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET status='disabled' WHERE id=$1`, id)
			case "excel task":
				_, err = integrationDB.ExecContext(ctx, `INSERT INTO openai_oauth_reauth_tasks(account_id,status,stage,expected_credentials_hash,oauth_profile)
                    VALUES($1,'queued','queued','synthetic','excel')`, id)
			}
			require.NoError(t, err)
			_, err = guard.RequestImmediateReauth(ctx, id, snapshot, "synthetic-hash", rules, time.Now())
			require.ErrorIs(t, err, service.ErrOAuthRecoveryUnavailable)
			active, err := guard.GetImmediateReauthTask(ctx, id)
			require.NoError(t, err)
			require.Nil(t, active)
			var outbox int
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM scheduler_outbox WHERE account_id=$1`, id).Scan(&outbox))
			require.Zero(t, outbox, fmt.Sprintf("%s must not alter account scheduling", name))
		})
	}
}

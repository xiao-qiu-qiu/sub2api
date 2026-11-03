package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.AccountTokenGuardV2RecoveryRepository = (*accountTokenGuardV2Repository)(nil)

// Internal provenance survives ordinary account edits and ClearError. It is
// tied to one task, so later manually requested re-logins retain their semantics.
const immediateOAuthRecoveryTaskExtraKey = "openai_oauth_recovery_task_id"

const immediateReauthTaskSelect = `SELECT task.id,task.account_id,task.status,task.stage,
    COALESCE(task.worker_id,''),COALESCE(task.auth_session_id,''),task.expected_credentials_hash,
    COALESCE(task.error_message,''),task.attempt,task.created_at,task.updated_at,task.finished_at,
    CASE WHEN EXISTS(SELECT 1 FROM openai_totp_rotations rotation WHERE rotation.task_id=task.id)
      THEN 'totp_rotation' ELSE task.oauth_profile END
    FROM openai_oauth_reauth_tasks task`

func activeImmediateReauthTx(ctx context.Context, tx *sql.Tx, id int64) (*service.OpenAIOAuthReauthTaskRecord, error) {
	task, err := scanOpenAIOAuthReauthTask(tx.QueryRowContext(ctx, immediateReauthTaskSelect+`
        WHERE task.account_id=$1 AND task.status IN ('queued','running','callback_processing')
        ORDER BY task.id DESC LIMIT 1 FOR UPDATE OF task`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return task, err
}

// Task locks precede the account lock, matching the worker's credential swap.
// The JSONB comparison is semantic and checks the actual rejected snapshot.
func lockImmediateReauthAccount(ctx context.Context, tx *sql.Tx, id int64, expected map[string]any) (*service.Account, bool, error) {
	payload, err := json.Marshal(expected)
	if err != nil {
		return nil, false, err
	}
	var account service.Account
	var matches bool
	var credentials, extra []byte
	var parent sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT credentials=$2::jsonb,platform,type,status,schedulable,credentials,extra,parent_account_id
        FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id, string(payload)).Scan(
		&matches, &account.Platform, &account.Type, &account.Status, &account.Schedulable, &credentials, &extra, &parent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, service.ErrOAuthRecoveryUnavailable
	}
	if err != nil {
		return nil, false, err
	}
	account.ID = id
	if parent.Valid {
		account.ParentAccountID = &parent.Int64
	}
	if err := json.Unmarshal(credentials, &account.Credentials); err != nil {
		return nil, false, err
	}
	if len(extra) > 0 {
		if err := json.Unmarshal(extra, &account.Extra); err != nil {
			return nil, false, err
		}
	}
	if account.Platform != service.PlatformOpenAI || account.Type != service.AccountTypeOAuth || account.IsCredentialShadow() ||
		account.IsOpenAIPersonalAccessToken() || account.IsOpenAIAgentIdentity() || !account.Schedulable ||
		(account.Status != service.StatusActive && account.Status != service.StatusError) {
		return nil, false, service.ErrOAuthRecoveryUnavailable
	}
	return &account, matches, nil
}

// A verified successful task may leave its insertion cooldown behind (for
// example after an older accounting watermark). Only that original cooldown
// can be ignored; explicit resume and failure backoff can extend it and remain.
func immediateReauthSuccessCooldown(record *service.AccountTokenGuardV2Record, latest *service.OpenAIOAuthReauthTaskRecord, rules service.AccountTokenGuardV2Rules) bool {
	return latest != nil && latest.Status == service.OpenAIOAuthReauthStatusSucceeded && latest.FinishedAt != nil &&
		record.ReloginFailStreak == 0 && !record.AutoReloginExhausted && record.LastCountedTaskID >= latest.ID &&
		record.ProbeState == service.AccountTokenGuardV2ProbeOK && record.LastProbeAt != nil && !record.LastProbeAt.Before(*latest.FinishedAt) &&
		record.LastReauthAt != nil && !record.LastReauthAt.After(*latest.FinishedAt) && record.CooldownUntil != nil &&
		!record.CooldownUntil.After(record.LastReauthAt.Add(time.Duration(rules.ReloginCooldownSeconds)*time.Second))
}

func markImmediateReauthAccount(ctx context.Context, tx *sql.Tx, id, taskID int64, expected map[string]any) error {
	payload, err := json.Marshal(expected)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `WITH changed AS (
        UPDATE accounts SET status=$3,error_message=$4,
          extra=jsonb_set(COALESCE(extra,'{}'::jsonb),'{`+immediateOAuthRecoveryTaskExtraKey+`}',to_jsonb($6::text),TRUE),updated_at=NOW()
        WHERE id=$1 AND credentials=$2::jsonb AND schedulable=TRUE AND status IN ('active','error')
          AND (status<>$3 OR COALESCE(error_message,'')<>$4 OR extra->>'`+immediateOAuthRecoveryTaskExtraKey+`' IS DISTINCT FROM $6::text) RETURNING id
    ) INSERT INTO scheduler_outbox(event_type,account_id,group_id,payload)
      SELECT $5,id,NULL,NULL FROM changed`, id, string(payload), service.StatusError,
		service.ImmediateOAuthRecoveryErrorMessage, service.SchedulerOutboxEventAccountChanged, taskID)
	return err
}

// RequestImmediateReauth serializes with manual/periodic Codex insertion and
// explicit budget resume on the guard row. Inspection leases and due times do
// not gate business events. Active tasks are locked before account credentials,
// so a finishing worker either wins the snapshot CAS or is reused safely.
func (r *accountTokenGuardV2Repository) RequestImmediateReauth(ctx context.Context, id int64, expected map[string]any, hash string, rules service.AccountTokenGuardV2Rules, now time.Time) (*service.OpenAIOAuthReauthTaskRecord, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := lockReloginBudget(ctx, tx, id, "")
	if infraerrors.Reason(err) == "TOKEN_GUARD_V2_BUDGET_BUSY" {
		return nil, service.ErrOAuthRecoveryUnavailable
	}
	if err != nil {
		return nil, err
	}
	if !record.AutoReloginEnabled {
		return nil, service.ErrOAuthRecoveryUnavailable
	}
	active, err := activeImmediateReauthTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	var latest *service.OpenAIOAuthReauthTaskRecord
	var pending bool
	if active == nil {
		// An unverified saved token rejected by a new business request is a
		// failed re-login outcome; a previously verified success is not recounted.
		record, pending, err = reconcileReloginBudgetTx(ctx, tx, record, rules, now, service.AccountTokenGuardV2ProbeAuth)
		if err != nil {
			return nil, err
		}
		latest, err = scanOpenAIOAuthReauthTask(tx.QueryRowContext(ctx, immediateReauthTaskSelect+`
            WHERE task.account_id=$1 AND task.oauth_profile='codex'
              AND NOT EXISTS(SELECT 1 FROM openai_totp_rotations rotation WHERE rotation.task_id=task.id)
            ORDER BY task.id DESC LIMIT 1 FOR UPDATE OF task`, id))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	ignoreSuccessCooldown := immediateReauthSuccessCooldown(record, latest, rules)
	canCreate := !record.AutoReloginExhausted && !pending &&
		(record.CooldownUntil == nil || !record.CooldownUntil.After(now) || ignoreSuccessCooldown)
	var task *service.OpenAIOAuthReauthTaskRecord
	if active == nil && canCreate {
		// Reserve the unique task slot before locking the account. Excel/SIWC
		// task creation does not lock the guard row; waiting on its uniqueness
		// conflict while holding its worker's account lock could deadlock.
		// The reservation is invisible until all credential/switch checks pass.
		task, err = scanOpenAIOAuthReauthTask(tx.QueryRowContext(ctx, `
            INSERT INTO openai_oauth_reauth_tasks(account_id,status,stage,expected_credentials_hash,oauth_profile)
            VALUES($1,'queued','queued',$2,'codex') ON CONFLICT DO NOTHING
            RETURNING id,account_id,status,stage,COALESCE(worker_id,''),COALESCE(auth_session_id,''),
              expected_credentials_hash,COALESCE(error_message,''),attempt,created_at,updated_at,finished_at,oauth_profile`, id, hash))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrOAuthRecoveryUnavailable
		}
		if err != nil {
			return nil, err
		}
	}
	_, matches, err := lockImmediateReauthAccount(ctx, tx, id, expected)
	if err != nil {
		return nil, err
	}
	if !matches {
		return nil, nil // Roll back accounting as well: this was an obsolete 401.
	}
	if active != nil {
		if active.OAuthProfile != "codex" {
			return nil, service.ErrOAuthRecoveryUnavailable
		}
		if err := markImmediateReauthAccount(ctx, tx, id, active.ID, expected); err != nil {
			return nil, err
		}
		return active, tx.Commit()
	}
	if !canCreate {
		// Commit accounted failures even when no further task is permitted.
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, service.ErrOAuthRecoveryUnavailable
	}
	if err := markImmediateReauthAccount(ctx, tx, id, task.ID, expected); err != nil {
		return nil, err
	}
	until := now.Add(time.Duration(rules.ReloginCooldownSeconds) * time.Second)
	_, err = tx.ExecContext(ctx, `UPDATE account_token_guard_v2_accounts
        SET last_reauth_at=$2,cooldown_until=GREATEST(cooldown_until,$3::timestamptz),updated_at=NOW() WHERE account_id=$1`, id, now, until)
	if err != nil {
		return nil, err
	}
	return task, tx.Commit()
}

func (r *accountTokenGuardV2Repository) GetImmediateReauthTask(ctx context.Context, id int64) (*service.OpenAIOAuthReauthTaskRecord, error) {
	task, err := scanOpenAIOAuthReauthTask(r.db.QueryRowContext(ctx, immediateReauthTaskSelect+`
        JOIN account_token_guard_v2_accounts guard ON guard.account_id=task.account_id
        JOIN accounts account ON account.id=task.account_id
        WHERE task.account_id=$1 AND task.oauth_profile='codex'
          AND task.status IN ('queued','running','callback_processing') AND guard.auto_relogin_enabled=TRUE
          AND account.deleted_at IS NULL AND account.platform='openai' AND account.type='oauth'
          AND account.parent_account_id IS NULL
          AND account.schedulable=TRUE AND account.status IN ('active','error')
          AND NOT EXISTS(SELECT 1 FROM openai_totp_rotations rotation WHERE rotation.task_id=task.id)
        ORDER BY task.id DESC LIMIT 1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return task, err
}

// Verification does not acquire or overwrite a periodic probe lease. Its
// snapshot CAS and durable accounting are atomic with the stored probe result.
func (r *accountTokenGuardV2Repository) CompleteImmediateReauthVerification(ctx context.Context, id int64, expected map[string]any, result service.AccountTokenGuardV2ProbeCompletion, rules service.AccountTokenGuardV2Rules) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := lockReloginBudget(ctx, tx, id, "")
	if err != nil {
		return err
	}
	record, pending, err := reconcileReloginBudgetTx(ctx, tx, record, rules, result.LastProbeAt, result.ProbeState)
	if err != nil {
		return err
	}
	_, matches, err := lockImmediateReauthAccount(ctx, tx, id, expected)
	if err != nil {
		return err
	}
	if !matches {
		return infraerrors.Conflict("TOKEN_GUARD_V2_RECOVERY_CREDENTIALS_CHANGED", "Credentials changed during recovery verification")
	}
	if pending {
		result.BlockedReason = "re-login task active or history reconciliation pending"
	}
	failures := record.FailStreak
	if result.ProbeState == service.AccountTokenGuardV2ProbeOK {
		failures = 0
	} else if result.ProbeState == service.AccountTokenGuardV2ProbeAuth {
		failures++
	}
	_, err = tx.ExecContext(ctx, `UPDATE account_token_guard_v2_accounts
        SET probe_state=$2,probe_detail=$3,fail_streak=$4,last_probe_at=$5,next_probe_at=$6,
          blocked_reason=$7,unauthorized_retry_at=NULL,unauthorized_credentials_hash='',updated_at=NOW() WHERE account_id=$1`, id, result.ProbeState, result.ProbeDetail,
		failures, result.LastProbeAt, result.NextProbeAt, result.BlockedReason)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if pending {
		return infraerrors.Conflict("TOKEN_GUARD_V2_RECOVERY_TASK_PENDING", "Re-login task or budget reconciliation is still pending")
	}
	return nil
}

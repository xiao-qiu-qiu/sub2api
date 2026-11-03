package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

var _ service.AccountTrashRepository = (*accountRepository)(nil)
var _ service.AccountDeleteNoteRepository = (*accountRepository)(nil)

// No ciphertext is selected by the listing, even for old deleted rows which
// predate archives and still have a live reauthentication configuration.
const accountTrashFrom = ` FROM accounts a
	LEFT JOIN account_trash_archives ar ON ar.account_id = a.id
	LEFT JOIN openai_oauth_reauth_configs c ON c.account_id = a.id
	WHERE a.deleted_at IS NOT NULL AND COALESCE(a.extra->>'account_trash_purged', 'false') <> 'true' AND ($1 = ''
		OR strpos(lower(a.name), lower($1)) > 0
		OR strpos(lower(COALESCE(ar.login_email, c.login_email, '')), lower($1)) > 0
		OR strpos(lower(COALESCE(a.credentials->>'email', a.extra->>'email', '')), lower($1)) > 0)`

func (r *accountRepository) ListAccountTrash(ctx context.Context, params pagination.PaginationParams, search string) ([]service.AccountTrashItem, *pagination.PaginationResult, error) {
	if params.Page < 1 {
		params.Page = 1
	}
	exec := txAwareSQLExecutor(ctx, r.sql, r.client)
	search = strings.TrimSpace(search)
	rows, err := exec.QueryContext(ctx, `SELECT COUNT(*)`+accountTrashFrom, search)
	if err != nil {
		return nil, nil, err
	}
	var total int64
	if rows.Next() {
		err = rows.Scan(&total)
	}
	readErr := rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, nil, err
	}
	if readErr != nil {
		return nil, nil, readErr
	}
	rows, err = exec.QueryContext(ctx, `SELECT a.id, a.name, a.platform, a.type, a.deleted_at,
		COALESCE(NULLIF(ar.login_email, ''), NULLIF(c.login_email, ''),
			NULLIF(a.credentials->>'email', ''), a.extra->>'email', ''),
		(COALESCE(length(ar.password_ciphertext), length(c.password_ciphertext), 0) > 0
		 OR COALESCE(length(ar.totp_secret_ciphertext), length(c.totp_secret_ciphertext), 0) > 0
		 OR COALESCE(length(ar.otp_url_ciphertext), length(c.otp_url_ciphertext), 0) > 0),
		COALESCE(ar.delete_note, '')`+
		accountTrashFrom+` ORDER BY a.deleted_at DESC, a.id DESC LIMIT $2 OFFSET $3`, search, params.Limit(), params.Offset())
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]service.AccountTrashItem, 0)
	for rows.Next() {
		var item service.AccountTrashItem
		if err := rows.Scan(&item.ID, &item.Name, &item.Platform, &item.Type, &item.DeletedAt, &item.LoginEmail, &item.HasLoginCredentials, &item.DeleteNote); err != nil {
			return nil, nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return items, paginationResultFromTotal(total, params), nil
}

type accountTrashSkipCacheKey struct{}

// All mutations, including outbox insertion, belong to the same transaction.
// Context transactions also defer cache publication until their actual commit.
func (r *accountRepository) accountTrashTransaction(ctx context.Context, run func(context.Context, *dbent.Client) error) error {
	if tx := dbent.TxFromContext(ctx); tx != nil {
		return run(ctx, tx.Client())
	}
	tx, err := r.client.Tx(ctx)
	if errors.Is(err, dbent.ErrTxStarted) {
		// A caller supplied a transactional client without its context. The durable
		// outbox still propagates after commit; never publish uncommitted state.
		return run(context.WithValue(ctx, accountTrashSkipCacheKey{}, true), r.client)
	}
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := run(dbent.NewTxContext(ctx, tx), tx.Client()); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *accountRepository) accountTrashAfterCommit(ctx context.Context, id int64) {
	if skip, _ := ctx.Value(accountTrashSkipCacheKey{}).(bool); skip {
		return
	}
	service.AfterAccountCommit(ctx, func(committed context.Context) {
		// Read the latest version under the existing snapshot lock, including if
		// another request has already restored/deleted the account again.
		r.syncSchedulerAccountSnapshotDetached(committed, id)
	})
}

type lockedTrashAccount struct {
	deletedAt sql.NullTime
	parentID  sql.NullInt64
	purged    bool
}

func lockTrashAccount(ctx context.Context, exec sqlExecutor, id int64) (lockedTrashAccount, error) {
	var account lockedTrashAccount
	rows, err := exec.QueryContext(ctx, `SELECT deleted_at, parent_account_id,
		COALESCE(extra->>'account_trash_purged', 'false') = 'true' FROM accounts WHERE id = $1 FOR UPDATE`, id)
	if err != nil {
		return account, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return account, err
		}
		return account, service.ErrAccountNotFound
	}
	if err := rows.Scan(&account.deletedAt, &account.parentID, &account.purged); err != nil {
		return account, err
	}
	if account.purged {
		return account, service.ErrAccountNotFound
	}
	return account, nil
}

func lockTrashLoginConfig(ctx context.Context, exec sqlExecutor, id int64) error {
	rows, err := exec.QueryContext(ctx, `SELECT account_id FROM openai_oauth_reauth_configs WHERE account_id = $1 FOR UPDATE`, id)
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	return nil
}

func clearTrashWork(ctx context.Context, exec sqlExecutor, id int64, purge bool) error {
	// Test plans/results are intentionally not archived. In-flight callbacks are
	// invalidated so they cannot apply stale credentials after a later restore.
	if _, err := exec.ExecContext(ctx, `UPDATE openai_totp_rotations
		SET state = CASE WHEN $2 OR (action = 'rotate' AND state IN ('queued', 'running'))
			THEN 'failed' ELSE 'uncertain' END,
			worker_id = '', error_code = 'account_deleted', updated_at = NOW()
		WHERE account_id = $1 AND state NOT IN ('succeeded', 'failed')`, id, purge); err != nil {
		return err
	}
	// An uncertain rotation still owns its active-task slot: a new login or
	// rotation must not overwrite the credentials needed for manual recovery.
	if _, err := exec.ExecContext(ctx, `UPDATE openai_oauth_reauth_tasks
		SET status = 'failed', stage = 'failed', error_message = 'Account deleted',
			worker_id = NULL, auth_session_id = NULL, finished_at = NOW(), updated_at = NOW()
		WHERE account_id = $1 AND status IN ('queued', 'running', 'callback_processing')
			AND NOT EXISTS (SELECT 1 FROM openai_totp_rotations r
				WHERE r.task_id = openai_oauth_reauth_tasks.id AND r.state = 'uncertain')`, id); err != nil {
		return err
	}
	if _, err := exec.ExecContext(ctx, `UPDATE openai_oauth_reauth_tasks t
		SET status = 'callback_processing', stage = 'applying_credentials',
			worker_id = NULL, auth_session_id = NULL, finished_at = NULL,
			error_message = 'Account deleted; verify the unresolved 2FA rotation after restoring', updated_at = NOW()
		FROM openai_totp_rotations r WHERE t.id = r.task_id AND r.account_id = $1 AND r.state = 'uncertain'`, id); err != nil {
		return err
	}
	_, err := exec.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE account_id = $1`, id)
	return err
}

// Park unresolved rotations only inside the restore transaction so the existing
// config-update trigger permits restoring the archive. Both config and rotation
// rows are locked; a mismatch rolls back without losing any recovery material.
func parkTrashTOTPRotations(ctx context.Context, exec sqlExecutor, id int64) ([]int64, error) {
	rows, err := exec.QueryContext(ctx, `SELECT r.task_id, COALESCE(
		c.updated_at = r.config_updated_at AND ar.credential_mode = 'password_totp'
		AND ar.password_ciphertext = r.password_ciphertext
		AND ar.totp_secret_ciphertext = r.previous_ciphertext
		AND ROW(c.login_email, c.credential_mode, c.engine, c.proxy_source, c.proxy_id,
			c.password_ciphertext, c.totp_secret_ciphertext, c.otp_url_ciphertext, c.created_at)
			IS NOT DISTINCT FROM ROW(ar.login_email, ar.credential_mode, ar.engine, ar.proxy_source, ar.login_proxy_id,
				ar.password_ciphertext, ar.totp_secret_ciphertext, ar.otp_url_ciphertext, ar.login_created_at), FALSE)
		FROM openai_totp_rotations r JOIN account_trash_archives ar ON ar.account_id = r.account_id
		LEFT JOIN openai_oauth_reauth_configs c ON c.account_id = r.account_id
		WHERE r.account_id = $1 AND r.state = 'uncertain' FOR UPDATE OF r`, id)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0)
	for rows.Next() {
		var taskID int64
		var matches bool
		if err := rows.Scan(&taskID, &matches); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if !matches {
			_ = rows.Close()
			return nil, service.ErrAccountTrashTOTPConfigChanged
		}
		ids = append(ids, taskID)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil || len(ids) == 0 {
		return ids, err
	}
	_, err = exec.ExecContext(ctx, `UPDATE openai_totp_rotations SET state = 'failed'
		WHERE task_id = ANY($1)`, pq.Array(ids))
	return ids, err
}

func (r *accountRepository) deleteAccountToTrash(ctx context.Context, id int64) error {
	return r.DeleteWithNote(ctx, id, "")
}

func (r *accountRepository) DeleteWithNote(ctx context.Context, id int64, note string) error {
	note, err := service.NormalizeAccountDeleteNote(note)
	if err != nil {
		return err
	}
	return r.accountTrashTransaction(ctx, func(ctx context.Context, client *dbent.Client) error {
		account, err := lockTrashAccount(ctx, client, id)
		if err != nil {
			return err
		}
		if account.deletedAt.Valid {
			return nil // Idempotent delete must not overwrite the original archive.
		}
		if err := lockTrashLoginConfig(ctx, client, id); err != nil {
			return err
		}
		if _, err := client.ExecContext(ctx, `INSERT INTO account_trash_archives (
			account_id, delete_note, group_bindings, original_proxy_id, original_proxy_fallback_origin_id,
			login_email, credential_mode, engine, proxy_source,
			login_proxy_id, password_ciphertext, totp_secret_ciphertext, otp_url_ciphertext, login_created_at
		) SELECT a.id, $2, COALESCE((SELECT jsonb_agg(jsonb_build_object(
			'group_id', ag.group_id, 'priority', ag.priority, 'allowed_models', ag.allowed_models,
			'created_at', ag.created_at) ORDER BY ag.group_id)
			FROM account_groups ag WHERE ag.account_id = a.id), '[]'::jsonb),
			a.proxy_id, a.proxy_fallback_origin_id,
			c.login_email, c.credential_mode, c.engine, c.proxy_source, c.proxy_id,
			c.password_ciphertext, c.totp_secret_ciphertext, c.otp_url_ciphertext, c.created_at
		FROM accounts a LEFT JOIN openai_oauth_reauth_configs c ON c.account_id = a.id
		WHERE a.id = $1
		ON CONFLICT (account_id) DO UPDATE SET
			delete_note = EXCLUDED.delete_note,
			group_bindings = EXCLUDED.group_bindings,
			original_proxy_id = EXCLUDED.original_proxy_id,
			original_proxy_fallback_origin_id = EXCLUDED.original_proxy_fallback_origin_id,
			login_email = EXCLUDED.login_email,
			credential_mode = EXCLUDED.credential_mode, engine = EXCLUDED.engine,
			proxy_source = EXCLUDED.proxy_source, login_proxy_id = EXCLUDED.login_proxy_id,
			password_ciphertext = EXCLUDED.password_ciphertext,
			totp_secret_ciphertext = EXCLUDED.totp_secret_ciphertext,
			otp_url_ciphertext = EXCLUDED.otp_url_ciphertext,
			login_created_at = EXCLUDED.login_created_at, archived_at = NOW()`, id, note); err != nil {
			return err
		}
		groupIDs, err := trashGroupIDs(ctx, client, id, false)
		if err != nil {
			return err
		}
		if err := clearTrashWork(ctx, client, id, false); err != nil {
			return err
		}
		for _, query := range []string{
			`DELETE FROM account_groups WHERE account_id = $1`,
		} {
			if _, err := client.ExecContext(ctx, query, id); err != nil {
				return err
			}
		}
		// Keep the established soft-delete hook and retained analytics ownership.
		if _, err := client.Account.Delete().Where(dbaccount.IDEQ(id)).Exec(ctx); err != nil {
			return err
		}
		if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &id, nil, buildSchedulerGroupPayload(groupIDs)); err != nil {
			return err
		}
		r.accountTrashAfterCommit(ctx, id)
		return nil
	})
}

func trashGroupIDs(ctx context.Context, exec sqlExecutor, id int64, liveOnly bool) ([]int64, error) {
	query := `SELECT (binding->>'group_id')::bigint FROM account_trash_archives ar,
		jsonb_array_elements(ar.group_bindings) binding WHERE ar.account_id = $1`
	if liveOnly {
		// Share locks participate in the existing guarded group-delete protocol.
		query = `SELECT g.id FROM groups g JOIN account_trash_archives ar ON ar.account_id = $1
			WHERE g.deleted_at IS NULL AND EXISTS (SELECT 1 FROM jsonb_array_elements(ar.group_bindings) binding
				WHERE (binding->>'group_id')::bigint = g.id) ORDER BY g.id FOR SHARE OF g`
	}
	rows, err := exec.QueryContext(ctx, query, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0)
	for rows.Next() {
		var groupID int64
		if err := rows.Scan(&groupID); err != nil {
			return nil, err
		}
		ids = append(ids, groupID)
	}
	return ids, rows.Err()
}

func lockTrashProxies(ctx context.Context, exec sqlExecutor, id int64) error {
	// Archived references remain meaningful even after ON DELETE SET NULL has
	// changed a live row. Never silently restore onto a different network route.
	rows, err := exec.QueryContext(ctx, `SELECT DISTINCT wanted.proxy_id FROM (
		SELECT CASE WHEN ar.account_id IS NULL THEN a.proxy_id ELSE ar.original_proxy_id END AS proxy_id
		FROM accounts a LEFT JOIN account_trash_archives ar ON ar.account_id = a.id WHERE a.id = $1
		UNION SELECT CASE WHEN ar.account_id IS NULL THEN a.proxy_fallback_origin_id ELSE ar.original_proxy_fallback_origin_id END
		FROM accounts a LEFT JOIN account_trash_archives ar ON ar.account_id = a.id WHERE a.id = $1
		UNION SELECT CASE WHEN ar.login_email IS NULL THEN c.proxy_id ELSE ar.login_proxy_id END
		FROM accounts a LEFT JOIN account_trash_archives ar ON ar.account_id = a.id
		LEFT JOIN openai_oauth_reauth_configs c ON c.account_id = a.id WHERE a.id = $1
	) wanted WHERE wanted.proxy_id IS NOT NULL`, id)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var proxyID int64
		if err := rows.Scan(&proxyID); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, proxyID)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil || len(ids) == 0 {
		return err
	}
	rows, err = exec.QueryContext(ctx, `SELECT id FROM proxies
		WHERE id = ANY($1) AND deleted_at IS NULL ORDER BY id FOR SHARE`, pq.Array(ids))
	if err != nil {
		return err
	}
	locked := 0
	for rows.Next() {
		locked++
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if locked != len(ids) {
		return service.ErrAccountTrashProxyUnavailable
	}
	return nil
}

func (r *accountRepository) RestoreAccount(ctx context.Context, id int64) error {
	err := r.accountTrashTransaction(ctx, func(ctx context.Context, client *dbent.Client) error {
		account, err := lockTrashAccount(ctx, client, id)
		if err != nil {
			return err
		}
		if !account.deletedAt.Valid {
			return service.ErrAccountNotDeleted
		}
		if account.parentID.Valid {
			rows, err := client.QueryContext(ctx, `SELECT id FROM accounts
				WHERE id = $1 AND deleted_at IS NULL AND parent_account_id IS NULL
				AND COALESCE(extra->>'account_trash_purged', 'false') <> 'true' FOR SHARE`, account.parentID.Int64)
			if err != nil {
				return err
			}
			live := rows.Next()
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return err
			}
			if !live {
				return service.ErrAccountTrashParent
			}
		}
		if err := lockTrashLoginConfig(ctx, client, id); err != nil {
			return err
		}
		if err := lockTrashProxies(ctx, client, id); err != nil {
			return err
		}
		groupIDs, err := trashGroupIDs(ctx, client, id, true)
		if err != nil {
			return err
		}
		if err := clearTrashWork(ctx, client, id, false); err != nil {
			return err
		}
		rotationIDs, err := parkTrashTOTPRotations(ctx, client, id)
		if err != nil {
			return err
		}
		if _, err := client.ExecContext(ctx, `UPDATE accounts a SET deleted_at = NULL, updated_at = NOW(),
			proxy_id = original.proxy_id, proxy_fallback_origin_id = original.fallback_id
			FROM (SELECT a2.id,
				CASE WHEN ar.account_id IS NULL THEN a2.proxy_id ELSE ar.original_proxy_id END AS proxy_id,
				CASE WHEN ar.account_id IS NULL THEN a2.proxy_fallback_origin_id ELSE ar.original_proxy_fallback_origin_id END AS fallback_id
				FROM accounts a2 LEFT JOIN account_trash_archives ar ON ar.account_id = a2.id WHERE a2.id = $1) original
			WHERE a.id = original.id AND a.deleted_at IS NOT NULL`, id); err != nil {
			return err
		}
		if _, err := client.ExecContext(ctx, `INSERT INTO account_groups (account_id, group_id, priority, allowed_models, created_at)
			SELECT ar.account_id, b.group_id, b.priority, b.allowed_models, COALESCE(b.created_at, NOW())
			FROM account_trash_archives ar,
			jsonb_to_recordset(ar.group_bindings) AS b(group_id bigint, priority int, allowed_models jsonb, created_at timestamptz)
			WHERE ar.account_id = $1 AND b.group_id = ANY($2)
			ON CONFLICT (account_id, group_id) DO NOTHING`, id, pq.Array(groupIDs)); err != nil {
			return err
		}
		// The archive wins over any leftover live configuration, including its
		// original proxy override. Validation above never changes that route.
		if _, err := client.ExecContext(ctx, `INSERT INTO openai_oauth_reauth_configs (
			account_id, login_email, credential_mode, engine, proxy_source, proxy_id,
			password_ciphertext, totp_secret_ciphertext, otp_url_ciphertext, created_at, updated_at
		) SELECT ar.account_id, ar.login_email, ar.credential_mode, ar.engine, ar.proxy_source, ar.login_proxy_id,
			ar.password_ciphertext, ar.totp_secret_ciphertext, ar.otp_url_ciphertext, COALESCE(ar.login_created_at, NOW()), NOW()
		FROM account_trash_archives ar
		WHERE ar.account_id = $1 AND ar.login_email IS NOT NULL
		ON CONFLICT (account_id) DO UPDATE SET
			login_email = EXCLUDED.login_email, credential_mode = EXCLUDED.credential_mode,
			engine = EXCLUDED.engine, proxy_source = EXCLUDED.proxy_source, proxy_id = EXCLUDED.proxy_id,
			password_ciphertext = EXCLUDED.password_ciphertext, totp_secret_ciphertext = EXCLUDED.totp_secret_ciphertext,
			otp_url_ciphertext = EXCLUDED.otp_url_ciphertext, updated_at = NOW()`, id); err != nil {
			return err
		}
		if len(rotationIDs) > 0 {
			// Restore the same unresolved task and its exact candidate, rebasing only
			// after the original archive/config/ciphertexts passed the locked check.
			if _, err := client.ExecContext(ctx, `UPDATE openai_totp_rotations r
				SET state = 'uncertain', config_updated_at = c.updated_at
				FROM openai_oauth_reauth_configs c
				WHERE r.task_id = ANY($1) AND c.account_id = r.account_id`, pq.Array(rotationIDs)); err != nil {
				return err
			}
		}
		if _, err := client.ExecContext(ctx, `DELETE FROM account_trash_archives WHERE account_id = $1`, id); err != nil {
			return err
		}
		if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &id, nil, buildSchedulerGroupPayload(groupIDs)); err != nil {
			return err
		}
		r.accountTrashAfterCommit(ctx, id)
		return nil
	})
	var pgErr *pq.Error
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.Constraint == "uq_accounts_spark_shadow_per_parent" {
		return service.ErrAccountTrashShadowExists
	}
	return err
}

func (r *accountRepository) PermanentlyDeleteAccount(ctx context.Context, id int64) error {
	err := r.accountTrashTransaction(ctx, func(ctx context.Context, client *dbent.Client) error {
		account, err := lockTrashAccount(ctx, client, id)
		if err != nil {
			return err
		}
		if !account.deletedAt.Valid {
			return service.ErrAccountNotDeleted
		}
		groupIDs, err := trashGroupIDs(ctx, client, id, false)
		if err != nil {
			return err
		}
		if err := lockTrashLoginConfig(ctx, client, id); err != nil {
			return err
		}
		if err := clearTrashWork(ctx, client, id, true); err != nil {
			return err
		}
		rows, err := client.QueryContext(ctx, `SELECT
			EXISTS (SELECT 1 FROM usage_logs WHERE account_id = $1),
			EXISTS (SELECT 1 FROM accounts WHERE parent_account_id = $1)`, id)
		if err != nil {
			return err
		}
		var hasUsage, hasShadows bool
		if rows.Next() {
			err = rows.Scan(&hasUsage, &hasShadows)
		}
		readErr := rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		if readErr != nil {
			return readErr
		}
		if hasShadows {
			return service.ErrAccountTrashReferenced
		}
		if hasUsage {
			// usage_logs has an ON DELETE CASCADE FK. Preserve the original ID as
			// an accounting tombstone instead of cascading away consumer history.
			for _, table := range []string{
				"openai_totp_rotations", "openai_oauth_reauth_tasks", "openai_oauth_reauth_configs",
				"openai_session_export_tasks", "account_token_guard_v2_accounts", "openai_auth_connection_affinity",
				"openai_excel_oauth_credentials", "openai_siwc_credentials",
				"openai_session_cookies", "new_api_account_bindings", "account_trash_archives", "account_groups",
			} {
				if _, err := client.ExecContext(ctx, `DELETE FROM `+table+` WHERE account_id = $1`, id); err != nil {
					return err
				}
			}
			// Connection attribution has a foreign key to these rows. Retain the
			// metadata for accounting, erase both credential slots and fence workers.
			if _, err := client.ExecContext(ctx, `UPDATE openai_auth_connections SET
				credentials_ciphertext = '', refresh_candidate_ciphertext = '',
				status = 'disabled', preferred = FALSE, lease_owner = '', lease_until = NULL,
				lease_epoch = lease_epoch + 1, refresh_state = 'idle', refresh_attempt_id = '',
				revision = revision + 1, updated_at = NOW() WHERE account_id = $1`, id); err != nil {
				return err
			}
			if _, err := client.ExecContext(ctx, `UPDATE accounts SET credentials = '{}'::jsonb,
				extra = '{"account_trash_purged":true}'::jsonb, notes = NULL, error_message = NULL,
				proxy_id = NULL, proxy_fallback_origin_id = NULL, parent_account_id = NULL,
				quota_dimension = 'global', schedulable = FALSE, status = 'disabled', updated_at = NOW()
				WHERE id = $1 AND deleted_at IS NOT NULL`, id); err != nil {
				return err
			}
		} else {
			// With no consumption rows, perform the requested physical deletion.
			// Other dependencies retain their established FK strategies.
			if _, err := client.ExecContext(ctx, `DELETE FROM accounts WHERE id = $1 AND deleted_at IS NOT NULL`, id); err != nil {
				return err
			}
		}
		if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &id, nil, buildSchedulerGroupPayload(groupIDs)); err != nil {
			return err
		}
		r.accountTrashAfterCommit(ctx, id)
		return nil
	})
	var pgErr *pq.Error
	if errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "23001") {
		return service.ErrAccountTrashReferenced
	}
	return err
}

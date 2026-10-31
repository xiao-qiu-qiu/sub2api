//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountTrashRoundTripAndPurgePreservesUsage(t *testing.T) {
	ctx := context.Background()
	db, client := newImageUpgradeDatabase(t, ctx)
	repo := newAccountRepositoryWithSQL(client, db, nil)
	id := trashFixtureID(t, db, `INSERT INTO accounts(name,platform,type,credentials)
		VALUES ('trash round trip','openai','oauth','{"access_token":"fixture-token","email":"fixture@example.invalid"}') RETURNING id`)
	proxyID := trashFixtureID(t, db, `INSERT INTO proxies(name,protocol,host,port) VALUES ('trash-proxy','http','127.0.0.1',9999) RETURNING id`)
	groupID := trashFixtureID(t, db, `INSERT INTO groups(name,platform) VALUES ('trash-group','openai') RETURNING id`)
	deletedGroupID := trashFixtureID(t, db, `INSERT INTO groups(name,platform) VALUES ('trash-old-group','openai') RETURNING id`)
	trashFixtureExec(t, db, `UPDATE accounts SET proxy_id=$2 WHERE id=$1`, id, proxyID)
	trashFixtureExec(t, db, `INSERT INTO account_groups(account_id,group_id,priority,allowed_models)
		VALUES ($1,$2,7,'["gpt-6*"]'), ($1,$3,9,NULL)`, id, groupID, deletedGroupID)
	planID := trashFixtureID(t, db, `INSERT INTO scheduled_test_plans(account_id,model_id) VALUES($1,'fixture') RETURNING id`, id)
	trashFixtureExec(t, db, `INSERT INTO scheduled_test_results(plan_id) VALUES($1)`, planID)
	cfg := &config.Config{Totp: config.TotpConfig{EncryptionKey: strings.Repeat("ab", 32), EncryptionKeyConfigured: true}}
	encryptor, err := NewAESEncryptor(cfg)
	require.NoError(t, err)
	password, err := encryptor.Encrypt("fixture-password")
	require.NoError(t, err)
	totp, err := encryptor.Encrypt("fixture-totp")
	require.NoError(t, err)
	otpURL, err := encryptor.Encrypt("https://fixture.invalid/otp")
	require.NoError(t, err)
	trashFixtureExec(t, db, `INSERT INTO openai_oauth_reauth_configs(account_id,login_email,credential_mode,engine,
		proxy_source,proxy_id,password_ciphertext,totp_secret_ciphertext,otp_url_ciphertext)
		VALUES($1,'fixture@example.invalid','password_totp','session_studio','managed_proxy',$2,$3,$4,$5)`, id, proxyID, password, totp, otpURL)
	taskID := trashFixtureID(t, db, `INSERT INTO openai_oauth_reauth_tasks(account_id,status,stage,expected_credentials_hash)
		VALUES($1,'running','starting','fixture') RETURNING id`, id)
	trashFixtureExec(t, db, `INSERT INTO openai_totp_rotations(task_id,account_id,state,previous_ciphertext,password_ciphertext,config_updated_at)
		SELECT $2,$1,'running',totp_secret_ciphertext,password_ciphertext,updated_at FROM openai_oauth_reauth_configs WHERE account_id=$1`, id, taskID)
	userID := trashFixtureID(t, db, `INSERT INTO users(email,password_hash) VALUES('trash-consumer@fixture.invalid','fixture') RETURNING id`)
	keyID := trashFixtureID(t, db, `INSERT INTO api_keys(user_id,key,name) VALUES($1,'trash-fixture-key','fixture') RETURNING id`, userID)
	usageID := trashFixtureID(t, db, `INSERT INTO usage_logs(user_id,api_key_id,account_id,model,total_cost,actual_cost)
		VALUES($1,$2,$3,'fixture-model',0.25,0.125) RETURNING id`, userID, keyID, id)
	connectionID := "00000000-0000-4000-8000-000000000001"
	trashFixtureExec(t, db, `INSERT INTO openai_auth_connections
		(id,account_id,kind,label,status,credentials_ciphertext,refresh_candidate_ciphertext)
		VALUES($1,$2,'codex','fixture','active',$3,$4)`, connectionID, id, password, totp)
	trashFixtureExec(t, db, `INSERT INTO openai_auth_connection_usage(account_id,api_key_id,request_id,connection_id)
		VALUES($1,$2,'fixture-request',$3)`, id, keyID, connectionID)
	trashFixtureExec(t, db, `INSERT INTO openai_auth_connection_affinity(account_id,api_key_id,session_hash,connection_id,expires_at)
		VALUES($1,$2,'fixture-session',$3,NOW()+INTERVAL '1 hour')`, id, keyID, connectionID)
	assertUsage := func() {
		var accountID int64
		var actualCost float64
		require.NoError(t, db.QueryRow(`SELECT account_id,actual_cost FROM usage_logs WHERE id=$1`, usageID).Scan(&accountID, &actualCost))
		require.Equal(t, id, accountID)
		require.Equal(t, 0.125, actualCost)
	}
	require.ErrorIs(t, repo.PermanentlyDeleteAccount(ctx, id), service.ErrAccountNotDeleted)
	require.ErrorIs(t, repo.RestoreAccount(ctx, id), service.ErrAccountNotDeleted)
	// Active 2FA work does not introduce a new restriction on ordinary deletion.
	require.NoError(t, repo.Delete(ctx, id))
	require.NoError(t, repo.Delete(ctx, id), "repeated delete preserves the first archive")
	assertUsage()
	var archivedPassword, archivedTOTP, archivedOTP string
	require.NoError(t, db.QueryRow(`SELECT password_ciphertext,totp_secret_ciphertext,otp_url_ciphertext FROM account_trash_archives WHERE account_id=$1`, id).
		Scan(&archivedPassword, &archivedTOTP, &archivedOTP))
	require.Equal(t, password, archivedPassword)
	require.Equal(t, totp, archivedTOTP)
	require.Equal(t, otpURL, archivedOTP)
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM openai_oauth_reauth_configs WHERE account_id=$1`, id)
	trashFixtureCount(t, db, 0, `SELECT count(*) FROM account_groups WHERE account_id=$1`, id)
	trashFixtureCount(t, db, 0, `SELECT count(*) FROM scheduled_test_plans WHERE account_id=$1`, id)
	trashFixtureCount(t, db, 0, `SELECT count(*) FROM scheduled_test_results WHERE plan_id=$1`, planID)
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM openai_oauth_reauth_tasks WHERE id=$1 AND status='failed'`, taskID)
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM openai_totp_rotations WHERE task_id=$1 AND state='failed'`, taskID)
	items, page, err := repo.ListAccountTrash(ctx, pagination.PaginationParams{Page: 1, PageSize: 1}, "TURE@EXAMPLE")
	require.NoError(t, err)
	require.EqualValues(t, 1, page.Total)
	require.Len(t, items, 1)
	require.Equal(t, id, items[0].ID)
	require.True(t, items[0].HasLoginCredentials)
	encoded, err := json.Marshal(items)
	require.NoError(t, err)
	for _, secret := range []string{password, totp, otpURL, "fixture-token", "password_ciphertext"} {
		require.NotContains(t, string(encoded), secret)
	}
	items, page, err = repo.ListAccountTrash(ctx, pagination.PaginationParams{Page: 2, PageSize: 1}, "round trip")
	require.NoError(t, err)
	require.Empty(t, items)
	require.EqualValues(t, 1, page.Total)
	// The archive alone must prevent replacement-key initialization after key loss.
	trashFixtureExec(t, db, `DELETE FROM openai_oauth_reauth_configs WHERE account_id=$1`, id)
	t.Setenv("DATA_DIR", t.TempDir())
	_, err = NewOpenAICredentialEncryptor(nil, nil, db).EncryptionStatus()
	require.ErrorContains(t, err, "restore the original credential encryption key")
	trashFixtureExec(t, db, `UPDATE groups SET deleted_at=NOW() WHERE id=$1`, deletedGroupID)
	trashFixtureExec(t, db, `UPDATE proxies SET deleted_at=NOW() WHERE id=$1`, proxyID)
	require.ErrorIs(t, repo.RestoreAccount(ctx, id), service.ErrAccountTrashProxyUnavailable)
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM accounts WHERE id=$1 AND deleted_at IS NOT NULL`, id)
	// Inactive/expired proxies retain their original status and exact references.
	trashFixtureExec(t, db, `UPDATE proxies SET deleted_at=NULL,status='inactive',expires_at=NOW()-INTERVAL '1 day' WHERE id=$1`, proxyID)
	require.NoError(t, repo.RestoreAccount(ctx, id))
	assertUsage()
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM accounts WHERE id=$1 AND deleted_at IS NULL AND proxy_id=$2
		AND credentials->>'access_token'='fixture-token'`, id, proxyID)
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM account_groups WHERE account_id=$1 AND group_id=$2
		AND priority=7 AND allowed_models='["gpt-6*"]'::jsonb`, id, groupID)
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM account_groups WHERE account_id=$1`, id)
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM openai_oauth_reauth_configs WHERE account_id=$1
		AND proxy_source='managed_proxy' AND proxy_id=$2 AND engine='session_studio'
		AND password_ciphertext=$3 AND totp_secret_ciphertext=$4 AND otp_url_ciphertext=$5`, id, proxyID, password, totp, otpURL)
	trashFixtureCount(t, db, 0, `SELECT count(*) FROM scheduled_test_plans WHERE account_id=$1`, id)
	trashFixtureCount(t, db, 0, `SELECT count(*) FROM account_trash_archives WHERE account_id=$1`, id)
	require.NoError(t, repo.Delete(ctx, id))
	require.NoError(t, repo.PermanentlyDeleteAccount(ctx, id))
	assertUsage()
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM openai_auth_connections WHERE account_id=$1
		AND credentials_ciphertext='' AND refresh_candidate_ciphertext='' AND status='disabled'`, id)
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM openai_auth_connection_usage WHERE account_id=$1`, id)
	trashFixtureCount(t, db, 0, `SELECT count(*) FROM openai_auth_connection_affinity WHERE account_id=$1`, id)
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM accounts WHERE id=$1 AND deleted_at IS NOT NULL
		AND credentials='{}'::jsonb AND extra->>'account_trash_purged'='true' AND schedulable=FALSE`, id)
	for _, table := range []string{"account_trash_archives", "openai_oauth_reauth_configs", "openai_oauth_reauth_tasks", "openai_totp_rotations"} {
		trashFixtureCount(t, db, 0, `SELECT count(*) FROM `+table+` WHERE account_id=$1`, id)
	}
	items, _, err = repo.ListAccountTrash(ctx, pagination.DefaultPagination(), "")
	require.NoError(t, err)
	require.Empty(t, items)
	require.ErrorIs(t, repo.RestoreAccount(ctx, id), service.ErrAccountNotFound)
	// Rows without consumption can be physically removed with their archives.
	unusedID := trashFixtureID(t, db, `INSERT INTO accounts(name,platform,type) VALUES('unused','gemini','oauth') RETURNING id`)
	require.NoError(t, repo.Delete(ctx, unusedID))
	require.NoError(t, repo.PermanentlyDeleteAccount(ctx, unusedID))
	trashFixtureCount(t, db, 0, `SELECT count(*) FROM accounts WHERE id=$1`, unusedID)
	trashFixtureCount(t, db, 0, `SELECT count(*) FROM account_trash_archives WHERE account_id=$1`, unusedID)
}

func TestAccountTrashLegacyShadowAndAtomicCascade(t *testing.T) {
	ctx := context.Background()
	db, client := newImageUpgradeDatabase(t, ctx)
	repo := newAccountRepositoryWithSQL(client, db, nil)
	legacyID := trashFixtureID(t, db, `INSERT INTO accounts(name,platform,type,deleted_at)
		VALUES('legacy trash','openai','oauth',NOW()) RETURNING id`)
	trashFixtureExec(t, db, `INSERT INTO openai_oauth_reauth_configs(account_id,login_email,otp_url_ciphertext)
		VALUES($1,'legacy@fixture.invalid','fixture-existing-ciphertext')`, legacyID)
	items, page, err := repo.ListAccountTrash(ctx, pagination.DefaultPagination(), "legacy@fixture")
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.EqualValues(t, 1, page.Total)
	require.True(t, items[0].HasLoginCredentials)
	require.NoError(t, repo.RestoreAccount(ctx, legacyID))
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM openai_oauth_reauth_configs WHERE account_id=$1
		AND otp_url_ciphertext='fixture-existing-ciphertext'`, legacyID)
	parentID := trashFixtureID(t, db, `INSERT INTO accounts(name,platform,type) VALUES('trash parent','openai','oauth') RETURNING id`)
	shadowID := trashFixtureID(t, db, `INSERT INTO accounts(name,platform,type,parent_account_id,quota_dimension)
		VALUES('trash shadow','openai','oauth',$1,'spark') RETURNING id`, parentID)
	admin := service.NewAdminService(nil, nil, nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		client, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	// A failed durable outbox write must roll back both shadow and parent.
	trashFixtureExec(t, db, `CREATE FUNCTION reject_trash_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'fixture outbox failure'; END; $$;
		CREATE TRIGGER reject_trash_outbox BEFORE INSERT ON scheduler_outbox FOR EACH ROW EXECUTE FUNCTION reject_trash_outbox()`)
	require.ErrorContains(t, admin.DeleteAccount(ctx, parentID), "fixture outbox failure")
	trashFixtureCount(t, db, 2, `SELECT count(*) FROM accounts WHERE id IN ($1,$2) AND deleted_at IS NULL`, parentID, shadowID)
	trashFixtureCount(t, db, 0, `SELECT count(*) FROM account_trash_archives WHERE account_id IN ($1,$2)`, parentID, shadowID)
	trashFixtureExec(t, db, `DROP TRIGGER reject_trash_outbox ON scheduler_outbox; DROP FUNCTION reject_trash_outbox()`)
	require.NoError(t, admin.DeleteAccount(ctx, parentID))
	trashFixtureCount(t, db, 2, `SELECT count(*) FROM account_trash_archives WHERE account_id IN ($1,$2)`, parentID, shadowID)
	require.ErrorIs(t, repo.RestoreAccount(ctx, shadowID), service.ErrAccountTrashParent)
	require.ErrorIs(t, repo.PermanentlyDeleteAccount(ctx, parentID), service.ErrAccountTrashReferenced)
	require.NoError(t, repo.RestoreAccount(ctx, parentID))
	replacementID := trashFixtureID(t, db, `INSERT INTO accounts(name,platform,type,parent_account_id,quota_dimension)
		VALUES('replacement shadow','openai','oauth',$1,'spark') RETURNING id`, parentID)
	require.ErrorIs(t, repo.RestoreAccount(ctx, shadowID), service.ErrAccountTrashShadowExists)
	require.NoError(t, repo.Delete(ctx, replacementID))
	require.NoError(t, repo.PermanentlyDeleteAccount(ctx, replacementID))
	require.NoError(t, repo.RestoreAccount(ctx, shadowID))
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM accounts WHERE id=$1 AND parent_account_id=$2 AND deleted_at IS NULL`, shadowID, parentID)
	require.NoError(t, admin.DeleteAccount(ctx, parentID))
	require.NoError(t, repo.PermanentlyDeleteAccount(ctx, shadowID))
	require.NoError(t, repo.PermanentlyDeleteAccount(ctx, parentID))
	trashFixtureCount(t, db, 0, `SELECT count(*) FROM accounts WHERE id IN ($1,$2)`, parentID, shadowID)
}

func TestAccountTrashTOTPRotationRecovery(t *testing.T) {
	ctx := context.Background()
	db, client := newImageUpgradeDatabase(t, ctx)
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	rotations := NewOpenAIOAuthReauthRepository(db).(*openAIOAuthReauthRepository)
	for _, tc := range []struct {
		state, action, candidate string
		cancel                   bool
	}{
		{state: "queued", action: "rotate", cancel: true},
		{state: "running", action: "rotate", cancel: true},
		{state: "enrolling", action: "rotate"},
		{state: "prepared", action: "rotate", candidate: "enc:candidate"},
		{state: "activating", action: "rotate", candidate: "enc:candidate"},
		{state: "verifying", action: "rotate", candidate: "enc:candidate"},
		{state: "uncertain", action: "rotate", candidate: "enc:candidate"},
		{state: "queued", action: "verify_new", candidate: "enc:candidate"},
		{state: "running", action: "verify_old", candidate: "enc:candidate"},
	} {
		t.Run(tc.state+"_"+tc.action, func(t *testing.T) {
			id, rotation := trashTOTPFixture(t, db, rotations)
			trashFixtureExec(t, db, `UPDATE openai_totp_rotations SET state=$2,action=$3,
				candidate_ciphertext=$4,worker_id='trash-old-worker' WHERE task_id=$1`,
				rotation.TaskID, tc.state, tc.action, tc.candidate)
			require.NoError(t, accounts.Delete(ctx, id))
			require.NoError(t, accounts.Delete(ctx, id), "repeated deletion must retain recovery material")
			saved, err := rotations.GetTOTPRotation(ctx, id)
			require.NoError(t, err)
			wantState := "uncertain"
			wantTaskStatus := "callback_processing"
			if tc.cancel {
				wantState, wantTaskStatus = "failed", "failed"
			}
			require.Equal(t, wantState, saved.State)
			require.Empty(t, saved.WorkerID)
			require.Equal(t, tc.candidate, saved.CandidateCiphertext)
			trashFixtureCount(t, db, 1, `SELECT count(*) FROM openai_totp_rotations WHERE task_id=$1
				AND previous_ciphertext='enc:old' AND password_ciphertext='enc:password'`, rotation.TaskID)
			trashFixtureCount(t, db, 1, `SELECT count(*) FROM openai_oauth_reauth_tasks
				WHERE id=$1 AND status=$2 AND worker_id IS NULL`, rotation.TaskID, wantTaskStatus)
			require.Error(t, rotations.FinishTOTPRotation(ctx, rotation.TaskID, "trash-old-worker", true, ""))
			if !tc.cancel {
				_, err := rotations.CreateTask(ctx, id, "overlapping-login")
				require.Error(t, err, "unresolved 2FA must retain the active-account reservation")
			}
			require.NoError(t, accounts.RestoreAccount(ctx, id))
			cfg, err := rotations.GetConfig(ctx, id)
			require.NoError(t, err)
			require.True(t, cfg.UpdatedAt.After(rotation.ConfigUpdatedAt))
			saved, err = rotations.GetTOTPRotation(ctx, id)
			require.NoError(t, err)
			require.Equal(t, wantState, saved.State)
			require.Equal(t, tc.candidate, saved.CandidateCiphertext)
			require.Empty(t, saved.WorkerID)
			if !tc.cancel {
				require.True(t, saved.ConfigUpdatedAt.Equal(cfg.UpdatedAt), "recovery must use the restored config version")
				claim, err := rotations.ClaimTOTPRotation(ctx, "automatic-worker")
				require.NoError(t, err)
				require.Nil(t, claim, "restoration must not replay upstream operations automatically")
				candidate := tc.candidate
				if candidate == "" {
					candidate = "enc:journal-candidate"
					require.NoError(t, rotations.RecoverTOTPCandidate(ctx, rotation.TaskID, candidate))
				}
				action := "verify_new"
				wantTOTP := candidate
				if tc.action == "verify_old" {
					action, wantTOTP = "verify_old", "enc:old"
				}
				require.NoError(t, rotations.RetryTOTPRotation(ctx, id, rotation.TaskID, action))
				claim, err = rotations.ClaimTOTPRotation(ctx, "recovery-worker")
				require.NoError(t, err)
				require.NotNil(t, claim)
				require.Equal(t, rotation.TaskID, claim.TaskID)
				require.Equal(t, action, claim.Action)
				require.Equal(t, candidate, claim.CandidateCiphertext)
				require.Error(t, rotations.TransitionTOTPRotation(ctx, rotation.TaskID, "trash-old-worker", "running", "verifying", ""))
				require.NoError(t, rotations.TransitionTOTPRotation(ctx, rotation.TaskID, "recovery-worker", "running", "verifying", ""))
				require.Error(t, rotations.FinishTOTPRotation(ctx, rotation.TaskID, "trash-old-worker", true, ""))
				require.NoError(t, rotations.FinishTOTPRotation(ctx, rotation.TaskID, "recovery-worker", true, ""))
				cfg, err = rotations.GetConfig(ctx, id)
				require.NoError(t, err)
				require.Equal(t, wantTOTP, cfg.TOTPSecretCiphertext)
			}
			require.NoError(t, accounts.Delete(ctx, id))
			require.NoError(t, accounts.PermanentlyDeleteAccount(ctx, id))
			trashFixtureCount(t, db, 0, `SELECT count(*) FROM openai_totp_rotations WHERE task_id=$1`, rotation.TaskID)
		})
	}
}

func TestAccountTrashTOTPConfigMismatchAndPurge(t *testing.T) {
	ctx := context.Background()
	db, client := newImageUpgradeDatabase(t, ctx)
	accounts := newAccountRepositoryWithSQL(client, db, nil)
	rotations := NewOpenAIOAuthReauthRepository(db).(*openAIOAuthReauthRepository)
	for _, mismatch := range []string{"config_version", "archive_ciphertext", "archive_identity"} {
		t.Run(mismatch, func(t *testing.T) {
			id, rotation := trashTOTPFixture(t, db, rotations)
			trashFixtureExec(t, db, `UPDATE openai_totp_rotations SET state='uncertain',
				candidate_ciphertext='enc:candidate',worker_id='trash-old-worker' WHERE task_id=$1`, rotation.TaskID)
			if mismatch == "config_version" {
				trashFixtureExec(t, db, `UPDATE openai_totp_rotations SET config_updated_at=config_updated_at-INTERVAL '1 second'
					WHERE task_id=$1`, rotation.TaskID)
			}
			require.NoError(t, accounts.Delete(ctx, id), "unresolved 2FA must not block ordinary deletion")
			switch mismatch {
			case "archive_ciphertext":
				trashFixtureExec(t, db, `UPDATE account_trash_archives SET totp_secret_ciphertext='enc:different' WHERE account_id=$1`, id)
			case "archive_identity":
				trashFixtureExec(t, db, `UPDATE account_trash_archives SET login_email='another@fixture.invalid' WHERE account_id=$1`, id)
			}
			require.ErrorIs(t, accounts.RestoreAccount(ctx, id), service.ErrAccountTrashTOTPConfigChanged)
			trashFixtureCount(t, db, 1, `SELECT count(*) FROM accounts WHERE id=$1 AND deleted_at IS NOT NULL`, id)
			trashFixtureCount(t, db, 1, `SELECT count(*) FROM account_trash_archives WHERE account_id=$1`, id)
			trashFixtureCount(t, db, 1, `SELECT count(*) FROM openai_totp_rotations WHERE task_id=$1
				AND state='uncertain' AND candidate_ciphertext='enc:candidate' AND worker_id=''`, rotation.TaskID)
			if mismatch == "archive_ciphertext" {
				// Exercise the accounting-tombstone purge path with an unresolved rotation.
				userID := trashFixtureID(t, db, `INSERT INTO users(email,password_hash) VALUES('totp-purge@fixture.invalid','fixture') RETURNING id`)
				keyID := trashFixtureID(t, db, `INSERT INTO api_keys(user_id,key,name) VALUES($1,'totp-purge-key','fixture') RETURNING id`, userID)
				trashFixtureExec(t, db, `INSERT INTO usage_logs(user_id,api_key_id,account_id,model) VALUES($1,$2,$3,'fixture')`, userID, keyID, id)
			}
			require.NoError(t, accounts.PermanentlyDeleteAccount(ctx, id))
			trashFixtureCount(t, db, 0, `SELECT count(*) FROM openai_totp_rotations WHERE task_id=$1`, rotation.TaskID)
			trashFixtureCount(t, db, 0, `SELECT count(*) FROM openai_oauth_reauth_configs WHERE account_id=$1`, id)
			trashFixtureCount(t, db, 0, `SELECT count(*) FROM account_trash_archives WHERE account_id=$1`, id)
			if mismatch == "archive_ciphertext" {
				trashFixtureCount(t, db, 1, `SELECT count(*) FROM usage_logs WHERE account_id=$1`, id)
			}
		})
	}
}

func trashTOTPFixture(t *testing.T, db *sql.DB, rotations *openAIOAuthReauthRepository) (int64, *service.OpenAITOTPRotation) {
	t.Helper()
	id := trashFixtureID(t, db, `INSERT INTO accounts(name,platform,type,credentials)
		VALUES('trash 2FA','openai','oauth','{"email":"totp@fixture.invalid"}') RETURNING id`)
	trashFixtureExec(t, db, `INSERT INTO openai_oauth_reauth_configs(account_id,login_email,credential_mode,
		password_ciphertext,totp_secret_ciphertext,updated_at)
		VALUES($1,'totp@fixture.invalid','password_totp','enc:password','enc:old',NOW()-INTERVAL '1 hour')`, id)
	cfg, err := rotations.GetConfig(context.Background(), id)
	require.NoError(t, err)
	rotation, err := rotations.CreateTOTPRotation(context.Background(), id, "fixture-snapshot", cfg.UpdatedAt)
	require.NoError(t, err)
	return id, rotation
}

func trashFixtureID(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var id int64
	require.NoError(t, db.QueryRow(query, args...).Scan(&id))
	return id
}

func trashFixtureExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, query, args...)
	require.NoError(t, err)
}

func trashFixtureCount(t *testing.T, db *sql.DB, want int64, query string, args ...any) {
	t.Helper()
	var count int64
	require.NoError(t, db.QueryRow(query, args...).Scan(&count))
	require.Equal(t, want, count)
}

//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountTrashDeleteNoteTransactionAndLifecycle(t *testing.T) {
	ctx := context.Background()
	db, client := newImageUpgradeDatabase(t, ctx)
	repo := newAccountRepositoryWithSQL(client, db, nil)
	id := trashFixtureID(t, db, `INSERT INTO accounts(name,platform,type,notes)
		VALUES('delete note lifecycle','openai','oauth','original account notes') RETURNING id`)
	note := "暂停使用 'quoted'\n待检查"

	// A caller-owned transaction must contain both the archive note and deletion.
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	txctx := dbent.NewTxContext(ctx, tx)
	require.NoError(t, repo.DeleteWithNote(txctx, id, " \t"+note+"\n "))
	items, _, err := repo.ListAccountTrash(txctx, pagination.DefaultPagination(), "delete note lifecycle")
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, note, items[0].DeleteNote)
	trashFixtureCount(t, db, 0, `SELECT count(*) FROM account_trash_archives WHERE account_id=$1`, id)
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM accounts WHERE id=$1 AND deleted_at IS NULL`, id)
	require.NoError(t, tx.Rollback())
	trashFixtureCount(t, db, 0, `SELECT count(*) FROM account_trash_archives WHERE account_id=$1`, id)
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM accounts WHERE id=$1 AND deleted_at IS NULL`, id)

	require.NoError(t, repo.DeleteWithNote(ctx, id, " \t"+note+"\n "))
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM account_trash_archives WHERE account_id=$1 AND delete_note=$2`, id, note)
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM accounts WHERE id=$1 AND deleted_at IS NOT NULL AND notes='original account notes'`, id)
	// Both legacy and new repeat calls preserve the first note and archive time.
	var archivedAt string
	require.NoError(t, db.QueryRow(`SELECT archived_at::text FROM account_trash_archives WHERE account_id=$1`, id).Scan(&archivedAt))
	require.NoError(t, repo.DeleteWithNote(ctx, id, "replacement must not win"))
	require.NoError(t, repo.Delete(ctx, id))
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM account_trash_archives WHERE account_id=$1 AND delete_note=$2 AND archived_at::text=$3`, id, note, archivedAt)
	items, _, err = repo.ListAccountTrash(ctx, pagination.DefaultPagination(), "delete note lifecycle")
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, note, items[0].DeleteNote)

	require.NoError(t, repo.RestoreAccount(ctx, id))
	trashFixtureCount(t, db, 0, `SELECT count(*) FROM account_trash_archives WHERE account_id=$1`, id)
	require.NoError(t, repo.Delete(ctx, id))
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM account_trash_archives WHERE account_id=$1 AND delete_note=''`, id)
	require.NoError(t, repo.RestoreAccount(ctx, id))
	require.NoError(t, repo.DeleteWithNote(ctx, id, "new deletion"))
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM account_trash_archives WHERE account_id=$1 AND delete_note='new deletion'`, id)

	// Legacy trash without an archive still serializes an empty string.
	legacyID := trashFixtureID(t, db, `INSERT INTO accounts(name,platform,type,deleted_at)
		VALUES('legacy no delete note','openai','oauth',NOW()) RETURNING id`)
	items, _, err = repo.ListAccountTrash(ctx, pagination.DefaultPagination(), "legacy no delete note")
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, legacyID, items[0].ID)
	encoded, err := json.Marshal(items[0])
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"delete_note":""`)

	// Repository and SQL constraints agree on characters, rather than UTF-8 bytes.
	validID := trashFixtureID(t, db, `INSERT INTO accounts(name,platform,type) VALUES('note limit','openai','oauth') RETURNING id`)
	require.Error(t, repo.DeleteWithNote(ctx, validID, strings.Repeat("备", 1001)))
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM accounts WHERE id=$1 AND deleted_at IS NULL`, validID)
	trashFixtureCount(t, db, 0, `SELECT count(*) FROM account_trash_archives WHERE account_id=$1`, validID)
	require.NoError(t, repo.DeleteWithNote(ctx, validID, strings.Repeat("备", 1000)))
	_, err = db.Exec(`UPDATE account_trash_archives SET delete_note=$2 WHERE account_id=$1`, validID, strings.Repeat("备", 1001))
	require.Error(t, err)
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM account_trash_archives WHERE account_id=$1 AND char_length(delete_note)=1000`, validID)
}

func TestAccountTrashDeleteNoteAtomicCascade(t *testing.T) {
	ctx := context.Background()
	db, client := newImageUpgradeDatabase(t, ctx)
	repo := newAccountRepositoryWithSQL(client, db, nil)
	parentID := trashFixtureID(t, db, `INSERT INTO accounts(name,platform,type,notes) VALUES('noted parent','openai','oauth','parent notes') RETURNING id`)
	shadowID := trashFixtureID(t, db, `INSERT INTO accounts(name,platform,type,parent_account_id,quota_dimension)
		VALUES('noted shadow','openai','oauth',$1,'spark') RETURNING id`, parentID)
	admin := service.NewAdminService(nil, nil, nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		client, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	noted := admin.(service.AccountDeleteNoteAdminService)
	// Fail only on the parent, after the shadow archive/deletion has been written.
	trashFixtureExec(t, db, fmt.Sprintf(`CREATE FUNCTION reject_noted_parent_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.account_id = %d THEN RAISE EXCEPTION 'fixture noted parent outbox failure'; END IF; RETURN NEW; END; $$;
		CREATE TRIGGER reject_noted_parent_outbox BEFORE INSERT ON scheduler_outbox FOR EACH ROW EXECUTE FUNCTION reject_noted_parent_outbox()`, parentID))
	require.ErrorContains(t, noted.DeleteAccountWithNote(ctx, parentID, "family note"), "fixture noted parent outbox failure")
	trashFixtureCount(t, db, 2, `SELECT count(*) FROM accounts WHERE id IN ($1,$2) AND deleted_at IS NULL`, parentID, shadowID)
	trashFixtureCount(t, db, 0, `SELECT count(*) FROM account_trash_archives WHERE account_id IN ($1,$2)`, parentID, shadowID)
	trashFixtureExec(t, db, `DROP TRIGGER reject_noted_parent_outbox ON scheduler_outbox; DROP FUNCTION reject_noted_parent_outbox()`)
	require.NoError(t, noted.DeleteAccountWithNote(ctx, parentID, "  family note \n"))
	trashFixtureCount(t, db, 2, `SELECT count(*) FROM account_trash_archives WHERE account_id IN ($1,$2) AND delete_note='family note'`, parentID, shadowID)
	require.NoError(t, noted.DeleteAccountWithNote(ctx, parentID, "must not overwrite"))
	trashFixtureCount(t, db, 2, `SELECT count(*) FROM account_trash_archives WHERE account_id IN ($1,$2) AND delete_note='family note'`, parentID, shadowID)
	require.NoError(t, repo.RestoreAccount(ctx, parentID))
	require.NoError(t, repo.RestoreAccount(ctx, shadowID))
	require.NoError(t, admin.DeleteAccount(ctx, parentID))
	trashFixtureCount(t, db, 2, `SELECT count(*) FROM account_trash_archives WHERE account_id IN ($1,$2) AND delete_note=''`, parentID, shadowID)
	trashFixtureCount(t, db, 1, `SELECT count(*) FROM accounts WHERE id=$1 AND notes='parent notes'`, parentID)
}

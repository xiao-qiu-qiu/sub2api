package repository

import (
	"errors"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func newProxyTrashTestRepository(t *testing.T) (*proxyRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	return newProxyRepositoryWithSQL(client, db), mock
}

func TestProxyTrashListSelectsDeletedRowsWithoutCredentialsAndSearchesHost(t *testing.T) {
	repo, mock := newProxyTrashTestRepository(t)
	mock.ExpectQuery(`(?s)SELECT COUNT.*FROM "proxies" WHERE "proxies"\."deleted_at" IS NOT NULL AND \(.*"name".*OR.*"host"`).
		WithArgs("%exit%", "%exit%").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	deleted := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT "proxies"\."id", "proxies"\."name", "proxies"\."protocol", "proxies"\."host", "proxies"\."port", "proxies"\."status", "proxies"\."deleted_at" FROM "proxies" WHERE "proxies"\."deleted_at" IS NOT NULL.*ORDER BY "proxies"\."deleted_at" DESC, "proxies"\."id" DESC LIMIT 20`).
		WithArgs("%exit%", "%exit%").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "protocol", "host", "port", "status", "deleted_at"}).AddRow(9, "proxy", "http", "exit.example", 8080, "inactive", deleted))
	items, page, err := repo.ListProxyTrash(t.Context(), pagination.PaginationParams{Page: 1, PageSize: 20}, " exit ")
	require.NoError(t, err)
	require.EqualValues(t, 1, page.Total)
	require.Equal(t, []service.ProxyTrashItem{{ID: 9, Name: "proxy", Protocol: "http", Host: "exit.example", Port: 8080, Status: "inactive", DeletedAt: deleted}}, items)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestProxyTrashPermanentDeleteGuardsStateAndReferences(t *testing.T) {
	for _, tc := range []struct {
		name                string
		missing, referenced bool
		deleteErr           error
		want                error
	}{
		{name: "live or missing proxy", missing: true, want: service.ErrProxyTrashNotFound},
		{name: "account or fallback or reauth reference", referenced: true, want: service.ErrProxyReferenced},
		{name: "foreign key constraint", deleteErr: &pq.Error{Code: "23503"}, want: service.ErrProxyReferenced},
		{name: "unreferenced trash is physically deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, mock := newProxyTrashTestRepository(t)
			mock.ExpectBegin()
			rows := sqlmock.NewRows([]string{"id"})
			if !tc.missing {
				rows.AddRow(9)
			}
			mock.ExpectQuery(`SELECT id FROM proxies WHERE id = \$1 AND deleted_at IS NOT NULL FOR UPDATE`).WithArgs(int64(9)).WillReturnRows(rows)
			if !tc.missing {
				mock.ExpectQuery(`(?s)SELECT.*EXISTS \(SELECT 1 FROM accounts WHERE proxy_id = \$1 OR proxy_fallback_origin_id = \$1\).*EXISTS \(SELECT 1 FROM proxies WHERE backup_proxy_id = \$1 AND id <> \$1\).*EXISTS \(SELECT 1 FROM openai_oauth_reauth_configs WHERE proxy_id = \$1\)`).
					WithArgs(int64(9)).WillReturnRows(sqlmock.NewRows([]string{"referenced"}).AddRow(tc.referenced))
				if !tc.referenced {
					expect := mock.ExpectExec(`DELETE FROM proxies WHERE id = \$1 AND deleted_at IS NOT NULL`).WithArgs(int64(9))
					if tc.deleteErr != nil {
						expect.WillReturnError(tc.deleteErr)
					} else {
						expect.WillReturnResult(sqlmock.NewResult(0, 1))
					}
				}
			}
			if tc.want != nil {
				mock.ExpectRollback()
			} else {
				mock.ExpectCommit()
			}
			err := repo.PermanentlyDeleteProxy(t.Context(), 9)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestProxyTrashRestorePreservesConfigurationAndHandlesConflict(t *testing.T) {
	for _, tc := range []struct {
		name           string
		conflict       bool
		writeErr, want error
	}{
		{name: "restore only clears tombstone"},
		{name: "live node or endpoint conflict", conflict: true, want: service.ErrProxyRestoreConflict},
		{name: "concurrent unique index conflict", writeErr: &pq.Error{Code: "23505"}, want: service.ErrProxyRestoreConflict},
		{name: "database failure", writeErr: errors.New("write failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, mock := newProxyTrashTestRepository(t)
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT id FROM proxies WHERE id = \$1 AND deleted_at IS NOT NULL FOR UPDATE`).WithArgs(int64(9)).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(9))
			mock.ExpectQuery(`SELECT protocol, host, port, COALESCE\(mihomo_node_id, ''\) FROM proxies WHERE id = \$1 AND deleted_at IS NOT NULL`).WithArgs(int64(9)).
				WillReturnRows(sqlmock.NewRows([]string{"protocol", "host", "port", "mihomo_node_id"}).AddRow("http", "127.0.0.1", 32000, "node"))
			mock.ExpectQuery(`(?s)SELECT EXISTS.*deleted_at IS NULL.*mihomo_node_id = \$2 OR \(protocol = \$3 AND host = \$4 AND port = \$5\)`).WithArgs(int64(9), "node", "http", "127.0.0.1", 32000).
				WillReturnRows(sqlmock.NewRows([]string{"conflict"}).AddRow(tc.conflict))
			if !tc.conflict {
				expect := mock.ExpectExec(`^UPDATE proxies SET deleted_at = NULL, updated_at = NOW\(\) WHERE id = \$1 AND deleted_at IS NOT NULL$`).WithArgs(int64(9))
				if tc.writeErr != nil {
					expect.WillReturnError(tc.writeErr)
				} else {
					expect.WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			if tc.conflict || tc.writeErr != nil {
				mock.ExpectRollback()
			} else {
				mock.ExpectCommit()
			}
			err := repo.RestoreProxy(t.Context(), 9)
			want := tc.want
			if want == nil {
				want = tc.writeErr
			}
			if want != nil {
				require.ErrorIs(t, err, want)
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

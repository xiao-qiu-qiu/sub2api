package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/proxy"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

var _ service.ProxyTrashRepository = (*proxyRepository)(nil)

func (r *proxyRepository) ListProxyTrash(ctx context.Context, params pagination.PaginationParams, search string) ([]service.ProxyTrashItem, *pagination.PaginationResult, error) {
	ctx = mixins.SkipSoftDelete(ctx)
	query := r.client.Proxy.Query().Where(proxy.DeletedAtNotNil())
	if search = strings.TrimSpace(search); search != "" {
		query = query.Where(proxy.Or(proxy.NameContainsFold(search), proxy.HostContainsFold(search)))
	}
	total, err := query.Count(ctx)
	if err != nil {
		return nil, nil, err
	}
	items, err := query.Select(proxy.FieldID, proxy.FieldName, proxy.FieldProtocol, proxy.FieldHost, proxy.FieldPort, proxy.FieldStatus, proxy.FieldDeletedAt).
		Order(dbent.Desc(proxy.FieldDeletedAt), dbent.Desc(proxy.FieldID)).Offset(params.Offset()).Limit(params.Limit()).All(ctx)
	if err != nil {
		return nil, nil, err
	}
	out := make([]service.ProxyTrashItem, 0, len(items))
	for _, item := range items {
		out = append(out, service.ProxyTrashItem{ID: item.ID, Name: item.Name, Protocol: item.Protocol, Host: item.Host, Port: item.Port, Status: item.Status, DeletedAt: *item.DeletedAt})
	}
	return out, paginationResultFromTotal(int64(total), params), nil
}

func (r *proxyRepository) GetTrashedProxy(ctx context.Context, id int64) (*service.Proxy, error) {
	item, err := r.client.Proxy.Query().Where(proxy.IDEQ(id), proxy.DeletedAtNotNil()).Only(mixins.SkipSoftDelete(ctx))
	if dbent.IsNotFound(err) {
		return nil, service.ErrProxyTrashNotFound
	}
	if err != nil {
		return nil, err
	}
	return proxyEntityToService(item), nil
}

func (r *proxyRepository) CheckProxyTrashRestore(ctx context.Context, item *service.Proxy) error {
	return checkProxyTrashRestore(ctx, r.sql, item)
}

func checkProxyTrashRestore(ctx context.Context, exec sqlExecutor, item *service.Proxy) error {
	if item.MihomoNodeID == "" {
		return nil
	}
	var conflict bool
	err := scanSingleRow(ctx, exec, `SELECT EXISTS (
		SELECT 1 FROM proxies WHERE deleted_at IS NULL AND id <> $1
		AND (mihomo_node_id = $2 OR (protocol = $3 AND host = $4 AND port = $5))
	)`, []any{item.ID, item.MihomoNodeID, item.Protocol, item.Host, item.Port}, &conflict)
	if err != nil {
		return err
	}
	if conflict {
		return service.ErrProxyRestoreConflict
	}
	return nil
}

func (r *proxyRepository) RestoreProxy(ctx context.Context, id int64) error {
	err := r.withProxyTrashTx(ctx, func(exec sqlExecutor) error {
		if err := lockTrashedProxy(ctx, exec, id); err != nil {
			return err
		}
		// Re-read the identity under the lock; the preflight before touching
		// Mihomo cannot replace validation inside the database transaction.
		item := &service.Proxy{ID: id}
		if err := scanSingleRow(ctx, exec, `SELECT protocol, host, port, COALESCE(mihomo_node_id, '') FROM proxies WHERE id = $1 AND deleted_at IS NOT NULL`, []any{id}, &item.Protocol, &item.Host, &item.Port, &item.MihomoNodeID); err != nil {
			return err
		}
		if err := checkProxyTrashRestore(ctx, exec, item); err != nil {
			return err
		}
		result, err := exec.ExecContext(ctx, `UPDATE proxies SET deleted_at = NULL, updated_at = NOW() WHERE id = $1 AND deleted_at IS NOT NULL`, id)
		return proxyTrashMutationResult(result, err)
	})
	var pgErr *pq.Error
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return service.ErrProxyRestoreConflict
	}
	return err
}

func (r *proxyRepository) PermanentlyDeleteProxy(ctx context.Context, id int64) error {
	err := r.withProxyTrashTx(ctx, func(exec sqlExecutor) error {
		// FOR UPDATE also conflicts with FK KEY SHARE locks. References committed
		// before this lock are visible to the next statement; later FK inserts
		// wait and fail rather than silently having their proxy_id cleared.
		if err := lockTrashedProxy(ctx, exec, id); err != nil {
			return err
		}
		var referenced bool
		if err := scanSingleRow(ctx, exec, `SELECT
			EXISTS (SELECT 1 FROM accounts WHERE proxy_id = $1 OR proxy_fallback_origin_id = $1)
			OR EXISTS (SELECT 1 FROM proxies WHERE backup_proxy_id = $1 AND id <> $1)
			OR EXISTS (SELECT 1 FROM openai_oauth_reauth_configs WHERE proxy_id = $1)
			OR EXISTS (SELECT 1 FROM account_trash_archives
				WHERE login_proxy_id = $1 OR original_proxy_id = $1 OR original_proxy_fallback_origin_id = $1)`, []any{id}, &referenced); err != nil {
			return err
		}
		// Include deleted accounts/proxies: restoring one later must retain its
		// original endpoint and fallback configuration as well.
		if referenced {
			return service.ErrProxyReferenced
		}
		result, err := exec.ExecContext(ctx, `DELETE FROM proxies WHERE id = $1 AND deleted_at IS NOT NULL`, id)
		return proxyTrashMutationResult(result, err)
	})
	var pgErr *pq.Error
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return service.ErrProxyReferenced
	}
	return err
}

func lockTrashedProxy(ctx context.Context, exec sqlExecutor, id int64) error {
	var lockedID int64
	err := scanSingleRow(ctx, exec, `SELECT id FROM proxies WHERE id = $1 AND deleted_at IS NOT NULL FOR UPDATE`, []any{id}, &lockedID)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrProxyTrashNotFound
	}
	return err
}

func proxyTrashMutationResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return service.ErrProxyTrashNotFound
	}
	return nil
}

func (r *proxyRepository) withProxyTrashTx(ctx context.Context, mutate func(sqlExecutor) error) error {
	if tx := dbent.TxFromContext(ctx); tx != nil {
		return mutate(tx)
	}
	tx, err := r.client.Tx(ctx)
	if errors.Is(err, dbent.ErrTxStarted) {
		return mutate(r.sql)
	}
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := mutate(tx); err != nil {
		return err
	}
	return tx.Commit()
}

package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/mihomo"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

var (
	ErrProxyTrashNotFound    = infraerrors.NotFound("PROXY_TRASH_NOT_FOUND", "proxy is not in the trash")
	ErrProxyReferenced       = infraerrors.Conflict("PROXY_REFERENCED", "proxy is referenced by accounts, fallback configuration or OAuth re-login configuration, including items in the trash")
	ErrProxyRestoreConflict  = infraerrors.Conflict("PROXY_RESTORE_CONFLICT", "an existing proxy already uses this Mihomo node or endpoint")
	ErrProxyTrashUnavailable = infraerrors.New(http.StatusServiceUnavailable, "PROXY_TRASH_UNAVAILABLE", "proxy trash is unavailable")
)

// ProxyTrashItem deliberately contains no credentials or internal node metadata.
type ProxyTrashItem struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Protocol  string    `json:"protocol"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Status    string    `json:"status"`
	DeletedAt time.Time `json:"deleted_at"`
}

// Optional capabilities keep the existing AdminService/ProxyRepository and mocks stable.
type ProxyTrashAdminService interface {
	ListProxyTrash(ctx context.Context, page, pageSize int, search string) ([]ProxyTrashItem, int64, error)
	RestoreProxy(ctx context.Context, id int64) error
	PermanentlyDeleteProxy(ctx context.Context, id int64) error
}

type ProxyTrashRepository interface {
	ListProxyTrash(ctx context.Context, params pagination.PaginationParams, search string) ([]ProxyTrashItem, *pagination.PaginationResult, error)
	GetTrashedProxy(ctx context.Context, id int64) (*Proxy, error)
	CheckProxyTrashRestore(ctx context.Context, proxy *Proxy) error
	RestoreProxy(ctx context.Context, id int64) error
	PermanentlyDeleteProxy(ctx context.Context, id int64) error
}

type proxyTrashMihomoKey struct{}

// WithProxyTrashMihomoBinder supplies the already running manager for this request.
func WithProxyTrashMihomoBinder(ctx context.Context, kernel MihomoStaticNodeBinder) context.Context {
	return context.WithValue(ctx, proxyTrashMihomoKey{}, kernel)
}

var _ ProxyTrashAdminService = (*adminServiceImpl)(nil)

func (s *adminServiceImpl) ListProxyTrash(ctx context.Context, page, pageSize int, search string) ([]ProxyTrashItem, int64, error) {
	repo, ok := s.proxyRepo.(ProxyTrashRepository)
	if !ok {
		return nil, 0, ErrProxyTrashUnavailable
	}
	items, result, err := repo.ListProxyTrash(ctx, pagination.PaginationParams{Page: page, PageSize: pageSize}, strings.TrimSpace(search))
	if err != nil {
		return nil, 0, err
	}
	return items, result.Total, nil
}

func (s *adminServiceImpl) RestoreProxy(ctx context.Context, id int64) error {
	s.mihomoProxyMu.Lock()
	defer s.mihomoProxyMu.Unlock()
	repo, ok := s.proxyRepo.(ProxyTrashRepository)
	if !ok {
		return ErrProxyTrashUnavailable
	}
	proxy, err := repo.GetTrashedProxy(ctx, id)
	if err != nil {
		return err
	}
	if err := repo.CheckProxyTrashRestore(ctx, proxy); err != nil {
		return err
	}
	if proxy.MihomoNodeID == "" {
		return repo.RestoreProxy(ctx, id)
	}
	if proxy.Protocol != "http" || proxy.Host != "127.0.0.1" || proxy.Username != "" || proxy.Password != "" {
		return infraerrors.Conflict("MIHOMO_STATIC_ENDPOINT_CONFLICT", "saved proxy endpoint does not match a managed static listener")
	}
	kernel, ok := ctx.Value(proxyTrashMihomoKey{}).(MihomoStaticNodeBinder)
	if !ok || kernel == nil {
		return infraerrors.New(http.StatusServiceUnavailable, "MIHOMO_STATIC_RESTORE_UNAVAILABLE", "managed Mihomo is unavailable for proxy restoration")
	}
	bindings, err := kernel.BindStaticNodes(ctx, []string{proxy.MihomoNodeID})
	if err != nil {
		if errors.Is(err, mihomo.ErrUnknownNode) {
			return infraerrors.NotFound("MIHOMO_NODE_NOT_FOUND", "node no longer exists; restore its source before restoring the proxy")
		}
		return infraerrors.Conflict("MIHOMO_STATIC_RESTORE_FAILED", "could not restore the static listener; check Mihomo readiness, node availability and the reserved port").WithCause(err)
	}
	// BindStaticNodes validates node availability and port ownership. Never change
	// the saved endpoint if its reservation was lost or recreated on another port.
	if len(bindings) != 1 || bindings[0].Name != proxy.MihomoNodeID || bindings[0].Port != proxy.Port {
		err = infraerrors.Conflict("MIHOMO_STATIC_ENDPOINT_CONFLICT", "reserved static listener does not match the saved proxy endpoint")
	} else {
		err = repo.RestoreProxy(ctx, id)
	}
	if err == nil {
		return nil
	}
	for _, binding := range bindings {
		// New also means reactivated: BindStaticNodes sets it whenever the
		// previous binding was disabled, even when its reserved port existed.
		if binding.Name != proxy.MihomoNodeID || !binding.New {
			continue
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		cleanupErr := kernel.DeactivateStaticNodes(cleanup, []string{proxy.MihomoNodeID})
		cancel()
		if cleanupErr != nil {
			return infraerrors.New(http.StatusInternalServerError, "MIHOMO_STATIC_ROLLBACK_FAILED", "proxy remains in the trash but its static listener could not be deactivated").WithCause(errors.Join(err, cleanupErr))
		}
		break
	}
	return err
}

func (s *adminServiceImpl) PermanentlyDeleteProxy(ctx context.Context, id int64) error {
	s.mihomoProxyMu.Lock()
	defer s.mihomoProxyMu.Unlock()
	repo, ok := s.proxyRepo.(ProxyTrashRepository)
	if !ok {
		return ErrProxyTrashUnavailable
	}
	// A deleted node may already have a new live proxy. Its listener belongs to
	// that live proxy, so permanent deletion must not deactivate it again.
	return repo.PermanentlyDeleteProxy(ctx, id)
}

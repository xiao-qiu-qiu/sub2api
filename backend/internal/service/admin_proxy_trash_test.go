package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/mihomo"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type proxyTrashRepoStub struct {
	ProxyRepository
	ProxyTrashRepository
	item                         *Proxy
	preflight, restoreErr        error
	restored, permanentlyDeleted bool
}

func (r *proxyTrashRepoStub) GetTrashedProxy(context.Context, int64) (*Proxy, error) {
	return r.item, nil
}
func (r *proxyTrashRepoStub) CheckProxyTrashRestore(context.Context, *Proxy) error {
	return r.preflight
}
func (r *proxyTrashRepoStub) RestoreProxy(context.Context, int64) error {
	r.restored = true
	return r.restoreErr
}
func (r *proxyTrashRepoStub) PermanentlyDeleteProxy(context.Context, int64) error {
	r.permanentlyDeleted = true
	return nil
}

type proxyTrashKernelStub struct {
	bindings            []mihomo.StaticNodeBinding
	bindErr, cleanupErr error
	bound, deactivated  bool
}

func (k *proxyTrashKernelStub) BindStaticNodes(context.Context, []string) ([]mihomo.StaticNodeBinding, error) {
	k.bound = true
	return k.bindings, k.bindErr
}
func (k *proxyTrashKernelStub) DeactivateStaticNodes(ctx context.Context, _ []string) error {
	k.deactivated = true
	if err := ctx.Err(); err != nil {
		return err
	}
	return k.cleanupErr
}

func TestProxyTrashRestoreMihomoBindingAndCompensation(t *testing.T) {
	for _, tc := range []struct {
		name                                       string
		port                                       int
		newBinding                                 bool
		preflight, bindErr, restoreErr, cleanupErr error
		wantReason                                 string
		wantBound, wantRestore, wantCleanup        bool
	}{
		{name: "restore same reserved port", port: 32000, newBinding: true, wantBound: true, wantRestore: true},
		{name: "active proxy conflict stops before kernel", preflight: ErrProxyRestoreConflict, wantReason: "PROXY_RESTORE_CONFLICT"},
		{name: "port was recreated elsewhere", port: 32001, newBinding: true, wantBound: true, wantCleanup: true, wantReason: "MIHOMO_STATIC_ENDPOINT_CONFLICT"},
		{name: "unavailable node or port", bindErr: errors.New("port busy"), wantBound: true, wantReason: "MIHOMO_STATIC_RESTORE_FAILED"},
		{name: "removed node", bindErr: mihomo.ErrUnknownNode, wantBound: true, wantReason: "MIHOMO_NODE_NOT_FOUND"},
		{name: "database failure rolls back new or reactivated listener", port: 32000, newBinding: true, restoreErr: ErrProxyRestoreConflict, wantBound: true, wantRestore: true, wantCleanup: true, wantReason: "PROXY_RESTORE_CONFLICT"},
		{name: "existing listener survives database failure", port: 32000, restoreErr: ErrProxyRestoreConflict, wantBound: true, wantRestore: true, wantReason: "PROXY_RESTORE_CONFLICT"},
		{name: "failed compensation is reported", port: 32000, newBinding: true, restoreErr: ErrProxyRestoreConflict, cleanupErr: errors.New("kernel busy"), wantBound: true, wantRestore: true, wantCleanup: true, wantReason: "MIHOMO_STATIC_ROLLBACK_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &proxyTrashRepoStub{item: &Proxy{ID: 9, Protocol: "http", Host: "127.0.0.1", Port: 32000, MihomoNodeID: "node"}, preflight: tc.preflight, restoreErr: tc.restoreErr}
			kernel := &proxyTrashKernelStub{bindings: []mihomo.StaticNodeBinding{{Name: "node", Port: tc.port, New: tc.newBinding}}, bindErr: tc.bindErr, cleanupErr: tc.cleanupErr}
			svc := &adminServiceImpl{proxyRepo: repo}
			err := svc.RestoreProxy(WithProxyTrashMihomoBinder(t.Context(), kernel), 9)
			if tc.wantReason == "" {
				require.NoError(t, err)
			} else {
				require.Equal(t, tc.wantReason, infraerrors.Reason(err))
			}
			require.Equal(t, tc.wantBound, kernel.bound)
			require.Equal(t, tc.wantRestore, repo.restored)
			require.Equal(t, tc.wantCleanup, kernel.deactivated)
		})
	}
}

func TestProxyTrashOrdinaryRestoreAndPermanentDeleteDoNotNeedKernel(t *testing.T) {
	repo := &proxyTrashRepoStub{item: &Proxy{ID: 9, Protocol: "socks5", Host: "exit.example", Port: 1080, Username: "user", Password: "secret", Status: "inactive"}}
	svc := &adminServiceImpl{proxyRepo: repo}
	require.NoError(t, svc.RestoreProxy(t.Context(), 9))
	require.True(t, repo.restored)
	repo.item.MihomoNodeID = "node"
	require.NoError(t, svc.PermanentlyDeleteProxy(t.Context(), 9))
	require.True(t, repo.permanentlyDeleted)
}

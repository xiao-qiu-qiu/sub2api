package service

import (
	"context"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

// AccountTrashItem deliberately exposes neither account credentials nor login ciphertext.
type AccountTrashItem struct {
	ID                  int64     `json:"id"`
	Name                string    `json:"name"`
	Platform            string    `json:"platform"`
	Type                string    `json:"type"`
	DeletedAt           time.Time `json:"deleted_at"`
	LoginEmail          string    `json:"login_email,omitempty"`
	HasLoginCredentials bool      `json:"has_login_credentials"`
}

// Optional contracts keep AdminService/AccountRepository mocks unchanged.
type AccountTrashRepository interface {
	ListAccountTrash(context.Context, pagination.PaginationParams, string) ([]AccountTrashItem, *pagination.PaginationResult, error)
	RestoreAccount(context.Context, int64) error
	PermanentlyDeleteAccount(context.Context, int64) error
}

type AccountTrashAdminService interface {
	ListAccountTrash(context.Context, int, int, string) ([]AccountTrashItem, int64, error)
	RestoreAccount(context.Context, int64) error
	PermanentlyDeleteAccount(context.Context, int64) error
}

var (
	ErrAccountTrashUnavailable       = infraerrors.New(501, "ACCOUNT_TRASH_UNAVAILABLE", "Account trash is unavailable")
	ErrAccountNotDeleted             = infraerrors.Conflict("ACCOUNT_NOT_DELETED", "Account must be deleted first")
	ErrAccountTrashParent            = infraerrors.Conflict("ACCOUNT_TRASH_PARENT_UNAVAILABLE", "Restore the parent account before restoring its shadow")
	ErrAccountTrashReferenced        = infraerrors.Conflict("ACCOUNT_TRASH_REFERENCED", "Account is still referenced; permanently delete its shadows first")
	ErrAccountTrashShadowExists      = infraerrors.Conflict("ACCOUNT_TRASH_SHADOW_EXISTS", "An active Spark shadow already exists for this parent")
	ErrAccountTrashProxyUnavailable  = infraerrors.Conflict("ACCOUNT_TRASH_PROXY_UNAVAILABLE", "Restore the original proxy before restoring this account")
	ErrAccountTrashTOTPConfigChanged = infraerrors.Conflict("ACCOUNT_TRASH_TOTP_CONFIG_CHANGED", "Unresolved 2FA recovery credentials do not match the archived login configuration; restore the original configuration before restoring this account")
)

var _ AccountTrashAdminService = (*adminServiceImpl)(nil)

func (s *adminServiceImpl) ListAccountTrash(ctx context.Context, page, pageSize int, search string) ([]AccountTrashItem, int64, error) {
	repo, ok := s.accountRepo.(AccountTrashRepository)
	if !ok {
		return nil, 0, ErrAccountTrashUnavailable
	}
	items, result, err := repo.ListAccountTrash(ctx, pagination.PaginationParams{Page: page, PageSize: pageSize}, search)
	if err != nil {
		return nil, 0, err
	}
	return items, result.Total, nil
}

func (s *adminServiceImpl) RestoreAccount(ctx context.Context, id int64) error {
	repo, ok := s.accountRepo.(AccountTrashRepository)
	if !ok {
		return ErrAccountTrashUnavailable
	}
	// Restoration only restores stored state. Do not enqueue login, test, or harvest work.
	if err := repo.RestoreAccount(ctx, id); err != nil {
		return err
	}
	AfterAccountCommit(ctx, func(context.Context) {
		if s.runtimeBlocker != nil {
			s.runtimeBlocker.ClearAccountSchedulingBlock(id)
		}
	})
	return nil
}

func (s *adminServiceImpl) PermanentlyDeleteAccount(ctx context.Context, id int64) error {
	repo, ok := s.accountRepo.(AccountTrashRepository)
	if !ok {
		return ErrAccountTrashUnavailable
	}
	return repo.PermanentlyDeleteAccount(ctx, id)
}

func (s *adminServiceImpl) deleteAccountWithTrash(ctx context.Context, id int64) error {
	deleteAll := func(ctx context.Context) error {
		shadows, err := s.accountRepo.ListShadowsByParent(ctx, id)
		if err != nil {
			return err
		}
		for _, shadow := range shadows {
			if err := s.accountRepo.Delete(ctx, shadow.ID); err != nil {
				return err
			}
		}
		return s.accountRepo.Delete(ctx, id)
	}
	if s.entClient == nil || dbent.TxFromContext(ctx) != nil {
		return deleteAll(ctx)
	}
	// The established shadow-first cascade now shares the parent's transaction,
	// so an archive/outbox failure cannot leave half the family deleted.
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Client().ExecContext(ctx, `SELECT id FROM accounts WHERE id = $1 FOR UPDATE`, id); err != nil {
		return err
	}
	if err := deleteAll(dbent.NewTxContext(ctx, tx)); err != nil {
		return err
	}
	return tx.Commit()
}

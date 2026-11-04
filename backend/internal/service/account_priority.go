package service

import infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"

const (
	MinAccountPriority = -9999
	MaxAccountPriority = 9999
)

// ValidateAccountPriority validates the signed account scheduling priority.
func ValidateAccountPriority(priority int) error {
	if priority < MinAccountPriority || priority > MaxAccountPriority {
		return infraerrors.BadRequest("ACCOUNT_PRIORITY_INVALID", "priority must be an integer between -9999 and 9999")
	}
	return nil
}

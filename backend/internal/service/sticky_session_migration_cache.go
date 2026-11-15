package service

import (
	"context"
	"time"
)

// StickySessionMigrationCache is an optional capability for moving a soft
// session binding after a successful account switch. Implementations must
// compare and update atomically; callers must not fall back to an unconditional
// SetSessionAccountID when this capability is absent or returns an error.
type StickySessionMigrationCache interface {
	// ReadSessionAccountBinding atomically reads the primary binding and its
	// revision, falling back to the legacy format only when the primary owner
	// is absent. Old bindings without a revision return an empty revision.
	// Missing bindings return ErrStickySessionNotFound.
	ReadSessionAccountBinding(ctx context.Context, groupID int64, sessionHash string, legacySessionHash string) (accountID int64, revision string, err error)

	// CompareAndSwapSessionAccountID replaces an existing expectedAccountID
	// binding only while its revision still equals expectedRevision. A new
	// revision on every write prevents stale requests from winning after an
	// A -> B -> A cycle or a delete/recreate. sessionHash and legacySessionHash
	// are already namespaced session hashes, such as "openai:hash", under the
	// supplied group.
	//
	// The primary binding takes precedence. Only when it is missing may a
	// nonempty legacy hash supply the expected owner. Missing bindings and a
	// different current owner return false without writing or extending TTLs.
	// A successful migration updates the primary TTL and, when supplied, the
	// legacy binding with legacyTTL so old-format readers cannot restore the
	// previous owner. Both account IDs and the applicable TTLs must be positive.
	// Clearing a session must remove both formats when legacy fallback is used.
	CompareAndSwapSessionAccountID(ctx context.Context, groupID int64, sessionHash string, legacySessionHash string, expectedAccountID int64, expectedRevision string, accountID int64, ttl time.Duration, legacyTTL time.Duration) (bool, error)
}

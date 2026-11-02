package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Move only membership: scheduling, concurrency and model isolation stay under
// their existing owners. All validation and writes share the caller's transaction.
func moveQualityGroups(ctx context.Context, tx *sql.Tx, accountID int64, targets []int64, state *qualityState) (bool, error) {
	if len(targets) == 0 {
		return true, nil
	}
	targets = slices.Clone(targets)
	slices.Sort(targets)
	selected := make(map[int64]bool, len(targets))
	for _, id := range targets {
		var locked int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM groups WHERE id=$1 AND deleted_at IS NULL FOR SHARE`, id).Scan(&locked)
		if err == sql.ErrNoRows {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		selected[id] = true
	}
	if err := lockQualityMoveMemberships(ctx, tx, accountID); err != nil {
		return false, err
	}
	raw, err := qualityGroups(ctx, tx, accountID)
	if err != nil {
		return false, err
	}
	var before []qualityGroup
	if err = json.Unmarshal(raw, &before); err != nil {
		return false, err
	}
	existing := make(map[int64]bool, len(before))
	var remove []int64
	for _, group := range before {
		existing[group.GroupID] = true
		if !selected[group.GroupID] {
			remove = append(remove, group.GroupID)
		}
	}
	if err = removeQualityGroups(ctx, tx, accountID, remove, state); err != nil {
		return false, err
	}
	for _, id := range targets {
		if existing[id] {
			continue
		}
		var group qualityGroup
		var allowedModels []byte
		err = tx.QueryRowContext(ctx, `INSERT INTO account_groups(account_id,group_id,priority,created_at,allowed_models)
 VALUES($1,$2,50,clock_timestamp(),NULL)
 RETURNING account_id,group_id,priority,created_at,allowed_models`, accountID, id).
			Scan(&group.AccountID, &group.GroupID, &group.Priority, &group.CreatedAt, &allowedModels)
		if err != nil {
			return false, err
		}
		group.AllowedModels = allowedModels
		state.Added = append(state.Added, group)
	}
	state.MoveGroupIDs = targets
	if len(state.Added) > 0 {
		_, err = tx.ExecContext(ctx, `UPDATE accounts SET updated_at=clock_timestamp() WHERE id=$1`, accountID)
	}
	return false, err
}

func restoreQualityMovedGroups(ctx context.Context, tx *sql.Tx, accountID int64, state *qualityState) (bool, error) {
	ids := make([]int64, 0, len(state.Added)+len(state.Removed))
	for _, group := range append(append([]qualityGroup(nil), state.Removed...), state.Added...) {
		ids = append(ids, group.GroupID)
	}
	slices.Sort(ids)
	for _, id := range ids {
		var locked int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM groups WHERE id=$1 FOR SHARE`, id).Scan(&locked); err != nil && err != sql.ErrNoRows {
			return false, err
		}
	}
	if err := lockQualityMoveMemberships(ctx, tx, accountID); err != nil {
		return false, err
	}
	// Only remove memberships we inserted and still own. A manual replacement,
	// priority change or model restriction must not be silently overwritten.
	for _, group := range state.Added {
		var exists, present, matches bool
		var allowed any
		if len(group.AllowedModels) > 0 && string(group.AllowedModels) != "null" {
			allowed = string(group.AllowedModels)
		}
		err := tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM groups WHERE id=$1 AND deleted_at IS NULL),
 EXISTS(SELECT 1 FROM account_groups WHERE account_id=$2 AND group_id=$1),
 EXISTS(SELECT 1 FROM account_groups WHERE account_id=$2 AND group_id=$1
 AND priority=$3 AND created_at=$4 AND allowed_models IS NOT DISTINCT FROM $5::jsonb)`,
			group.GroupID, accountID, group.Priority, group.CreatedAt, allowed).Scan(&exists, &present, &matches)
		if err != nil {
			return false, err
		}
		if !exists || !matches {
			state.RestoreDiagnostic = qualityRestoreConflict("target_membership_changed")
			state.RestoreDiagnostic.Groups = []service.QualityRestoreGroupEvidence{{GroupID: group.GroupID, GroupExists: exists, MembershipPresent: present}}
			return true, nil
		}
	}
	if conflict, err := restoreQualityGroups(ctx, tx, accountID, state); conflict || err != nil {
		return conflict, err
	}
	for _, group := range state.Added {
		if _, err := tx.ExecContext(ctx, `DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, accountID, group.GroupID); err != nil {
			return false, err
		}
	}
	return false, nil
}

func lockQualityMoveMemberships(ctx context.Context, tx *sql.Tx, accountID int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT group_id FROM account_groups WHERE account_id=$1 ORDER BY group_id FOR UPDATE`, accountID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return err
		}
	}
	return rows.Err()
}

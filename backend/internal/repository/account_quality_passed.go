package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// An independent recovery rule executes an explicit healthy action. It never
// borrows the main rule's removed-membership snapshot or creates quarantine ownership.
func applyPassedQualityOutcome(ctx context.Context, tx *sql.Tx, plan *service.ScheduledTestPlan, until time.Time, outcome, status string, schedulable bool, state *qualityState, commit *bool) (string, error) {
	if state.Action != "" {
		return "action_conflict", nil
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT quality_recovery_action`); err != nil {
		return "", err
	}
	now := time.Now()
	hold := advanceQualityRecoveryProbe(state, plan, until, outcome, now)
	if err := qualityUpsertState(ctx, tx, plan.ID, *state); err != nil {
		return "", err
	}
	if outcome != "passed" {
		*commit = true
		if outcome == "failed" {
			return "no_change", nil
		}
		return "inconclusive", nil
	}
	if hold {
		*commit = true
		return "recovery_confirmation_pending", nil
	}
	q := plan.PelicanConfig.Quality
	if q.Action != service.QualityActionObserveOnly {
		var unexpired bool
		if err := tx.QueryRowContext(ctx, `SELECT expires_at IS NULL OR expires_at>NOW() FROM accounts WHERE id=$1`, plan.AccountID).Scan(&unexpired); err != nil {
			return "", err
		}
		if status != service.StatusActive || !unexpired {
			return "restore_conflict", nil
		}
	}
	action := "observed"
	switch q.Action {
	case service.QualityActionMoveGroups:
		before, err := qualityGroups(ctx, tx, plan.AccountID)
		if err != nil {
			return "", err
		}
		mutation := qualityState{}
		conflict, err := moveQualityGroups(ctx, tx, plan.AccountID, q.MoveGroupIDs, &mutation)
		if err != nil {
			return "", err
		}
		if conflict {
			return "restore_conflict", nil
		}
		if len(mutation.Removed)+len(mutation.Added) > 0 {
			affected := append(mutation.Removed, mutation.Added...)
			if err = qualityGroupsChanged(ctx, tx, plan.AccountID, before, affected); err != nil {
				return "", err
			}
		}
		action = "groups_moved"
	case service.QualityActionEnableScheduling:
		if !schedulable {
			if _, err := tx.ExecContext(ctx, `UPDATE accounts SET schedulable=true,updated_at=clock_timestamp() WHERE id=$1`, plan.AccountID); err != nil {
				return "", err
			}
			before, err := qualityGroups(ctx, tx, plan.AccountID)
			if err != nil {
				return "", err
			}
			if err = qualityGroupsChanged(ctx, tx, plan.AccountID, before, nil); err != nil {
				return "", err
			}
		}
		action = "scheduling_enabled"
	case service.QualityActionObserveOnly:
	default:
		return "action_conflict", nil
	}
	state.RecoveryProbe = &qualityRecoveryProbeState{PolicyVersion: plan.UpdatedAt, NextProbeAt: now.Add(q.RecoveryProbeInterval())}
	if err := qualityUpsertState(ctx, tx, plan.ID, *state); err != nil {
		return "", err
	}
	*commit = true
	return action, nil
}

package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/robfig/cron/v3"
)

// Stored with the original rule's ownership, so recovery survives a restart
// and can restore the exact memberships the rule removed before quarantine.
type qualityRecoveryProbeState struct {
	PolicyVersion    time.Time  `json:"policy_version"`
	NextProbeAt      time.Time  `json:"next_probe_at"`
	FirstPassedAt    *time.Time `json:"first_passed_at,omitempty"`
	FirstPassedLease time.Time  `json:"first_passed_lease,omitempty"`
	PendingLease     time.Time  `json:"pending_lease,omitempty"`
}

func qualityRecoveryProbeOwns(state *qualityState) bool {
	return state.Action == service.QualityActionMoveGroups || state.Action == "remove_groups" || state.Action == "disable_scheduling"
}

// The caller already holds the plan and account locks and has checked the lease.
// Only a new round started after the confirmation delay can release ownership.
func advanceQualityRecoveryProbe(state *qualityState, plan *service.ScheduledTestPlan, until time.Time, outcome string, now time.Time) bool {
	q := plan.PelicanConfig.Quality
	probe := state.RecoveryProbe
	if probe == nil || !probe.PolicyVersion.Equal(plan.UpdatedAt) {
		probe = &qualityRecoveryProbeState{PolicyVersion: plan.UpdatedAt}
		state.RecoveryProbe = probe
	}
	probe.PendingLease = time.Time{}
	if outcome != "passed" {
		probe.FirstPassedAt = nil
		probe.FirstPassedLease = time.Time{}
		probe.NextProbeAt = now.Add(q.RecoveryProbeInterval())
		return false
	}
	if probe.FirstPassedAt == nil {
		probe.FirstPassedAt = &now
		probe.FirstPassedLease = until
		probe.NextProbeAt = now.Add(q.RecoveryConfirmationInterval())
		return true
	}
	started := plan.QualityRecoveryProbeStartedAt
	waitUntil := probe.FirstPassedAt.Add(q.RecoveryConfirmationInterval())
	if until.Equal(probe.FirstPassedLease) || started == nil || started.Before(waitUntil) {
		probe.NextProbeAt = waitUntil
		return true
	}
	return false
}

// An earlier lease still marked pending never committed a complete outcome.
// Treat that missing result as unknown before starting the next probe.
func claimQualityRecoveryProbe(state *qualityState, plan *service.ScheduledTestPlan, now, until time.Time) time.Time {
	q := plan.PelicanConfig.Quality
	probe := state.RecoveryProbe
	if probe == nil || !probe.PolicyVersion.Equal(plan.UpdatedAt) {
		probe = &qualityRecoveryProbeState{PolicyVersion: plan.UpdatedAt}
		state.RecoveryProbe = probe
	}
	if !probe.PendingLease.IsZero() || plan.TriggerSource == "upstream_5xx" {
		probe.FirstPassedAt = nil
		probe.FirstPassedLease = time.Time{}
	}
	next := now.Add(q.RecoveryProbeInterval())
	if probe.FirstPassedAt != nil {
		next = now.Add(q.RecoveryConfirmationInterval())
	}
	probe.PendingLease, probe.NextProbeAt = until, next
	return next
}

func retryConflictedQualityRecovery(ctx context.Context, tx *sql.Tx, plan *service.ScheduledTestPlan, now time.Time, commit *bool) error {
	state, err := readQualityState(ctx, tx, plan.ID)
	if err != nil || (!plan.PelicanConfig.Quality.RunsOnPassed() && !qualityRecoveryProbeOwns(&state)) {
		return err
	}
	if state.RecoveryProbe == nil || !state.RecoveryProbe.PolicyVersion.Equal(plan.UpdatedAt) {
		state.RecoveryProbe = &qualityRecoveryProbeState{PolicyVersion: plan.UpdatedAt}
	}
	state.RecoveryProbe.PendingLease = time.Time{}
	state.RecoveryProbe.NextProbeAt = now.Add(plan.PelicanConfig.Quality.RecoveryProbeInterval())
	if err = qualityUpsertState(ctx, tx, plan.ID, state); err != nil {
		return err
	}
	*commit = true
	return nil
}

func scheduleQualityRecoveryProbe(ctx context.Context, tx *sql.Tx, plan *service.ScheduledTestPlan, now time.Time) error {
	state, err := readQualityState(ctx, tx, plan.ID)
	if err != nil {
		return err
	}
	q := plan.PelicanConfig.Quality
	var next time.Time
	if q.RunsOnPassed() || qualityRecoveryProbeOwns(&state) {
		if state.RecoveryProbe == nil || !state.RecoveryProbe.PolicyVersion.Equal(plan.UpdatedAt) {
			state.RecoveryProbe = &qualityRecoveryProbeState{PolicyVersion: plan.UpdatedAt, NextProbeAt: now.Add(q.RecoveryProbeInterval())}
		}
		next = state.RecoveryProbe.NextProbeAt
		if err = qualityUpsertState(ctx, tx, plan.ID, state); err != nil {
			return err
		}
	} else {
		// Once restored, return to the original normal-pool schedule.
		schedule, parseErr := cron.ParseStandard(plan.CronExpression)
		if parseErr != nil {
			return parseErr
		}
		next = schedule.Next(now)
	}
	_, err = tx.ExecContext(ctx, `UPDATE scheduled_test_plans SET next_run_at=$2 WHERE id=$1`, plan.ID, next)
	return err
}

// Saving or pausing a rule invalidates its earlier confirmation, while retaining
// the original ownership snapshot. Old in-flight runs fail the version guard.
func resetQualityRecoveryProbe(ctx context.Context, tx *sql.Tx, plan *service.ScheduledTestPlan, state *qualityState, now time.Time) error {
	if plan.PelicanConfig != nil && plan.PelicanConfig.Quality.RecoveryProbeActive() && (plan.PelicanConfig.Quality.RunsOnPassed() || qualityRecoveryProbeOwns(state)) {
		state.RecoveryProbe = &qualityRecoveryProbeState{PolicyVersion: plan.UpdatedAt, NextProbeAt: now.Add(plan.PelicanConfig.Quality.RecoveryProbeInterval())}
		plan.NextRunAt = &state.RecoveryProbe.NextProbeAt
		if err := qualityUpsertState(ctx, tx, plan.ID, *state); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE scheduled_test_plans SET next_run_at=$2 WHERE id=$1`, plan.ID, plan.NextRunAt)
		return err
	}
	if state.RecoveryProbe != nil {
		state.RecoveryProbe = nil
		if qualityRecoveryProbeOwns(state) {
			// Legacy account actions do not claim an episode. Turning this feature
			// off must not leave their original recovery path permanently stale.
			state.Quality5xxEpisode = 0
		}
		return qualityUpsertState(ctx, tx, plan.ID, *state)
	}
	return nil
}

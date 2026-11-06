//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func enableRecoveryProbe(t *testing.T, f *qualityBPSFixture) {
	t.Helper()
	f.plan.PelicanConfig.Quality.RecoveryProbeEnabled = true
	updated, err := f.plans.Update(context.Background(), f.plan)
	require.NoError(t, err)
	f.plan = updated
}

// Start a fresh scheduled round without modifying the policy version, and age
// the first pass instead of waiting five real minutes in an integration test.
func claimRecoveryProbe(t *testing.T, f *qualityMoveGroupsFixture, confirmationDue bool) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.plans.FinishPelican(ctx, f.plan.ID, f.until, time.Now()))
	if confirmationDue {
		state := f.state()
		first := time.Now().Add(-6 * time.Minute)
		state.RecoveryProbe.FirstPassedAt = &first
		state.RecoveryProbe.NextProbeAt = first.Add(5 * time.Minute)
		raw, err := json.Marshal(state)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, `UPDATE account_quality_states SET state=$2 WHERE plan_id=$1`, f.plan.ID, raw)
		require.NoError(t, err)
	}
	_, err := integrationDB.ExecContext(ctx, `UPDATE scheduled_test_plans SET next_run_at=clock_timestamp()-interval '1 second' WHERE id=$1`, f.plan.ID)
	require.NoError(t, err)
	plan, err := f.plans.GetByID(ctx, f.plan.ID)
	require.NoError(t, err)
	now := time.Now().Add(time.Second).Truncate(time.Microsecond)
	if !now.Add(15 * time.Minute).After(f.until) {
		now = f.until.Add(-15*time.Minute + time.Second)
	}
	f.until = now.Add(15 * time.Minute)
	claimed, err := f.plans.ClaimPelican(ctx, plan, now, f.until, now.Add(30*time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	f.plan = plan
}

func TestQualityRecoveryProbeMoveGroupsTwoPassesAndRestart(t *testing.T) {
	f := newQualityMoveGroupsFixture(t, true)
	enableRecoveryProbe(t, f.qualityBPSFixture)
	original := f.memberships()
	before := time.Now()
	require.Equal(t, "groups_moved", f.apply("failed"))
	state := f.state()
	require.WithinDuration(t, before.Add(time.Hour), state.RecoveryProbe.NextProbeAt, 3*time.Second)
	require.Nil(t, state.RecoveryProbe.FirstPassedAt)
	quarantined := f.memberships()
	claimRecoveryProbe(t, f, false)
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
	require.Equal(t, quarantined, f.memberships())
	state = f.state()
	require.NotNil(t, state.RecoveryProbe.FirstPassedAt)
	require.WithinDuration(t, state.RecoveryProbe.FirstPassedAt.Add(5*time.Minute), state.RecoveryProbe.NextProbeAt, time.Microsecond)
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"), "same lease cannot count twice")
	// Rebuild the repository to prove no process-local confirmation state is used.
	f.plans = NewScheduledTestPlanRepository(integrationDB)
	claimRecoveryProbe(t, f, true)
	require.Equal(t, "restored", f.apply("passed"))
	require.Equal(t, original, f.memberships())
	require.Zero(t, f.count(fmt.Sprintf(`SELECT count(*) FROM account_quality_states WHERE plan_id=%d AND $1::bigint>0`, f.plan.ID)))
	plan, err := f.plans.GetByID(context.Background(), f.plan.ID)
	require.NoError(t, err)
	require.NotNil(t, plan.NextRunAt)
	require.LessOrEqual(t, time.Until(*plan.NextRunAt), 30*time.Minute, "restored accounts resume the normal cron")
}

func TestQualityRecoveryProbeFailureAndUnknownBreakConfirmation(t *testing.T) {
	for _, outcome := range []string{"failed", "inconclusive", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			f := newQualityMoveGroupsFixture(t, true)
			enableRecoveryProbe(t, f.qualityBPSFixture)
			require.Equal(t, "groups_moved", f.apply("failed"))
			claimRecoveryProbe(t, f, false)
			require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
			claimRecoveryProbe(t, f, true)
			f.apply(outcome)
			state := f.state()
			require.Nil(t, state.RecoveryProbe.FirstPassedAt)
			require.WithinDuration(t, time.Now().Add(time.Hour), state.RecoveryProbe.NextProbeAt, 3*time.Second)
			claimRecoveryProbe(t, f, false)
			require.Equal(t, "recovery_confirmation_pending", f.apply("passed"), "one fresh pass cannot restore after an interrupted confirmation")
		})
	}
}

func TestQualityRecoveryProbeEditPauseAndExpiredLeaseFence(t *testing.T) {
	for _, mutation := range []string{"edit", "pause", "expired_lease", "replaced_lease"} {
		t.Run(mutation, func(t *testing.T) {
			f := newQualityMoveGroupsFixture(t, true)
			enableRecoveryProbe(t, f.qualityBPSFixture)
			require.Equal(t, "groups_moved", f.apply("failed"))
			claimRecoveryProbe(t, f, false)
			require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
			claimRecoveryProbe(t, f, true)
			switch mutation {
			case "edit", "pause":
				plan, err := f.plans.GetByID(context.Background(), f.plan.ID)
				require.NoError(t, err)
				plan.Enabled = mutation != "pause"
				plan.PelicanConfig.Quality.RecoveryConfirmationMinutes = 7
				_, err = f.plans.Update(context.Background(), plan)
				require.NoError(t, err)
				require.Nil(t, f.state().RecoveryProbe.FirstPassedAt)
			case "expired_lease":
				_, err := integrationDB.ExecContext(context.Background(), `UPDATE scheduled_test_plans SET running_until=clock_timestamp()-interval '1 second' WHERE id=$1`, f.plan.ID)
				require.NoError(t, err)
			case "replaced_lease":
				_, err := integrationDB.ExecContext(context.Background(), `UPDATE scheduled_test_plans SET running_until=running_until+interval '1 second' WHERE id=$1`, f.plan.ID)
				require.NoError(t, err)
			}
			before := f.snapshot()
			require.Equal(t, "stale_run", f.apply("passed"))
			require.JSONEq(t, before, f.snapshot())
		})
	}
}

func TestQualityRecoveryProbeDisabledPreservesLegacyRestore(t *testing.T) {
	t.Run("probe disabled", func(t *testing.T) {
		f := newQualityMoveGroupsFixture(t, true)
		require.Equal(t, "groups_moved", f.apply("failed"))
		require.Nil(t, f.state().RecoveryProbe)
		require.Equal(t, "restored", f.apply("passed"))
	})
	t.Run("auto restore disabled", func(t *testing.T) {
		f := newQualityMoveGroupsFixture(t, false)
		enableRecoveryProbe(t, f.qualityBPSFixture)
		require.Equal(t, "groups_moved", f.apply("failed"))
		f.apply("passed")
		require.Nil(t, f.state().RecoveryProbe)
		require.Equal(t, service.QualityActionMoveGroups, f.state().Action)
	})
}

func TestQualityRecoveryProbeOtherAccountActionsAndExistingOwnership(t *testing.T) {
	for _, tc := range []struct{ action, applied string }{{"remove_groups", "groups_removed"}, {"disable_scheduling", "scheduling_disabled"}} {
		t.Run(tc.action, func(t *testing.T) {
			f := newQualityMoveGroupsFixture(t, true)
			original := f.memberships()
			q := f.plan.PelicanConfig.Quality
			q.Action, q.MoveGroupIDs, q.RemoveGroupIDs = tc.action, nil, []int64{f.source}
			plan, err := f.plans.Update(context.Background(), f.plan)
			require.NoError(t, err)
			f.plan = plan
			require.Equal(t, tc.applied, f.apply("failed"))
			// An existing quarantine starts recovery under its original rule.
			enableRecoveryProbe(t, f.qualityBPSFixture)
			require.NotNil(t, f.state().RecoveryProbe)
			require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
			claimRecoveryProbe(t, f, true)
			require.Equal(t, "restored", f.apply("passed"))
			require.Equal(t, original, f.memberships())
			var schedulable bool
			require.NoError(t, integrationDB.QueryRowContext(context.Background(), `SELECT schedulable FROM accounts WHERE id=$1`, f.account).Scan(&schedulable))
			require.True(t, schedulable)
		})
	}
}

func TestQualityRecoveryProbeClaimKeepsDurableRetryOnCrash(t *testing.T) {
	f := newQualityMoveGroupsFixture(t, true)
	enableRecoveryProbe(t, f.qualityBPSFixture)
	require.Equal(t, "groups_moved", f.apply("failed"))
	claimRecoveryProbe(t, f, false)
	stored, err := f.plans.GetByID(context.Background(), f.plan.ID)
	require.NoError(t, err)
	require.WithinDuration(t, f.plan.QualityRecoveryProbeStartedAt.Add(time.Hour), *stored.NextRunAt, time.Microsecond)
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
	claimRecoveryProbe(t, f, true)
	stored, err = f.plans.GetByID(context.Background(), f.plan.ID)
	require.NoError(t, err)
	require.WithinDuration(t, f.plan.QualityRecoveryProbeStartedAt.Add(5*time.Minute), *stored.NextRunAt, time.Microsecond)
	// Simulate process death after claim; a fresh repository sees the due retry.
	_, err = integrationDB.ExecContext(context.Background(), `UPDATE scheduled_test_plans SET running_until=clock_timestamp()-interval '1 second' WHERE id=$1`, f.plan.ID)
	require.NoError(t, err)
	f.plans = NewScheduledTestPlanRepository(integrationDB)
	due, err := f.plans.ListDue(context.Background(), stored.NextRunAt.Add(time.Second))
	require.NoError(t, err)
	found := false
	for _, plan := range due {
		found = found || plan.ID == f.plan.ID
	}
	require.True(t, found)
	require.NotNil(t, f.state().RecoveryProbe.FirstPassedAt)
}

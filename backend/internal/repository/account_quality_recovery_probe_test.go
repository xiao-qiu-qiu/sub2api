package repository

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func recoveryProbePlan(now time.Time) *service.ScheduledTestPlan {
	return &service.ScheduledTestPlan{UpdatedAt: now.Add(-time.Hour), PelicanConfig: &service.PelicanTestConfig{Quality: &service.QualityPolicy{
		Action: service.QualityActionMoveGroups, AutoRestore: true, RecoveryProbeEnabled: true,
	}}}
}

func TestQualityRecoveryProbeRequiresNewDelayedRoundAndSurvivesJSON(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	plan := recoveryProbePlan(now)
	state := qualityState{Action: service.QualityActionMoveGroups}
	lease := now.Add(15 * time.Minute)
	require.True(t, advanceQualityRecoveryProbe(&state, plan, lease, "passed", now))
	require.Equal(t, now.Add(5*time.Minute), state.RecoveryProbe.NextProbeAt)
	// Replaying a completed round, even much later, is never a second pass.
	started := now.Add(6 * time.Minute)
	plan.QualityRecoveryProbeStartedAt = &started
	require.True(t, advanceQualityRecoveryProbe(&state, plan, lease, "passed", started))
	early := now.Add(4 * time.Minute)
	plan.QualityRecoveryProbeStartedAt = &early
	require.True(t, advanceQualityRecoveryProbe(&state, plan, lease.Add(time.Minute), "passed", started), "finishing later cannot validate an early probe")
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	var restarted qualityState
	require.NoError(t, json.Unmarshal(raw, &restarted))
	plan.QualityRecoveryProbeStartedAt = &started
	require.False(t, advanceQualityRecoveryProbe(&restarted, plan, lease.Add(time.Minute), "passed", started))
}

func TestQualityRecoveryProbeFailureUnknownAndEditResetConfirmation(t *testing.T) {
	now := time.Now().UTC()
	for _, outcome := range []string{"failed", "inconclusive", "unknown", "skipped"} {
		t.Run(outcome, func(t *testing.T) {
			plan := recoveryProbePlan(now)
			state := qualityState{Action: plan.PelicanConfig.Quality.Action}
			lease := now.Add(15 * time.Minute)
			require.True(t, advanceQualityRecoveryProbe(&state, plan, lease, "passed", now))
			require.False(t, advanceQualityRecoveryProbe(&state, plan, lease.Add(time.Minute), outcome, now.Add(time.Minute)))
			require.Nil(t, state.RecoveryProbe.FirstPassedAt)
			require.Equal(t, now.Add(61*time.Minute), state.RecoveryProbe.NextProbeAt)
			require.True(t, advanceQualityRecoveryProbe(&state, plan, lease.Add(2*time.Minute), "passed", now.Add(2*time.Minute)))
			plan.UpdatedAt = plan.UpdatedAt.Add(time.Second)
			require.True(t, advanceQualityRecoveryProbe(&state, plan, lease.Add(8*time.Minute), "passed", now.Add(8*time.Minute)), "edited policies require two new passes")
		})
	}
}

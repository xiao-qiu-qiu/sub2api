package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQualityRecoveryProbeDefaultsAndValidation(t *testing.T) {
	for _, action := range []string{QualityActionMoveGroups, "remove_groups", "disable_scheduling"} {
		q := &QualityPolicy{Action: action, AutoRestore: true, RecoveryProbeEnabled: true}
		require.NoError(t, q.validateRecoveryProbe())
		require.True(t, q.RecoveryProbeActive())
		require.Equal(t, 60, q.RecoveryProbeIntervalMinutes)
		require.Equal(t, 5, q.RecoveryConfirmationMinutes)
		require.Equal(t, time.Hour, q.RecoveryProbeInterval())
		require.Equal(t, 5*time.Minute, q.RecoveryConfirmationInterval())
		q.AutoRestore = false
		require.False(t, q.RecoveryProbeActive())
	}
	for _, action := range []string{QualityActionRemoveModel, QualityActionIsolateModels, QualityActionEnableBPS, QualityActionObserveOnly, QualityActionOpenAITwoFAAdmission} {
		q := &QualityPolicy{Action: action, AutoRestore: true, RecoveryProbeEnabled: true}
		require.Error(t, q.validateRecoveryProbe())
		require.False(t, q.RecoveryProbeActive())
	}
	for _, value := range []int{-1, 10081} {
		require.Error(t, (&QualityPolicy{RecoveryProbeIntervalMinutes: value}).validateRecoveryProbe())
		require.Error(t, (&QualityPolicy{RecoveryConfirmationMinutes: value}).validateRecoveryProbe())
	}
	for _, value := range []int{1, 10080} {
		require.NoError(t, (&QualityPolicy{RecoveryProbeIntervalMinutes: value, RecoveryConfirmationMinutes: value}).validateRecoveryProbe())
	}
	q := &QualityPolicy{Action: "disable_scheduling", AutoRestore: true}
	require.NoError(t, q.validateRecoveryProbe())
	require.False(t, q.RecoveryProbeActive(), "legacy rules keep their original behavior")
}

func TestQualityRecoveryProbeReusableConfig(t *testing.T) {
	cfg, err := reusableQualityConfig(&PelicanTestConfig{Quality: &QualityPolicy{
		Action: "disable_scheduling", AutoRestore: true, RecoveryProbeEnabled: true,
		RecoveryProbeIntervalMinutes: 120, RecoveryConfirmationMinutes: 7,
	}})
	require.NoError(t, err)
	require.True(t, cfg.Quality.RecoveryProbeActive())
	require.Equal(t, 120, cfg.Quality.RecoveryProbeIntervalMinutes)
	require.Equal(t, 7, cfg.Quality.RecoveryConfirmationMinutes)
}

func TestQualityRecoveryProbeAllSkippedRoundBreaksConfirmation(t *testing.T) {
	plans, saved := &qualityPlanRepo{}, &pelicanResults{}
	svc := NewScheduledTestService(plans, saved)
	svc.qualityModels = func(context.Context, int64) ([]string, error) { return nil, nil }
	runner := &ScheduledTestRunnerService{planRepo: plans, scheduledSvc: svc}
	plan := pelicanPlan()
	plan.PelicanConfig.QuestionKind = "candy"
	plan.PelicanConfig.Quality = &QualityPolicy{ExpectedAnswer: "21", Action: "disable_scheduling", AutoRestore: true, RecoveryProbeEnabled: true}
	require.True(t, runner.runOnePlan(context.Background(), plan))
	require.Equal(t, []string{"inconclusive"}, plans.outcomes, "all-skipped rounds must still reset a pending confirmation")
	for _, result := range saved.results {
		require.Equal(t, "skipped", result.Status)
	}
}

func TestQualityRecoveryProbeSharedBatchSkippedSampleCannotConfirm(t *testing.T) {
	batch, err := newQualitySharedBatch(time.Now())
	require.NoError(t, err)
	run := sharedBatchFixture(t, batch, 1, "correct")
	run.Plan.PelicanConfig.Quality = &QualityPolicy{Action: "disable_scheduling", AutoRestore: true, RecoveryProbeEnabled: true}
	run.Plan.PelicanConfig.ParallelCount = 2
	second := *run.Results[0]
	second.Status, second.QualityJudgment = "skipped", nil
	run.Results = append(run.Results, &second)
	require.Equal(t, "inconclusive", EvaluateQualityBatch(run, nil))
}

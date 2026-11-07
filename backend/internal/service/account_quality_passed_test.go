package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQualityPassedPolicyHasIndependentScopeAndInterval(t *testing.T) {
	for _, action := range []string{QualityActionMoveGroups, QualityActionEnableScheduling, QualityActionObserveOnly} {
		t.Run(action, func(t *testing.T) {
			plan := &ScheduledTestPlan{ModelID: "gpt-test", CronExpression: "* * * * *", MaxResults: 100, PelicanConfig: &PelicanTestConfig{QuestionKind: OpenAICodexStateProbeQuestionKind, ReasoningEffort: "medium", ParallelCount: 1, Quality: &QualityPolicy{}}}
			q := plan.PelicanConfig.Quality
			q.ActionOn, q.Action, q.AutoRestore = "passed", action, true
			q.MoveGroupIDs = []int64{21}
			q.TriggerOnUpstream5xx = true
			now := time.Now()
			next, err := nextPlanRun(plan, now)
			require.NoError(t, err)
			require.Equal(t, now.Add(time.Hour), next, "initial recovery run ignores cron")
			require.False(t, q.AutoRestore)
			require.False(t, q.TriggerOnUpstream5xx)
			require.True(t, q.RecoveryProbeActive())
			require.Equal(t, "recovery", qualityRuleScope(plan.PelicanConfig))
			q.RecoveryProbeIntervalMinutes = 17
			next, err = nextPlanRun(plan, now)
			require.NoError(t, err)
			require.Equal(t, now.Add(17*time.Minute), next)
			cfg, err := reusableQualityConfig(plan.PelicanConfig)
			require.NoError(t, err)
			require.Equal(t, "passed", cfg.Quality.ActionOn)
			require.Equal(t, 17, cfg.Quality.RecoveryProbeIntervalMinutes)
		})
	}
}

func TestQualityPassedPolicyRejectsFailureActions(t *testing.T) {
	for _, action := range []string{"disable_scheduling", "remove_groups", QualityActionRemoveModel, QualityActionIsolateModels, QualityActionEnableBPS} {
		require.Error(t, (&QualityPolicy{ActionOn: "passed", Action: action}).validateRecoveryProbe())
	}
	require.Error(t, (&QualityPolicy{ActionOn: "unknown", Action: QualityActionObserveOnly}).validateRecoveryProbe())
	for _, actionOn := range []string{"", "failed"} {
		q := &QualityPolicy{ActionOn: actionOn, Action: QualityActionMoveGroups}
		require.NoError(t, q.validateRecoveryProbe())
		require.False(t, q.RecoveryProbeActive())
		require.Equal(t, "quarantine", qualityRuleScope(&PelicanTestConfig{Quality: q}))
	}
}

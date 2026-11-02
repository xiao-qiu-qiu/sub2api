package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQualityMoveGroupsValidatesTargetIDs(t *testing.T) {
	groupIDs := make([]int64, 101)
	for i := range groupIDs {
		groupIDs[i] = int64(i + 1)
	}
	for _, tc := range []struct {
		name string
		ids  []int64
		ok   bool
	}{
		{name: "one", ids: []int64{7}, ok: true},
		{name: "one_hundred", ids: groupIDs[:100], ok: true},
		{name: "missing"},
		{name: "empty", ids: []int64{}},
		{name: "too_many", ids: groupIDs},
		{name: "duplicate", ids: []int64{7, 9, 7}},
		{name: "zero", ids: []int64{7, 0}},
		{name: "negative", ids: []int64{-1, 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := multiModelQualityPlan(t, `["test-model"]`)
			q := plan.PelicanConfig.Quality
			q.Action, q.MoveGroupIDs = QualityActionMoveGroups, tc.ids
			_, err := nextPlanRun(plan, time.Now())
			if tc.ok {
				require.NoError(t, err)
				require.Equal(t, tc.ids, q.MoveGroupIDs)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestQualityMoveGroupsSupportsAnswersAndStateProbe(t *testing.T) {
	for _, tc := range []struct{ name, question, models string }{
		{"one_answer_model", "candy", `["test-model"]`},
		{"multiple_answer_models", "candy", `["first-model","second-model"]`},
		{"random_question", QualityRandomQuestionKind, `["test-model"]`},
		{"state_probe", OpenAICodexStateProbeQuestionKind, `["test-model"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := multiModelQualityPlan(t, tc.models)
			plan.PelicanConfig.QuestionKind = tc.question
			q := plan.PelicanConfig.Quality
			q.Action, q.MoveGroupIDs, q.AutoRestore = QualityActionMoveGroups, []int64{9, 7}, true
			// Leftover options from other actions must not invalidate a move.
			q.RemoveGroupIDs, q.RemoveModels = []int64{0, 0}, []string{"*"}
			q.BPS = &QualityBPSPolicy{FailureThreshold: -1}
			if tc.question != "candy" {
				q.ExpectedAnswer = ""
			}
			_, err := nextPlanRun(plan, time.Now())
			require.NoError(t, err)
			require.Equal(t, []int64{9, 7}, q.MoveGroupIDs)
			require.Nil(t, q.RemoveGroupIDs)
			require.Nil(t, q.RemoveModels)
			require.Nil(t, q.BPS)
			require.True(t, q.AutoRestore)
			require.False(t, q.UsesModelResults(), "moving groups uses the account verdict")
		})
	}
}

func TestQualityOtherActionsDiscardMoveGroups(t *testing.T) {
	for _, action := range []string{
		"remove_groups", "disable_scheduling", QualityActionObserveOnly, QualityActionRemoveModel,
		QualityActionIsolateModels, QualityActionEnableBPS, QualityActionOpenAITwoFAAdmission,
	} {
		t.Run(action, func(t *testing.T) {
			plan := multiModelQualityPlan(t, `["test-model"]`)
			q := plan.PelicanConfig.Quality
			q.Action, q.MoveGroupIDs = action, []int64{0, 0}
			q.RemoveGroupIDs, q.RemoveModels = []int64{7}, []string{"test-model"}
			if action == QualityActionEnableBPS {
				plan.PelicanConfig.QuestionKind = OpenAICodexStateProbeQuestionKind
				q.BPS = &QualityBPSPolicy{FailureThreshold: 2, AllModels: true, PassThreshold: 3}
			}
			if action == QualityActionOpenAITwoFAAdmission {
				admission := DefaultOpenAITwoFAAdmissionConfig()
				admission.Enabled, admission.IsolationGroupID = true, 7
				q.Admission = &admission
				plan.ModelID = admission.ModelID
				plan.PelicanConfig.ModelIDs = nil
				plan.PelicanConfig.ReasoningEffort = admission.ReasoningEffort
				plan.PelicanConfig.Prompt = CandyPrompt
			}
			_, err := nextPlanRun(plan, time.Now())
			require.NoError(t, err)
			require.Nil(t, q.MoveGroupIDs)
		})
	}
}

func TestQualityMoveGroupsTemplateAndOAuthReuseCopiesTargets(t *testing.T) {
	input := presetInput()
	input.PelicanConfig.Quality.Action = QualityActionMoveGroups
	input.PelicanConfig.Quality.MoveGroupIDs = []int64{7, 9}
	template := &QualityRuleTemplate{ModelID: input.ModelID, CronExpression: input.CronExpression,
		MaxResults: input.MaxResults, PelicanConfig: input.PelicanConfig}
	require.NoError(t, prepareQualityTemplate(template))
	first, second := template.planFor(1), template.planFor(2)
	require.Equal(t, []int64{7, 9}, first.PelicanConfig.Quality.MoveGroupIDs)
	first.PelicanConfig.Quality.MoveGroupIDs[0] = 99
	require.Equal(t, []int64{7, 9}, second.PelicanConfig.Quality.MoveGroupIDs)
	require.Equal(t, []int64{7, 9}, template.PelicanConfig.Quality.MoveGroupIDs)

	rule := &OAuthInitialQualityRule{ModelID: input.ModelID, CronExpression: input.CronExpression,
		MaxResults: input.MaxResults, PelicanConfig: input.PelicanConfig}
	plan, err := rule.plan(PlatformOpenAI)
	require.NoError(t, err)
	require.Equal(t, []int64{7, 9}, plan.PelicanConfig.Quality.MoveGroupIDs)
	plan.PelicanConfig.Quality.MoveGroupIDs[0] = 88
	require.Equal(t, []int64{7, 9}, input.PelicanConfig.Quality.MoveGroupIDs)
}

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQualityBPSActionsValidationAndLegacyObservation(t *testing.T) {
	for _, action := range []string{QualityActionObserveOnly, "disable_scheduling", "remove_groups", QualityActionMoveGroups, QualityActionIsolateModels, QualityActionRemoveModel, QualityActionEnableScheduling} {
		t.Run(action, func(t *testing.T) {
			plan := pelicanPlan()
			plan.PelicanConfig = bpsObservationConfig()
			q := plan.PelicanConfig.Quality
			q.Action, q.RemoveGroupIDs, q.MoveGroupIDs, q.RemoveModels = action, []int64{3}, []int64{4}, []string{plan.ModelID}
			if action == QualityActionEnableScheduling {
				q.ActionOn = "passed"
			}
			_, err := nextPlanRun(plan, time.Now())
			require.NoError(t, err)
			require.Equal(t, action, q.Action, "changing channel cannot silently upgrade observation or replace a chosen action")
			if action == QualityActionObserveOnly {
				require.False(t, QualityProbeRequiresBPS(plan.PelicanConfig))
			} else {
				require.True(t, QualityProbeRequiresBPS(plan.PelicanConfig))
			}
		})
	}
}

func TestQualityBPSTemplateActionsOnlyMatchEffectiveBPS(t *testing.T) {
	for _, observe := range []bool{false, true} {
		plans := newTemplatePlanRepo()
		repo := newFakeTemplateRepo(plans,
			QualityTemplateAccount{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			QualityTemplateAccount{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_excel_bps": true}},
			QualityTemplateAccount{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_excel_bps": true, ExcelBPSAuthorizationPendingKey: true}},
			QualityTemplateAccount{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"plan_type": "free"}, Extra: map[string]any{"openai_excel_bps": true}})
		svc := NewScheduledTestService(plans, &pelicanResults{})
		svc.templateRepo = repo
		tpl := probeTemplate()
		tpl.PelicanConfig = bpsObservationConfig()
		if !observe {
			tpl.PelicanConfig.Quality.Action = "disable_scheduling"
		}
		_, count, err := svc.CreateQualityTemplate(context.Background(), tpl)
		require.NoError(t, err)
		require.Equal(t, 1, count, "observation templates share the effective BPS account scope")
	}
}

func TestQualityBPSActionSkipsNativeAndChangedModelBeforeDispatch(t *testing.T) {
	for _, mode := range []string{"disabled", "pending", "model_removed"} {
		t.Run(mode, func(t *testing.T) {
			account := excelAccount()
			switch mode {
			case "disabled":
				account.Extra["openai_excel_bps"] = false
			case "pending":
				account.Extra[ExcelBPSAuthorizationPendingKey] = true
			case "model_removed":
				account.Extra["openai_excel_bps_models"] = []any{"different"}
			}
			svc := &AccountTestService{accountRepo: &stateProbeAccountRepo{account: account}}
			cfg := bpsObservationConfig()
			cfg.ModelID = "gpt-6-astra"
			cfg.Quality.Action = "disable_scheduling"
			result, err := svc.RunPelicanBackground(context.Background(), account.ID, cfg.ModelID, cfg)
			require.NoError(t, err)
			require.Equal(t, "skipped", result.Status)
			require.Empty(t, result.PelicanConfig.TestRoute)
			plans, saved := &qualityTriggerPlans{}, &pelicanResults{}
			runner := &ScheduledTestRunnerService{planRepo: plans, scheduledSvc: NewScheduledTestService(plans, saved), accountTestSvc: svc}
			runner.runPelican = func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
				t.Fatal("ineligible BPS action was dispatched")
				return nil, nil
			}
			plan := pelicanPlan()
			plan.PelicanConfig = cfg
			require.True(t, runner.runOnePlan(context.Background(), plan))
			require.Empty(t, saved.results)
			require.Zero(t, plans.outcomes)
		})
	}
}

func TestQualityBPS5xxKeepsAnswerQuestionAndAction(t *testing.T) {
	cfg := *bpsObservationConfig()
	cfg.Quality.Action = QualityActionMoveGroups
	cfg.Quality.MoveGroupIDs = []int64{3}
	converted, apply := quality5xxTestConfig(stateProbeAccount(), "gpt-6-astra", cfg)
	require.True(t, apply)
	require.Equal(t, "candy", converted.QuestionKind)
	require.Equal(t, "bps", converted.TestChannel)
	require.Equal(t, QualityActionMoveGroups, converted.Quality.Action)
}

func TestQualityProbeMappedModelsFenceMixedSamples(t *testing.T) {
	plan := pelicanPlan()
	makeResult := func(mapped string) *ScheduledTestResult {
		return &ScheduledTestResult{PelicanConfig: &PelicanTestConfig{ModelID: "public", TestRoute: "openai:bps", TestMappedModel: mapped}}
	}
	setQualityProbeModels(plan, []*ScheduledTestResult{makeResult("first"), makeResult("second"), makeResult("first")})
	require.Equal(t, "", plan.QualityProbeModels["public"])
}

type qualityRouteFilteredPlans struct {
	qualityRecentPlanRepo
}

func (r *qualityRouteFilteredPlans) QualityProbeRouteMatches(context.Context, *ScheduledTestPlan) (bool, error) {
	return false, nil
}

func TestQuality5xxIneligibleRouteDoesNotClaimOrDispatch(t *testing.T) {
	plan := quality5xxStaleStatePlan()
	plans := &qualityRouteFilteredPlans{qualityRecentPlanRepo: qualityRecentPlanRepo{plan: plan}}
	runner := &ScheduledTestRunnerService{planRepo: plans}
	require.NoError(t, runner.runQualityTriggeredAccount(context.Background(), plan.AccountID, time.Now()))
	require.False(t, plans.claimed)
}

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQualityTemplateIdentityThreeIndependentProbes(t *testing.T) {
	ctx := context.Background()
	plans := newTemplatePlanRepo()
	repo := newFakeTemplateRepo(plans, openAIOAuthAccount(1))
	svc := newTemplateService(plans, repo)
	// The manual scope is still unique among manual rules.
	manual := probeTemplate().planFor(1)
	_, ok := plans.add(manual)
	require.True(t, ok)
	_, ok = plans.add(manual)
	require.False(t, ok)
	var templates []*QualityRuleTemplate
	for i := 0; i < 3; i++ {
		tpl := probeTemplate()
		tpl.PelicanConfig.Quality.Action = QualityActionMoveGroups
		tpl.PelicanConfig.Quality.MoveGroupIDs = []int64{7}
		if i == 1 {
			tpl.PelicanConfig.Quality.ActionOn = "passed"
		}
		if i == 2 {
			tpl.PelicanConfig.RuleName = "bps is only a display name"
			tpl.PelicanConfig.QuestionKind = "candy"
			tpl.PelicanConfig.Prompt = "Synthetic question"
			tpl.PelicanConfig.TestChannel = "account"
			tpl.PelicanConfig.Quality.ExpectedAnswer = "42"
		}
		saved, n, err := svc.CreateQualityTemplate(ctx, tpl)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		require.Len(t, saved.PlanIDs, 1)
		require.Equal(t, "ready", saved.Diagnostics.Status)
		require.Equal(t, 1, saved.Diagnostics.MatchedAccounts)
		require.Equal(t, 1, saved.Diagnostics.ActivePlans)
		plan := plans.plans[saved.PlanIDs[0]]
		require.Equal(t, saved.ID, plan.SourceTemplateID)
		if i == 2 {
			require.Equal(t, "account", plan.PelicanConfig.TestChannel)
		}
		// Changing the action scope never allocates another plan to the same identity.
		extra := saved.planFor(1)
		extra.PelicanConfig.Quality.Action = QualityActionObserveOnly
		_, ok := plans.add(extra)
		require.False(t, ok)
		templates = append(templates, saved)
	}
	require.Len(t, plans.plans, 4)
	n, err := svc.SyncQualityTemplates(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	_, err = svc.DeleteQualityTemplate(ctx, templates[0].ID, false)
	require.NoError(t, err)
	require.Equal(t, templates[0].ID, plans.plans[templates[0].PlanIDs[0]].SourceTemplateID)
	require.Len(t, plans.plans, 4)
}

func TestQualityTemplateDiagnosticsZeroAndNoReadSideProbes(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, reason string
		accounts     []QualityTemplateAccount
		models       func(context.Context, int64) ([]string, error)
	}{
		{name: "empty", reason: "no_matching_accounts"},
		{name: "unsupported_account", reason: "unsupported_account", accounts: []QualityTemplateAccount{{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth}}},
		{name: "unsupported_models", reason: "unsupported_models", accounts: []QualityTemplateAccount{openAIOAuthAccount(1)}, models: func(context.Context, int64) ([]string, error) { return nil, nil }},
		{name: "model_check_failed", reason: "model_check_failed", accounts: []QualityTemplateAccount{openAIOAuthAccount(1)}, models: func(context.Context, int64) ([]string, error) { return nil, errors.New("catalog unavailable") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plans := newTemplatePlanRepo()
			repo := newFakeTemplateRepo(plans, tc.accounts...)
			svc := newTemplateService(plans, repo)
			calls := 0
			if tc.models != nil {
				svc.qualityModels = func(ctx context.Context, id int64) ([]string, error) { calls++; return tc.models(ctx, id) }
			}
			saved, n, err := svc.CreateQualityTemplate(ctx, probeTemplate())
			require.NoError(t, err)
			require.Zero(t, n)
			require.Equal(t, "zero", saved.Diagnostics.Status)
			require.Equal(t, 1, saved.Diagnostics.Reasons[tc.reason])
			before := calls
			_, err = svc.ListQualityTemplates(ctx)
			require.NoError(t, err)
			_, err = svc.GetQualityTemplate(ctx, saved.ID)
			require.NoError(t, err)
			require.Equal(t, before, calls, "reading diagnostics must not refresh account models")
		})
	}
}

func TestQualityTemplateDiagnosticsCoverageReasons(t *testing.T) {
	tpl := probeTemplate()
	tpl.ExcludedAccountIDs = []int64{4}
	tpl.SyncIssues = map[int64]string{5: "unsupported_models", 6: "identity_conflict", 7: "create_failed", 1: "concurrent_change"}
	accounts := []QualityTemplateAccount{}
	for i := int64(1); i <= 8; i++ {
		accounts = append(accounts, openAIOAuthAccount(i))
	}
	plans := []*ScheduledTestPlan{{AccountID: 1, Enabled: true}, {AccountID: 2, Enabled: true, ScopeSuspended: true}, {AccountID: 3, Enabled: false}}
	d := DescribeQualityTemplate(tpl, accounts, plans)
	require.Equal(t, 8, d.MatchedAccounts)
	require.Equal(t, 1, d.ActivePlans)
	require.Equal(t, 2, d.SuspendedPlans)
	require.Equal(t, 1, d.ExcludedAccounts)
	require.Equal(t, 1, d.UnsupportedAccounts)
	require.Equal(t, 1, d.ConflictingAccounts)
	require.Equal(t, 1, d.FailedAccounts)
	require.Equal(t, 1, d.PendingAccounts)
	require.Equal(t, "partial", d.Status)
	tpl.Enabled = false
	require.Equal(t, "disabled", DescribeQualityTemplate(tpl, accounts, plans).Status)
}

func TestQualityTemplatePauseDoesNotDiscoverModels(t *testing.T) {
	ctx := context.Background()
	plans := newTemplatePlanRepo()
	repo := newFakeTemplateRepo(plans, openAIOAuthAccount(1))
	svc := newTemplateService(plans, repo)
	tpl, _, err := svc.CreateQualityTemplate(ctx, probeTemplate())
	require.NoError(t, err)
	calls := 0
	svc.qualityModels = func(context.Context, int64) ([]string, error) {
		calls++
		return nil, errors.New("discovery unavailable")
	}
	tpl.Enabled = false
	result, err := svc.UpdateQualityTemplate(ctx, tpl)
	require.NoError(t, err)
	require.Zero(t, result.Failed)
	require.Equal(t, 1, result.Updated)
	require.Zero(t, calls)
	require.False(t, plans.plans[tpl.PlanIDs[0]].Enabled)
}

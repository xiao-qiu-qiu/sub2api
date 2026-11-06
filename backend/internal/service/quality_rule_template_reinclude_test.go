package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type reincludeTemplateRepo struct {
	*fakeTemplateRepo
	updates  int
	attempts []int64
	fail     error
}

func (r *reincludeTemplateRepo) Update(ctx context.Context, tpl *QualityRuleTemplate) (*QualityRuleTemplate, error) {
	r.updates++
	return r.fakeTemplateRepo.Update(ctx, tpl)
}

func (r *reincludeTemplateRepo) ReincludeAccount(ctx context.Context, tpl *QualityRuleTemplate, id int64,
	prepare func(context.Context, QualityTemplateAccount) (*ScheduledTestPlan, string)) (QualityTemplateReincludeResult, error) {
	r.attempts = append(r.attempts, id)
	out := QualityTemplateReincludeResult{AccountID: id, Status: "failed"}
	if r.fail != nil {
		return out, r.fail
	}
	planID, linked := r.links[tpl.ID][id]
	if !linked {
		out.Reason = "not_excluded"
		return out, nil
	}
	if planID != 0 {
		out.Status = "already_covered"
		return out, nil
	}
	for _, account := range r.accounts {
		if account.ID != id {
			continue
		}
		plan, reason := prepare(ctx, account)
		if reason != "" {
			out.Reason = reason
			return out, nil
		}
		created, ok := r.plans.add(plan)
		if !ok {
			out.Reason = "rule_exists"
			return out, nil
		}
		r.links[tpl.ID][id] = created.ID
		out.Status = "created"
		return out, nil
	}
	out.Reason = "not_matching"
	return out, nil
}

func newReincludeService(t *testing.T) (*ScheduledTestService, *reincludeTemplateRepo, *QualityRuleTemplate) {
	t.Helper()
	plans := newTemplatePlanRepo()
	repo := &reincludeTemplateRepo{fakeTemplateRepo: newFakeTemplateRepo(plans, openAIOAuthAccount(1), openAIOAuthAccount(2))}
	svc := newTemplateService(plans, repo.fakeTemplateRepo)
	svc.templateRepo = repo
	tpl, _, err := svc.CreateQualityTemplate(context.Background(), probeTemplate())
	require.NoError(t, err)
	delete(plans.plans, repo.links[tpl.ID][1])
	repo.links[tpl.ID][1] = 0
	tpl, err = repo.Get(context.Background(), tpl.ID)
	require.NoError(t, err)
	return svc, repo, tpl
}

func TestQualityTemplateReincludeExplicitOnlyAndIdempotent(t *testing.T) {
	ctx := context.Background()
	svc, repo, tpl := newReincludeService(t)
	// Leaving the filter and returning, syncing, and ordinary save all retain NULL.
	accounts := repo.accounts
	repo.accounts = nil
	_, err := svc.SyncQualityTemplates(ctx)
	require.NoError(t, err)
	repo.accounts = accounts
	_, err = svc.SyncQualityTemplates(ctx)
	require.NoError(t, err)
	ordinary, err := svc.UpdateQualityTemplate(ctx, tpl)
	require.NoError(t, err)
	require.Zero(t, ordinary.Created)
	require.Empty(t, ordinary.ReincludeResults)
	require.Zero(t, repo.links[tpl.ID][1])

	tpl = ordinary.Template
	tpl.CronExpression, tpl.MaxResults = "7 */2 * * *", 137
	tpl.PelicanConfig.RuleName = "Reincluded rule"
	tpl.PelicanConfig.ModelIDs = []string{tpl.ModelID}
	tpl.PelicanConfig.Quality.TriggerOnUpstream5xx = true
	tpl.PelicanConfig.Quality.RecoveryProbeEnabled = true
	tpl.PelicanConfig.Quality.RecoveryProbeIntervalMinutes = 120
	tpl.PelicanConfig.Quality.RecoveryConfirmationMinutes = 7
	result, err := svc.UpdateQualityTemplate(ctx, tpl, 1, 1, 999)
	require.NoError(t, err)
	require.Equal(t, 1, result.Updated)
	require.Zero(t, result.Failed, "reinclude failures do not count as existing-plan update failures")
	require.Equal(t, 1, result.Created)
	require.Equal(t, []QualityTemplateReincludeResult{{AccountID: 1, Status: "created"}, {AccountID: 999, Status: "failed", Reason: "not_excluded"}}, result.ReincludeResults)
	require.Equal(t, []int64{1, 999}, repo.attempts, "deduplicate while preserving request order")
	plan := repo.plans.plans[repo.links[tpl.ID][1]]
	require.Equal(t, result.Template.PelicanConfig, plan.PelicanConfig)
	require.NotSame(t, result.Template.PelicanConfig, plan.PelicanConfig)
	require.NotNil(t, plan.NextRunAt)
	require.Equal(t, 137, plan.MaxResults)
	require.Equal(t, tpl.CronExpression, plan.CronExpression)
	require.False(t, plan.AutoRecover)

	retry, err := svc.UpdateQualityTemplate(ctx, result.Template, 1)
	require.NoError(t, err)
	require.Zero(t, retry.Created)
	require.Equal(t, 2, retry.Updated)
	require.Equal(t, []QualityTemplateReincludeResult{{AccountID: 1, Status: "already_covered"}}, retry.ReincludeResults)
	require.Len(t, repo.plans.plans, 2)
}

func TestQualityTemplateReincludeValidationBeforeAnyUpdate(t *testing.T) {
	for _, ids := range [][]int64{{1, 0}, {1, -2}, make([]int64, MaxQualityTemplateReincludeAccounts+1)} {
		svc, repo, tpl := newReincludeService(t)
		before := repo.templates[tpl.ID].UpdatedAt
		_, err := svc.UpdateQualityTemplate(context.Background(), tpl, ids...)
		require.Error(t, err)
		require.Zero(t, repo.updates)
		require.Equal(t, before, repo.templates[tpl.ID].UpdatedAt)
		require.Empty(t, repo.attempts)
		require.Zero(t, repo.links[tpl.ID][1])
	}
}

func TestQualityTemplateReincludeFailuresKeepTombstone(t *testing.T) {
	for _, name := range []string{"not_matching", "unsupported_account", "unsupported_models", "model_check_failed", "rule_exists", "template_disabled", "create_failed"} {
		t.Run(name, func(t *testing.T) {
			svc, repo, tpl := newReincludeService(t)
			switch name {
			case "not_matching":
				repo.accounts = repo.accounts[1:]
			case "unsupported_account":
				repo.accounts[0].Type = AccountTypeAPIKey
			case "unsupported_models":
				svc.qualityModels = func(context.Context, int64) ([]string, error) { return nil, nil }
			case "model_check_failed":
				svc.qualityModels = func(context.Context, int64) ([]string, error) { return nil, errors.New("catalog unavailable") }
			case "rule_exists":
				_, ok := repo.plans.add(tpl.planFor(1))
				require.True(t, ok)
			case "template_disabled":
				tpl.Enabled = false
			case "create_failed":
				repo.fail = errors.New("storage unavailable")
			}
			result, err := svc.UpdateQualityTemplate(context.Background(), tpl, 1)
			require.NoError(t, err)
			require.Equal(t, []QualityTemplateReincludeResult{{AccountID: 1, Status: "failed", Reason: name}}, result.ReincludeResults)
			require.Zero(t, result.Created)
			require.Zero(t, repo.links[tpl.ID][1])
		})
	}
}

func TestQualityTemplateReincludeFiltersModelsWithoutMutatingTemplate(t *testing.T) {
	svc, repo, tpl := newReincludeService(t)
	modelPlan := multiModelQualityPlan(t, `["first", "second"]`)
	tpl.ModelID, tpl.PelicanConfig = modelPlan.ModelID, modelPlan.PelicanConfig
	svc.qualityModels = func(context.Context, int64) ([]string, error) { return []string{"second"}, nil }
	result, err := svc.UpdateQualityTemplate(context.Background(), tpl, 1)
	require.NoError(t, err)
	require.Equal(t, "created", result.ReincludeResults[0].Status)
	require.Equal(t, []string{"first", "second"}, result.Template.PelicanConfig.ModelIDs)
	plan := repo.plans.plans[repo.links[tpl.ID][1]]
	require.Equal(t, []string{"second"}, plan.PelicanConfig.ModelIDs)
	require.Equal(t, "second", plan.ModelID)
}

func TestQualityTemplateReincludeOldRepositoryRemainsCompatible(t *testing.T) {
	svc, repo, tpl := newReincludeService(t)
	svc.templateRepo = repo.fakeTemplateRepo
	result, err := svc.UpdateQualityTemplate(context.Background(), tpl, 1)
	require.NoError(t, err)
	require.Equal(t, 1, result.Updated)
	require.Equal(t, "create_failed", result.ReincludeResults[0].Reason)
	require.Zero(t, repo.links[tpl.ID][1])
}

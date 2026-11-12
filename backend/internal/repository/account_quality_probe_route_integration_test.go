//go:build integration

package repository

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestQualityProbeRouteMembershipSuspendsAndResumesPostgres(t *testing.T) {
	for _, mode := range []string{"native", "bps_action", "bps_observation"} {
		bps := mode != "native"
		f := newQualityReincludeFixture(t)
		if bps {
			_, err := integrationDB.ExecContext(f.ctx, `UPDATE accounts SET extra='{"openai_excel_bps":true}' WHERE id=$1`, f.account)
			require.NoError(t, err)
			f.tpl.PelicanConfig.QuestionKind = "candy"
			f.tpl.PelicanConfig.TestChannel = "bps"
			f.tpl.PelicanConfig.Prompt = "Return 21"
			f.tpl.PelicanConfig.Quality.ExpectedAnswer = "21"
			if mode == "bps_observation" {
				f.tpl.PelicanConfig.Quality.Action = service.QualityActionObserveOnly
			}
		}
		updated, err := f.svc.UpdateQualityTemplate(f.ctx, f.tpl, f.account)
		require.NoError(t, err)
		require.Equal(t, 1, updated.Created)
		planID := f.binding().Int64
		for _, enabled := range []bool{!bps, bps} {
			_, err = integrationDB.ExecContext(f.ctx, `UPDATE accounts SET extra=jsonb_build_object('openai_excel_bps',$2::boolean) WHERE id=$1`, f.account, enabled)
			require.NoError(t, err)
			_, err = f.svc.SyncQualityTemplates(f.ctx)
			require.NoError(t, err)
			plan, err := f.plans.GetByID(f.ctx, planID)
			require.NoError(t, err)
			require.Equal(t, enabled != bps, plan.ScopeSuspended)
			require.True(t, plan.Enabled)
			require.Equal(t, planID, f.binding().Int64)
			require.Equal(t, updated.Template.PelicanConfig.TestChannel, plan.PelicanConfig.TestChannel)
		}
	}
}

func TestQualityNativeProbeClaimRejectsBPSChangePostgres(t *testing.T) {
	f := newQualityReincludeFixture(t)
	updated, err := f.svc.UpdateQualityTemplate(f.ctx, f.tpl, f.account)
	require.NoError(t, err)
	require.Equal(t, 1, updated.Created)
	planID := f.binding().Int64
	require.NoError(t, f.plans.TriggerQuality(f.ctx, planID))
	plan, err := f.plans.GetByID(f.ctx, planID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(f.ctx, `UPDATE accounts SET extra='{"openai_excel_bps":true}' WHERE id=$1`, f.account)
	require.NoError(t, err)
	now := time.Now().Add(time.Second)
	claimed, err := f.plans.ClaimPelican(f.ctx, plan, now, now.Add(time.Minute), now.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, claimed)
	plan, err = f.plans.GetByID(f.ctx, planID)
	require.NoError(t, err)
	require.True(t, plan.ScopeSuspended)
	require.Nil(t, plan.RunningUntil)
}

func TestQualityBPSAnswerActionCanQuarantineAndRestorePostgres(t *testing.T) {
	f := newQualityBPSFixture(t, `{"openai_excel_bps":true}`, &service.QualityPolicy{Action: "disable_scheduling", AutoRestore: true})
	f.plan.PelicanConfig.QuestionKind = "candy"
	f.plan.PelicanConfig.TestChannel = "bps"
	f.plan.PelicanConfig.Prompt = "Return 21"
	f.plan.PelicanConfig.Quality.ExpectedAnswer = "21"
	raw, err := json.Marshal(f.plan.PelicanConfig)
	require.NoError(t, err)
	f.exec(`UPDATE scheduled_test_plans SET pelican_config=$2::jsonb WHERE account_id=$1`, string(raw))
	require.Equal(t, "scheduling_disabled", f.apply("failed"))
	f.exec(`UPDATE scheduled_test_plans SET running_until=NULL WHERE account_id=$1`)
	f.claim()
	require.Equal(t, "restored", f.apply("passed"))
	var schedulable bool
	require.NoError(t, integrationDB.QueryRow(`SELECT schedulable FROM accounts WHERE id=$1`, f.account).Scan(&schedulable))
	require.True(t, schedulable)
	require.Equal(t, true, f.extra()["openai_excel_bps"])
}

func TestQualityNativeOutcomeRejectsBPSChangePostgres(t *testing.T) {
	f := newQualityBPSFixture(t, `{}`, &service.QualityPolicy{Action: "disable_scheduling", AutoRestore: true})
	f.exec(`UPDATE accounts SET extra='{"openai_excel_bps":true}' WHERE id=$1`)
	require.Equal(t, "stale_run", f.apply("failed"))
	var schedulable bool
	require.NoError(t, integrationDB.QueryRow(`SELECT schedulable FROM accounts WHERE id=$1`, f.account).Scan(&schedulable))
	require.True(t, schedulable)
}

func TestQualityBPSIndependentCandyRecoveryPostgres(t *testing.T) {
	f := newQualityBPSFixture(t, `{"openai_excel_bps":true}`, &service.QualityPolicy{Action: "disable_scheduling"})
	f.plan.PelicanConfig.QuestionKind = "candy"
	f.plan.PelicanConfig.TestChannel = "bps"
	f.plan.PelicanConfig.Prompt = "Return 21"
	f.plan.PelicanConfig.Quality.ExpectedAnswer = "21"
	raw, err := json.Marshal(f.plan.PelicanConfig)
	require.NoError(t, err)
	f.exec(`UPDATE scheduled_test_plans SET pelican_config=$2::jsonb WHERE account_id=$1`, string(raw))
	require.Equal(t, "scheduling_disabled", f.apply("failed"))
	f.exec(`UPDATE scheduled_test_plans SET running_until=NULL WHERE account_id=$1`)
	svc := service.NewScheduledTestService(f.plans, NewScheduledTestResultRepository(integrationDB))
	f.plan, err = svc.CreatePlan(t.Context(), &service.ScheduledTestPlan{AccountID: f.account, ModelID: "gpt-6-astra", CronExpression: "*/30 * * * *", Enabled: true,
		PelicanConfig: &service.PelicanTestConfig{QuestionKind: "candy", TestChannel: "bps", Prompt: "Return 21", ReasoningEffort: "high", ParallelCount: 1,
			Quality: &service.QualityPolicy{ActionOn: "passed", Action: service.QualityActionEnableScheduling, ExpectedAnswer: "21"}}})
	require.NoError(t, err)
	f.claim()
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
	f.exec(`UPDATE scheduled_test_plans SET running_until=NULL WHERE account_id=$1`)
	_, err = integrationDB.ExecContext(t.Context(), `UPDATE account_quality_states SET state=jsonb_set(state,'{recovery_probe,first_passed_at}',to_jsonb(clock_timestamp()-interval '6 minutes')) WHERE plan_id=$1`, f.plan.ID)
	require.NoError(t, err)
	f.claim()
	require.Equal(t, "scheduling_enabled", f.apply("passed"))
	var schedulable bool
	require.NoError(t, integrationDB.QueryRow(`SELECT schedulable FROM accounts WHERE id=$1`, f.account).Scan(&schedulable))
	require.True(t, schedulable)
	require.Equal(t, true, f.extra()["openai_excel_bps"])
}

func TestQualityReadOnlyRouteAndStableMembershipPreserveLeasePostgres(t *testing.T) {
	f := newQualityReincludeFixture(t)
	updated, err := f.svc.UpdateQualityTemplate(f.ctx, f.tpl, f.account)
	require.NoError(t, err)
	require.Equal(t, 1, updated.Created)
	planID := f.binding().Int64
	require.NoError(t, f.plans.TriggerQuality(f.ctx, planID))
	plan, err := f.plans.GetByID(f.ctx, planID)
	require.NoError(t, err)
	repo := f.plans.(*scheduledTestPlanRepository)
	matching, err := repo.QualityProbeRouteMatches(f.ctx, plan)
	require.NoError(t, err, "the preflight transaction is read-only on PostgreSQL")
	require.True(t, matching)
	now := time.Now().Add(time.Second).Truncate(time.Microsecond)
	until := now.Add(15 * time.Minute)
	claimed, err := f.plans.ClaimPelican(f.ctx, plan, now, until, now.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	for i := 0; i < 2; i++ {
		_, err = f.svc.SyncQualityTemplates(f.ctx)
		require.NoError(t, err)
		current, err := f.plans.GetByID(f.ctx, planID)
		require.NoError(t, err)
		require.True(t, current.UpdatedAt.Equal(plan.UpdatedAt), "stable membership must not fence the active run")
		require.NotNil(t, current.RunningUntil)
		require.True(t, current.RunningUntil.Equal(until))
	}
	_, err = integrationDB.ExecContext(f.ctx, `UPDATE accounts SET extra='{"openai_excel_bps":true}' WHERE id=$1`, f.account)
	require.NoError(t, err)
	matching, err = repo.QualityProbeRouteMatches(f.ctx, plan)
	require.NoError(t, err)
	require.False(t, matching)
}

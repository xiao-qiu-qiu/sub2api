//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func independentRecoveryFixture(t *testing.T, action string) *qualityMoveGroupsFixture {
	t.Helper()
	f := newQualityMoveGroupsFixture(t, false)
	q := f.plan.PelicanConfig.Quality
	q.ActionOn, q.Action = "passed", action
	q.MoveGroupIDs = []int64{f.unrelated}
	svc := service.NewScheduledTestService(f.plans, NewScheduledTestResultRepository(integrationDB))
	plan, err := svc.UpdatePlan(context.Background(), f.plan)
	require.NoError(t, err)
	f.plan = plan
	claimRecoveryProbe(t, f, false)
	return f
}

func TestQualityPassedProbeIndependentTwoPassesAndManualEarlyTrigger(t *testing.T) {
	f := independentRecoveryFixture(t, service.QualityActionMoveGroups)
	original := f.memberships()
	require.Empty(t, f.state().Action, "recovery does not need a main probe ownership")
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
	require.Equal(t, original, f.memberships())
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"), "same lease never counts twice")
	claimRecoveryProbe(t, f, false)
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"), "manual early trigger must wait five minutes")
	require.Equal(t, original, f.memberships())
	f.plans = NewScheduledTestPlanRepository(integrationDB)
	claimRecoveryProbe(t, f, true)
	require.Equal(t, "groups_moved", f.apply("passed"))
	require.Len(t, f.memberships(), 1)
	require.Contains(t, f.memberships(), f.unrelated)
	require.Empty(t, f.state().Action, "healthy actions never create quarantine ownership")
	require.Nil(t, f.state().RecoveryProbe.FirstPassedAt)
	require.WithinDuration(t, time.Now().Add(time.Hour), f.state().RecoveryProbe.NextProbeAt, 3*time.Second)
	f.requireOutboxGroups(f.source, f.sourcePeer, f.existingTarget, f.unrelated)
}

func TestQualityPassedProbeUnknownFailureAndMissingRoundReset(t *testing.T) {
	for _, outcome := range []string{"failed", "inconclusive", "unknown", "skipped", "missing"} {
		t.Run(outcome, func(t *testing.T) {
			f := independentRecoveryFixture(t, service.QualityActionMoveGroups)
			original := f.memberships()
			require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
			claimRecoveryProbe(t, f, true)
			if outcome != "missing" {
				f.apply(outcome)
			}
			claimRecoveryProbe(t, f, false)
			require.Nil(t, f.state().RecoveryProbe.FirstPassedAt)
			require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
			require.Equal(t, original, f.memberships())
		})
	}
}

func TestQualityPassedProbeEnableSchedulingAndObserveOnly(t *testing.T) {
	for _, action := range []string{service.QualityActionEnableScheduling, service.QualityActionObserveOnly} {
		t.Run(action, func(t *testing.T) {
			f := independentRecoveryFixture(t, action)
			f.exec(`UPDATE accounts SET schedulable=false WHERE id=$1`)
			original := f.memberships()
			require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
			claimRecoveryProbe(t, f, true)
			expected := "observed"
			if action == service.QualityActionEnableScheduling {
				expected = "scheduling_enabled"
			}
			require.Equal(t, expected, f.apply("passed"))
			require.Equal(t, original, f.memberships())
			var enabled bool
			require.NoError(t, integrationDB.QueryRowContext(context.Background(), `SELECT schedulable FROM accounts WHERE id=$1`, f.account).Scan(&enabled))
			require.Equal(t, action == service.QualityActionEnableScheduling, enabled)
			require.Equal(t, "kept", f.extra()["unrelated"])
		})
	}
}

func TestQualityPassedProbeIntervalEditInvalidatesConfirmation(t *testing.T) {
	f := independentRecoveryFixture(t, service.QualityActionMoveGroups)
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
	stale := *f.plan
	f.plan.PelicanConfig.Quality.RecoveryProbeIntervalMinutes = 17
	svc := service.NewScheduledTestService(f.plans, NewScheduledTestResultRepository(integrationDB))
	updated, err := svc.UpdatePlan(context.Background(), f.plan)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(17*time.Minute), *updated.NextRunAt, 3*time.Second)
	require.Nil(t, f.state().RecoveryProbe.FirstPassedAt)
	action, err := f.plans.ApplyQualityOutcome(context.Background(), &stale, f.until, "passed")
	require.NoError(t, err)
	require.Equal(t, "stale_run", action)
	f.plan = updated
	claimRecoveryProbe(t, f, false)
	f.apply("inconclusive")
	require.WithinDuration(t, time.Now().Add(17*time.Minute), f.state().RecoveryProbe.NextProbeAt, 3*time.Second)
}

func TestQualityPassedProbeDynamicMainRecoveryCompleteLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newQualityMoveGroupsFixture(t, false)
	require.NoError(t, f.plans.Delete(ctx, f.plan.ID))
	templates := NewQualityRuleTemplateRepository(integrationEntClient, integrationDB)
	svc := service.ProvideScheduledTestService(f.plans, NewScheduledTestResultRepository(integrationDB), templates, nil)
	create := func(group int64, passed bool, target int64) *service.QualityRuleTemplate {
		q := &service.QualityPolicy{Action: service.QualityActionMoveGroups, MoveGroupIDs: []int64{target}}
		if passed {
			q.ActionOn = "passed"
		}
		template, n, err := svc.CreateQualityTemplate(ctx, &service.QualityRuleTemplate{
			AccountFilter: service.QualityRuleAccountFilter{Group: strconv.FormatInt(group, 10), Statuses: []string{"active"}},
			ModelID:       "gpt-6-astra", CronExpression: "* * * * *", Enabled: true, MaxResults: 100,
			PelicanConfig: &service.PelicanTestConfig{QuestionKind: service.OpenAICodexStateProbeQuestionKind, ReasoningEffort: "medium", ParallelCount: 1, Quality: q},
		})
		require.NoError(t, err)
		require.Equal(t, 1, n)
		t.Cleanup(func() {
			_, _ = integrationDB.ExecContext(ctx, `DELETE FROM quality_rule_templates WHERE id=$1`, template.ID)
		})
		return template
	}
	mainTemplate := create(f.source, false, f.newTarget)
	mainID := mainTemplate.PlanIDs[0]
	f.plan, _ = f.plans.GetByID(ctx, mainID)
	f.claim()
	_, err := NewScheduledTestResultRepository(integrationDB).Create(ctx, &service.ScheduledTestResult{PlanID: mainID, Status: "success", StartedAt: time.Now(), FinishedAt: time.Now()})
	require.NoError(t, err)
	require.Equal(t, "groups_moved", f.apply("failed"))
	main, err := f.plans.GetByID(ctx, mainID)
	require.NoError(t, err)
	require.True(t, main.ScopeSuspended, "moving to degraded pool immediately suspends main membership")
	require.NotNil(t, main.RunningUntil, "immediate membership sync retains the completed lease for FinishPelican")
	finished := time.Now().Truncate(time.Microsecond)
	require.NoError(t, f.plans.FinishPelican(ctx, mainID, f.until, finished))
	main, err = f.plans.GetByID(ctx, mainID)
	require.NoError(t, err)
	require.Nil(t, main.RunningUntil)
	require.NotNil(t, main.LastRunAt)
	require.WithinDuration(t, finished, *main.LastRunAt, time.Microsecond)
	require.Equal(t, service.QualityActionMoveGroups, f.state().Action, "suspension preserves ownership")
	current, err := f.plans.ListQualityPlans(ctx)
	require.NoError(t, err)
	foundSuspended := false
	for _, p := range current {
		if p.ID == mainID {
			foundSuspended = p.ScopeSuspended
		}
	}
	require.True(t, foundSuspended, "management list returns the retained plan with suspended flag")
	duplicate := *main
	duplicate.ID = 0
	_, err = f.plans.Create(ctx, &duplicate)
	require.True(t, isUniqueViolation(err), "suspended main scope remains reserved")
	recoveryTemplate := create(f.newTarget, true, f.source)
	recoveryID := recoveryTemplate.PlanIDs[0]
	f.plan, err = f.plans.GetByID(ctx, recoveryID)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(time.Hour), *f.plan.NextRunAt, 3*time.Second)
	f.claim()
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
	require.Contains(t, f.memberships(), f.newTarget)
	claimRecoveryProbe(t, f, true)
	require.Equal(t, "groups_moved", f.apply("passed"))
	finished = time.Now().Truncate(time.Microsecond)
	require.NoError(t, f.plans.FinishPelican(ctx, recoveryID, f.until, finished))
	require.Contains(t, f.memberships(), f.source)
	main, err = f.plans.GetByID(ctx, mainID)
	require.NoError(t, err)
	require.False(t, main.ScopeSuspended)
	recovery, err := f.plans.GetByID(ctx, recoveryID)
	require.NoError(t, err)
	require.True(t, recovery.ScopeSuspended)
	require.NotNil(t, recovery.LastRunAt)
	require.WithinDuration(t, finished, *recovery.LastRunAt, time.Microsecond)
	require.Nil(t, recovery.RunningUntil)
	var stateRaw []byte
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT state FROM account_quality_states WHERE plan_id=$1`, mainID).Scan(&stateRaw))
	var state qualityState
	require.NoError(t, json.Unmarshal(stateRaw, &state))
	require.Empty(t, state.Action, "old group ownership must not block a later quarantine")
	var history int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_test_results WHERE plan_id=$1`, mainID).Scan(&history))
	require.Equal(t, 1, history, "temporary removal preserves detection history")
	f.plan = main
	f.claim()
	require.Equal(t, "groups_moved", f.apply("failed"), "main probe takes over and can quarantine again")
	finished = time.Now().Truncate(time.Microsecond)
	require.NoError(t, f.plans.FinishTriggeredQuality(ctx, f.plan, f.until, finished, finished.Add(30*time.Minute)))
	main, err = f.plans.GetByID(ctx, mainID)
	require.NoError(t, err)
	require.Nil(t, main.RunningUntil)
	require.NotNil(t, main.LastRunAt)
	require.WithinDuration(t, finished, *main.LastRunAt, time.Microsecond, "triggered finish also records completion after immediate membership sync")
	created, err := svc.SyncQualityTemplates(ctx)
	require.NoError(t, err)
	require.Zero(t, created, "returning accounts reuse the original plans")
	recovery, err = f.plans.GetByID(ctx, recoveryID)
	require.NoError(t, err)
	require.False(t, recovery.ScopeSuspended)
}

func dynamicBoundQualityFixture(t *testing.T, autoRestore bool) (*qualityMoveGroupsFixture, service.QualityRuleTemplateRepository, *service.QualityRuleTemplate) {
	t.Helper()
	f := newQualityMoveGroupsFixture(t, autoRestore)
	ctx := context.Background()
	templates := NewQualityRuleTemplateRepository(integrationEntClient, integrationDB)
	template, err := templates.Create(ctx, &service.QualityRuleTemplate{
		AccountFilter: service.QualityRuleAccountFilter{Group: strconv.FormatInt(f.source, 10), Statuses: []string{"active"}},
		ModelID:       f.plan.ModelID, CronExpression: f.plan.CronExpression, Enabled: true, MaxResults: 100, PelicanConfig: f.plan.PelicanConfig,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM quality_rule_templates WHERE id=$1`, template.ID)
	})
	_, err = integrationDB.ExecContext(ctx, `INSERT INTO quality_rule_template_accounts(template_id,account_id,plan_id) VALUES($1,$2,$3)`, template.ID, f.account, f.plan.ID)
	require.NoError(t, err)
	f.plan, err = f.plans.GetByID(ctx, f.plan.ID)
	require.NoError(t, err)
	return f, templates, template
}

func TestQualityPassedProbeTemplateClaimFenceAndManualExclusion(t *testing.T) {
	f, templates, template := dynamicBoundQualityFixture(t, false)
	ctx := context.Background()
	require.NoError(t, f.plans.FinishPelican(ctx, f.plan.ID, f.until, time.Now()))
	require.NoError(t, f.plans.TriggerQuality(ctx, f.plan.ID))
	candidate, err := f.plans.GetByID(ctx, f.plan.ID)
	require.NoError(t, err)
	f.exec(`DELETE FROM account_groups WHERE account_id=$1`)
	f.exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, f.newTarget)
	now := time.Now().Add(time.Second)
	claimed, err := f.plans.ClaimPelican(ctx, candidate, now, now.Add(15*time.Minute), now.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, claimed, "a queued plan must recheck current membership before probing")
	retained, err := f.plans.GetByID(ctx, candidate.ID)
	require.NoError(t, err)
	require.True(t, retained.ScopeSuspended)
	require.Nil(t, retained.RunningUntil)
	require.Error(t, f.plans.TriggerQuality(ctx, retained.ID), "manual trigger cannot run a suspended member")
	svc := service.ProvideScheduledTestService(f.plans, NewScheduledTestResultRepository(integrationDB), templates, nil)
	f.exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, f.source)
	created, err := svc.SyncQualityTemplates(ctx)
	require.NoError(t, err)
	require.Zero(t, created)
	retained, err = f.plans.GetByID(ctx, candidate.ID)
	require.NoError(t, err)
	require.False(t, retained.ScopeSuspended)
	require.NoError(t, f.plans.Delete(ctx, retained.ID))
	f.exec(`DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, f.source)
	_, err = svc.SyncQualityTemplates(ctx)
	require.NoError(t, err)
	f.exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, f.source)
	created, err = svc.SyncQualityTemplates(ctx)
	require.NoError(t, err)
	require.Zero(t, created)
	fresh, err := templates.Get(ctx, template.ID)
	require.NoError(t, err)
	require.Empty(t, fresh.PlanIDs)
	require.Equal(t, []int64{f.account}, fresh.ExcludedAccountIDs, "NULL tombstone never revives after leave/rejoin")
}

func TestQualityPassedProbeTemplateInFlightActionFence(t *testing.T) {
	f, _, _ := dynamicBoundQualityFixture(t, false)
	f.exec(`DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, f.source)
	before := f.memberships()
	require.Equal(t, "stale_run", f.apply("failed"), "leaving the template during the probe fences its action")
	require.Equal(t, before, f.memberships())
}

func TestQualityPassedProbeTemplateLegacyAutoRestoreOnlyDrainsOwnership(t *testing.T) {
	f, templates, _ := dynamicBoundQualityFixture(t, true)
	ctx := context.Background()
	require.Equal(t, "groups_moved", f.apply("failed"))
	require.NoError(t, f.plans.FinishPelican(ctx, f.plan.ID, f.until, time.Now()))
	plan, err := f.plans.GetByID(ctx, f.plan.ID)
	require.NoError(t, err)
	require.True(t, plan.ScopeSuspended)
	// The legacy owner can restore its old memberships even while outside scope.
	_, err = integrationDB.ExecContext(ctx, `UPDATE scheduled_test_plans SET next_run_at=clock_timestamp()-interval '1 second' WHERE id=$1`, plan.ID)
	require.NoError(t, err)
	plan, err = f.plans.GetByID(ctx, plan.ID)
	require.NoError(t, err)
	now := time.Now().Add(time.Second)
	f.until = now.Add(15 * time.Minute)
	claimed, err := f.plans.ClaimPelican(ctx, plan, now, f.until, now.Add(30*time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	f.plan = plan
	require.Equal(t, "already_quarantined", f.apply("failed"), "an unmatched old rule cannot create a second quarantine")
	require.Equal(t, "restored", f.apply("passed"))
	require.NoError(t, f.plans.FinishPelican(ctx, plan.ID, f.until, time.Now()))
	restored, err := f.plans.GetByID(ctx, plan.ID)
	require.NoError(t, err)
	require.False(t, restored.ScopeSuspended)
	// Simulate leaving again after the previous ownership has been drained.
	f.exec(`DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, f.source)
	svc := service.ProvideScheduledTestService(f.plans, NewScheduledTestResultRepository(integrationDB), templates, nil)
	_, err = svc.SyncQualityTemplates(ctx)
	require.NoError(t, err)
	plan, err = f.plans.GetByID(ctx, plan.ID)
	require.NoError(t, err)
	require.True(t, plan.ScopeSuspended)
	_, err = integrationDB.ExecContext(ctx, `UPDATE scheduled_test_plans SET next_run_at=clock_timestamp()-interval '1 second' WHERE id=$1`, plan.ID)
	require.NoError(t, err)
	plan, err = f.plans.GetByID(ctx, plan.ID)
	require.NoError(t, err)
	claimed, err = f.plans.ClaimPelican(ctx, plan, now, now.Add(16*time.Minute), now.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, claimed, "an unmatched plan with no ownership cannot start a fresh quarantine")
}

func claimLegacyMemberForTest(t *testing.T, f *qualityMoveGroupsFixture) bool {
	t.Helper()
	ctx := context.Background()
	_, err := integrationDB.ExecContext(ctx, `UPDATE scheduled_test_plans SET next_run_at=clock_timestamp()-interval '1 second' WHERE id=$1`, f.plan.ID)
	require.NoError(t, err)
	plan, err := f.plans.GetByID(ctx, f.plan.ID)
	require.NoError(t, err)
	now := time.Now().Add(time.Second).Truncate(time.Microsecond)
	f.until = now.Add(15 * time.Minute)
	claimed, err := f.plans.ClaimPelican(ctx, plan, now, f.until, now.Add(30*time.Minute))
	require.NoError(t, err)
	f.plan = plan
	return claimed
}

func TestQualityPassedProbeTakesOverLegacyEvenWithStaleSuspendedFlag(t *testing.T) {
	for _, staleFlag := range []bool{false, true} {
		t.Run(strconv.FormatBool(staleFlag), func(t *testing.T) {
			f, templates, _ := dynamicBoundQualityFixture(t, true)
			ctx := context.Background()
			require.Equal(t, "groups_moved", f.apply("failed"))
			require.NoError(t, f.plans.FinishPelican(ctx, f.plan.ID, f.until, time.Now()))
			require.True(t, claimLegacyMemberForTest(t, f), "no independent recovery keeps the legacy path")
			svc := service.ProvideScheduledTestService(f.plans, NewScheduledTestResultRepository(integrationDB), templates, nil)
			recovery, n, err := svc.CreateQualityTemplate(ctx, &service.QualityRuleTemplate{
				AccountFilter: service.QualityRuleAccountFilter{Group: strconv.FormatInt(f.newTarget, 10)},
				ModelID:       f.plan.ModelID, CronExpression: "0 * * * *", Enabled: true, MaxResults: 100,
				PelicanConfig: &service.PelicanTestConfig{QuestionKind: service.OpenAICodexStateProbeQuestionKind, ReasoningEffort: "medium", ParallelCount: 1,
					Quality: &service.QualityPolicy{ActionOn: "passed", Action: service.QualityActionMoveGroups, MoveGroupIDs: []int64{f.source}}},
			})
			require.NoError(t, err)
			require.Equal(t, 1, n)
			t.Cleanup(func() {
				_, _ = integrationDB.ExecContext(ctx, `DELETE FROM quality_rule_templates WHERE id=$1`, recovery.ID)
			})
			_, err = integrationDB.ExecContext(ctx, `UPDATE scheduled_test_plans SET scope_suspended=$2 WHERE id=$1`, recovery.PlanIDs[0], staleFlag)
			require.NoError(t, err)
			before := f.memberships()
			require.Equal(t, "stale_run", f.apply("passed"), "a new recovery owner fences the already-running legacy restore")
			require.Equal(t, before, f.memberships())
			require.Equal(t, service.QualityActionMoveGroups, f.state().Action)
			require.NoError(t, f.plans.FinishPelican(ctx, f.plan.ID, f.until, time.Now()))
			require.False(t, claimLegacyMemberForTest(t, f), "live template match fences new legacy claims despite stale suspended flag")
			_, err = integrationDB.ExecContext(ctx, `UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`, recovery.PlanIDs[0])
			require.NoError(t, err)
			require.True(t, claimLegacyMemberForTest(t, f), "disabling independent recovery preserves the old fallback")
			require.Equal(t, "restored", f.apply("passed"))
		})
	}
}

func TestQualityPassedProbeNonMatchingRecoveryDoesNotBlockLegacyFallback(t *testing.T) {
	f, templates, _ := dynamicBoundQualityFixture(t, true)
	ctx := context.Background()
	require.Equal(t, "groups_moved", f.apply("failed"))
	require.NoError(t, f.plans.FinishPelican(ctx, f.plan.ID, f.until, time.Now()))
	svc := service.ProvideScheduledTestService(f.plans, NewScheduledTestResultRepository(integrationDB), templates, nil)
	recovery, n, err := svc.CreateQualityTemplate(ctx, &service.QualityRuleTemplate{
		AccountFilter: service.QualityRuleAccountFilter{Group: strconv.FormatInt(f.newTarget, 10)},
		ModelID:       f.plan.ModelID, CronExpression: "0 * * * *", Enabled: true, MaxResults: 100,
		PelicanConfig: &service.PelicanTestConfig{QuestionKind: service.OpenAICodexStateProbeQuestionKind, ReasoningEffort: "medium", ParallelCount: 1,
			Quality: &service.QualityPolicy{ActionOn: "passed", Action: service.QualityActionMoveGroups, MoveGroupIDs: []int64{f.source}}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM quality_rule_templates WHERE id=$1`, recovery.ID)
	})
	// Save a new filter without a membership sync: the cached scope flag remains
	// active, but actual scope no longer includes this account.
	recovery.AccountFilter.Group = strconv.FormatInt(f.unrelated, 10)
	_, err = templates.Update(ctx, recovery)
	require.NoError(t, err)
	require.True(t, claimLegacyMemberForTest(t, f), "cached active recovery outside its real filter cannot suppress legacy recovery")
	require.Equal(t, "restored", f.apply("passed"))
}

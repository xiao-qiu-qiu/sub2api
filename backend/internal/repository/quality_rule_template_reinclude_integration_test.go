//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

type qualityReincludeFixture struct {
	t        *testing.T
	ctx      context.Context
	group    int64
	account  int64
	accounts []int64
	tpl      *service.QualityRuleTemplate
	repo     *qualityRuleTemplateRepository
	plans    service.ScheduledTestPlanRepository
	svc      *service.ScheduledTestService
}

func newQualityReincludeFixture(t *testing.T) *qualityReincludeFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	f := &qualityReincludeFixture{t: t, ctx: ctx, repo: &qualityRuleTemplateRepository{client: integrationEntClient, db: integrationDB}, plans: NewScheduledTestPlanRepository(integrationDB)}
	require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO groups(name,platform) VALUES($1,'openai') RETURNING id`, fmt.Sprintf("reinclude-%d", time.Now().UnixNano())).Scan(&f.group))
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM quality_rule_templates WHERE account_filter->>'group'=$1`, strconv.FormatInt(f.group, 10))
		_, _ = integrationDB.Exec(`DELETE FROM scheduled_test_plans WHERE account_id=ANY($1)`, pq.Array(f.accounts))
		_, _ = integrationDB.Exec(`DELETE FROM scheduler_outbox WHERE account_id=ANY($1)`, pq.Array(f.accounts))
		_, _ = integrationDB.Exec(`DELETE FROM account_groups WHERE account_id=ANY($1)`, pq.Array(f.accounts))
		_, _ = integrationDB.Exec(`DELETE FROM accounts WHERE id=ANY($1)`, pq.Array(f.accounts))
		_, _ = integrationDB.Exec(`DELETE FROM groups WHERE id=$1`, f.group)
	})
	f.account = f.addAccount()
	f.svc = service.ProvideScheduledTestService(f.plans, NewScheduledTestResultRepository(integrationDB), f.repo, nil)
	var err error
	f.tpl, _, err = f.svc.CreateQualityTemplate(ctx, &service.QualityRuleTemplate{
		AccountFilter: service.QualityRuleAccountFilter{Group: strconv.FormatInt(f.group, 10)},
		ModelID:       "gpt-test", CronExpression: "7 */2 * * *", Enabled: true, MaxResults: 137,
		PelicanConfig: &service.PelicanTestConfig{RuleName: "Explicit reinclude", QuestionKind: service.OpenAICodexStateProbeQuestionKind, ReasoningEffort: "high", ParallelCount: 1, ModelIDs: []string{"gpt-test"},
			Quality: &service.QualityPolicy{Action: "disable_scheduling", AutoRestore: true, TriggerOnUpstream5xx: true,
				RecoveryProbeEnabled: true, RecoveryProbeIntervalMinutes: 120, RecoveryConfirmationMinutes: 7}},
	})
	require.NoError(t, err)
	require.Len(t, f.tpl.PlanIDs, 1)
	require.NoError(t, f.plans.Delete(ctx, f.tpl.PlanIDs[0]))
	f.tpl, err = f.repo.Get(ctx, f.tpl.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{f.account}, f.tpl.ExcludedAccountIDs)
	return f
}

func (f *qualityReincludeFixture) addAccount() int64 {
	f.t.Helper()
	var id int64
	require.NoError(f.t, integrationDB.QueryRowContext(f.ctx, `INSERT INTO accounts(name,platform,type,status,extra) VALUES($1,'openai','oauth','active','{}') RETURNING id`, fmt.Sprintf("reinclude-account-%d", time.Now().UnixNano())).Scan(&id))
	f.accounts = append(f.accounts, id)
	_, err := integrationDB.ExecContext(f.ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, id, f.group)
	require.NoError(f.t, err)
	return id
}

func (f *qualityReincludeFixture) binding() sql.NullInt64 {
	f.t.Helper()
	var id sql.NullInt64
	require.NoError(f.t, integrationDB.QueryRowContext(f.ctx, `SELECT plan_id FROM quality_rule_template_accounts WHERE template_id=$1 AND account_id=$2`, f.tpl.ID, f.account).Scan(&id))
	return id
}

func (f *qualityReincludeFixture) countPlans() int {
	f.t.Helper()
	var count int
	require.NoError(f.t, integrationDB.QueryRowContext(f.ctx, `SELECT count(*) FROM scheduled_test_plans WHERE account_id=$1`, f.account).Scan(&count))
	return count
}

func (f *qualityReincludeFixture) prepare(_ context.Context, account service.QualityTemplateAccount) (*service.ScheduledTestPlan, string) {
	next := time.Now().Add(time.Hour)
	return &service.ScheduledTestPlan{AccountID: account.ID, ModelID: f.tpl.ModelID, CronExpression: f.tpl.CronExpression, Enabled: true, MaxResults: f.tpl.MaxResults, PelicanConfig: f.tpl.PelicanConfig, NextRunAt: &next}, ""
}

func TestQualityTemplateReincludePostgresOrdinarySaveAndExplicitRetry(t *testing.T) {
	f := newQualityReincludeFixture(t)
	// Leaving and returning does not revoke an admin's explicit deletion.
	_, err := integrationDB.ExecContext(f.ctx, `DELETE FROM account_groups WHERE account_id=$1`, f.account)
	require.NoError(t, err)
	_, err = f.svc.SyncQualityTemplates(f.ctx)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(f.ctx, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, f.account, f.group)
	require.NoError(t, err)
	ordinary, err := f.svc.UpdateQualityTemplate(f.ctx, f.tpl)
	require.NoError(t, err)
	require.Zero(t, ordinary.Created)
	require.Zero(t, f.countPlans())
	require.False(t, f.binding().Valid)
	require.Equal(t, []int64{f.account}, ordinary.Template.ExcludedAccountIDs)

	result, err := f.svc.UpdateQualityTemplate(f.ctx, ordinary.Template, f.account, f.account)
	require.NoError(t, err)
	require.Equal(t, 1, result.Created)
	require.Zero(t, result.Updated)
	require.Zero(t, result.Failed)
	require.Equal(t, []service.QualityTemplateReincludeResult{{AccountID: f.account, Status: "created"}}, result.ReincludeResults)
	require.Empty(t, result.Template.ExcludedAccountIDs)
	require.NotNil(t, result.Template.ExcludedAccountIDs, "JSON must encode [] rather than null")
	plan, err := f.plans.GetByID(f.ctx, f.binding().Int64)
	require.NoError(t, err)
	require.Equal(t, result.Template.PelicanConfig, plan.PelicanConfig, "clone the whole validated configuration, including recovery settings")
	require.Equal(t, result.Template.CronExpression, plan.CronExpression)
	require.Equal(t, result.Template.MaxResults, plan.MaxResults)
	require.Equal(t, result.Template.ModelID, plan.ModelID)
	require.True(t, plan.Enabled)
	require.NotNil(t, plan.NextRunAt)
	require.Nil(t, plan.LastRunAt)
	require.Nil(t, plan.RunningUntil)
	require.False(t, plan.AutoRecover)

	retry, err := f.svc.UpdateQualityTemplate(f.ctx, result.Template, f.account)
	require.NoError(t, err)
	require.Equal(t, 1, retry.Updated)
	require.Zero(t, retry.Created)
	require.Equal(t, "already_covered", retry.ReincludeResults[0].Status)
	require.Equal(t, 1, f.countPlans())
	// A later deletion is a new tombstone; ordinary sync must not undo it.
	require.NoError(t, f.plans.Delete(f.ctx, plan.ID))
	_, err = f.svc.SyncQualityTemplates(f.ctx)
	require.NoError(t, err)
	require.False(t, f.binding().Valid)
	require.Zero(t, f.countPlans())
}

func TestQualityTemplateReincludePostgresExcludedListOnlyLiveNullBindings(t *testing.T) {
	f := newQualityReincludeFixture(t)
	deleted := f.addAccount()
	covered := f.addAccount()
	_, err := integrationDB.ExecContext(f.ctx, `INSERT INTO quality_rule_template_accounts(template_id,account_id) VALUES($1,$2)`, f.tpl.ID, deleted)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(f.ctx, `UPDATE accounts SET deleted_at=NOW() WHERE id=$1`, deleted)
	require.NoError(t, err)
	_, err = f.svc.SyncQualityTemplates(f.ctx)
	require.NoError(t, err)
	all, err := f.repo.List(f.ctx)
	require.NoError(t, err)
	for _, tpl := range all {
		if tpl.ID == f.tpl.ID {
			require.Equal(t, []int64{f.account}, tpl.ExcludedAccountIDs)
			require.Len(t, tpl.PlanIDs, 1)
		}
	}
	current, err := f.repo.Get(f.ctx, f.tpl.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{f.account}, current.ExcludedAccountIDs)
	plans, err := f.plans.ListByAccountID(f.ctx, covered)
	require.NoError(t, err)
	require.Len(t, plans, 1)
}

func TestQualityTemplateReincludePostgresFailuresPreserveTombstone(t *testing.T) {
	for _, reason := range []string{"not_matching", "unsupported_account", "template_disabled", "rule_exists", "other_template"} {
		t.Run(reason, func(t *testing.T) {
			f := newQualityReincludeFixture(t)
			switch reason {
			case "not_matching":
				_, err := integrationDB.ExecContext(f.ctx, `DELETE FROM account_groups WHERE account_id=$1`, f.account)
				require.NoError(t, err)
			case "unsupported_account":
				_, err := integrationDB.ExecContext(f.ctx, `UPDATE accounts SET type='apikey' WHERE id=$1`, f.account)
				require.NoError(t, err)
			case "template_disabled":
				f.tpl.Enabled = false
			case "rule_exists", "other_template":
				plan, _ := f.prepare(f.ctx, service.QualityTemplateAccount{ID: f.account})
				if reason == "other_template" {
					other := *f.tpl
					other.ID = 0
					saved, err := f.repo.Create(f.ctx, &other)
					require.NoError(t, err)
					_, err = f.repo.CreateLinkedPlan(f.ctx, saved, plan)
					require.NoError(t, err)
				} else {
					_, err := f.plans.Create(f.ctx, plan)
					require.NoError(t, err)
				}
			}
			before := f.countPlans()
			result, err := f.svc.UpdateQualityTemplate(f.ctx, f.tpl, f.account)
			require.NoError(t, err)
			expected := reason
			if expected == "other_template" {
				expected = "rule_exists"
			}
			require.Equal(t, []service.QualityTemplateReincludeResult{{AccountID: f.account, Status: "failed", Reason: expected}}, result.ReincludeResults)
			require.Zero(t, result.Created)
			require.Zero(t, result.Failed)
			require.False(t, f.binding().Valid)
			require.Equal(t, before, f.countPlans())
		})
	}
}

func TestQualityTemplateReincludePostgresVersionNotExcludedAndRollback(t *testing.T) {
	f := newQualityReincludeFixture(t)
	current, err := f.repo.Update(f.ctx, f.tpl)
	require.NoError(t, err)
	stale, err := f.repo.ReincludeAccount(f.ctx, f.tpl, f.account, f.prepare)
	require.NoError(t, err)
	require.Equal(t, "template_changed", stale.Reason)
	f.tpl = current
	outside := f.addAccount()
	notExcluded, err := f.repo.ReincludeAccount(f.ctx, current, outside, f.prepare)
	require.NoError(t, err)
	require.Equal(t, "not_excluded", notExcluded.Reason)
	failed, err := f.repo.ReincludeAccount(f.ctx, current, f.account, func(ctx context.Context, a service.QualityTemplateAccount) (*service.ScheduledTestPlan, string) {
		plan, _ := f.prepare(ctx, a)
		plan.ModelID = strings.Repeat("x", 101) // real PostgreSQL insert failure
		return plan, ""
	})
	require.Error(t, err)
	require.Equal(t, "create_failed", failed.Reason)
	require.False(t, f.binding().Valid)
	require.Zero(t, f.countPlans())
	retry, err := f.repo.ReincludeAccount(f.ctx, current, f.account, f.prepare)
	require.NoError(t, err)
	require.Equal(t, "created", retry.Status)
	require.True(t, f.binding().Valid)
}

func TestQualityTemplateReincludePostgresModelChecksPreserveTombstone(t *testing.T) {
	for _, reason := range []string{"unsupported_models", "model_check_failed"} {
		t.Run(reason, func(t *testing.T) {
			f := newQualityReincludeFixture(t)
			if reason == "unsupported_models" {
				_, err := integrationDB.ExecContext(f.ctx, `UPDATE accounts SET credentials='{"model_mapping":{"other":"other"}}' WHERE id=$1`, f.account)
				require.NoError(t, err)
			}
			// The real account repository enforces its whitelist. No gateway is
			// installed, making discovery failure deterministic without networking.
			accountTests := service.NewAccountTestService(NewAccountRepository(integrationEntClient, integrationDB, nil), nil, nil, nil, nil, nil, nil, nil)
			svc := service.ProvideScheduledTestService(f.plans, NewScheduledTestResultRepository(integrationDB), f.repo, accountTests)
			result, err := svc.UpdateQualityTemplate(f.ctx, f.tpl, f.account)
			require.NoError(t, err)
			require.Equal(t, []service.QualityTemplateReincludeResult{{AccountID: f.account, Status: "failed", Reason: reason}}, result.ReincludeResults)
			require.Zero(t, result.Created)
			require.False(t, f.binding().Valid)
			require.Zero(t, f.countPlans())
		})
	}
}

func TestQualityTemplateReincludePostgresBindingFailureRollsBackPlan(t *testing.T) {
	f := newQualityReincludeFixture(t)
	name := fmt.Sprintf("reinclude_binding_failure_%d", f.tpl.ID)
	// Fail after INSERT succeeded, precisely at the NULL -> plan_id transition.
	_, err := integrationDB.ExecContext(f.ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.template_id=%d THEN RAISE EXCEPTION 'synthetic binding failure'; END IF; RETURN NEW; END $$`, name, f.tpl.ID))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON quality_rule_template_accounts`, name))
		_, _ = integrationDB.Exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
	})
	_, err = integrationDB.ExecContext(f.ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE OF plan_id ON quality_rule_template_accounts FOR EACH ROW EXECUTE FUNCTION %s()`, name, name))
	require.NoError(t, err)
	result, err := f.svc.UpdateQualityTemplate(f.ctx, f.tpl, f.account)
	require.NoError(t, err)
	require.Equal(t, "create_failed", result.ReincludeResults[0].Reason)
	require.Zero(t, result.Created)
	require.False(t, f.binding().Valid)
	require.Zero(t, f.countPlans(), "a failed binding update must roll back the newly inserted plan")
}

func TestQualityTemplateReincludePostgresOtherScopeIsNotStolen(t *testing.T) {
	f := newQualityReincludeFixture(t)
	other := *f.tpl
	config := *other.PelicanConfig
	policy := *config.Quality
	policy.Action, policy.AutoRestore, policy.RecoveryProbeEnabled = service.QualityActionObserveOnly, false, false
	config.Quality, other.PelicanConfig = &policy, &config
	saved, _, err := f.svc.CreateQualityTemplate(f.ctx, &other)
	require.NoError(t, err)
	require.Len(t, saved.PlanIDs, 1)
	result, err := f.svc.UpdateQualityTemplate(f.ctx, f.tpl, f.account)
	require.NoError(t, err)
	require.Equal(t, "created", result.ReincludeResults[0].Status)
	require.Equal(t, 2, f.countPlans(), "independent scopes may coexist")
	otherReloaded, err := f.repo.Get(f.ctx, saved.ID)
	require.NoError(t, err)
	require.Equal(t, saved.PlanIDs, otherReloaded.PlanIDs)
	require.NotEqual(t, saved.PlanIDs[0], f.binding().Int64)
}

func TestQualityTemplateReincludePostgresRechecksMatchAfterAccountLock(t *testing.T) {
	f := newQualityReincludeFixture(t)
	f.tpl.AccountFilter.Statuses = []string{"active"}
	current, err := f.repo.Update(f.ctx, f.tpl)
	require.NoError(t, err)
	f.tpl = current
	tx, err := integrationDB.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var blockerPID int
	require.NoError(t, tx.QueryRowContext(f.ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))
	_, err = tx.ExecContext(f.ctx, `UPDATE accounts SET status='disabled' WHERE id=$1`, f.account)
	require.NoError(t, err)
	type outcome struct {
		result service.QualityTemplateReincludeResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := f.repo.ReincludeAccount(f.ctx, current, f.account, f.prepare)
		done <- outcome{result: result, err: err}
	}()
	// Observe the actual lock wait rather than guessing when the worker started.
	require.Eventually(t, func() bool {
		var waiting bool
		err := integrationDB.QueryRowContext(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blockerPID).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, tx.Commit())
	select {
	case out := <-done:
		require.NoError(t, out.err)
		require.Equal(t, "not_matching", out.result.Reason)
	case <-f.ctx.Done():
		t.Fatal("reinclude did not finish after releasing the account lock")
	}
	require.False(t, f.binding().Valid)
	require.Zero(t, f.countPlans())
}

func TestQualityTemplateReincludePostgresConcurrentSyncAndReinclude(t *testing.T) {
	f := newQualityReincludeFixture(t)
	const workers = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan service.QualityTemplateReincludeResult, workers)
	errors := make(chan error, workers*2)
	for range workers {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			result, err := f.repo.ReincludeAccount(f.ctx, f.tpl, f.account, f.prepare)
			results <- result
			errors <- err
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := f.svc.SyncQualityTemplates(f.ctx)
			errors <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	created := 0
	for result := range results {
		require.Contains(t, []string{"created", "already_covered"}, result.Status)
		if result.Status == "created" {
			created++
		}
	}
	require.Equal(t, 1, created)
	require.Equal(t, 1, f.countPlans())
	require.True(t, f.binding().Valid)
	// An old sync that read the account before its first link existed cannot
	// overwrite a tombstone left after this successful concurrent round.
	require.NoError(t, f.plans.Delete(f.ctx, f.binding().Int64))
	plan, _ := f.prepare(f.ctx, service.QualityTemplateAccount{ID: f.account})
	_, err := f.repo.CreateLinkedPlan(f.ctx, f.tpl, plan)
	require.ErrorIs(t, err, service.ErrQualityTemplateSkipped)
	require.False(t, f.binding().Valid)
	require.Zero(t, f.countPlans())
}

func TestQualityTemplateReincludePostgresSingleConnectionModelPreparation(t *testing.T) {
	f := newQualityReincludeFixture(t)
	db, err := sql.Open("postgres", integrationPostgresDSN)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	accountTests := service.NewAccountTestService(NewAccountRepository(client, db, nil), nil, nil, nil, nil, nil, nil, nil)
	svc := service.ProvideScheduledTestService(NewScheduledTestPlanRepository(db), NewScheduledTestResultRepository(db), NewQualityRuleTemplateRepository(client, db), accountTests)
	_, err = integrationDB.ExecContext(f.ctx, `UPDATE accounts SET credentials='{"model_mapping":{"gpt-test":"gpt-test"}}' WHERE id=$1`, f.account)
	require.NoError(t, err)
	f.tpl.PelicanConfig.QuestionKind = "candy"
	f.tpl.PelicanConfig.Prompt = "Synthetic question"
	f.tpl.PelicanConfig.Quality.ExpectedAnswer = "42"
	ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	defer cancel()
	// The real model validator reads the account through this same one-slot pool.
	result, err := svc.UpdateQualityTemplate(ctx, f.tpl, f.account)
	require.NoError(t, err)
	require.Equal(t, "created", result.ReincludeResults[0].Status, "%+v", result.ReincludeResults)
	require.NoError(t, ctx.Err())
	require.Equal(t, 1, f.countPlans())
}

func TestQualityTemplateReincludePostgresModelPreparationCanWriteAccount(t *testing.T) {
	f := newQualityReincludeFixture(t)
	var writeErr error
	result, err := f.repo.ReincludeAccount(f.ctx, f.tpl, f.account, func(ctx context.Context, a service.QualityTemplateAccount) (*service.ScheduledTestPlan, string) {
		writeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		// Model discovery's OAuth 401 handler can write this very account on
		// another connection. Raw SQL also proves the fence covers writes that
		// do not explicitly bump updated_at.
		_, writeErr = integrationDB.ExecContext(writeCtx, `UPDATE accounts SET extra=extra || '{"reinclude_discovery_write":true}' WHERE id=$1`, a.ID)
		if writeErr != nil {
			return nil, "model_check_failed"
		}
		return f.prepare(ctx, a)
	})
	require.NoError(t, err)
	require.NoError(t, writeErr, "model preparation must not run while holding account row locks")
	require.Equal(t, "account_changed", result.Reason)
	require.False(t, f.binding().Valid)
	require.Zero(t, f.countPlans())
	retry, err := f.repo.ReincludeAccount(f.ctx, f.tpl, f.account, f.prepare)
	require.NoError(t, err)
	require.Equal(t, "created", retry.Status, "a new request may prepare against the changed account")
}

func TestQualityTemplateReincludePostgresUsageWritesDuringModelPreparation(t *testing.T) {
	f := newQualityReincludeFixture(t)
	var writeErr error
	result, err := f.repo.ReincludeAccount(f.ctx, f.tpl, f.account, func(ctx context.Context, a service.QualityTemplateAccount) (*service.ScheduledTestPlan, string) {
		_, writeErr = integrationDB.ExecContext(ctx, `UPDATE accounts SET last_used_at=NOW(), updated_at=NOW() WHERE id=$1`, a.ID)
		if writeErr != nil {
			return nil, "model_check_failed"
		}
		return f.prepare(ctx, a)
	})
	require.NoError(t, err)
	require.NoError(t, writeErr)
	require.Equal(t, "created", result.Status, "ordinary traffic must not invalidate model preparation")
	require.Empty(t, result.Reason)
	require.True(t, f.binding().Valid)
	require.Equal(t, 1, f.countPlans())
	var lastUsedAt time.Time
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT last_used_at FROM accounts WHERE id=$1`, f.account).Scan(&lastUsedAt))
	require.False(t, lastUsedAt.IsZero())
}

func TestQualityTemplateReincludePostgresTemplateChangesDuringModelPreparation(t *testing.T) {
	for _, pause := range []bool{false, true} {
		t.Run(fmt.Sprintf("pause_%t", pause), func(t *testing.T) {
			f := newQualityReincludeFixture(t)
			var updateErr error
			result, err := f.repo.ReincludeAccount(f.ctx, f.tpl, f.account, func(ctx context.Context, a service.QualityTemplateAccount) (*service.ScheduledTestPlan, string) {
				writeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
				defer cancel()
				changed := *f.tpl
				changed.Enabled = !pause
				changed.CronExpression = "11 */3 * * *"
				_, updateErr = f.repo.Update(writeCtx, &changed)
				return f.prepare(ctx, a)
			})
			require.NoError(t, err)
			require.NoError(t, updateErr, "model preparation must not hold a template row lock")
			expected := "template_changed"
			if pause {
				expected = "template_disabled"
			}
			require.Equal(t, expected, result.Reason)
			require.False(t, f.binding().Valid)
			require.Zero(t, f.countPlans())
		})
	}
}

func TestQualityTemplateReincludePostgresNewTombstoneDuringModelPreparation(t *testing.T) {
	f := newQualityReincludeFixture(t)
	result, err := f.repo.ReincludeAccount(f.ctx, f.tpl, f.account, func(ctx context.Context, a service.QualityTemplateAccount) (*service.ScheduledTestPlan, string) {
		// Another request wins, then the admin deletes its rule during discovery.
		// This request must not consume that fresh tombstone using an old intent.
		other, err := f.repo.ReincludeAccount(ctx, f.tpl, f.account, f.prepare)
		require.NoError(t, err)
		require.Equal(t, "created", other.Status)
		require.NoError(t, f.plans.Delete(ctx, f.binding().Int64))
		return f.prepare(ctx, a)
	})
	require.NoError(t, err)
	require.Equal(t, "not_excluded", result.Reason)
	require.False(t, f.binding().Valid)
	require.Zero(t, f.countPlans())
}

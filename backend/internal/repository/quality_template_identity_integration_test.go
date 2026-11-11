//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestQualityTemplateIdentityPostgresConcurrentAndDetach(t *testing.T) {
	f := newQualityReincludeFixture(t)
	// Match the production probe shape: failed move_groups, passed recovery,
	// and account-routed candy with move_groups share the same account.
	f.tpl.PelicanConfig.Quality.Action = service.QualityActionMoveGroups
	f.tpl.PelicanConfig.Quality.MoveGroupIDs = []int64{f.group}
	// Reinclude the original identity, then make the two other probe identities.
	updated, err := f.svc.UpdateQualityTemplate(f.ctx, f.tpl, f.account)
	require.NoError(t, err)
	require.Equal(t, 1, updated.Created)
	f.tpl = updated.Template
	var templates []*service.QualityRuleTemplate
	templates = append(templates, f.tpl)
	for i := 0; i < 2; i++ {
		candidate := *f.tpl
		candidate.ID = 0
		raw := *f.tpl.PelicanConfig
		policy := *raw.Quality
		raw.Quality = &policy
		candidate.PelicanConfig = &raw
		if i == 0 {
			policy.ActionOn = "passed"
		} else {
			raw.QuestionKind = "candy"
			raw.Prompt = "Synthetic question"
			raw.TestChannel = "account"
			raw.RuleName = "bps display text is not a channel selector"
			policy.ExpectedAnswer = "42"
		}
		// Same quarantine scope for template 1 and candy template 3.
		saved, err := f.repo.Create(f.ctx, &candidate)
		require.NoError(t, err)
		templates = append(templates, saved)
	}
	const workers = 18
	var wg sync.WaitGroup
	var created atomic.Int32
	failures := make(chan error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		tpl := templates[1+i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			plan, _ := f.prepare(f.ctx, service.QualityTemplateAccount{ID: f.account})
			plan.PelicanConfig = tpl.PelicanConfig
			_, err := f.repo.CreateLinkedPlan(f.ctx, tpl, plan)
			if err == nil {
				created.Add(1)
			} else if !errors.Is(err, service.ErrQualityTemplateSkipped) {
				failures <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	require.EqualValues(t, 2, created.Load())
	require.Equal(t, 3, f.countPlans())
	n, err := f.svc.SyncQualityTemplates(f.ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	listed, err := f.svc.ListQualityTemplates(f.ctx)
	require.NoError(t, err)
	for _, tpl := range listed {
		if tpl.AccountFilter.Group != f.tpl.AccountFilter.Group {
			continue
		}
		require.Equal(t, "ready", tpl.Diagnostics.Status)
		require.Equal(t, 1, tpl.Diagnostics.ActivePlans)
		require.Equal(t, 1, tpl.Diagnostics.MatchedAccounts)
	}
	// Exercise the actual service sync concurrently against a newly matching account,
	// rather than only calling the atomic repository insertion method.
	joined := f.addAccount()
	var synced atomic.Int32
	syncErrors := make(chan error, 12)
	syncStart := make(chan struct{})
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-syncStart
			n, err := f.svc.SyncQualityTemplates(f.ctx)
			synced.Add(int32(n))
			syncErrors <- err
		}()
	}
	close(syncStart)
	wg.Wait()
	close(syncErrors)
	for err := range syncErrors {
		require.NoError(t, err)
	}
	require.EqualValues(t, 3, synced.Load())
	var joinedPlans, joinedOrigins int
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT count(*),count(DISTINCT source_template_id) FROM scheduled_test_plans WHERE account_id=$1`, joined).Scan(&joinedPlans, &joinedOrigins))
	require.Equal(t, 3, joinedPlans)
	require.Equal(t, 3, joinedOrigins)
	// Preserve a real ownership snapshot and result across deleting only the template.
	plan, err := f.plans.GetByID(f.ctx, f.tpl.PlanIDs[0])
	require.NoError(t, err)
	// Configuration updates cannot erase an established source identity.
	source := plan.SourceTemplateID
	plan.SourceTemplateID = 0
	plan, err = f.plans.Update(f.ctx, plan)
	require.NoError(t, err)
	require.Equal(t, source, plan.SourceTemplateID)
	snapshot := `{"action":"disable_scheduling","prior_schedulable":true,"synthetic_marker":"unchanged"}`
	_, err = integrationDB.ExecContext(f.ctx, `INSERT INTO account_quality_states(plan_id,state) VALUES($1,$2)`, plan.ID, snapshot)
	require.NoError(t, err)
	result, err := NewScheduledTestResultRepository(integrationDB).Create(f.ctx, &service.ScheduledTestResult{PlanID: plan.ID, Status: "success", StartedAt: time.Now(), FinishedAt: time.Now(), PelicanConfig: plan.PelicanConfig})
	require.NoError(t, err)
	_, err = f.svc.DeleteQualityTemplate(f.ctx, f.tpl.ID, false)
	require.NoError(t, err)
	detached, err := f.plans.GetByID(f.ctx, plan.ID)
	require.NoError(t, err)
	require.Zero(t, detached.QualityTemplateID)
	require.Equal(t, f.tpl.ID, detached.SourceTemplateID)
	require.False(t, detached.ScopeSuspended)
	var stored string
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT state::text FROM account_quality_states WHERE plan_id=$1`, plan.ID).Scan(&stored))
	require.JSONEq(t, snapshot, stored)
	history := NewScheduledTestResultRepository(integrationDB).(service.FilteredQualityHistoryRepository)
	rows, err := history.ListFilteredQualityHistory(f.ctx, 0, 100, service.QualityHistoryFilter{TemplateID: f.tpl.ID})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, result.ID, rows[0].ID)
	rows, err = history.ListFilteredQualityHistory(f.ctx, 0, 100, service.QualityHistoryFilter{Unassigned: true, PlanID: plan.ID})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	all, err := f.svc.ListQualityPlansWithSources(f.ctx)
	require.NoError(t, err)
	found := false
	for _, p := range all {
		if p.ID == plan.ID {
			found = true
			require.Equal(t, f.tpl.ID, p.SourceTemplateID)
			require.Zero(t, p.QualityTemplateID)
		}
	}
	require.True(t, found)
	// Detached origins keep separate slots, while manual-to-manual conflicts remain.
	manual, _ := f.prepare(f.ctx, service.QualityTemplateAccount{ID: f.account})
	_, err = f.plans.Create(f.ctx, manual)
	require.NoError(t, err)
	_, err = f.plans.Create(f.ctx, manual)
	require.True(t, isUniqueViolation(err))
}

func TestQualityTemplateIdentityMigrationBackfillsAndReservesIndependentSlots(t *testing.T) {
	ctx := context.Background()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	schema := fmt.Sprintf("quality_identity_%d", time.Now().UnixNano())
	_, err = tx.ExecContext(ctx, `CREATE SCHEMA `+pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SET LOCAL search_path TO `+pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `CREATE TABLE scheduled_test_plans(id BIGINT PRIMARY KEY,account_id BIGINT,pelican_config JSONB,enabled BOOLEAN DEFAULT true,updated_at TIMESTAMPTZ DEFAULT NOW());
 CREATE TABLE quality_rule_templates(id BIGINT PRIMARY KEY,enabled BOOLEAN DEFAULT true);
 CREATE TABLE quality_rule_template_accounts(template_id BIGINT,account_id BIGINT,plan_id BIGINT);
 INSERT INTO scheduled_test_plans(id,account_id,pelican_config) VALUES(1,8,'{"quality":{"action":"move_groups"}}'),(2,8,'{"quality":{"action":"enable_bps"}}');
 INSERT INTO quality_rule_templates VALUES(10);
 INSERT INTO quality_rule_template_accounts VALUES(10,8,1);`)
	require.NoError(t, err)
	raw, err := migrations.FS.ReadFile("338_quality_template_identity.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = tx.ExecContext(ctx, string(raw))
		require.NoError(t, err, "migration must be repeatable")
	}
	var source int64
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT source_template_id FROM scheduled_test_plans WHERE id=1`).Scan(&source))
	require.EqualValues(t, 10, source)
	// Two different template identities share an account and action.
	_, err = tx.ExecContext(ctx, `INSERT INTO scheduled_test_plans(id,account_id,pelican_config,source_template_id) VALUES(3,8,'{"quality":{"action":"move_groups"}}',11),(4,8,'{"quality":{"action":"move_groups"}}',NULL)`)
	require.NoError(t, err)
	for _, query := range []string{
		`INSERT INTO quality_rule_template_accounts(template_id,account_id,plan_id) VALUES(11,8,1)`,
		`INSERT INTO scheduled_test_plans(id,account_id,pelican_config,source_template_id) VALUES(5,8,'{"quality":{"action":"observe_only"}}',10)`,
		`INSERT INTO scheduled_test_plans(id,account_id,pelican_config,source_template_id) VALUES(6,8,'{"quality":{"action":"move_groups"}}',NULL)`,
	} {
		_, err = tx.ExecContext(ctx, "SAVEPOINT expected_conflict")
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, query)
		require.True(t, isUniqueViolation(err))
		_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT expected_conflict")
		require.NoError(t, err)
	}
}

func TestQualityTemplateIdentityMigrationRejectsSharedPlanWithoutDataLoss(t *testing.T) {
	ctx := context.Background()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	schema := fmt.Sprintf("quality_identity_shared_%d", time.Now().UnixNano())
	_, err = tx.ExecContext(ctx, `CREATE SCHEMA `+pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SET LOCAL search_path TO `+pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `CREATE TABLE scheduled_test_plans(id BIGINT PRIMARY KEY,account_id BIGINT,pelican_config JSONB);
 CREATE TABLE quality_rule_templates(id BIGINT PRIMARY KEY);
 CREATE TABLE quality_rule_template_accounts(template_id BIGINT,account_id BIGINT,plan_id BIGINT);
 CREATE TABLE scheduled_test_results(id BIGINT PRIMARY KEY,plan_id BIGINT,response_text TEXT);
 CREATE TABLE account_quality_states(plan_id BIGINT PRIMARY KEY,state JSONB);
 INSERT INTO scheduled_test_plans VALUES(1,8,'{"quality":{"action":"move_groups"}}');
 INSERT INTO quality_rule_templates VALUES(10),(11);
 INSERT INTO quality_rule_template_accounts VALUES(10,8,1),(11,8,1);
 INSERT INTO scheduled_test_results VALUES(99,1,'synthetic retained history');
 INSERT INTO account_quality_states VALUES(1,'{"action":"move_groups","removed":[{"group_id":7}]}');`)
	require.NoError(t, err)
	snapshot := func() string {
		var value string
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT jsonb_build_object(
 'plans',(SELECT jsonb_agg(to_jsonb(p)) FROM scheduled_test_plans p),
 'bindings',(SELECT jsonb_agg(to_jsonb(l) ORDER BY template_id) FROM quality_rule_template_accounts l),
 'templates',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM quality_rule_templates t),
 'results',(SELECT jsonb_agg(to_jsonb(r)) FROM scheduled_test_results r),
 'states',(SELECT jsonb_agg(to_jsonb(s)) FROM account_quality_states s))::text`).Scan(&value))
		return value
	}
	before := snapshot()
	raw, err := migrations.FS.ReadFile("338_quality_template_identity.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "SAVEPOINT before_338")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(raw))
	require.Error(t, err)
	var pgErr *pq.Error
	require.ErrorAs(t, err, &pgErr)
	require.EqualValues(t, "23514", pgErr.Code)
	require.Contains(t, pgErr.Message, "shared non-NULL plan_id")
	_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT before_338")
	require.NoError(t, err)
	require.JSONEq(t, before, snapshot())
	var sourceColumns int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name='scheduled_test_plans' AND column_name='source_template_id'`, schema).Scan(&sourceColumns))
	require.Zero(t, sourceColumns, "refuse ambiguity before touching the legacy plan schema")
}

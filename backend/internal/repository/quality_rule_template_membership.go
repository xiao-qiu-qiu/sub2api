package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/robfig/cron/v3"
)

// This SQL selects legacy candidates only. A template's cached suspended flag
// may lag its current filter: claim/action must also call legacyQualityRecoveryTx.
const legacyOwnedRecoverySQL = `(COALESCE(pelican_config->'quality'->>'action_on','')<>'passed'
 AND pelican_config->'quality'->>'auto_restore'='true'
 AND EXISTS (SELECT 1 FROM account_quality_states qs WHERE qs.plan_id=scheduled_test_plans.id
 AND qs.state->>'action' IN ('move_groups','remove_groups','disable_scheduling'))
 AND NOT EXISTS (SELECT 1 FROM scheduled_test_plans recovery
 WHERE recovery.account_id=scheduled_test_plans.account_id AND recovery.enabled
 AND recovery.pelican_config->'quality'->>'action_on'='passed'
 AND NOT EXISTS (SELECT 1 FROM quality_rule_template_accounts binding WHERE binding.plan_id=recovery.id))
 AND NOT EXISTS (SELECT 1 FROM quality_rule_template_accounts l JOIN quality_rule_templates t
 ON t.id=l.template_id WHERE l.plan_id=scheduled_test_plans.id AND NOT t.enabled))`

func legacyQualityRecoveryTx(ctx context.Context, tx *sql.Tx, plan *service.ScheduledTestPlan, state *qualityState) (bool, error) {
	if matching, err := qualityProbeRouteMatchesTx(ctx, tx, plan, state); err != nil || !matching {
		return false, err
	}
	q := plan.PelicanConfig.Quality
	if q == nil || q.RunsOnPassed() || !q.AutoRestore || !qualityRecoveryProbeOwns(state) {
		return false, nil
	}
	var paused bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM quality_rule_template_accounts l
 JOIN quality_rule_templates t ON t.id=l.template_id WHERE l.plan_id=$1 AND NOT t.enabled)`, plan.ID).Scan(&paused); err != nil {
		return false, err
	}
	if paused {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM scheduled_test_plans WHERE account_id=$1 AND enabled
 AND pelican_config->'quality'->>'action_on'='passed' ORDER BY id`, plan.AccountID)
	if err != nil {
		return false, err
	}
	var recoveryIDs []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return false, err
		}
		recoveryIDs = append(recoveryIDs, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return false, err
	}
	for _, id := range recoveryIDs {
		matching, err := qualityTemplatePlanMatchesTx(ctx, tx, id, plan.AccountID)
		if err != nil {
			return false, err
		}
		if matching {
			return false, nil
		}
	}
	return true, nil
}

// The caller holds the plan lock and account lock. Use exactly the selector's
// predicates, on this transaction, instead of trusting an earlier sync snapshot.
func qualityTemplatePlanMatchesTx(ctx context.Context, tx *sql.Tx, planID, accountID int64) (bool, error) {
	// Route isolation also applies to standalone plans and legacy recovery.
	var config []byte
	if err := tx.QueryRowContext(ctx, `SELECT pelican_config FROM scheduled_test_plans WHERE id=$1`, planID).Scan(&config); err != nil {
		return false, err
	}
	var cfg *service.PelicanTestConfig
	if len(config) > 0 {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return false, err
		}
	}
	if cfg != nil && (cfg.QuestionKind == service.OpenAICodexStateProbeQuestionKind || cfg.TestChannel == "bps") {
		state, err := readQualityState(ctx, tx, planID)
		if err != nil {
			return false, err
		}
		if matching, err := qualityProbeRouteMatchesTx(ctx, tx, &service.ScheduledTestPlan{ID: planID, AccountID: accountID, PelicanConfig: cfg}, &state); err != nil || !matching {
			return false, err
		}
	}
	var raw []byte
	var enabled bool
	err := tx.QueryRowContext(ctx, `SELECT t.account_filter,t.enabled FROM quality_rule_template_accounts l
 JOIN quality_rule_templates t ON t.id=l.template_id WHERE l.plan_id=$1 AND l.account_id=$2`, planID, accountID).Scan(&raw, &enabled)
	if err == sql.ErrNoRows {
		return true, nil // Standalone or explicitly detached plan.
	}
	if err != nil {
		return false, err
	}
	if !enabled {
		return false, nil
	}
	var filter service.QualityRuleAccountFilter
	if err = json.Unmarshal(raw, &filter); err != nil {
		return false, err
	}
	client := dbent.NewClient(dbent.Driver(entsql.NewDriver(dialect.Postgres, entsql.Conn{ExecQuerier: tx})))
	return qualityTemplateAccountsQuery(client, filter).Where(dbaccount.IDEQ(accountID)).Exist(ctx)
}

// Reconcile both directions without deleting plans or touching NULL tombstones.
// Each member follows the same plan -> account lock order as quality actions.
func (r *qualityRuleTemplateRepository) SyncMembership(ctx context.Context, t *service.QualityRuleTemplate) error {
	if len(t.PlanIDs) == 0 {
		return nil
	}
	type member struct {
		planID, accountID int64
		suspended         bool
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id,account_id,scope_suspended FROM scheduled_test_plans WHERE id=ANY($1) ORDER BY id`, pq.Array(t.PlanIDs))
	if err != nil {
		return err
	}
	var members []member
	var accountIDs []int64
	for rows.Next() {
		var m member
		if err = rows.Scan(&m.planID, &m.accountID, &m.suspended); err != nil {
			_ = rows.Close()
			return err
		}
		members = append(members, m)
		accountIDs = append(accountIDs, m.accountID)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil || len(members) == 0 {
		return err
	}
	client := dbent.NewClient(dbent.Driver(entsql.NewDriver(dialect.Postgres, entsql.Conn{ExecQuerier: r.db})))
	matchingIDs, err := qualityTemplateAccountsQuery(client, t.AccountFilter).Where(dbaccount.IDIn(accountIDs...)).IDs(ctx)
	if err != nil {
		return err
	}
	matching := make(map[int64]bool, len(matchingIDs))
	for _, id := range matchingIDs {
		matching[id] = true
	}
	for _, m := range members {
		if m.suspended == !matching[m.accountID] && (t.PelicanConfig == nil || t.PelicanConfig.QuestionKind != service.OpenAICodexStateProbeQuestionKind && t.PelicanConfig.TestChannel != "bps") {
			continue
		}
		if err := r.syncMember(ctx, t, m.planID); err != nil {
			return err
		}
	}
	return nil
}

func (r *qualityRuleTemplateRepository) syncMember(ctx context.Context, t *service.QualityRuleTemplate, planID int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	plan, err := scanPlan(tx.QueryRowContext(ctx, `SELECT id,account_id,model_id,cron_expression,enabled,max_results,auto_recover,
 last_run_at,next_run_at,created_at,updated_at,pelican_config,running_until,scope_suspended,
 COALESCE((SELECT template_id FROM quality_rule_template_accounts WHERE plan_id=scheduled_test_plans.id LIMIT 1),0)
 FROM scheduled_test_plans WHERE id=$1 FOR UPDATE`, planID))
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var current bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM quality_rule_template_accounts l
 JOIN quality_rule_templates t ON t.id=l.template_id WHERE l.plan_id=$1 AND t.id=$2 AND t.updated_at=$3)`, planID, t.ID, t.UpdatedAt).Scan(&current); err != nil || !current {
		return err
	}
	var schedulable bool
	if err = tx.QueryRowContext(ctx, `SELECT schedulable FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR NO KEY UPDATE`, plan.AccountID).Scan(&schedulable); err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	matching, err := qualityTemplatePlanMatchesTx(ctx, tx, plan.ID, plan.AccountID)
	if err != nil || plan.ScopeSuspended == !matching {
		return err
	}
	state, err := readQualityState(ctx, tx, plan.ID)
	if err != nil {
		return err
	}
	if matching {
		// A recovered/manual move supersedes a former group quarantine. Relinquish
		// that stale group-only ownership, without restoring any membership. Model
		// restrictions keep their own owners and are never cleared by this handoff.
		if state.Action == service.QualityActionMoveGroups || state.Action == "remove_groups" {
			groups, readErr := qualityGroups(ctx, tx, plan.AccountID)
			if readErr != nil {
				return readErr
			}
			if len(state.Remaining) > 0 && !sameQualityMembershipJSON(groups, state.Remaining) {
				state = qualityState{}
			}
		} else if state.Action == "disable_scheduling" && schedulable {
			state = qualityState{}
		}
	}
	state.RecoveryProbe = nil // Crossing the boundary invalidates confirmation.
	now := time.Now()
	var next time.Time
	if plan.PelicanConfig != nil && plan.PelicanConfig.Quality.RunsOnPassed() {
		next = now.Add(plan.PelicanConfig.Quality.RecoveryProbeInterval())
	} else {
		schedule, parseErr := cron.ParseStandard(plan.CronExpression)
		if parseErr != nil {
			return parseErr
		}
		next = schedule.Next(now)
	}
	// Keep an executing lease until its runner writes last_run_at and releases
	// it. The version change fences further actions; scope fences new claims.
	err = tx.QueryRowContext(ctx, `UPDATE scheduled_test_plans SET scope_suspended=$2,updated_at=clock_timestamp(),
 next_run_at=$3 WHERE id=$1 RETURNING updated_at`, plan.ID, !matching, next).Scan(&plan.UpdatedAt)
	if err != nil {
		return err
	}
	if err = qualityUpsertState(ctx, tx, plan.ID, state); err != nil {
		return err
	}
	if err = resetQualityRecoveryProbe(ctx, tx, plan, &state, now); err != nil {
		return err
	}
	return tx.Commit()
}

func sameQualityMembershipJSON(left, right []byte) bool {
	var a, b any
	if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil {
		return false
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func syncQualityMembershipAfterAction(ctx context.Context, db *sql.DB, plan *service.ScheduledTestPlan, action string) {
	switch action {
	case "groups_moved", "groups_removed", "groups_restored_models_isolated", "restored", "scheduling_enabled", "scheduling_disabled":
	default:
		return
	}
	if plan.QualityTemplateID == 0 && (plan.PelicanConfig == nil || !plan.PelicanConfig.Quality.RunsOnPassed()) {
		return
	}
	if err := syncQualityAccountMembership(ctx, db, plan.AccountID); err != nil {
		// The account action already committed. Preserve its result; the runner's
		// next sync retries membership, and claim/action guards fence stale scope.
		logger.LegacyPrintf("repository.scheduled_test", "quality template membership sync failed for account=%d", plan.AccountID)
	}
}

func syncQualityAccountMembership(ctx context.Context, db *sql.DB, accountID int64) error {
	rows, err := db.QueryContext(ctx, `SELECT template_id,plan_id FROM quality_rule_template_accounts WHERE account_id=$1 AND plan_id IS NOT NULL ORDER BY template_id,plan_id`, accountID)
	if err != nil {
		return err
	}
	members := map[int64][]int64{}
	for rows.Next() {
		var templateID, planID int64
		if err = rows.Scan(&templateID, &planID); err != nil {
			_ = rows.Close()
			return err
		}
		members[templateID] = append(members[templateID], planID)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	repo := &qualityRuleTemplateRepository{db: db}
	for templateID, plans := range members {
		template, err := repo.Get(ctx, templateID)
		if err != nil {
			return err
		}
		template.PlanIDs = plans
		if err = repo.SyncMembership(ctx, template); err != nil {
			return err
		}
	}
	return nil
}

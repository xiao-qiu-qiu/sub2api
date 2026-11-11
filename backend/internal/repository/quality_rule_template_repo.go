package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type qualityRuleTemplateRepository struct {
	client *dbent.Client
	db     *sql.DB
}

func NewQualityRuleTemplateRepository(client *dbent.Client, db *sql.DB) service.QualityRuleTemplateRepository {
	return &qualityRuleTemplateRepository{client: client, db: db}
}

// plan_ids lists the rules the template created that still exist on live accounts.
const qualityRuleTemplateColumns = `
	t.id, t.account_filter, t.model_id, t.cron_expression, t.enabled, t.max_results, t.pelican_config,
	t.last_synced_at, t.created_at, t.updated_at,
	ARRAY(
		SELECT l.plan_id FROM quality_rule_template_accounts l
		JOIN scheduled_test_plans p ON p.id = l.plan_id
		JOIN accounts a ON a.id = p.account_id AND a.deleted_at IS NULL
		WHERE l.template_id = t.id
		ORDER BY l.plan_id
	),
	ARRAY(
		SELECT l.account_id FROM quality_rule_template_accounts l
		JOIN accounts a ON a.id = l.account_id AND a.deleted_at IS NULL
		WHERE l.template_id = t.id AND l.plan_id IS NULL
		ORDER BY l.account_id
	)`

func scanQualityRuleTemplate(row scannable) (*service.QualityRuleTemplate, error) {
	t := &service.QualityRuleTemplate{}
	var filter, config []byte
	var planIDs, excludedAccountIDs pq.Int64Array
	if err := row.Scan(&t.ID, &filter, &t.ModelID, &t.CronExpression, &t.Enabled, &t.MaxResults, &config,
		&t.LastSyncedAt, &t.CreatedAt, &t.UpdatedAt, &planIDs, &excludedAccountIDs); err != nil {
		return nil, err
	}
	if len(filter) > 0 {
		if err := json.Unmarshal(filter, &t.AccountFilter); err != nil {
			return nil, err
		}
	}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &t.PelicanConfig); err != nil {
			return nil, err
		}
	}
	t.PlanIDs = []int64(planIDs)
	if t.PlanIDs == nil {
		t.PlanIDs = []int64{}
	}
	t.ExcludedAccountIDs = []int64(excludedAccountIDs)
	if t.ExcludedAccountIDs == nil {
		t.ExcludedAccountIDs = []int64{}
	}
	return t, nil
}

func marshalQualityRuleAccountFilter(filter service.QualityRuleAccountFilter) (string, error) {
	data, err := json.Marshal(filter)
	return string(data), err
}

func (r *qualityRuleTemplateRepository) List(ctx context.Context) ([]*service.QualityRuleTemplate, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+qualityRuleTemplateColumns+` FROM quality_rule_templates t ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	templates := []*service.QualityRuleTemplate{}
	for rows.Next() {
		t, err := scanQualityRuleTemplate(rows)
		if err != nil {
			return nil, err
		}
		templates = append(templates, t)
	}
	return templates, rows.Err()
}

func (r *qualityRuleTemplateRepository) Get(ctx context.Context, id int64) (*service.QualityRuleTemplate, error) {
	t, err := scanQualityRuleTemplate(r.db.QueryRowContext(ctx,
		`SELECT `+qualityRuleTemplateColumns+` FROM quality_rule_templates t WHERE t.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrQualityTemplateNotFound
	}
	return t, err
}

func (r *qualityRuleTemplateRepository) Create(ctx context.Context, t *service.QualityRuleTemplate) (*service.QualityRuleTemplate, error) {
	if err := validateQualityGroupReferences(ctx, r.db, t.PelicanConfig); err != nil {
		return nil, err
	}
	filter, err := marshalQualityRuleAccountFilter(t.AccountFilter)
	if err != nil {
		return nil, err
	}
	var id int64
	if err := r.db.QueryRowContext(ctx, `
		INSERT INTO quality_rule_templates (account_filter, model_id, cron_expression, enabled, max_results, pelican_config)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`, filter, t.ModelID, t.CronExpression, t.Enabled, t.MaxResults, marshalPelicanConfig(t.PelicanConfig)).Scan(&id); err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}

// Update bumps updated_at, which makes any sync still holding the old version skip its accounts.
func (r *qualityRuleTemplateRepository) Update(ctx context.Context, t *service.QualityRuleTemplate) (*service.QualityRuleTemplate, error) {
	if err := validateQualityGroupReferences(ctx, r.db, t.PelicanConfig); err != nil {
		return nil, err
	}
	filter, err := marshalQualityRuleAccountFilter(t.AccountFilter)
	if err != nil {
		return nil, err
	}
	saved, err := scanQualityRuleTemplate(r.db.QueryRowContext(ctx, `
		UPDATE quality_rule_templates t
		SET account_filter = $2, model_id = $3, cron_expression = $4, enabled = $5, max_results = $6,
			pelican_config = $7, updated_at = NOW()
		WHERE id = $1
		RETURNING `+qualityRuleTemplateColumns,
		t.ID, filter, t.ModelID, t.CronExpression, t.Enabled, t.MaxResults, marshalPelicanConfig(t.PelicanConfig)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrQualityTemplateNotFound
	}
	return saved, err
}

// Delete locks the template first so a sync in flight either finishes its plan (and it is
// returned here) or sees the template gone and skips.
func (r *qualityRuleTemplateRepository) Delete(ctx context.Context, id int64) (planIDs []int64, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var locked int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM quality_rule_templates WHERE id = $1 FOR UPDATE`, id).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		err = service.ErrQualityTemplateNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT plan_id FROM quality_rule_template_accounts
		WHERE template_id = $1 AND plan_id IS NOT NULL
		ORDER BY plan_id
	`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var planID int64
		if err = rows.Scan(&planID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		planIDs = append(planIDs, planID)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM quality_rule_templates WHERE id = $1`, id); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE scheduled_test_plans SET scope_suspended=false WHERE id=ANY($1)`, pq.Array(planIDs)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return planIDs, nil
}

// MatchingAccounts reuses the admin account list predicates so the selector's count and the
// accounts a group rule covers always agree.
func (r *qualityRuleTemplateRepository) MatchingAccounts(ctx context.Context, filter service.QualityRuleAccountFilter) ([]service.QualityTemplateAccount, error) {
	rows, err := qualityTemplateAccountsQuery(r.client, filter).
		Order(dbent.Asc(dbaccount.FieldID)).
		Select(dbaccount.FieldID, dbaccount.FieldPlatform, dbaccount.FieldType, dbaccount.FieldExtra).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.QualityTemplateAccount, 0, len(rows))
	for _, row := range rows {
		out = append(out, service.QualityTemplateAccount{ID: row.ID, Platform: row.Platform, Type: row.Type, Extra: row.Extra})
	}
	return out, nil
}

func qualityTemplateAccountsQuery(client *dbent.Client, filter service.QualityRuleAccountFilter) *dbent.AccountQuery {
	accounts := &accountRepository{client: client}
	return accounts.accountListFilteredQuery("", filter.Type, strings.Join(filter.Statuses, ","), filter.Search, service.QualityTemplateGroupID(filter), "").
		Where(dbaccount.DeletedAtIsNil())
}

func (r *qualityRuleTemplateRepository) LinkedAccountIDs(ctx context.Context, id int64) (map[int64]struct{}, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT account_id FROM quality_rule_template_accounts WHERE template_id = $1`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	linked := map[int64]struct{}{}
	for rows.Next() {
		var accountID int64
		if err := rows.Scan(&accountID); err != nil {
			return nil, err
		}
		linked[accountID] = struct{}{}
	}
	return linked, rows.Err()
}

// CreateLinkedPlan links the account and creates its plan atomically. The template must still
// be enabled at the version the caller read; the link must be new; and the account must not
// already hold a rule of the same kind (unique index). Otherwise nothing is written.
func (r *qualityRuleTemplateRepository) CreateLinkedPlan(ctx context.Context, t *service.QualityRuleTemplate, plan *service.ScheduledTestPlan) (created *service.ScheduledTestPlan, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var current int64
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM quality_rule_templates WHERE id = $1 AND enabled AND updated_at = $2 FOR SHARE
	`, t.ID, t.UpdatedAt).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		err = service.ErrQualityTemplateSkipped
	}
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO quality_rule_template_accounts (template_id, account_id)
		SELECT $1, id FROM accounts WHERE id = $2 AND deleted_at IS NULL
		ON CONFLICT DO NOTHING
	`, t.ID, plan.AccountID)
	if err != nil {
		return nil, err
	}
	if n, rowsErr := res.RowsAffected(); rowsErr != nil {
		err = rowsErr
		return nil, err
	} else if n == 0 {
		err = service.ErrQualityTemplateSkipped
		return nil, err
	}
	created, err = insertScheduledTestPlan(ctx, tx, plan)
	if isUniqueViolation(err) {
		err = service.ErrQualityTemplateSkipped
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `
		UPDATE quality_rule_template_accounts SET plan_id = $3 WHERE template_id = $1 AND account_id = $2
	`, t.ID, plan.AccountID, created.ID); err != nil {
		return nil, err
	}
	var accountID int64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR NO KEY UPDATE`, plan.AccountID).Scan(&accountID); err != nil {
		return nil, err
	}
	matching, err := qualityTemplatePlanMatchesTx(ctx, tx, created.ID, plan.AccountID)
	if err != nil {
		return nil, err
	}
	if !matching {
		return nil, service.ErrQualityTemplateSkipped
	}
	created.QualityTemplateID = t.ID
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return created, nil
}

// Only configuration used by type/model validation and discovery belongs in this
// snapshot. Traffic timestamps and other runtime fields must not invalidate it;
// current filter membership is checked separately inside the transaction.
// Plan lifecycle publishes admission independently of account configuration.
// Strip only that server-owned key; all other Extra still fences discovery.
const qualityTemplateAccountConfigSnapshot = `jsonb_build_array(a.platform, a.type, a.credentials, CASE WHEN jsonb_typeof(a.extra)='object' THEN a.extra-'` + service.QualityProbeAdmissionKey + `' ELSE a.extra END, a.proxy_id, a.parent_account_id)::text`

// ReincludeAccount prepares models before opening a transaction, then consumes
// only the same NULL binding against an unchanged account snapshot. Lock order is
// template -> existing plans (ascending ID) -> account -> binding, following
// plan update/deletion's plan -> account order.
// Ordinary sync still treats every binding (including NULL) as permanent coverage.
func (r *qualityRuleTemplateRepository) ReincludeAccount(ctx context.Context, t *service.QualityRuleTemplate, accountID int64,
	prepare func(context.Context, service.QualityTemplateAccount) (*service.ScheduledTestPlan, string)) (service.QualityTemplateReincludeResult, error) {
	result := service.QualityTemplateReincludeResult{AccountID: accountID, Status: "failed", Reason: "create_failed"}
	// The config snapshot catches credential/extra changes without rejecting
	// ordinary usage writes. Binding xmin detects NULL -> plan -> NULL during a
	// slow discovery, so a concurrent fresh deletion cannot be undone here.
	var accountSnapshot string
	var bindingVersion sql.NullString
	var priorPlanID sql.NullInt64
	err := r.db.QueryRowContext(ctx, `
		SELECT `+qualityTemplateAccountConfigSnapshot+`, l.xmin::text, l.plan_id FROM accounts a
		LEFT JOIN quality_rule_template_accounts l ON l.account_id=a.id AND l.template_id=$1
		WHERE a.id=$2 AND a.deleted_at IS NULL`, t.ID, accountID).Scan(&accountSnapshot, &bindingVersion, &priorPlanID)
	if errors.Is(err, sql.ErrNoRows) {
		result.Reason = "not_matching"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if !bindingVersion.Valid {
		result.Reason = "not_excluded"
		return result, nil
	}
	var plan *service.ScheduledTestPlan
	var prepareReason string
	if !priorPlanID.Valid {
		account, err := qualityTemplateAccountsQuery(r.client, t.AccountFilter).Where(dbaccount.IDEQ(accountID)).Only(ctx)
		if dbent.IsNotFound(err) {
			prepareReason = "not_matching"
		} else if err != nil {
			return result, err
		} else {
			plan, prepareReason = prepare(ctx, service.QualityTemplateAccount{ID: account.ID, Platform: account.Platform, Type: account.Type, Extra: account.Extra})
		}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	var enabled bool
	var version time.Time
	err = tx.QueryRowContext(ctx, `SELECT enabled, updated_at FROM quality_rule_templates WHERE id=$1 FOR SHARE`, t.ID).Scan(&enabled, &version)
	if errors.Is(err, sql.ErrNoRows) {
		result.Reason = "template_changed"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if !enabled {
		result.Reason = "template_disabled"
		return result, nil
	}
	if !version.Equal(t.UpdatedAt) {
		result.Reason = "template_changed"
		return result, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM scheduled_test_plans WHERE account_id=$1 ORDER BY id FOR UPDATE`, accountID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return result, err
		}
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return result, err
	}
	if closeErr != nil {
		return result, closeErr
	}
	var lockedSnapshot string
	// NO KEY UPDATE still fences account edits, but permits FK KEY SHARE checks
	// by concurrent plan inserts before they reach the unique scope constraint.
	err = tx.QueryRowContext(ctx, `SELECT `+qualityTemplateAccountConfigSnapshot+` FROM accounts a WHERE a.id=$1 AND a.deleted_at IS NULL FOR NO KEY UPDATE`, accountID).Scan(&lockedSnapshot)
	if errors.Is(err, sql.ErrNoRows) {
		result.Reason = "not_matching"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	var planID sql.NullInt64
	var lockedBindingVersion string
	err = tx.QueryRowContext(ctx, `SELECT plan_id, xmin::text FROM quality_rule_template_accounts WHERE template_id=$1 AND account_id=$2 FOR UPDATE`, t.ID, accountID).Scan(&planID, &lockedBindingVersion)
	if errors.Is(err, sql.ErrNoRows) {
		result.Reason = "not_excluded"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if planID.Valid {
		result.Status, result.Reason = "already_covered", ""
		return result, nil
	}
	// Reuse all admin selector predicates, on the very transaction holding the
	// account lock. Never trust a match collected before waiting for that lock.
	client := dbent.NewClient(dbent.Driver(entsql.NewDriver(dialect.Postgres, entsql.Conn{ExecQuerier: tx})))
	matching, err := qualityTemplateAccountsQuery(client, t.AccountFilter).Where(dbaccount.IDEQ(accountID)).Exist(ctx)
	if err != nil {
		return result, err
	}
	if !matching {
		result.Reason = "not_matching"
		return result, nil
	}
	if lockedSnapshot != accountSnapshot {
		result.Reason = "account_changed"
		return result, nil
	}
	if priorPlanID.Valid || lockedBindingVersion != bindingVersion.String {
		result.Reason = "not_excluded"
		return result, nil
	}
	if prepareReason != "" {
		result.Reason = prepareReason
		return result, nil
	}
	if plan == nil || plan.AccountID != accountID {
		return result, nil
	}
	var scopeTaken bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM scheduled_test_plans WHERE account_id=$1 AND pelican_config->'quality' IS NOT NULL
		AND (CASE WHEN pelican_config->'quality'->>'action_on'='passed' THEN 'recovery' WHEN pelican_config->'quality'->>'action'='enable_bps' THEN 'bps' WHEN pelican_config->'quality'->>'action'='observe_only' THEN 'observation' ELSE 'quarantine' END)
		  = (CASE WHEN $3::text='passed' THEN 'recovery' WHEN $2::text='enable_bps' THEN 'bps' WHEN $2::text='observe_only' THEN 'observation' ELSE 'quarantine' END)
	)`, accountID, plan.PelicanConfig.Quality.Action, plan.PelicanConfig.Quality.ActionOn).Scan(&scopeTaken)
	if err != nil {
		return result, err
	}
	if scopeTaken {
		result.Reason = "rule_exists"
		return result, nil
	}
	// The unique index is the final fence against ordinary plan creation, which
	// need not acquire this account lock. A conflict must roll back, keeping NULL.
	created, err := insertScheduledTestPlan(ctx, tx, plan)
	if isUniqueViolation(err) {
		result.Reason = "rule_exists"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE quality_rule_template_accounts SET plan_id=$3 WHERE template_id=$1 AND account_id=$2 AND plan_id IS NULL`, t.ID, accountID, created.ID)
	if err != nil {
		return result, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return result, err
	} else if n != 1 {
		result.Reason = "not_excluded"
		return result, nil
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	result.Status, result.Reason = "created", ""
	return result, nil
}

// MarkSynced leaves updated_at alone so it never invalidates a concurrent sync.
func (r *qualityRuleTemplateRepository) MarkSynced(ctx context.Context, id int64, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE quality_rule_templates SET last_synced_at = $2 WHERE id = $1`, id, at)
	return err
}

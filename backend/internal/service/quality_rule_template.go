package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// ErrQualityTemplateSkipped means an account was not linked this round: it is already
// linked, the template changed since it was read,
// or the account is gone. Nothing is recorded, so the next sync looks at it again.
var ErrQualityTemplateSkipped = errors.New("quality rule template account skipped")

var ErrQualityTemplateConflict = errors.New("quality rule template identity conflict")

var ErrQualityTemplateNotFound = errors.New("quality rule template not found")

var errQualityTemplatesUnavailable = errors.New("quality rule templates are not configured")

// QualityRuleAccountFilter mirrors the quality-ops account selector. Group is "" (all
// groups), "ungrouped" or a group id; any listed status matches (none = every status).
type QualityRuleAccountFilter struct {
	Group    string   `json:"group,omitempty"`
	Type     string   `json:"type,omitempty"`
	Search   string   `json:"search,omitempty"`
	Statuses []string `json:"statuses,omitempty"`
}

// QualityTemplateDiagnostics describes current coverage, never an implicit success.
// Reasons contains stable machine codes with affected account counts.
type QualityTemplateDiagnostics struct {
	MatchedAccounts     int            `json:"matched_accounts"`
	ActivePlans         int            `json:"active_plans"`
	SuspendedPlans      int            `json:"suspended_plans"`
	ExcludedAccounts    int            `json:"excluded_accounts"`
	UnsupportedAccounts int            `json:"unsupported_accounts"`
	ConflictingAccounts int            `json:"conflicting_accounts"`
	PendingAccounts     int            `json:"pending_accounts"`
	FailedAccounts      int            `json:"failed_accounts"`
	Status              string         `json:"status"`
	Reasons             map[string]int `json:"reasons"`
}

// QualityRuleTemplate gives each matching account one independently owned plan.
// Source identity persists on the plan even when the template is deleted.
type QualityRuleTemplate struct {
	SyncIssues         map[int64]string            `json:"-"`
	Diagnostics        *QualityTemplateDiagnostics `json:"diagnostics"`
	PelicanConfig      *PelicanTestConfig          `json:"pelican_config"`
	LastSyncedAt       *time.Time                  `json:"last_synced_at"`
	AccountFilter      QualityRuleAccountFilter    `json:"account_filter"`
	PlanIDs            []int64                     `json:"plan_ids"`
	ExcludedAccountIDs []int64                     `json:"excluded_account_ids"`
	ModelID            string                      `json:"model_id"`
	CronExpression     string                      `json:"cron_expression"`
	CreatedAt          time.Time                   `json:"created_at"`
	UpdatedAt          time.Time                   `json:"updated_at"`
	ID                 int64                       `json:"id"`
	MaxResults         int                         `json:"max_results"`
	Enabled            bool                        `json:"enabled"`
}

// QualityTemplateAccount is the account data a template sync needs.
type QualityTemplateAccount struct {
	Credentials     map[string]any
	ParentAccountID *int64
	Extra           map[string]any
	Platform        string
	Type            string
	ID              int64
}

// QualityTemplateUpdate reports how an edit reached the template's existing rules.
type QualityTemplateUpdate struct {
	Template         *QualityRuleTemplate             `json:"template"`
	Updated          int                              `json:"updated"`
	Failed           int                              `json:"failed"`
	Created          int                              `json:"created"`
	ReincludeResults []QualityTemplateReincludeResult `json:"reinclude_results,omitempty"`
}

// QualityTemplateReincludeResult uses stable machine codes; callers localize Reason.
// Reinclude failures are independent of Failed, which counts existing-plan updates.
type QualityTemplateReincludeResult struct {
	AccountID int64  `json:"account_id"`
	Status    string `json:"status"` // created, already_covered, failed
	Reason    string `json:"reason,omitempty"`
}

// QualityRuleTemplateReincludeRepository is optional so existing repository adapters
// retain ordinary update/sync behavior. prepare runs without a transaction or row
// locks: model discovery may read or update the account on another connection.
// Creation then locks and rechecks the template, account snapshot and NULL binding.
type QualityRuleTemplateReincludeRepository interface {
	ReincludeAccount(ctx context.Context, template *QualityRuleTemplate, accountID int64,
		prepare func(context.Context, QualityTemplateAccount) (*ScheduledTestPlan, string)) (QualityTemplateReincludeResult, error)
}

// Template-owned propagation must recheck this saved version after any slow discovery.
// Older adapters may retain their original Update path.
type QualityTemplatePlanPropagationRepository interface {
	UpdateLinkedPlan(context.Context, *QualityRuleTemplate, *ScheduledTestPlan) (*ScheduledTestPlan, error)
}

type QualityTemplateDiagnosticsRepository interface {
	TemplateDiagnostics(context.Context, *QualityRuleTemplate) (*QualityTemplateDiagnostics, error)
}

type QualityTemplateSyncIssuesRepository interface {
	SaveSyncIssues(context.Context, *QualityRuleTemplate, time.Time, map[int64]string) error
}

type QualityRuleTemplateMembershipRepository interface {
	SyncMembership(context.Context, *QualityRuleTemplate) error
}

const MaxQualityTemplateReincludeAccounts = 1000

func validateQualityTemplateReincludeIDs(ids []int64) error {
	if len(ids) > MaxQualityTemplateReincludeAccounts {
		return fmt.Errorf("reinclude_account_ids must contain at most %d entries", MaxQualityTemplateReincludeAccounts)
	}
	for _, id := range ids {
		if id <= 0 {
			return fmt.Errorf("reinclude_account_ids must contain positive account IDs")
		}
	}
	return nil
}

// QualityRuleTemplateRepository stores group rules and the accounts each one has covered.
type QualityRuleTemplateRepository interface {
	// Presets are configuration snapshots in separate storage, never sync inputs.
	ListPresets(ctx context.Context) ([]*QualityRulePreset, error)
	CreatePreset(ctx context.Context, preset *QualityRulePreset) (*QualityRulePreset, error)
	List(ctx context.Context) ([]*QualityRuleTemplate, error)
	Get(ctx context.Context, id int64) (*QualityRuleTemplate, error)
	Create(ctx context.Context, template *QualityRuleTemplate) (*QualityRuleTemplate, error)
	Update(ctx context.Context, template *QualityRuleTemplate) (*QualityRuleTemplate, error)
	// Delete removes the template and returns the rules it had created that still exist.
	Delete(ctx context.Context, id int64) ([]int64, error)
	// MatchingAccounts uses the same predicates as the admin account list.
	MatchingAccounts(ctx context.Context, filter QualityRuleAccountFilter) ([]QualityTemplateAccount, error)
	LinkedAccountIDs(ctx context.Context, id int64) (map[int64]struct{}, error)
	// CreateLinkedPlan records the account link and creates its plan in one transaction,
	// or returns ErrQualityTemplateSkipped.
	CreateLinkedPlan(ctx context.Context, template *QualityRuleTemplate, plan *ScheduledTestPlan) (*ScheduledTestPlan, error)
	MarkSynced(ctx context.Context, id int64, at time.Time) error
}

var qualityTemplateStatuses = map[string]struct{}{
	StatusActive: {}, StatusDisabled: {}, StatusError: {}, "inactive": {},
	"rate_limited": {}, "temp_unschedulable": {}, "unschedulable": {},
}

func normalizeQualityRuleAccountFilter(filter QualityRuleAccountFilter) (QualityRuleAccountFilter, error) {
	out := QualityRuleAccountFilter{Group: strings.TrimSpace(filter.Group), Type: strings.TrimSpace(filter.Type), Search: strings.TrimSpace(filter.Search)}
	if out.Group != "" && out.Group != "ungrouped" {
		id, err := strconv.ParseInt(out.Group, 10, 64)
		if err != nil || id <= 0 {
			return out, fmt.Errorf("invalid group filter")
		}
		out.Group = strconv.FormatInt(id, 10)
	}
	if len(out.Type) > 50 {
		return out, fmt.Errorf("invalid account type filter")
	}
	if utf8.RuneCountInString(out.Search) > 100 {
		return out, fmt.Errorf("account search must be at most 100 characters")
	}
	seen := make(map[string]struct{}, len(filter.Statuses))
	for _, status := range filter.Statuses {
		status = strings.TrimSpace(status)
		if _, ok := qualityTemplateStatuses[status]; !ok {
			return out, fmt.Errorf("invalid account status filter")
		}
		if _, dup := seen[status]; !dup {
			seen[status] = struct{}{}
			out.Statuses = append(out.Statuses, status)
		}
	}
	return out, nil
}

// QualityTemplateGroupID converts the filter group to the account list's group argument.
func QualityTemplateGroupID(filter QualityRuleAccountFilter) int64 {
	switch filter.Group {
	case "":
		return 0
	case "ungrouped":
		return AccountListGroupUngrouped
	}
	id, _ := strconv.ParseInt(filter.Group, 10, 64)
	return id
}

func qualityRuleScope(cfg *PelicanTestConfig) string {
	if cfg == nil || cfg.Quality == nil {
		return ""
	}
	if cfg.Quality.RunsOnPassed() {
		return "recovery"
	}
	switch cfg.Quality.Action {
	case QualityActionEnableBPS:
		return "bps"
	case QualityActionObserveOnly:
		return "observation"
	}
	return "quarantine"
}

// Same rule as the quality-ops account picker: probes only run on real OpenAI OAuth accounts.
func qualityProbeSupportsAccount(account QualityTemplateAccount, configs ...*PelicanTestConfig) bool {
	if account.Platform != PlatformOpenAI || (account.Type != AccountTypeOAuth && account.Type != AccountTypeSetupToken) {
		return false
	}
	synthetic, _ := account.Extra["synthetic_ui_test"].(bool)
	if synthetic {
		return false
	}
	cfg := &PelicanTestConfig{QuestionKind: OpenAICodexStateProbeQuestionKind}
	if len(configs) > 0 {
		cfg = configs[0]
	}
	return QualityStateProbeMatchesAccount(&Account{Platform: account.Platform, Type: account.Type,
		Credentials: account.Credentials, Extra: account.Extra, ParentAccountID: account.ParentAccountID}, cfg)
}

func qualityTemplateSupportsAccount(account QualityTemplateAccount, cfg *PelicanTestConfig) bool {
	if isOpenAICodexStateProbePlan(cfg) {
		return qualityProbeSupportsAccount(account, cfg)
	}
	return QualityProbeMatchesAccount(&Account{Platform: account.Platform, Type: account.Type,
		Credentials: account.Credentials, Extra: account.Extra, ParentAccountID: account.ParentAccountID}, cfg)
}

// Every plan gets its own copy: validation writes the model into the config.
func cloneQualityPelicanConfig(cfg *PelicanTestConfig) *PelicanTestConfig {
	if cfg == nil {
		return nil
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil
	}
	var out PelicanTestConfig
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return &out
}

func (t *QualityRuleTemplate) planFor(accountID int64) *ScheduledTestPlan {
	return &ScheduledTestPlan{
		AccountID:        accountID,
		SourceTemplateID: t.ID,
		ModelID:          t.ModelID,
		CronExpression:   t.CronExpression,
		Enabled:          t.Enabled,
		MaxResults:       t.MaxResults,
		PelicanConfig:    cloneQualityPelicanConfig(t.PelicanConfig),
	}
}

// prepareQualityTemplate validates the template exactly as a plan created from it would be.
func prepareQualityTemplate(t *QualityRuleTemplate) error {
	filter, err := normalizeQualityRuleAccountFilter(t.AccountFilter)
	if err != nil {
		return err
	}
	t.AccountFilter = filter
	if t.PelicanConfig == nil || t.PelicanConfig.Quality == nil || t.PelicanConfig.Quality.Action == QualityActionOpenAITwoFAAdmission {
		return fmt.Errorf("group rules must be quality rules")
	}
	config, err := reusableQualityConfig(t.PelicanConfig)
	if err != nil {
		return err
	}
	config.RuleName = t.PelicanConfig.RuleName
	t.PelicanConfig = config
	sample := t.planFor(0)
	if _, err := nextPlanRun(sample, time.Now()); err != nil {
		return fmt.Errorf("invalid test schedule: %w", err)
	}
	t.ModelID = sample.ModelID
	t.MaxResults = sample.MaxResults
	t.PelicanConfig = sample.PelicanConfig
	return nil
}

func (s *ScheduledTestService) ListQualityTemplates(ctx context.Context) ([]*QualityRuleTemplate, error) {
	if s.templateRepo == nil {
		return []*QualityRuleTemplate{}, nil
	}
	templates, err := s.templateRepo.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range templates {
		s.attachQualityTemplateDiagnostics(ctx, t)
	}
	return templates, nil
}

func (s *ScheduledTestService) GetQualityTemplate(ctx context.Context, id int64) (*QualityRuleTemplate, error) {
	if s.templateRepo == nil {
		return nil, errQualityTemplatesUnavailable
	}
	t, err := s.templateRepo.Get(ctx, id)
	if err == nil {
		s.attachQualityTemplateDiagnostics(ctx, t)
	}
	return t, err
}

// CreateQualityTemplate saves the group rule and immediately covers the accounts that
// match now. It returns how many rules were created.
func (s *ScheduledTestService) CreateQualityTemplate(ctx context.Context, t *QualityRuleTemplate) (*QualityRuleTemplate, int, error) {
	if s.templateRepo == nil {
		return nil, 0, errQualityTemplatesUnavailable
	}
	if err := prepareQualityTemplate(t); err != nil {
		return nil, 0, err
	}
	created, err := s.templateRepo.Create(ctx, t)
	if err != nil {
		return nil, 0, err
	}
	count := 0
	if created.Enabled {
		if count, err = s.syncQualityTemplate(ctx, created); err != nil {
			// The runner retries every minute; the template itself is saved.
			logger.LegacyPrintf("service.scheduled_test", "quality template %d initial sync: %v", created.ID, err)
		}
	}
	return s.reloadQualityTemplate(ctx, created), count, nil
}

// UpdateQualityTemplate saves the template and applies the same settings to every rule it
// created (pausing the template pauses them). Membership follows the current filter
// in both directions, while temporarily unmatched plans keep their history.
func (s *ScheduledTestService) UpdateQualityTemplate(ctx context.Context, t *QualityRuleTemplate, reincludeAccountIDs ...int64) (*QualityTemplateUpdate, error) {
	// Reject the whole request before even saving the template or updating its plans.
	if err := validateQualityTemplateReincludeIDs(reincludeAccountIDs); err != nil {
		return nil, err
	}
	if s.templateRepo == nil {
		return nil, errQualityTemplatesUnavailable
	}
	if err := prepareQualityTemplate(t); err != nil {
		return nil, err
	}
	saved, err := s.templateRepo.Update(ctx, t)
	if err != nil {
		return nil, err
	}
	reincludeVersion := saved
	// Re-read after the update commits so rules created by a concurrent sync are included.
	saved = s.reloadQualityTemplate(ctx, saved)
	result := &QualityTemplateUpdate{}
	saved.SyncIssues = make(map[int64]string)
	for _, planID := range saved.PlanIDs {
		plan, err := s.planRepo.GetByID(ctx, planID)
		if err != nil {
			result.Failed++
			continue
		}
		plan.ModelID = saved.ModelID
		plan.CronExpression = saved.CronExpression
		plan.MaxResults = saved.MaxResults
		plan.Enabled = saved.Enabled
		plan.PelicanConfig = cloneQualityPelicanConfig(saved.PelicanConfig)
		if !saved.Enabled {
			// Configuration can propagate while paused without model discovery. Existing
			// ownership must not be restored or discarded by this configuration save.
			plan.PreserveQualityState = true
		} else {
			supported, supportErr := s.filterQualityTemplatePlan(ctx, plan)
			if supportErr != nil {
				saved.SyncIssues[plan.AccountID] = "model_check_failed"
				result.Failed++
				continue
			}
			if !supported {
				saved.SyncIssues[plan.AccountID] = "unsupported_models"
				plan.Enabled = false
			}
			// A template may have no supported models for this linked account.
			// Keep it paused rather than falsely classifying or deleting the rule.
			next, validationErr := nextPlanRun(plan, time.Now())
			if validationErr != nil {
				saved.SyncIssues[plan.AccountID] = "invalid_schedule"
				result.Failed++
				continue
			}
			plan.NextRunAt = &next
		}
		var updateErr error
		if repo, ok := s.templateRepo.(QualityTemplatePlanPropagationRepository); ok {
			_, updateErr = repo.UpdateLinkedPlan(ctx, saved, plan)
		} else {
			_, updateErr = s.planRepo.Update(ctx, plan)
		}
		if updateErr != nil {
			saved.SyncIssues[plan.AccountID] = "plan_update_failed"
			logger.LegacyPrintf("service.scheduled_test", "quality template %d update plan %d: %v", saved.ID, planID, updateErr)
			result.Failed++
			continue
		}
		result.Updated++
	}
	seen := make(map[int64]struct{}, len(reincludeAccountIDs))
	for _, accountID := range reincludeAccountIDs {
		if _, duplicate := seen[accountID]; duplicate {
			continue
		}
		seen[accountID] = struct{}{}
		entry := s.reincludeQualityTemplateAccount(ctx, reincludeVersion, accountID)
		result.ReincludeResults = append(result.ReincludeResults, entry)
		if entry.Status == "created" {
			result.Created++
		}
	}
	if saved.Enabled {
		created, syncErr := s.syncQualityTemplate(ctx, saved)
		result.Created += created
		if syncErr != nil {
			logger.LegacyPrintf("service.scheduled_test", "quality template %d sync after edit: %v", saved.ID, syncErr)
		}
	}
	if !saved.Enabled {
		if repo, ok := s.templateRepo.(QualityTemplateSyncIssuesRepository); ok {
			if err := repo.SaveSyncIssues(ctx, saved, time.Now(), saved.SyncIssues); err != nil {
				result.Failed++
			}
		}
	}
	result.Template = s.reloadQualityTemplate(ctx, saved)
	return result, nil
}

func (s *ScheduledTestService) reincludeQualityTemplateAccount(ctx context.Context, t *QualityRuleTemplate, accountID int64) QualityTemplateReincludeResult {
	failed := QualityTemplateReincludeResult{AccountID: accountID, Status: "failed", Reason: "create_failed"}
	if !t.Enabled {
		failed.Reason = "template_disabled"
		return failed
	}
	repo, ok := s.templateRepo.(QualityRuleTemplateReincludeRepository)
	if !ok {
		return failed
	}
	entry, err := repo.ReincludeAccount(ctx, t, accountID, func(ctx context.Context, account QualityTemplateAccount) (*ScheduledTestPlan, string) {
		if !qualityTemplateSupportsAccount(account, t.PelicanConfig) {
			return nil, "unsupported_account"
		}
		plan := t.planFor(accountID)
		supported, err := s.filterQualityTemplatePlan(ctx, plan)
		if err != nil {
			return nil, "model_check_failed"
		}
		if !supported {
			return nil, "unsupported_models"
		}
		next, err := nextPlanRun(plan, time.Now())
		if err != nil {
			return nil, "create_failed"
		}
		plan.NextRunAt = &next
		return plan, ""
	})
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test", "quality template %d reinclude account %d: %v", t.ID, accountID, err)
		return failed
	}
	return entry
}

// DeleteQualityTemplate stops following the filter. With deletePlans the rules it created
// are deleted too; otherwise they run independently and retain immutable provenance.
func (s *ScheduledTestService) DeleteQualityTemplate(ctx context.Context, id int64, deletePlans bool) (int, error) {
	if s.templateRepo == nil {
		return 0, errQualityTemplatesUnavailable
	}
	planIDs, err := s.templateRepo.Delete(ctx, id)
	if err != nil || !deletePlans {
		return 0, err
	}
	deleted := 0
	for _, planID := range planIDs {
		if err := s.planRepo.Delete(ctx, planID); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// SyncQualityTemplates gives every enabled group rule to accounts that newly match it.
func (s *ScheduledTestService) SyncQualityTemplates(ctx context.Context) (int, error) {
	if s.templateRepo == nil {
		return 0, nil
	}
	templates, err := s.templateRepo.List(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, t := range templates {
		if !t.Enabled {
			if repo, ok := s.templateRepo.(QualityRuleTemplateMembershipRepository); ok {
				if err := repo.SyncMembership(ctx, t); err != nil {
					return total, err
				}
			}
			continue
		}
		created, err := s.syncQualityTemplate(ctx, t)
		total += created
		if err != nil {
			logger.LegacyPrintf("service.scheduled_test", "quality template %d sync: %v", t.ID, err)
		}
	}
	return total, nil
}

// Coverage is isolated by template identity. Binding tombstones still prevent
// automatically recreating a plan deliberately removed by an administrator.
func (s *ScheduledTestService) syncQualityTemplate(ctx context.Context, t *QualityRuleTemplate) (int, error) {
	if repo, ok := s.templateRepo.(QualityRuleTemplateMembershipRepository); ok {
		if err := repo.SyncMembership(ctx, t); err != nil {
			return 0, err
		}
	}
	accounts, err := s.templateRepo.MatchingAccounts(ctx, t.AccountFilter)
	if err != nil {
		return 0, err
	}
	linked, err := s.templateRepo.LinkedAccountIDs(ctx, t.ID)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	issues := make(map[int64]string)
	for accountID, reason := range t.SyncIssues {
		if _, ok := linked[accountID]; ok {
			issues[accountID] = reason
		}
	}
	created := 0
	var firstErr error
	for _, account := range accounts {
		if _, ok := linked[account.ID]; ok {
			continue
		}
		if !qualityTemplateSupportsAccount(account, t.PelicanConfig) {
			issues[account.ID] = "unsupported_account"
			continue
		}
		plan := t.planFor(account.ID)
		supported, supportErr := s.filterQualityTemplatePlan(ctx, plan)
		if supportErr != nil {
			issues[account.ID] = "model_check_failed"
			if firstErr == nil {
				firstErr = supportErr
			}
			continue
		}
		if !supported {
			issues[account.ID] = "unsupported_models"
			continue
		}
		next, err := nextPlanRun(plan, now)
		if err != nil {
			issues[account.ID] = "invalid_schedule"
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		plan.NextRunAt = &next
		if _, err := s.templateRepo.CreateLinkedPlan(ctx, t, plan); err != nil {
			switch {
			case errors.Is(err, ErrQualityTemplateConflict):
				issues[account.ID] = "identity_conflict"
			case errors.Is(err, ErrQualityTemplateSkipped):
				issues[account.ID] = "concurrent_change"
			default:
				issues[account.ID] = "create_failed"
				if firstErr == nil {
					firstErr = err
				}
			}
			continue
		}
		created++
	}
	t.SyncIssues = issues
	if repo, ok := s.templateRepo.(QualityTemplateSyncIssuesRepository); ok {
		err = repo.SaveSyncIssues(ctx, t, now, issues)
	} else {
		err = s.templateRepo.MarkSynced(ctx, t.ID, now)
	}
	if err != nil && firstErr == nil {
		firstErr = err
	}
	return created, firstErr
}

func (s *ScheduledTestService) reloadQualityTemplate(ctx context.Context, t *QualityRuleTemplate) *QualityRuleTemplate {
	if fresh, err := s.templateRepo.Get(ctx, t.ID); err == nil {
		if fresh.SyncIssues == nil {
			fresh.SyncIssues = t.SyncIssues
		}
		t = fresh
	}
	s.attachQualityTemplateDiagnostics(ctx, t)
	return t
}

func (s *ScheduledTestService) attachQualityTemplateDiagnostics(ctx context.Context, t *QualityRuleTemplate) {
	if repo, ok := s.templateRepo.(QualityTemplateDiagnosticsRepository); ok {
		diagnostics, err := repo.TemplateDiagnostics(ctx, t)
		if err == nil {
			t.Diagnostics = diagnostics
			return
		}
		t.Diagnostics = &QualityTemplateDiagnostics{Status: "error", Reasons: map[string]int{"diagnostics_unavailable": 1}}
		return
	}
	// Repository adapters may omit the optional batch diagnostic capability.
	t.Diagnostics = &QualityTemplateDiagnostics{Status: "unknown", Reasons: map[string]int{"diagnostics_unavailable": 1}}
}

// DescribeQualityTemplate uses current local membership plus the last sync's
// per-account reasons. Missing model discovery remains pending until the sync;
// viewing a template never contacts upstream accounts.
func DescribeQualityTemplate(t *QualityRuleTemplate, accounts []QualityTemplateAccount, plans []*ScheduledTestPlan) *QualityTemplateDiagnostics {
	d := &QualityTemplateDiagnostics{MatchedAccounts: len(accounts), ExcludedAccounts: len(t.ExcludedAccountIDs), Reasons: map[string]int{}}
	matching := make(map[int64]bool, len(accounts))
	excluded := make(map[int64]bool, len(t.ExcludedAccountIDs))
	covered := make(map[int64]bool, len(plans))
	for _, a := range accounts {
		matching[a.ID] = true
	}
	for _, id := range t.ExcludedAccountIDs {
		excluded[id] = true
	}
	addIssue := func(reason string) {
		d.Reasons[reason]++
		switch reason {
		case "unsupported_account", "unsupported_models":
			d.UnsupportedAccounts++
		case "identity_conflict":
			d.ConflictingAccounts++
		case "model_check_failed", "create_failed", "plan_update_failed", "invalid_schedule":
			d.FailedAccounts++
		default:
			d.PendingAccounts++
		}
	}
	for _, p := range plans {
		covered[p.AccountID] = true
		if t.Enabled && p.Enabled && !p.ScopeSuspended && matching[p.AccountID] {
			d.ActivePlans++
		} else {
			d.SuspendedPlans++
			if !matching[p.AccountID] || p.ScopeSuspended {
				d.Reasons["scope_suspended"]++
			} else if !p.Enabled {
				d.Reasons["plan_disabled"]++
			}
		}
		if reason := t.SyncIssues[p.AccountID]; reason != "" && matching[p.AccountID] && (!p.Enabled || reason == "plan_update_failed") {
			addIssue(reason)
		}
	}
	for _, a := range accounts {
		if covered[a.ID] || excluded[a.ID] {
			continue
		}
		if !qualityTemplateSupportsAccount(a, t.PelicanConfig) {
			addIssue("unsupported_account")
			continue
		}
		reason := t.SyncIssues[a.ID]
		if reason == "" {
			reason = "awaiting_sync"
		}
		addIssue(reason)
	}
	switch {
	case !t.Enabled:
		d.Status = "disabled"
		d.Reasons["template_disabled"] = 1
	case d.ActivePlans == 0:
		d.Status = "zero"
	case d.PendingAccounts+d.FailedAccounts+d.ConflictingAccounts+d.UnsupportedAccounts > 0:
		d.Status = "partial"
	default:
		d.Status = "ready"
	}
	if d.ExcludedAccounts > 0 {
		d.Reasons["excluded_accounts"] = d.ExcludedAccounts
	}
	if d.MatchedAccounts == 0 {
		d.Reasons["no_matching_accounts"] = 1
	}
	return d
}

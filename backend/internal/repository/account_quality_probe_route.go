package repository

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *scheduledTestPlanRepository) QualityProbeRouteMatches(ctx context.Context, plan *service.ScheduledTestPlan) (bool, error) {
	if plan.PelicanConfig == nil || plan.PelicanConfig.QuestionKind != service.OpenAICodexStateProbeQuestionKind && plan.PelicanConfig.TestChannel != "bps" {
		return true, nil
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	state, err := readQualityState(ctx, tx, plan.ID)
	if err != nil {
		return false, err
	}
	return qualityProbeRouteMatchesTx(ctx, tx, plan, &state)
}

// Call under the account lock when authorizing a claim or account mutation.
// Only persisted ownership may exempt a native recovery probe from BPS isolation.
func qualityProbeRouteMatchesTx(ctx context.Context, tx *sql.Tx, plan *service.ScheduledTestPlan, state *qualityState) (bool, error) {
	if plan.PelicanConfig == nil || plan.PelicanConfig.QuestionKind != service.OpenAICodexStateProbeQuestionKind && plan.PelicanConfig.TestChannel != "bps" {
		return true, nil
	}
	account := &service.Account{}
	var credentials, extra []byte
	var parent sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT platform,type,credentials,extra,parent_account_id FROM accounts WHERE id=$1 AND deleted_at IS NULL`, plan.AccountID).
		Scan(&account.Platform, &account.Type, &credentials, &extra, &parent)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(credentials) > 0 {
		if err = json.Unmarshal(credentials, &account.Credentials); err != nil {
			return false, err
		}
	}
	if len(extra) > 0 {
		if err = json.Unmarshal(extra, &account.Extra); err != nil {
			return false, err
		}
	}
	if parent.Valid {
		account.ParentAccountID = &parent.Int64
	}
	cfg := *plan.PelicanConfig
	if cfg.ModelID == "" {
		cfg.ModelID = plan.ModelID
	}
	cfg.BPSRecoveryPending = state != nil && state.Action == service.QualityActionEnableBPS
	if service.QualityProbeRequiresBPS(&cfg) {
		for model, mapped := range plan.QualityProbeModels {
			if mapped == "" || account.GetMappedModel(model) != mapped {
				return false, nil
			}
		}
	}
	return service.QualityProbeMatchesAccount(account, &cfg), nil
}

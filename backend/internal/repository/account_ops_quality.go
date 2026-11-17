package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// A notification is best effort, but a successful one must share the quality
// action's transaction. PostgreSQL errors abort a transaction until a savepoint
// is restored, so isolate the optional settings/queue work from that action.
func persistQualityDegradationNotificationTx(ctx context.Context, tx *sql.Tx, plan *service.ScheduledTestPlan, outcome string) error {
	event := service.QualityDegradationEvent(plan, outcome)
	if event == nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT quality_degradation_notification`); err != nil {
		return err
	}
	if err := recordQualityDegradationNotificationTx(ctx, tx, *event); err != nil {
		if _, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT quality_degradation_notification`); rollbackErr != nil {
			return rollbackErr
		}
		// Database errors may contain notification credentials or raw settings.
		// Keep diagnostics useful without including the error or event contents.
		slog.WarnContext(ctx, "quality degradation notification could not be queued", "account_id", plan.AccountID, "plan_id", plan.ID)
	}
	_, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT quality_degradation_notification`)
	return err
}

func recordQualityDegradationNotificationTx(ctx context.Context, tx *sql.Tx, event service.AccountOpsEvent) error {
	var raw string
	// Saving notification settings updates this row before suppressing queued
	// work. A shared lock ensures a concurrent disable also sees this event.
	err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1 FOR SHARE`, "account_ops_notifications_v1").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var stored struct {
		service.AccountOpsConfig
		Webhooks []struct {
			Provider    string `json:"provider"`
			Enabled     bool   `json:"enabled"`
			URLCipher   string `json:"url_cipher"`
			TokenCipher string `json:"token_cipher"`
		} `json:"encrypted_webhooks"`
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return err
	}
	cfg := stored.AccountOpsConfig
	// Persisted robot destinations use encrypted_webhooks. Ignore public
	// configured flags and plaintext fields; no credential decryption is needed.
	cfg.Webhooks = nil
	for _, webhook := range stored.Webhooks {
		cfg.Webhooks = append(cfg.Webhooks, service.AccountOpsWebhook{
			Provider: webhook.Provider, Enabled: webhook.Enabled,
			URLConfigured: webhook.URLCipher != "", TokenConfigured: webhook.TokenCipher != "",
		})
	}
	if !cfg.Allows("quality_degraded") {
		return nil
	}
	// Due-plan and batch snapshots may omit AccountName. Read the current name
	// only after notification admission, without adding work to disabled probes.
	err = tx.QueryRowContext(ctx, `SELECT LEFT(name,120) FROM accounts WHERE id=$1 AND deleted_at IS NULL`, event.AccountID).Scan(&event.AccountName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return recordAccountOpsEvent(ctx, tx, event)
}

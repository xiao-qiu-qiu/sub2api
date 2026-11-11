//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestQualityProbeAdmissionEffectiveThresholds(t *testing.T) {
	now := admissionNow(t)
	for _, tc := range []struct {
		name, extra string
		ops         string
		want        bool
	}{
		{"below", `{"auto_pause_7d_threshold":0.98}`, `{}`, false},
		{"account90", `{"auto_pause_7d_threshold":0.9}`, `{}`, true},
		{"global90", `{}`, `{"openai_account_quota_auto_pause":{"default_threshold_7d":0.9}}`, true},
		{"disabled", `{"auto_pause_7d_disabled":"true","auto_pause_7d_threshold":0.9}`, `{}`, false},
		{"numeric_disabled", `{"auto_pause_7d_disabled":2,"auto_pause_7d_threshold":0.9}`, `{}`, false},
		{"numeric_string_is_not_bool", `{"auto_pause_7d_disabled":"2","auto_pause_7d_threshold":0.9}`, `{}`, true},
		{"reset90", `{"auto_reset_credit_enabled":true,"auto_reset_credit_7d_threshold":0.9}`, `{}`, true},
		{"reset_ignored", `{"auto_reset_credit_enabled":true,"auto_reset_credit_7d_threshold":0,"auto_pause_7d_threshold":0.9}`, `{}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var paused bool
			require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT quality_probe_admission_codex_paused('{}',$1::jsonb,'7d',95,false,$2,$3::jsonb,'{}')`, tc.extra, now, tc.ops).Scan(&paused))
			require.Equal(t, tc.want, paused)
		})
	}
	extra := map[string]any{"auto_pause_7d_threshold": 0.9, "auto_reset_credit_enabled": true, "codex_auto_reset_credit_state": map[string]any{"status": "available", "available_count": 1, "checked_at": now.Add(-8 * time.Minute).Format(time.RFC3339Nano)}}
	raw, err := json.Marshal(extra)
	require.NoError(t, err)
	var paused bool
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT quality_probe_admission_codex_paused('{}',$1::jsonb,'7d',95,false,$2,'{}','{}')`, string(raw), now).Scan(&paused))
	require.False(t, paused, "credit freshness matches the service's ten minute TTL")
}

func TestQualityProbeAdmissionThresholdAndStaleSnapshotTransitions(t *testing.T) {
	for _, mode := range []string{"threshold_reset", "below_threshold", "disabled", "stale", "future_reset_wins", "still_paused", "early_zero", "untrusted"} {
		t.Run(mode, func(t *testing.T) {
			f := newQualityProbeAdmissionFixture(t)
			now := admissionNow(t)
			reset := now.Add(-time.Minute)
			observed := now.Add(-3 * time.Hour)
			extra := map[string]any{"codex_7d_used_percent": 95, "codex_7d_window_minutes": 10080, "codex_7d_reset_at": reset.Format(time.RFC3339Nano), "codex_usage_updated_at": observed.Format(time.RFC3339Nano), "auto_pause_7d_threshold": 0.9}
			switch mode {
			case "below_threshold":
				extra["auto_pause_7d_threshold"] = 0.98
			case "disabled":
				extra["auto_pause_7d_disabled"] = true
			case "stale":
				delete(extra, "codex_7d_reset_at")
				reset = observed.Add(2 * time.Hour)
			case "future_reset_wins", "still_paused", "early_zero":
				extra["codex_7d_reset_at"] = now.Add(7 * 24 * time.Hour).Format(time.RFC3339Nano)
			case "untrusted":
				extra["email"] = "foreign@example.invalid"
				f.exec(`UPDATE accounts SET credentials='{"email":"owner@example.invalid"}' WHERE id=$1`)
			}
			raw, err := json.Marshal(extra)
			require.NoError(t, err)
			f.exec(`UPDATE accounts SET extra=extra||$2::jsonb WHERE id=$1`, string(raw))
			f.exec(`UPDATE account_quality_probe_admissions SET required_after=NULL WHERE account_id=$1`)
			fresh := admissionNow(t)
			extra["codex_usage_updated_at"] = fresh.Format(time.RFC3339Nano)
			if mode == "still_paused" || mode == "future_reset_wins" {
				extra["codex_7d_used_percent"] = 96
			} else {
				extra["codex_7d_used_percent"] = 0
			}
			raw, err = json.Marshal(extra)
			require.NoError(t, err)
			f.exec(`UPDATE accounts SET extra=extra||$2::jsonb WHERE id=$1`, string(raw))
			state := admissionSnapshot(t, f.account)
			switch mode {
			case "below_threshold", "disabled", "future_reset_wins", "still_paused", "untrusted":
				require.NotContains(t, state, "required_after")
			case "early_zero":
				require.Equal(t, fresh, admissionTime(t, state, "required_after"))
			default:
				require.Equal(t, reset, admissionTime(t, state, "required_after"))
			}
		})
	}
}

func TestQualityProbeAdmissionGlobalPolicyRelease(t *testing.T) {
	var original sql.NullString
	err := integrationDB.QueryRowContext(t.Context(), `SELECT value FROM settings WHERE key='ops_advanced_settings'`).Scan(&original)
	require.True(t, err == nil || err == sql.ErrNoRows)
	t.Cleanup(func() {
		if original.Valid {
			_, _ = integrationDB.ExecContext(context.Background(), `UPDATE settings SET value=$1 WHERE key='ops_advanced_settings'`, original.String)
		} else {
			_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM settings WHERE key='ops_advanced_settings'`)
		}
	})
	_, err = integrationDB.ExecContext(t.Context(), `INSERT INTO settings(key,value) VALUES('ops_advanced_settings','{"openai_account_quota_auto_pause":{"default_threshold_7d":0.9}}') ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`)
	require.NoError(t, err)
	f := newQualityProbeAdmissionFixture(t)
	now := admissionNow(t)
	f.exec(`UPDATE accounts SET extra=extra||jsonb_build_object('codex_7d_used_percent',95,'codex_7d_reset_at',$2::text,'codex_usage_updated_at',$3::text) WHERE id=$1`, now.Add(7*24*time.Hour).Format(time.RFC3339Nano), now.Add(-time.Hour).Format(time.RFC3339Nano))
	f.exec(`UPDATE account_quality_probe_admissions SET required_after=NULL WHERE account_id=$1`)
	require.NotContains(t, admissionSnapshot(t, f.account), "required_after")
	before := admissionNow(t)
	_, err = integrationDB.ExecContext(t.Context(), `UPDATE settings SET value='{"openai_account_quota_auto_pause":{"default_threshold_7d":0}}' WHERE key='ops_advanced_settings'`)
	require.NoError(t, err)
	boundary := admissionTime(t, admissionSnapshot(t, f.account), "required_after")
	require.False(t, boundary.Before(before))
	require.True(t, boundary.Before(now.Add(time.Minute)))
	require.Positive(t, f.events())
}

func TestQualityProbeAdmissionAllLocalQuotaTypes(t *testing.T) {
	for _, kind := range []string{service.AccountTypeAPIKey, service.AccountTypeBedrock, service.AccountTypeServiceAccount} {
		t.Run(kind, func(t *testing.T) {
			f := newQualityProbeAdmissionFixture(t)
			now := admissionNow(t)
			start := now.Add(-8 * 24 * time.Hour)
			reset := start.Add(7 * 24 * time.Hour)
			f.exec(`UPDATE accounts SET type=$2,extra=extra||jsonb_build_object('quota_weekly_limit',10,'quota_weekly_used',10,'quota_weekly_start',$3::text) WHERE id=$1`, kind, start.Format(time.RFC3339Nano))
			f.exec(`UPDATE account_quality_probe_admissions SET required_after=NULL WHERE account_id=$1`)
			repo := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
			require.NoError(t, repo.ResetQuotaUsedAndClearRateLimitCooldown(t.Context(), f.account))
			require.Equal(t, reset, admissionTime(t, admissionSnapshot(t, f.account), "required_after"))
		})
	}
}

func TestQualityProbeAdmissionObservationChecksTemplateScope(t *testing.T) {
	f := newQualityProbeAdmissionFixture(t)
	admissionRecover(t, f)
	var templateID int64
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `INSERT INTO quality_rule_templates(account_filter,model_id,cron_expression,pelican_config)
 SELECT '{"statuses":["disabled"]}',model_id,cron_expression,pelican_config FROM scheduled_test_plans WHERE id=$1 RETURNING id`, f.plan.ID).Scan(&templateID))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM quality_rule_templates WHERE id=$1`, templateID)
	})
	_, err := integrationDB.ExecContext(t.Context(), `INSERT INTO quality_rule_template_accounts(template_id,account_id,plan_id) VALUES($1,$2,$3)`, templateID, f.account, f.plan.ID)
	require.NoError(t, err)
	f.plan.QualityTemplateID = templateID
	admissionProof(t, f)
	require.Equal(t, "observed", f.apply("passed"))
	require.NotContains(t, admissionSnapshot(t, f.account), "checked_started_at", "an old observation outside current template scope cannot unlock admission")
}

func TestQualityProbeAdmissionUpgradeAndChecksumReplay(t *testing.T) {
	f := newQualityProbeAdmissionFixture(t)
	raw, err := migrations.FS.ReadFile("337_quality_probe_admission.sql")
	require.NoError(t, err)
	script := string(raw)
	start := strings.Index(script, "INSERT INTO account_quality_probe_admissions(account_id, enabled, required_after)\nSELECT DISTINCT")
	require.NotEqual(t, -1, start)
	end := strings.Index(script[start:], ";")
	require.NotEqual(t, -1, end)
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(t.Context(), `DELETE FROM account_quality_probe_admissions WHERE account_id=$1`, f.account)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), script[start:start+end+1])
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	state := admissionSnapshot(t, f.account)
	require.Equal(t, true, state["enabled"])
	require.NotContains(t, state, "required_after", "upgrades do not stop an already stable account")
	repo := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	account, err := repo.GetByID(t.Context(), f.account)
	require.NoError(t, err)
	require.False(t, account.QualityProbeAdmissionPending(admissionNow(t)))
	require.NoError(t, ApplyMigrations(t.Context(), integrationDB), "the new migration participates in checksum replay")
	require.Equal(t, state, admissionSnapshot(t, f.account))
	_, err = integrationDB.ExecContext(t.Context(), `UPDATE account_quality_probe_admissions SET checked_started_at=clock_timestamp(),checked_finished_at=NULL WHERE account_id=$1`, f.account)
	require.ErrorContains(t, err, "check constraint", "a NULL finish must not pass the SQL CHECK")
}

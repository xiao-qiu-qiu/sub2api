package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestQualityProbeRouteMatchesPersistedAccountAndOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, kind, channel, action, owned, credentials, extra string
		models                                                 map[string]string
		want                                                   bool
	}{
		{name: "native_main_excludes_bps", kind: "state_probe", action: "disable_scheduling", extra: `{"openai_excel_bps":true}`},
		{name: "native_ignores_forged_owner", kind: "state_probe", action: "disable_scheduling", extra: `{"openai_excel_bps":true}`},
		{name: "owned_native_recovery", kind: "state_probe", action: "disable_scheduling", owned: "enable_bps", extra: `{"openai_excel_bps":true}`, want: true},
		{name: "explicit_enable_bps", kind: "state_probe", action: "enable_bps", extra: `{"openai_excel_bps":true}`, want: true},
		{name: "pending_grant_is_native", kind: "state_probe", action: "disable_scheduling", extra: `{"openai_excel_bps":true,"openai_excel_bps_authorization_pending":true}`, want: true},
		{name: "free_is_native", kind: "state_probe", action: "disable_scheduling", credentials: `{"plan_type":"free"}`, extra: `{"openai_excel_bps":true}`, want: true},
		{name: "bps_action_enabled", kind: "candy", channel: "bps", action: "move_groups", extra: `{"openai_excel_bps":true}`, want: true},
		{name: "bps_action_native", kind: "candy", channel: "bps", action: "move_groups", extra: `{}`},
		{name: "bps_model_scope_changed", kind: "candy", channel: "bps", action: "move_groups", extra: `{"openai_excel_bps":true,"openai_excel_bps_models":["different"]}`},
		{name: "bps_mapping_changed", kind: "candy", channel: "bps", action: "move_groups", credentials: `{"model_mapping":{"probe":"new-target"}}`, extra: `{"openai_excel_bps":true}`, models: map[string]string{"probe": "old-target"}},
		{name: "bps_mixed_sample_mapping", kind: "candy", channel: "bps", action: "move_groups", extra: `{"openai_excel_bps":true}`, models: map[string]string{"probe": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			if tc.credentials == "" {
				tc.credentials = `{}`
			}
			mock.ExpectBegin()
			tx, err := db.Begin()
			require.NoError(t, err)
			mock.ExpectQuery("SELECT platform,type,credentials,extra,parent_account_id").WithArgs(int64(8)).
				WillReturnRows(sqlmock.NewRows([]string{"platform", "type", "credentials", "extra", "parent"}).AddRow("openai", "oauth", tc.credentials, tc.extra, nil))
			plan := &service.ScheduledTestPlan{AccountID: 8, ModelID: "probe", QualityProbeModels: tc.models, PelicanConfig: &service.PelicanTestConfig{QuestionKind: tc.kind, TestChannel: tc.channel,
				BPSRecoveryPending: true, Quality: &service.QualityPolicy{Action: tc.action}}}
			got, err := qualityProbeRouteMatchesTx(context.Background(), tx, plan, &qualityState{Action: tc.owned})
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
			mock.ExpectRollback()
			require.NoError(t, tx.Rollback())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestQualityApplyOutcomeRejectsAccountRouteChangeUnderLock(t *testing.T) {
	for _, tc := range []struct{ kind, channel, extra string }{
		{"state_probe", "account", `{"openai_excel_bps":true}`},
		{"candy", "bps", `{}`},
		{"candy", "bps", `{"openai_excel_bps":true,"openai_excel_bps_models":["other"]}`},
	} {
		for _, outcome := range []string{"failed", "passed"} {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			plan := &service.ScheduledTestPlan{ID: 7, AccountID: 8, ModelID: "probe", UpdatedAt: time.Now(), PelicanConfig: &service.PelicanTestConfig{QuestionKind: tc.kind, TestChannel: tc.channel, Quality: &service.QualityPolicy{Action: "disable_scheduling"}}}
			until := time.Now().Add(time.Minute)
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT enabled AND updated_at").WillReturnRows(sqlmock.NewRows([]string{"valid"}).AddRow(true))
			mock.ExpectQuery("SELECT state FROM account_quality_states").WillReturnRows(sqlmock.NewRows([]string{"state"}).AddRow(`{}`))
			mock.ExpectQuery("SELECT updated_at, schedulable, status FROM accounts").WillReturnRows(sqlmock.NewRows([]string{"updated_at", "schedulable", "status"}).AddRow(time.Now(), true, "active"))
			mock.ExpectQuery("SELECT platform,type,credentials,extra,parent_account_id").WillReturnRows(sqlmock.NewRows([]string{"platform", "type", "credentials", "extra", "parent"}).AddRow("openai", "oauth", `{}`, tc.extra, nil))
			mock.ExpectRollback()
			action, err := (&scheduledTestPlanRepository{db: db}).ApplyQualityOutcome(context.Background(), plan, until, outcome)
			require.NoError(t, err)
			require.Equal(t, "stale_run", action)
			require.NoError(t, mock.ExpectationsWereMet(), "no mutation may follow the route fence")
			mock.ExpectClose()
			require.NoError(t, db.Close())
		}
	}
}

func TestQualityTemplateMatchRejectsRouteBeforeStandaloneBypass(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	cfg, err := json.Marshal(&service.PelicanTestConfig{QuestionKind: "state_probe", Quality: &service.QualityPolicy{Action: "disable_scheduling"}})
	require.NoError(t, err)
	mock.ExpectQuery("SELECT pelican_config FROM scheduled_test_plans").WillReturnRows(sqlmock.NewRows([]string{"config"}).AddRow(cfg))
	mock.ExpectQuery("SELECT state FROM account_quality_states").WillReturnRows(sqlmock.NewRows([]string{"state"}).AddRow(`{}`))
	mock.ExpectQuery("SELECT platform,type,credentials,extra,parent_account_id").WillReturnRows(sqlmock.NewRows([]string{"platform", "type", "credentials", "extra", "parent"}).AddRow("openai", "oauth", `{}`, `{"openai_excel_bps":true}`, nil))
	matching, err := qualityTemplatePlanMatchesTx(context.Background(), tx, 7, 8)
	require.NoError(t, err)
	require.False(t, matching)
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

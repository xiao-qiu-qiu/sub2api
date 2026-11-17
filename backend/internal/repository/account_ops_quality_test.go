package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func qualityNotificationPlan() *service.ScheduledTestPlan {
	plan := &service.ScheduledTestPlan{ID: 7, AccountID: 8, ModelID: "test-model", UpdatedAt: time.Now(),
		PelicanConfig: &service.PelicanTestConfig{RuleName: "synthetic-rule", QuestionKind: "candy", ParallelCount: 1,
			Quality: &service.QualityPolicy{Action: service.QualityActionObserveOnly}}}
	finished := time.Now().Add(-time.Second)
	service.SetQualityProbeFinishedAt(plan, []*service.ScheduledTestResult{{
		Status: "failed", ErrorMessage: "answer_mismatch", QualityJudgment: &service.QualityJudgment{Verdict: "incorrect"},
		StartedAt: finished.Add(-time.Second), FinishedAt: finished, PelicanConfig: plan.PelicanConfig,
	}})
	return plan
}

func expectQualityNotificationRecord(mock sqlmock.Sqlmock, event service.AccountOpsEvent, name string) {
	mock.ExpectQuery(`SELECT LEFT\(name,120\) FROM accounts`).WithArgs(event.AccountID).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow(name))
	details, _ := json.Marshal(event.Details)
	mock.ExpectExec("INSERT INTO account_ops_alerts").WithArgs(event.AccountID, event.GroupID, event.GroupName,
		event.Kind, name, event.Signal, event.HTTPStatus, string(details), event.Identity).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestQualityNotificationUsesStoredDestinationsAndRequiresExplicitOptIn(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		queue     bool
	}{
		{"missing-settings", "", false},
		{"default-off", `{"enabled":true,"recipient":"ops@example.test"}`, false},
		{"master-off", `{"quality_degraded":true,"recipient":"ops@example.test"}`, false},
		{"no-destination", `{"enabled":true,"quality_degraded":true}`, false},
		{"public-flags-ignored", `{"enabled":true,"quality_degraded":true,"webhooks":[{"enabled":true,"provider":"feishu","url_configured":true}]}`, false},
		{"plaintext-ignored", `{"enabled":true,"quality_degraded":true,"webhooks":[{"enabled":true,"provider":"pushplus","token":"synthetic"}]}`, false},
		{"disabled-robot", `{"enabled":true,"quality_degraded":true,"encrypted_webhooks":[{"enabled":false,"provider":"feishu","url_cipher":"synthetic"}]}`, false},
		{"pushplus-needs-token", `{"enabled":true,"quality_degraded":true,"encrypted_webhooks":[{"enabled":true,"provider":"pushplus","url_cipher":"synthetic"}]}`, false},
		{"email", `{"enabled":true,"quality_degraded":true,"recipient":"ops@example.test"}`, true},
		{"robot", `{"enabled":true,"quality_degraded":true,"encrypted_webhooks":[{"enabled":true,"provider":"feishu","url_cipher":"synthetic"}]}`, true},
		{"pushplus", `{"enabled":true,"quality_degraded":true,"encrypted_webhooks":[{"enabled":true,"provider":"pushplus","token_cipher":"synthetic"}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectBegin()
			tx, err := db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			event := service.QualityDegradationEvent(qualityNotificationPlan(), "failed")
			require.NotNil(t, event)
			require.Empty(t, event.AccountName, "due plans omit the account's display name")
			rows := sqlmock.NewRows([]string{"value"})
			if tc.raw != "" {
				rows.AddRow(tc.raw)
			}
			mock.ExpectQuery("SELECT value FROM settings WHERE key=\\$1 FOR SHARE").WithArgs("account_ops_notifications_v1").WillReturnRows(rows)
			if tc.queue {
				expectQualityNotificationRecord(mock, *event, "current-account-name")
			}
			require.NoError(t, recordQualityDegradationNotificationTx(context.Background(), tx, *event))
			mock.ExpectRollback()
			require.NoError(t, tx.Rollback())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestQualityNotificationErrorsRestoreSavepointWithoutRejectingQualityAction(t *testing.T) {
	for _, stage := range []string{"settings", "invalid-json", "account-name", "insert"} {
		t.Run(stage, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectBegin()
			tx, err := db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			plan := qualityNotificationPlan()
			mock.ExpectExec("SAVEPOINT quality_degradation_notification").WillReturnResult(sqlmock.NewResult(0, 0))
			q := mock.ExpectQuery("SELECT value FROM settings").WithArgs("account_ops_notifications_v1")
			if stage == "settings" {
				q.WillReturnError(errors.New("synthetic database failure"))
			} else {
				raw := `{"enabled":true,"quality_degraded":true,"recipient":"ops@example.test"}`
				if stage == "invalid-json" {
					raw = `{`
				}
				q.WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
				if stage != "invalid-json" {
					q := mock.ExpectQuery(`SELECT LEFT\(name,120\) FROM accounts`).WithArgs(plan.AccountID)
					if stage == "account-name" {
						q.WillReturnError(errors.New("synthetic account failure"))
					} else {
						q.WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("synthetic-account"))
						mock.ExpectExec("INSERT INTO account_ops_alerts").WillReturnError(errors.New("synthetic insert failure"))
					}
				}
			}
			mock.ExpectExec("ROLLBACK TO SAVEPOINT quality_degradation_notification").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec("RELEASE SAVEPOINT quality_degradation_notification").WillReturnResult(sqlmock.NewResult(0, 0))
			require.NoError(t, persistQualityDegradationNotificationTx(context.Background(), tx, plan, "failed"))
			mock.ExpectCommit()
			require.NoError(t, tx.Commit(), "the quality action can still commit")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestQualityNotificationCannotHideFailedSavepointRecovery(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	mock.ExpectExec("SAVEPOINT quality_degradation_notification").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT value FROM settings").WillReturnError(errors.New("synthetic read failure"))
	recoveryErr := errors.New("synthetic rollback failure")
	mock.ExpectExec("ROLLBACK TO SAVEPOINT quality_degradation_notification").WillReturnError(recoveryErr)
	require.ErrorIs(t, persistQualityDegradationNotificationTx(context.Background(), tx, qualityNotificationPlan(), "failed"), recoveryErr)
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestQualityNotificationSharesAcceptedOutcomeTransaction(t *testing.T) {
	for _, finalErr := range []error{nil, errors.New("synthetic commit failure")} {
		t.Run(map[bool]string{false: "commit", true: "commit-rejected"}[finalErr != nil], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			plan := qualityNotificationPlan()
			until := time.Now().Add(time.Minute)
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT enabled AND updated_at").WithArgs(plan.ID, plan.UpdatedAt, until).
				WillReturnRows(sqlmock.NewRows([]string{"valid"}).AddRow(true))
			mock.ExpectQuery("SELECT state FROM account_quality_states").WithArgs(plan.ID).
				WillReturnError(sql.ErrNoRows)
			mock.ExpectExec("DELETE FROM account_quality_states").WithArgs(plan.ID).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec("SELECT id FROM accounts").WithArgs(plan.AccountID).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec("UPDATE account_quality_probe_admissions").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec("SAVEPOINT quality_degradation_notification").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery("SELECT value FROM settings").WithArgs("account_ops_notifications_v1").
				WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(`{"enabled":true,"quality_degraded":true,"recipient":"ops@example.test"}`))
			expectQualityNotificationRecord(mock, *service.QualityDegradationEvent(plan, "failed"), "actual-name")
			mock.ExpectExec("RELEASE SAVEPOINT quality_degradation_notification").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectCommit().WillReturnError(finalErr)
			action, err := NewScheduledTestPlanRepository(db).ApplyQualityOutcome(context.Background(), plan, until, "failed")
			if finalErr != nil {
				require.ErrorIs(t, err, finalErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, "observed", action)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestQualityNotificationStaleRunNeverReadsSettings(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	plan := qualityNotificationPlan()
	until := time.Now().Add(time.Minute)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT enabled AND updated_at").WithArgs(plan.ID, plan.UpdatedAt, until).
		WillReturnRows(sqlmock.NewRows([]string{"valid"}).AddRow(false))
	mock.ExpectRollback()
	action, err := NewScheduledTestPlanRepository(db).ApplyQualityOutcome(context.Background(), plan, until, "failed")
	require.NoError(t, err)
	require.Equal(t, "stale_run", action)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestQualityNotificationTwoFAAdmissionRequiresAcceptedEvidence(t *testing.T) {
	for _, scenario := range []string{"accepted", "conflict", "no-evidence"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			plan := qualityNotificationPlan()
			policy := service.DefaultOpenAITwoFAAdmissionConfig()
			plan.PelicanConfig.Quality.Action = service.QualityActionOpenAITwoFAAdmission
			plan.PelicanConfig.Quality.Admission = &policy
			if scenario == "no-evidence" {
				plan.QualityDegradation = nil
			}
			state := service.OpenAITwoFAAdmissionState{PlanID: plan.ID, Policy: policy, Phase: "pending"}
			if scenario == "conflict" {
				state.PlanID++
			}
			raw, err := json.Marshal(state)
			require.NoError(t, err)
			until := time.Now().Add(time.Minute)
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT enabled AND updated_at").WithArgs(plan.ID, plan.UpdatedAt, until).
				WillReturnRows(sqlmock.NewRows([]string{"valid"}).AddRow(true))
			mock.ExpectQuery("SELECT COALESCE\\(extra->'openai_two_fa_admission'").WithArgs(plan.AccountID).
				WillReturnRows(sqlmock.NewRows([]string{"state", "status", "platform", "type", "parent", "schedulable"}).
					AddRow(raw, "active", "openai", "oauth", nil, false))
			if scenario != "conflict" {
				mock.ExpectExec("UPDATE accounts SET").WillReturnResult(sqlmock.NewResult(0, 1))
			}
			if scenario == "accepted" {
				event := service.QualityDegradationEvent(plan, "answer_29")
				require.NotNil(t, event)
				mock.ExpectExec("SAVEPOINT quality_degradation_notification").WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectQuery("SELECT value FROM settings").WithArgs("account_ops_notifications_v1").
					WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(`{"enabled":true,"quality_degraded":true,"recipient":"ops@example.test"}`))
				expectQualityNotificationRecord(mock, *event, "admission-account")
				mock.ExpectExec("RELEASE SAVEPOINT quality_degradation_notification").WillReturnResult(sqlmock.NewResult(0, 0))
			}
			mock.ExpectCommit()
			action, err := NewScheduledTestPlanRepository(db).ApplyQualityOutcome(context.Background(), plan, until, "answer_29")
			require.NoError(t, err)
			if scenario == "conflict" {
				require.Equal(t, "admission_conflict", action)
			} else {
				require.Equal(t, "observed", action)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

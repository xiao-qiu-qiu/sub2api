//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func setQualityNotificationTestSettings(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	const key = "account_ops_notifications_v1"
	var previous string
	err := integrationDB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&previous)
	require.True(t, err == nil || err == sql.ErrNoRows)
	t.Cleanup(func() {
		if err == nil {
			_, _ = integrationDB.ExecContext(ctx, `UPDATE settings SET value=$2 WHERE key=$1`, key, previous)
		} else {
			_, _ = integrationDB.ExecContext(ctx, `DELETE FROM settings WHERE key=$1`, key)
		}
	})
	_, writeErr := integrationDB.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES($1,$2,NOW()) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`,
		key, `{"enabled":true,"quality_degraded":true,"recipient":"ops@example.test","cooldown_minutes":60}`)
	require.NoError(t, writeErr)
}

func newQualityNotificationTestAccount(t *testing.T, name string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `INSERT INTO accounts(name,platform,type,status,schedulable)
 VALUES($1,'openai','apikey','active',true) RETURNING id`, name).Scan(&id))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM scheduler_outbox WHERE account_id=$1`, id)
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM accounts WHERE id=$1`, id)
	})
	return id
}

func qualityNotificationTestEvent(t *testing.T, id int64) service.AccountOpsEvent {
	t.Helper()
	plan := qualityNotificationPlan()
	plan.AccountID = id
	event := service.QualityDegradationEvent(plan, "failed")
	require.NotNil(t, event)
	return *event
}

func TestQualityNotificationPostgresIndependentAccountsCooldownAndDeliveryProgress(t *testing.T) {
	ctx := context.Background()
	id := newQualityNotificationTestAccount(t, "quality-first")
	other := newQualityNotificationTestAccount(t, "quality-second")
	repo := NewAccountOpsRepository(integrationDB).(*accountOpsRepository)
	first, second := qualityNotificationTestEvent(t, id), qualityNotificationTestEvent(t, other)
	first.GroupName, second.GroupName = "same notification team", "same notification team"
	require.NoError(t, repo.Record(ctx, first))
	require.NoError(t, repo.Record(ctx, second))
	require.NoError(t, repo.Record(ctx, first))
	var groups, occurrences int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*),SUM(occurrences) FROM account_ops_alerts WHERE account_id IN ($1,$2) AND kind='quality_degraded'`, id, other).Scan(&groups, &occurrences))
	require.Equal(t, 2, groups, "accounts in the same ops team must keep independent quality warnings")
	require.Equal(t, 3, occurrences)
	claimed := map[int64]*service.AccountOpsEvent{}
	for range 2 {
		event, err := repo.Claim(ctx)
		require.NoError(t, err)
		require.NotNil(t, event)
		require.Equal(t, "quality_degraded", event.Kind)
		claimed[event.AccountID] = event
	}
	require.Len(t, claimed, 2)
	require.NoError(t, repo.Complete(ctx, claimed[other], "sent", time.Hour))
	lastSent := time.Now()
	ok, err := repo.SaveDelivery(ctx, claimed[id], "email:fixture", service.AccountOpsDelivery{Provider: "email", Status: "sent", Attempts: 1, LastSentAt: &lastSent})
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, repo.Complete(ctx, claimed[id], "failed", time.Minute))
	require.NoError(t, repo.Record(ctx, first))
	var state string
	var attempts int
	var delivery []byte
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT state,attempts,deliveries FROM account_ops_alerts WHERE account_id=$1`, id).Scan(&state, &attempts, &delivery))
	require.Equal(t, "failed", state)
	require.Equal(t, 1, attempts, "a new detection cannot reset the delivery attempt budget")
	require.Contains(t, string(delivery), `"status": "sent"`, "successful channels survive retries")
	_, err = integrationDB.ExecContext(ctx, `UPDATE account_ops_alerts SET next_send_at=NOW()-INTERVAL '1 second' WHERE account_id=$1`, id)
	require.NoError(t, err)
	retry, err := repo.Claim(ctx)
	require.NoError(t, err)
	require.NotNil(t, retry)
	require.Equal(t, 2, retry.Attempts)
	require.Equal(t, "sent", retry.Deliveries["email:fixture"].Status)
	require.NoError(t, repo.Complete(ctx, retry, "sent", time.Hour))
	require.NoError(t, repo.Record(ctx, first))
	none, err := repo.Claim(ctx)
	require.NoError(t, err)
	require.Nil(t, none, "new detections respect the sent notification's cooldown")
	_, err = integrationDB.ExecContext(ctx, `UPDATE account_ops_alerts SET next_send_at=NOW()-INTERVAL '1 second' WHERE account_id=$1`, id)
	require.NoError(t, err)
	require.NoError(t, repo.Record(ctx, first))
	again, err := repo.Claim(ctx)
	require.NoError(t, err)
	require.NotNil(t, again)
	require.Equal(t, 1, again.Attempts, "only an expired delivery cycle gets a fresh attempt budget")
	require.Empty(t, again.Deliveries)
	cfg := service.AccountOpsConfig{Enabled: true, QualityDegraded: false, Recipient: "ops@example.test", CooldownMinutes: 60}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	setQualityNotificationTestSettings(t)
	require.NoError(t, repo.PersistAccountOpsConfig(ctx, string(raw), cfg))
	valid, err := repo.DeliveryLeaseValid(ctx, again)
	require.NoError(t, err)
	require.False(t, valid, "saving a disabled quality switch also suppresses in-flight queued work")
	_, err = integrationDB.ExecContext(ctx, `UPDATE account_ops_alerts SET state='pending',lease='',lease_until=NULL WHERE account_id=$1`, other)
	require.NoError(t, err)
	require.NoError(t, repo.PersistAccountOpsConfig(ctx, string(raw), cfg))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT state FROM account_ops_alerts WHERE account_id=$1`, other).Scan(&state))
	require.Equal(t, "suppressed", state, "pending work is suppressed in the same config-save transaction")
}

func newQualityNotificationTestPlan(t *testing.T, account int64, kind, action string) (*scheduledTestPlanRepository, *service.ScheduledTestPlan, time.Time) {
	t.Helper()
	repo := NewScheduledTestPlanRepository(integrationDB).(*scheduledTestPlanRepository)
	now := time.Now().UTC().Truncate(time.Microsecond)
	due := now.Add(-time.Minute)
	plan, err := repo.Create(t.Context(), &service.ScheduledTestPlan{AccountID: account, ModelID: "test-model",
		CronExpression: "*/5 * * * *", Enabled: true, MaxResults: 100, NextRunAt: &due,
		PelicanConfig: &service.PelicanTestConfig{RuleName: "synthetic-quality", QuestionKind: kind,
			ReasoningEffort: "medium", ParallelCount: 1, Quality: &service.QualityPolicy{Action: action}}})
	require.NoError(t, err)
	until := now.Add(15 * time.Minute)
	ok, err := repo.ClaimPelican(t.Context(), plan, now, until, now.Add(5*time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, plan.AccountName, "exercise the real due-plan shape")
	return repo, plan, until
}

func TestQualityNotificationPostgresOutcomeAtomicityAndBestEffortFailure(t *testing.T) {
	setQualityNotificationTestSettings(t)
	for _, scenario := range []string{"commit", "rollback", "notification-error"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			name := strings.Repeat("账", 100)
			account := newQualityNotificationTestAccount(t, name)
			repo, plan, until := newQualityNotificationTestPlan(t, account, "candy", "disable_scheduling")
			finished := time.Now().Add(-time.Second)
			service.SetQualityProbeFinishedAt(plan, []*service.ScheduledTestResult{{Status: "failed", ErrorMessage: "answer_mismatch",
				QualityJudgment: &service.QualityJudgment{Verdict: "incorrect"}, StartedAt: finished.Add(-time.Second), FinishedAt: finished,
				PelicanConfig: plan.PelicanConfig}})
			if scenario == "notification-error" {
				_, err := integrationDB.ExecContext(ctx, `CREATE FUNCTION quality_ops_reject_notification_test() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'synthetic notification insert failure'; END $$;
 CREATE TRIGGER quality_ops_reject_notification_test BEFORE INSERT ON account_ops_alerts FOR EACH ROW EXECUTE FUNCTION quality_ops_reject_notification_test()`)
				require.NoError(t, err)
				t.Cleanup(func() {
					_, _ = integrationDB.ExecContext(ctx, `DROP TRIGGER IF EXISTS quality_ops_reject_notification_test ON account_ops_alerts; DROP FUNCTION IF EXISTS quality_ops_reject_notification_test()`)
				})
			}
			if scenario == "rollback" {
				tx, err := integrationDB.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer tx.Rollback()
				commit := false
				action, err := applyQualityOutcomeTx(ctx, tx, plan, until, "failed", &commit)
				require.NoError(t, err)
				require.True(t, commit)
				require.Equal(t, "scheduling_disabled", action)
				var inside int
				require.NoError(t, tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_ops_alerts WHERE account_id=$1`, account).Scan(&inside))
				require.Equal(t, 1, inside)
				require.NoError(t, tx.Rollback())
			} else {
				action, err := repo.ApplyQualityOutcome(ctx, plan, until, "failed")
				require.NoError(t, err)
				require.Equal(t, "scheduling_disabled", action)
			}
			var count int
			var schedulable bool
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT schedulable FROM accounts WHERE id=$1`, account).Scan(&schedulable))
			require.Equal(t, scenario == "rollback", schedulable)
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_ops_alerts WHERE account_id=$1`, account).Scan(&count))
			if scenario == "commit" {
				require.Equal(t, 1, count)
				var storedName, groupID, model string
				require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT account_name,group_id,details->>'quality_model' FROM account_ops_alerts WHERE account_id=$1`, account).Scan(&storedName, &groupID, &model))
				require.Equal(t, name, storedName)
				require.Equal(t, "account:"+strconv.FormatInt(account, 10), groupID)
				require.Equal(t, plan.ModelID, model)
			} else {
				require.Zero(t, count)
			}
		})
	}
}

func TestQualityNotificationPostgresSharedBatchFinalization(t *testing.T) {
	setQualityNotificationTestSettings(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	batch := &service.QualitySharedBatch{ID: "synthetic-notify-" + uuid.NewString(), StartedAt: now.Add(-time.Minute),
		StartDeadline: now.Add(time.Minute), ClosesAt: now.Add(10 * time.Minute),
		Questions: []service.QualityBatchQuestion{{Prompt: "synthetic-question", Expected: "42",
			Question: &service.QualityRandomQuestion{ID: "synthetic-control", Family: "paths", Version: "fixture"}}}}
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM quality_probe_batches WHERE id=$1`, batch.ID)
	})
	var target int64
	var repo *scheduledTestPlanRepository
	for i := range 2 {
		account := newQualityNotificationTestAccount(t, "shared-quality-name")
		var plan *service.ScheduledTestPlan
		var until time.Time
		repo, plan, until = newQualityNotificationTestPlan(t, account, service.QualityRandomQuestionKind, "disable_scheduling")
		_, err := repo.StartQualityBatch(ctx, batch, plan, until)
		require.NoError(t, err)
		cfg := *plan.PelicanConfig
		cfg.ModelID, cfg.Prompt = "test-model", batch.Questions[0].Prompt
		cfg.RandomQuestion = batch.Questions[0].Question
		cfg.QualityBatch = &service.QualityBatchEvidence{ID: batch.ID, StartedAt: batch.StartedAt, StartDeadline: batch.StartDeadline,
			ClosesAt: batch.ClosesAt, Decision: "pending", Route: "synthetic", MappedModel: "test-model"}
		finished := time.Now().Add(-time.Second)
		result := &service.ScheduledTestResult{PlanID: plan.ID, Status: "failed", ErrorMessage: "answer_mismatch",
			QualityJudgment: &service.QualityJudgment{Verdict: "incorrect"}, StartedAt: finished.Add(-time.Second), FinishedAt: finished,
			QualityRoundID: until.Format(time.RFC3339Nano), QualityAction: "comparison_pending", PelicanConfig: &cfg}
		if i == 1 {
			result.Status, result.ErrorMessage, result.QualityJudgment.Verdict = "success", "", "correct"
		} else {
			target = account
		}
		require.NoError(t, repo.CollectQualityBatch(ctx, &service.QualityBatchRun{Apply: true, Plan: plan, Until: until, Results: []*service.ScheduledTestResult{result}}))
	}
	_, err := integrationDB.ExecContext(ctx, `UPDATE quality_probe_batches SET start_deadline=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, batch.ID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `CREATE FUNCTION quality_ops_reject_finalize_test() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.status='finalized' THEN RAISE EXCEPTION 'synthetic finalization failure'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER quality_ops_reject_finalize_test BEFORE UPDATE ON quality_probe_batch_runs FOR EACH ROW EXECUTE FUNCTION quality_ops_reject_finalize_test()`)
	require.NoError(t, err)
	dropFailure := func() {
		_, err := integrationDB.ExecContext(ctx, `DROP TRIGGER IF EXISTS quality_ops_reject_finalize_test ON quality_probe_batch_runs; DROP FUNCTION IF EXISTS quality_ops_reject_finalize_test()`)
		require.NoError(t, err)
	}
	t.Cleanup(dropFailure)
	require.Error(t, repo.FinalizeQualityBatches(ctx), "a later finalization failure rolls back the notification and account action")
	var count int
	var schedulable bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_ops_alerts WHERE account_id=$1`, target).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT schedulable FROM accounts WHERE id=$1`, target).Scan(&schedulable))
	require.True(t, schedulable)
	dropFailure()
	require.NoError(t, repo.FinalizeQualityBatches(ctx))
	require.NoError(t, repo.FinalizeQualityBatches(ctx), "restarts cannot duplicate a finalized notification")
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_ops_alerts WHERE account_id=$1 AND kind='quality_degraded'`, target).Scan(&count))
	require.Equal(t, 1, count)
	var occurrences int
	var name string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT occurrences,account_name FROM account_ops_alerts WHERE account_id=$1`, target).Scan(&occurrences, &name))
	require.Equal(t, 1, occurrences)
	require.Equal(t, "shared-quality-name", name)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT schedulable FROM accounts WHERE id=$1`, target).Scan(&schedulable))
	require.False(t, schedulable)
}

//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func admissionNow(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT clock_timestamp()`).Scan(&now))
	return now
}

func newQualityProbeAdmissionFixture(t *testing.T) *qualityBPSFixture {
	t.Helper()
	return newQualityBPSFixture(t, `{"unrelated":"kept"}`, &service.QualityPolicy{Action: service.QualityActionObserveOnly})
}

func admissionRecover(t *testing.T, f *qualityBPSFixture) {
	t.Helper()
	f.exec(`UPDATE accounts SET schedulable=false WHERE id=$1`)
	f.exec(`UPDATE accounts SET schedulable=true WHERE id=$1`)
	require.NotZero(t, admissionTime(t, admissionSnapshot(t, f.account), "required_after"))
}

func admissionSnapshot(t *testing.T, id int64) map[string]any {
	t.Helper()
	var raw []byte
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT extra->'quality_probe_admission' FROM accounts WHERE id=$1`, id).Scan(&raw))
	if len(raw) == 0 {
		return nil
	}
	var state map[string]any
	require.NoError(t, json.Unmarshal(raw, &state))
	return state
}

func admissionTime(t *testing.T, state map[string]any, key string) time.Time {
	t.Helper()
	value, ok := state[key].(string)
	require.True(t, ok, "missing timestamp %s: %v", key, state)
	parsed, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(t, err)
	return parsed
}

func admissionProof(t *testing.T, f *qualityBPSFixture) {
	t.Helper()
	started := admissionNow(t)
	finished := admissionNow(t)
	f.plan.QualityProbeStartedAt, f.plan.QualityProbeFinishedAt = &started, &finished
	f.plan.QualityProbeAdmissionChecked = true
}

func TestQualityProbeAdmissionRuleLifecycleAndOutbox(t *testing.T) {
	f := newQualityProbeAdmissionFixture(t)
	first := admissionSnapshot(t, f.account)
	require.Equal(t, true, first["enabled"])
	require.NotContains(t, first, "required_after", "creating a rule is not an account recovery")
	repo := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	account, err := repo.GetByID(t.Context(), f.account)
	require.NoError(t, err)
	require.True(t, account.IsSchedulable(), "a new main rule must not pause a stable account")
	require.Positive(t, f.events())
	f.exec(`UPDATE scheduled_test_plans SET pelican_config=jsonb_set(pelican_config,'{quality,expected_answer}','"edited"') WHERE account_id=$1`)
	require.Equal(t, first, admissionSnapshot(t, f.account), "editing a main rule does not create a recovery boundary")
	f.exec(`UPDATE scheduled_test_plans SET enabled=false WHERE account_id=$1`)
	disabled := admissionSnapshot(t, f.account)
	require.Equal(t, false, disabled["enabled"])
	require.Equal(t, first["required_after"], disabled["required_after"])
	require.Positive(t, f.events())
	f.exec(`UPDATE scheduled_test_plans SET enabled=true WHERE account_id=$1`)
	resumed := admissionSnapshot(t, f.account)
	require.Equal(t, true, resumed["enabled"])
	require.NotContains(t, resumed, "required_after", "enabling a rule is not an account recovery")
	require.Positive(t, f.events())
	f.exec(`UPDATE scheduled_test_plans SET scope_suspended=true WHERE account_id=$1`)
	require.Equal(t, false, admissionSnapshot(t, f.account)["enabled"])
	require.Positive(t, f.events())
	f.exec(`UPDATE scheduled_test_plans SET scope_suspended=false WHERE account_id=$1`)
	require.Equal(t, true, admissionSnapshot(t, f.account)["enabled"])
	require.NotContains(t, admissionSnapshot(t, f.account), "required_after")
	require.Positive(t, f.events())
	admissionRecover(t, f)
	recovered := admissionSnapshot(t, f.account)["required_after"]
	f.exec(`UPDATE scheduled_test_plans SET enabled=false WHERE account_id=$1`)
	f.exec(`UPDATE scheduled_test_plans SET enabled=true WHERE account_id=$1`)
	require.Equal(t, recovered, admissionSnapshot(t, f.account)["required_after"], "rule toggles preserve a real recovery boundary")
	f.exec(`UPDATE scheduled_test_plans SET pelican_config=jsonb_set(pelican_config,'{test_channel}','"bps"') WHERE account_id=$1`)
	require.Equal(t, false, admissionSnapshot(t, f.account)["enabled"])
	require.Positive(t, f.events())
	f.exec(`DELETE FROM scheduled_test_plans WHERE account_id=$1`)
	require.Equal(t, false, admissionSnapshot(t, f.account)["enabled"])
}

func TestQualityProbeAdmissionExcludesSecondaryRules(t *testing.T) {
	for _, config := range []string{
		`{"quality":{"action":"enable_bps"}}`,
		`{"quality":{"action":"openai_two_fa_admission"}}`,
		`{"quality":{"action":"enable_scheduling","action_on":"passed"}}`,
		`{"quality":{"action":"observe_only"},"test_channel":"bps"}`,
		`{}`, `{"quality":null}`,
	} {
		var active bool
		require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT quality_probe_admission_main_rule($1::jsonb)`, config).Scan(&active))
		require.False(t, active, config)
	}
}

func TestQualityProbeAdmissionRejectsImportedAndStaleExtra(t *testing.T) {
	var imported int64
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `INSERT INTO accounts(name,platform,type,status,schedulable,extra)
 VALUES('admission-import','openai','oauth','active',true,
 '{"ordinary":true,"quality_probe_admission":{"enabled":true,"required_after":"2020-01-01T00:00:00Z","checked_started_at":"2099-01-01T00:00:00Z"}}') RETURNING id`).Scan(&imported))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM accounts WHERE id=$1`, imported)
	})
	require.Nil(t, admissionSnapshot(t, imported), "imports cannot supply their own proof")
	f := newQualityProbeAdmissionFixture(t)
	admissionRecover(t, f)
	original := admissionSnapshot(t, f.account)
	f.exec(`UPDATE accounts SET extra='{"unrelated":"updated","quality_probe_admission":{"enabled":false}}' WHERE id=$1`)
	require.Equal(t, original, admissionSnapshot(t, f.account))
	repo := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	account, err := repo.GetByID(t.Context(), f.account)
	require.NoError(t, err)
	account.Extra = map[string]any{"ordinary": true}
	require.NoError(t, repo.Update(t.Context(), account))
	require.Equal(t, original, admissionSnapshot(t, f.account), "full saves preserve authoritative state")
	require.NoError(t, repo.UpdateExtra(t.Context(), f.account, map[string]any{service.QualityProbeAdmissionKey: map[string]any{"enabled": false}}))
	require.Equal(t, original, admissionSnapshot(t, f.account))
}

func TestQualityProbeAdmissionExpiredClearKeepsProbeValid(t *testing.T) {
	f := newQualityProbeAdmissionFixture(t)
	now := admissionNow(t)
	earliest, latest := now.Add(-2*time.Minute), now.Add(-time.Minute)
	f.exec(`UPDATE account_quality_probe_admissions SET required_after=$2 WHERE account_id=$1`, now.Add(-time.Hour))
	f.exec(`UPDATE accounts SET rate_limited_at=$2,rate_limit_reset_at=$3,overload_until=$4,temp_unschedulable_until=$4 WHERE id=$1`, now.Add(-time.Hour), earliest, latest)
	require.Equal(t, latest, admissionTime(t, admissionSnapshot(t, f.account), "required_after"))
	admissionProof(t, f)
	require.Equal(t, "observed", f.apply("passed"))
	before := admissionSnapshot(t, f.account)
	repo := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	require.NoError(t, repo.ClearRateLimit(t.Context(), f.account))
	require.NoError(t, repo.ClearTempUnschedulable(t.Context(), f.account))
	after := admissionSnapshot(t, f.account)
	require.Equal(t, before, after, "probe cleanup must not invalidate its own post-reset result")
	account, err := repo.GetByID(t.Context(), f.account)
	require.NoError(t, err)
	require.False(t, account.QualityProbeAdmissionPending(admissionNow(t)))
}

func TestQualityProbeAdmissionEarlyClearDoesNotWaitOldWeek(t *testing.T) {
	f := newQualityProbeAdmissionFixture(t)
	before := admissionNow(t)
	require.NotContains(t, admissionSnapshot(t, f.account), "required_after")
	future := admissionNow(t).Add(7 * 24 * time.Hour)
	f.exec(`UPDATE accounts SET rate_limit_reset_at=$2 WHERE id=$1`, future)
	require.NotContains(t, admissionSnapshot(t, f.account), "required_after", "future deadlines stay dynamic")
	repo := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	require.NoError(t, repo.ClearRateLimit(t.Context(), f.account))
	boundary := admissionTime(t, admissionSnapshot(t, f.account), "required_after")
	require.True(t, boundary.Before(future))
	require.False(t, boundary.Before(before))
	admissionProof(t, f)
	require.Equal(t, "observed", f.apply("passed"))
	account, err := repo.GetByID(t.Context(), f.account)
	require.NoError(t, err)
	require.False(t, account.QualityProbeAdmissionPending(admissionNow(t)))
}

func TestQualityProbeAdmissionQuotaRefreshRetainsRecovery(t *testing.T) {
	for _, source := range []string{"7d", "primary"} {
		t.Run(source, func(t *testing.T) {
			f := newQualityProbeAdmissionFixture(t)
			now := admissionNow(t)
			reset := now.Add(-time.Minute)
			fields := map[string]any{"codex_" + source + "_used_percent": 100, "codex_" + source + "_window_minutes": 10080, "codex_" + source + "_reset_at": reset.Format(time.RFC3339Nano), "codex_usage_updated_at": now.Add(-time.Hour).Format(time.RFC3339Nano)}
			raw, err := json.Marshal(fields)
			require.NoError(t, err)
			f.exec(`UPDATE accounts SET extra=extra||$2::jsonb WHERE id=$1`, string(raw))
			require.NotContains(t, admissionSnapshot(t, f.account), "required_after")
			admissionProof(t, f)
			require.Equal(t, "observed", f.apply("passed"))
			require.Equal(t, reset, admissionTime(t, admissionSnapshot(t, f.account), "required_after"))
			fields["codex_"+source+"_used_percent"] = 0
			fields["codex_"+source+"_reset_at"] = now.Add(7 * 24 * time.Hour).Format(time.RFC3339Nano)
			fields["codex_usage_updated_at"] = admissionNow(t).Format(time.RFC3339Nano)
			raw, err = json.Marshal(fields)
			require.NoError(t, err)
			f.exec(`UPDATE accounts SET extra=extra||$2::jsonb WHERE id=$1`, string(raw))
			require.Equal(t, reset, admissionTime(t, admissionSnapshot(t, f.account), "required_after"))
			require.NotEmpty(t, admissionSnapshot(t, f.account)["checked_started_at"])
		})
	}
}

func TestQualityProbeAdmissionConclusiveAndLeaseFence(t *testing.T) {
	for _, mode := range []string{"passed", "failed", "unknown", "inconclusive", "unchecked", "old_start", "expired_lease", "bps", "shared_pending"} {
		t.Run(mode, func(t *testing.T) {
			f := newQualityProbeAdmissionFixture(t)
			admissionRecover(t, f)
			admissionProof(t, f)
			outcome := "passed"
			switch mode {
			case "failed", "unknown", "inconclusive":
				outcome = mode
			case "unchecked":
				f.plan.QualityProbeAdmissionChecked = false
			case "old_start":
				old := admissionTime(t, admissionSnapshot(t, f.account), "required_after").Add(-time.Second)
				f.plan.QualityProbeStartedAt = &old
			case "expired_lease":
				f.exec(`UPDATE scheduled_test_plans SET running_until=clock_timestamp()-interval '1 second' WHERE account_id=$1`)
			case "bps":
				f.plan.PelicanConfig.TestChannel = "bps"
			case "shared_pending":
				f.plan.PelicanConfig.QuestionKind = service.QualityRandomQuestionKind
				f.plan.QualityBatchEvaluated = false
			}
			f.apply(outcome)
			state := admissionSnapshot(t, f.account)
			if mode == "passed" || mode == "failed" {
				require.NotEmpty(t, state["checked_started_at"])
				require.NotEmpty(t, state["checked_finished_at"])
			} else {
				require.NotContains(t, state, "checked_started_at")
			}
		})
	}
}

func TestQualityProbeAdmissionFailureCommitsWithMove(t *testing.T) {
	f := newQualityMoveGroupsFixture(t, false)
	admissionRecover(t, f.qualityBPSFixture)
	admissionProof(t, f.qualityBPSFixture)
	require.Equal(t, "groups_moved", f.apply("failed"))
	state := admissionSnapshot(t, f.account)
	require.NotEmpty(t, state["checked_started_at"])
	require.Contains(t, f.memberships(), f.newTarget)
	repo := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	account, err := repo.GetByID(t.Context(), f.account)
	require.NoError(t, err)
	require.False(t, account.QualityProbeAdmissionPending(admissionNow(t)), "a committed failure may serve its quarantine pool")
}

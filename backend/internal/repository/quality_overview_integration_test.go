//go:build integration

package repository

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type qualityOverviewFixture struct {
	t        *testing.T
	stamp    string
	accounts []int64
	groups   []int64
	next     int
}

func newQualityOverviewFixture(t *testing.T) *qualityOverviewFixture {
	f := &qualityOverviewFixture{t: t, stamp: fmt.Sprintf("overview-%d", time.Now().UnixNano())}
	t.Cleanup(func() {
		for _, id := range f.accounts {
			_, err := integrationDB.Exec(`DELETE FROM accounts WHERE id=$1`, id)
			require.NoError(t, err)
		}
		for _, id := range f.groups {
			_, err := integrationDB.Exec(`DELETE FROM groups WHERE id=$1`, id)
			require.NoError(t, err)
		}
	})
	return f
}

func (f *qualityOverviewFixture) account(name string) int64 {
	f.t.Helper()
	var id int64
	require.NoError(f.t, integrationDB.QueryRow(`INSERT INTO accounts(name,platform,type,status,schedulable) VALUES($1,'openai','oauth','active',true) RETURNING id`, f.stamp+name).Scan(&id))
	f.accounts = append(f.accounts, id)
	return id
}

func (f *qualityOverviewFixture) plan(account int64, channel string) int64 {
	f.t.Helper()
	cfg := map[string]any{"question_kind": "state_probe", "parallel_count": 1, "test_channel": channel,
		"quality": map[string]any{"action": "observe_only", "auto_restore": true, "recovery_probe_enabled": true, "recovery_probe_interval_minutes": 60, "recovery_confirmation_minutes": 5}}
	raw, err := json.Marshal(cfg)
	require.NoError(f.t, err)
	var id int64
	require.NoError(f.t, integrationDB.QueryRow(`INSERT INTO scheduled_test_plans(account_id,model_id,pelican_config,enabled) VALUES($1,'test-a',$2,true) RETURNING id`, account, raw).Scan(&id))
	return id
}

func (f *qualityOverviewFixture) sample(plan int64, round, model, status, verdict, errMsg, action string, cfg *service.PelicanTestConfig) int64 {
	f.t.Helper()
	f.next++
	copy := *cfg
	copy.ModelID = model
	if copy.Quality == nil {
		copy.Quality = &service.QualityPolicy{Action: "observe_only"}
	}
	started := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(f.next) * time.Second)
	result, err := NewScheduledTestResultRepository(integrationDB).Create(f.t.Context(), &service.ScheduledTestResult{
		PlanID: plan, QualityRoundID: round, Status: status, ErrorMessage: errMsg, QualityAction: action,
		QualityJudgment: &service.QualityJudgment{Verdict: verdict}, PelicanConfig: &copy,
		StartedAt: started, FinishedAt: started.Add(time.Second),
	})
	require.NoError(f.t, err)
	return result.ID
}

func (f *qualityOverviewFixture) overview(filter service.QualityOverviewFilter) *service.QualityOverviewPage {
	f.t.Helper()
	if filter.Search == "" {
		filter.Search = f.stamp
	}
	page, err := service.NewScheduledTestService(nil, NewScheduledTestResultRepository(integrationDB)).AccountQualityOverview(f.t.Context(), filter)
	require.NoError(f.t, err)
	return page
}

func TestQualityOverviewIntegrationClassificationAndScopes(t *testing.T) {
	f := newQualityOverviewFixture(t)
	account := f.account("-quality")
	plan := f.plan(account, "account")
	probe := &service.PelicanTestConfig{QuestionKind: "state_probe", ParallelCount: 1, ModelIDs: []string{"test-a"}}
	f.sample(plan, "pass", "test-a", "success", "correct", "", "observed", probe)
	f.sample(plan, "wrong", "test-a", "failed", "incorrect", "state_degraded", "observed", probe)
	f.sample(plan, "http", "test-a", "failed", "incorrect", "HTTP 503", "observed", probe)
	f.sample(plan, "pending", "test-a", "success", "correct", "", "comparison_pending", probe)
	f.sample(plan, "stale", "test-a", "success", "correct", "", "stale_run", probe)
	cfg := &service.PelicanTestConfig{QuestionKind: "random_reasoning", ParallelCount: 1, ModelIDs: []string{"test-a"},
		QualityBatch: &service.QualityBatchEvidence{Route: "openai:account", Decision: "no_passing_peer"}}
	f.sample(plan, "no-peer", "test-a", "failed", "incorrect", "answer_mismatch", "inconclusive", cfg)
	batch := *cfg.QualityBatch
	batch.Decision, batch.PeerPassed = "peer_passed", 1
	cfg.QualityBatch = &batch
	f.sample(plan, "peer", "test-a", "failed", "incorrect", "answer_mismatch", "observed", cfg)
	batch.Decision, batch.PeerPassed = "passed", 0
	f.sample(plan, "random-pass", "test-a", "success", "correct", "", "observed", cfg)
	batch.Route = "openai:bps"
	f.sample(plan, "bps-via-account", "test-a", "success", "correct", "", "observed", cfg)
	batch.Route = "openai:siwc"
	f.sample(plan, "siwc", "test-a", "success", "correct", "", "observed", cfg)
	batch.Route = "openai:prism"
	f.sample(plan, "prism", "test-a", "success", "correct", "", "observed", cfg)
	legacy := &service.PelicanTestConfig{QuestionKind: "candy", ParallelCount: 1, ModelIDs: []string{"test-a"}}
	f.sample(plan, "ambiguous-legacy", "test-a", "success", "correct", "", "observed", legacy)
	page := f.overview(service.QualityOverviewFilter{})
	require.EqualValues(t, 1, page.Total)
	require.Equal(t, f.stamp+"-quality", page.Items[0].AccountName)
	require.Equal(t, "unknown", page.Items[0].Status)
	history := page.Items[0].History
	require.Len(t, history, 11, "BPS-routed account samples must not enter native history")
	statuses := make([]string, 0, len(history))
	for _, item := range history {
		statuses = append(statuses, item.Status)
		require.Equal(t, plan, item.PlanID)
	}
	require.Equal(t, []string{"healthy", "degraded", "unknown", "unknown", "unknown", "unknown", "degraded", "healthy", "unknown", "unknown", "unknown"}, statuses)
	require.Equal(t, 50.0, *page.Items[0].HealthyRate)
	require.Equal(t, "observed", history[0].QualityAction)
	require.True(t, page.Items[0].Plans[0].RecoveryProbeEnabled)
	require.Equal(t, 60, page.Items[0].Plans[0].RecoveryProbeIntervalMinutes)
	require.Equal(t, 5, page.Items[0].Plans[0].RecoveryConfirmationMinutes)
	bps := f.overview(service.QualityOverviewFilter{TestChannel: "bps"})
	require.Len(t, bps.Items[0].History, 1)
	require.Equal(t, "healthy", bps.Items[0].Status)
	require.Equal(t, 100.0, *bps.Items[0].HealthyRate)
}

func TestQualityOverviewIntegrationMultiModelCompleteness(t *testing.T) {
	f := newQualityOverviewFixture(t)
	plan := f.plan(f.account("-multi"), "account")
	cfg := &service.PelicanTestConfig{QuestionKind: "random_reasoning", ParallelCount: 1, ModelIDs: []string{"test-a", "test-b"},
		QualityBatch: &service.QualityBatchEvidence{Route: "openai:account", Decision: "passed"}}
	f.sample(plan, "both-pass", "test-a", "success", "correct", "", "observed", cfg)
	f.sample(plan, "both-pass", "test-b", "success", "correct", "", "observed", cfg)
	f.sample(plan, "partial", "test-a", "success", "correct", "", "observed", cfg)
	f.sample(plan, "duplicate", "test-a", "success", "correct", "", "observed", cfg)
	f.sample(plan, "duplicate", "test-a", "success", "correct", "", "observed", cfg)
	f.sample(plan, "mixed", "test-a", "success", "correct", "", "observed", cfg)
	failedBatch := *cfg.QualityBatch
	failedBatch.Decision, failedBatch.PeerPassed = "peer_passed", 1
	cfg.QualityBatch = &failedBatch
	f.sample(plan, "mixed", "test-b", "failed", "incorrect", "answer_mismatch", "observed", cfg)
	failedBatch.Decision = "passed"
	f.sample(plan, "skip", "test-a", "success", "correct", "", "observed", cfg)
	f.sample(plan, "skip", "test-b", "skipped", "unknown", "unsupported_model", "observed", cfg)
	page := f.overview(service.QualityOverviewFilter{})
	h := page.Items[0].History
	require.Len(t, h, 5)
	require.Equal(t, []string{"healthy", "unknown", "unknown", "degraded", "healthy"}, []string{h[0].Status, h[1].Status, h[2].Status, h[3].Status, h[4].Status})
	require.Equal(t, []string{"test-a", "test-b"}, h[0].ModelIDs)
	require.InDelta(t, 100.0*2/3, *page.Items[0].HealthyRate, 0.001)
}

func TestQualityOverviewIntegrationPlanChannels(t *testing.T) {
	f := newQualityOverviewFixture(t)
	for _, tc := range []struct {
		kind, configured, want string
	}{
		{"state_probe", "bps", "account"},
		{"state_probe", "account", "account"},
		{"state_probe", "", "account"},
		{"candy", "bps", "bps"},
		{"candy", "account", "account"},
		{"candy", "", "account"},
		{"random_reasoning", "bps", "bps"},
	} {
		t.Run(tc.kind+"/"+tc.configured, func(t *testing.T) {
			account := f.account("-" + tc.kind + "-" + tc.configured)
			plan := f.plan(account, tc.configured)
			_, err := integrationDB.Exec(`UPDATE scheduled_test_plans SET pelican_config=jsonb_set(pelican_config,'{question_kind}',to_jsonb($2::text)) WHERE id=$1`, plan, tc.kind)
			require.NoError(t, err)
			for _, channel := range []string{"account", "bps"} {
				page := f.overview(service.QualityOverviewFilter{Search: fmt.Sprint(account), TestChannel: channel})
				var found *service.QualityOverviewItem
				for i := range page.Items {
					if page.Items[i].AccountID == account {
						found = &page.Items[i]
						break
					}
				}
				require.NotNil(t, found)
				require.Len(t, found.Plans, 1, "all existing plans remain visible in either overview channel")
				require.Equal(t, tc.want, found.Plans[0].TestChannel)
			}
		})
	}
}

func TestQualityOverviewIntegrationPreparedStatement(t *testing.T) {
	f := newQualityOverviewFixture(t)
	account := f.account("-prepared")
	plan := f.plan(account, "bps")
	f.sample(plan, "native-probe", "test-a", "success", "correct", "", "observed", &service.PelicanTestConfig{
		QuestionKind: "state_probe", TestChannel: "bps", ParallelCount: 1,
	})
	stmt, err := integrationDB.PrepareContext(t.Context(), qualityOverviewSQL)
	require.NoError(t, err, "PostgreSQL must infer all parameter types during statement preparation")
	defer stmt.Close()
	for _, group := range []any{nil, int64(0), int64(9223372036854775807)} {
		for _, channel := range []string{"account", "bps"} {
			var total int64
			var raw []byte
			err = stmt.QueryRowContext(t.Context(), f.stamp, "%"+f.stamp+"%", group, 20, int64(0), channel).Scan(&total, &raw)
			require.NoError(t, err)
			var records []service.QualityOverviewAccountEvidence
			require.NoError(t, json.Unmarshal(raw, &records))
			if group == int64(9223372036854775807) {
				require.Zero(t, total)
				require.Empty(t, records)
				continue
			}
			require.EqualValues(t, 1, total)
			require.Len(t, records, 1)
			require.Equal(t, "account", records[0].Plans[0].TestChannel)
			if channel == "account" {
				require.Len(t, records[0].Rounds, 1)
				require.True(t, records[0].Rounds[0].Complete)
			} else {
				require.Empty(t, records[0].Rounds, "a state probe never belongs to BPS history")
			}
		}
	}
}

func TestQualityOverviewIntegrationPaginationSearchAndLimit(t *testing.T) {
	f := newQualityOverviewFixture(t)
	a := f.account("-a")
	b := f.account("-b")
	deleted := f.account("-deleted")
	_, err := integrationDB.Exec(`UPDATE accounts SET deleted_at=NOW() WHERE id=$1`, deleted)
	require.NoError(t, err)
	var group int64
	require.NoError(t, integrationDB.QueryRow(`INSERT INTO groups(name,platform) VALUES($1,'openai') RETURNING id`, f.stamp).Scan(&group))
	f.groups = append(f.groups, group)
	_, err = integrationDB.Exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, a, group)
	require.NoError(t, err)
	plan := f.plan(a, "account")
	cfg := &service.PelicanTestConfig{QuestionKind: "state_probe", ParallelCount: 1, ModelIDs: []string{"test-a"}}
	for i := 0; i < 65; i++ {
		f.sample(plan, fmt.Sprint(i), "test-a", "success", "correct", "", "observed", cfg)
	}
	page := f.overview(service.QualityOverviewFilter{PageSize: 1})
	require.EqualValues(t, 2, page.Total)
	require.Equal(t, b, page.Items[0].AccountID)
	require.Equal(t, "untested", page.Items[0].Status)
	require.Empty(t, page.Items[0].Plans)
	page = f.overview(service.QualityOverviewFilter{Page: 2, PageSize: 1})
	require.Equal(t, a, page.Items[0].AccountID)
	require.Len(t, page.Items[0].History, 60)
	require.Equal(t, "healthy", page.Items[0].Status)
	for i := 1; i < len(page.Items[0].History); i++ {
		require.True(t, page.Items[0].History[i].StartedAt.After(page.Items[0].History[i-1].StartedAt))
	}
	require.Equal(t, group, page.Items[0].Groups[0].ID)
	page = f.overview(service.QualityOverviewFilter{GroupID: &group})
	require.EqualValues(t, 1, page.Total)
	require.Equal(t, a, page.Items[0].AccountID)
	zero := int64(0)
	page = f.overview(service.QualityOverviewFilter{GroupID: &zero})
	require.EqualValues(t, 1, page.Total)
	require.Equal(t, b, page.Items[0].AccountID)
	page = f.overview(service.QualityOverviewFilter{Page: 9, PageSize: 1})
	require.EqualValues(t, 2, page.Total)
	require.Empty(t, page.Items)
	page = f.overview(service.QualityOverviewFilter{Search: fmt.Sprint(a)})
	ids := make([]int64, 0, len(page.Items))
	for _, item := range page.Items {
		ids = append(ids, item.AccountID)
	}
	require.Contains(t, ids, a, "exact ID matches alongside account names containing the search term")
	literal := f.account("%_literal")
	f.account("xxliteral")
	page = f.overview(service.QualityOverviewFilter{Search: f.stamp + "%_"})
	require.EqualValues(t, 1, page.Total)
	require.Equal(t, literal, page.Items[0].AccountID)
}

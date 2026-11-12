//go:build integration

package repository

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestQualityOverviewIntegrationBPSAccountRoutesAnswerPlans(t *testing.T) {
	f := newQualityOverviewFixture(t)
	account := f.account("-bps-route")
	_, err := integrationDB.Exec(`UPDATE accounts SET extra='{"openai_excel_bps":true}'::jsonb WHERE id=$1`, account)
	require.NoError(t, err)
	native := f.scopedPlan(account, "account", "")
	_, err = integrationDB.Exec(`UPDATE scheduled_test_plans SET pelican_config=jsonb_set(pelican_config,'{quality,action}','"disable_scheduling"') WHERE id=$1`, native)
	require.NoError(t, err)
	recovery := f.scopedPlan(account, "account", "passed")
	answer := f.scopedPlan(account, "account", "")
	_, err = integrationDB.Exec(`UPDATE scheduled_test_plans SET pelican_config=jsonb_set(pelican_config,'{question_kind}','"candy"') WHERE id=$1`, answer)
	require.NoError(t, err)
	cfg := &service.PelicanTestConfig{QuestionKind: "candy", TestChannel: "account", ParallelCount: 1, ModelIDs: []string{"test-a"}}
	f.sample(answer, "legacy-without-route", "test-a", "success", "correct", "", "passed", cfg)
	require.Zero(t, f.overview(service.QualityOverviewFilter{TestChannel: "account", ProbeScope: "main"}).Total)
	require.Zero(t, f.overview(service.QualityOverviewFilter{TestChannel: "account", ProbeScope: "recovery"}).Total)
	bps := f.overview(service.QualityOverviewFilter{TestChannel: "bps", ProbeScope: "main"})
	require.EqualValues(t, 1, bps.Total)
	require.Len(t, bps.Items[0].Plans, 1)
	require.Equal(t, answer, bps.Items[0].Plans[0].ID)
	require.Equal(t, "bps", bps.Items[0].Plans[0].TestChannel)
	require.Equal(t, "unknown", bps.Items[0].Status, "current routing must not manufacture historical proof")
	require.True(t, bps.Items[0].History[0].RouteEvidenceMissing)
	result := f.sample(answer, "captured-bps-route", "test-a", "success", "correct", "", "passed", cfg)
	_, err = integrationDB.Exec(`UPDATE scheduled_test_results SET pelican_config=jsonb_set(pelican_config,'{test_route}','"openai:bps"') WHERE id=$1`, result)
	require.NoError(t, err)
	bps = f.overview(service.QualityOverviewFilter{TestChannel: "bps", ProbeScope: "main"})
	require.Equal(t, "healthy", bps.Items[0].Status)
	require.False(t, bps.Items[0].History[1].RouteEvidenceMissing)
	require.Equal(t, 100.0, *bps.Items[0].HealthyRate)
	_, err = integrationDB.Exec(`UPDATE scheduled_test_plans SET pelican_config=jsonb_set(pelican_config,'{question_kind}','"candy"') WHERE id=$1`, recovery)
	require.NoError(t, err)
	bpsRecovery := f.overview(service.QualityOverviewFilter{TestChannel: "bps", ProbeScope: "recovery"})
	require.EqualValues(t, 1, bpsRecovery.Total)
	require.Len(t, bpsRecovery.Items[0].Plans, 1)
	require.Equal(t, recovery, bpsRecovery.Items[0].Plans[0].ID)
}

func TestQualityOverviewIntegrationBPSPredicateMatchesAccount(t *testing.T) {
	f := newQualityOverviewFixture(t)
	account := f.account("-bps-predicate")
	for _, tc := range []struct {
		name               string
		extra, credentials map[string]any
	}{
		{"enabled", map[string]any{"openai_excel_bps": true}, nil},
		{"string-is-not-enabled", map[string]any{"openai_excel_bps": "true"}, nil},
		{"pending", map[string]any{"openai_excel_bps": true, "openai_excel_bps_authorization_pending": true}, nil},
		{"siwc", map[string]any{"openai_excel_bps": true, "openai_siwc": true}, nil},
		{"free", map[string]any{"openai_excel_bps": true}, map[string]any{"plan_type": " FREE "}},
		{"agent", map[string]any{"openai_excel_bps": true}, map[string]any{"auth_mode": "agentIdentity"}},
		{"pat", map[string]any{"openai_excel_bps": true}, map[string]any{"auth_mode": "personalAccessToken"}},
		{"legacy-pat", map[string]any{"openai_excel_bps": true}, map[string]any{"openai_auth_mode": "personal_access_token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra, err := json.Marshal(tc.extra)
			require.NoError(t, err)
			credentials := []byte(`{}`)
			if tc.credentials != nil {
				credentials, err = json.Marshal(tc.credentials)
				require.NoError(t, err)
			}
			_, err = integrationDB.Exec(`UPDATE accounts SET extra=$2::jsonb,credentials=$3::jsonb WHERE id=$1`, account, string(extra), string(credentials))
			require.NoError(t, err)
			var enabled bool
			require.NoError(t, integrationDB.QueryRow(`SELECT `+qualityOverviewBPSEnabledSQL+` FROM accounts a WHERE a.id=$1`, account).Scan(&enabled))
			a := &service.Account{ID: account, Platform: "openai", Type: "oauth", Extra: tc.extra, Credentials: tc.credentials}
			require.Equal(t, a.IsExcelBPSEnabled(), enabled)
		})
	}
}

func TestQualityOverviewIntegrationDisabledBPSObservationHasNoCurrentScope(t *testing.T) {
	f := newQualityOverviewFixture(t)
	account := f.account("-bps-disabled")
	plan := f.plan(account, "bps")
	_, err := integrationDB.Exec(`UPDATE scheduled_test_plans SET pelican_config=jsonb_set(pelican_config,'{question_kind}','"candy"') WHERE id=$1`, plan)
	require.NoError(t, err)
	require.Zero(t, f.overview(service.QualityOverviewFilter{TestChannel: "bps", ProbeScope: "main"}).Total)
	all := f.overview(service.QualityOverviewFilter{TestChannel: "bps"})
	require.EqualValues(t, 1, all.Total)
	require.Equal(t, plan, all.Items[0].Plans[0].ID)
	_, err = integrationDB.Exec(`UPDATE accounts SET extra='{"openai_excel_bps":true}'::jsonb WHERE id=$1`, account)
	require.NoError(t, err)
	require.EqualValues(t, 1, f.overview(service.QualityOverviewFilter{TestChannel: "bps", ProbeScope: "main"}).Total)
}

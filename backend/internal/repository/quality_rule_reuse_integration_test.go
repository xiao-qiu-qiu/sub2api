//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestQualityReusePreservesTargetOwnershipWithoutImplicitRestoration(t *testing.T) {
	ctx := context.Background()
	for _, action := range []string{"remove_groups", "disable_scheduling"} {
		t.Run(action, func(t *testing.T) {
			f, _, _ := legacyQualityActionFixture(t, action)
			f.exec(`DELETE FROM scheduler_outbox WHERE account_id=$1`)
			var beforeState, beforeAccount string
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT state::text FROM account_quality_states WHERE plan_id=$1`, f.plan.ID).Scan(&beforeState))
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, f.account).Scan(&beforeAccount))
			groupCount := f.count(`SELECT count(*) FROM account_groups WHERE account_id=$1`)
			plan, err := f.plans.GetByID(ctx, f.plan.ID)
			require.NoError(t, err)
			plan.PreserveQualityState = true
			plan.PelicanConfig.Quality.Action = service.QualityActionIsolateModels
			_, err = f.plans.Update(ctx, plan)
			require.ErrorContains(t, err, "restore this rule's previous account action")
			persisted, err := f.plans.GetByID(ctx, plan.ID)
			require.NoError(t, err)
			require.Equal(t, action, persisted.PelicanConfig.Quality.Action, "unsafe switch rolls back")

			persisted.PreserveQualityState = true
			persisted.PelicanConfig.Quality.Action = service.QualityActionObserveOnly
			persisted.Enabled = false
			updated, err := f.plans.Update(ctx, persisted)
			require.NoError(t, err)
			require.Equal(t, plan.ID, updated.ID)
			require.Equal(t, f.account, updated.AccountID)
			require.Equal(t, persisted.LastRunAt, updated.LastRunAt)
			require.Equal(t, persisted.RunningUntil, updated.RunningUntil)
			var afterState, afterAccount string
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT state::text FROM account_quality_states WHERE plan_id=$1`, plan.ID).Scan(&afterState))
			require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT row_to_json(a)::text FROM accounts a WHERE id=$1`, f.account).Scan(&afterAccount))
			require.JSONEq(t, beforeState, afterState)
			var expectedAccount, actualAccount map[string]any
			require.NoError(t, json.Unmarshal([]byte(beforeAccount), &expectedAccount))
			require.NoError(t, json.Unmarshal([]byte(afterAccount), &actualAccount))
			beforeAdmission, ok := expectedAccount["extra"].(map[string]any)[service.QualityProbeAdmissionKey].(map[string]any)
			require.True(t, ok)
			require.Equal(t, true, beforeAdmission["enabled"])
			afterAdmission, ok := actualAccount["extra"].(map[string]any)[service.QualityProbeAdmissionKey].(map[string]any)
			require.True(t, ok)
			require.Equal(t, false, afterAdmission["enabled"], "pausing the last rule disables admission without restoring its account action")
			beforeAdmission["enabled"] = false
			require.Equal(t, expectedAccount, actualAccount, "only admission.enabled may change; all account configuration and other runtime evidence must remain identical")
			require.Equal(t, groupCount, f.count(`SELECT count(*) FROM account_groups WHERE account_id=$1`))
			require.Equal(t, 1, f.count(`SELECT count(*) FROM scheduler_outbox WHERE account_id=$1`), "the changed admission gate needs one scheduler refresh")
			require.Equal(t, 1, f.count(`SELECT count(*) FROM scheduler_outbox WHERE account_id=$1 AND event_type='account_changed' AND group_id IS NULL`))
		})
	}
}

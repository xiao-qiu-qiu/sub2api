//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestQualityTemplateAccountAdmissionSnapshotPreservesJSONKinds(t *testing.T) {
	for _, tc := range []struct {
		name            string
		extra, expected any
	}{
		{"SQL NULL", nil, nil},
		{"JSON null", `null`, `null`},
		{"boolean", `true`, `true`},
		{"number", `42`, `42`},
		{"string", `"legacy"`, `"legacy"`},
		{"array", `["quality_probe_admission",1]`, `["quality_probe_admission",1]`},
		{"empty object", `{}`, `{}`},
		{"object runtime key only", `{"quality_probe_admission":{"enabled":true},"quality_probe_admission_other":42}`, `{"quality_probe_admission_other":42}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var equal bool
			err := integrationDB.QueryRowContext(t.Context(), `SELECT (`+qualityTemplateAccountConfigSnapshot+`) =
 jsonb_build_array(a.platform,a.type,a.credentials,$2::jsonb,a.proxy_id,a.parent_account_id)::text
 FROM (SELECT 'openai'::text AS platform,'oauth'::text AS type,'{}'::jsonb AS credentials,
 $1::jsonb AS extra,NULL::bigint AS proxy_id,NULL::bigint AS parent_account_id) a`, tc.extra, tc.expected).Scan(&equal)
			require.NoError(t, err)
			require.True(t, equal)
		})
	}
}

func TestQualityTemplateReincludeAdmissionSnapshotDoesNotInvalidatePreparation(t *testing.T) {
	for _, actualEdit := range []bool{false, true} {
		name := "admission only"
		if actualEdit {
			name = "admission and unrelated extra"
		}
		t.Run(name, func(t *testing.T) {
			f := newQualityReincludeFixture(t)
			var beforeVersion time.Time
			var beforeExtra string
			require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT updated_at,extra::text FROM accounts WHERE id=$1`, f.account).Scan(&beforeVersion, &beforeExtra))
			result, err := f.repo.ReincludeAccount(f.ctx, f.tpl, f.account, func(ctx context.Context, a service.QualityTemplateAccount) (*service.ScheduledTestPlan, string) {
				// The production admission publisher changes Extra without updating
				// the account's configuration version or the template binding.
				changed, err := integrationDB.ExecContext(ctx, `UPDATE account_quality_probe_admissions SET required_after=clock_timestamp() WHERE account_id=$1`, a.ID)
				require.NoError(t, err)
				count, err := changed.RowsAffected()
				require.NoError(t, err)
				require.EqualValues(t, 1, count)
				var afterVersion time.Time
				var afterExtra string
				require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT updated_at,extra::text FROM accounts WHERE id=$1`, a.ID).Scan(&afterVersion, &afterExtra))
				require.True(t, beforeVersion.Equal(afterVersion))
				require.NotEqual(t, beforeExtra, afterExtra, "exercise a real runtime snapshot publication")
				if actualEdit {
					_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET extra=extra || '{"quality_probe_admission_other":true}'::jsonb WHERE id=$1`, a.ID)
					require.NoError(t, err)
				}
				return f.prepare(ctx, a)
			})
			require.NoError(t, err)
			if actualEdit {
				require.Equal(t, "account_changed", result.Reason)
				require.False(t, f.binding().Valid)
				require.Zero(t, f.countPlans())
			} else {
				require.Equal(t, "created", result.Status)
				require.Empty(t, result.Reason)
				require.True(t, f.binding().Valid)
				require.Equal(t, 1, f.countPlans())
			}
		})
	}
}

func TestQualityOwnershipAdmissionSnapshotDoesNotChangeFence(t *testing.T) {
	ctx := context.Background()
	f, _, _ := newQualityIsolationFixture(t)
	isolationRound(f, "failed", "passed")
	repo := f.plans.(service.QualityOwnershipRepository)
	before, err := repo.QualityOwnership(ctx, f.plan.ID, nil)
	require.NoError(t, err)
	changed, err := integrationDB.ExecContext(ctx, `UPDATE account_quality_probe_admissions SET required_after=clock_timestamp() WHERE account_id=$1`, f.account)
	require.NoError(t, err)
	count, err := changed.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	after, err := repo.QualityOwnership(ctx, f.plan.ID, nil)
	require.NoError(t, err)
	require.Equal(t, before.AccountVersion, after.AccountVersion)
	require.Equal(t, before.Fence, after.Fence, "independent admission cannot invalidate ownership consent")
	// A real owned entry still invalidates consent even without updated_at.
	f.exec(`UPDATE accounts SET extra=jsonb_set(extra,'{quality_model_isolation,sol,isolated_at}','"changed-by-administrator"'::jsonb) WHERE id=$1`)
	_, err = repo.QualityOwnership(ctx, f.plan.ID, &service.QualityOwnershipRequest{Fence: before.Fence, Confirm: true})
	require.Equal(t, "QUALITY_OWNERSHIP_PREVIEW_CHANGED", infraerrors.Reason(err))
}

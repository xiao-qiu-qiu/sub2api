//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type qualityMoveGroupsFixture struct {
	*qualityBPSFixture
	source, sourcePeer, existingTarget, newTarget, unrelated int64
}

func newQualityMoveGroupsFixture(t *testing.T, autoRestore bool) *qualityMoveGroupsFixture {
	t.Helper()
	ctx := context.Background()
	f := &qualityMoveGroupsFixture{}
	for i, id := range []*int64{&f.source, &f.sourcePeer, &f.existingTarget, &f.newTarget, &f.unrelated} {
		require.NoError(t, integrationDB.QueryRowContext(ctx, `INSERT INTO groups(name,platform) VALUES($1,'openai') RETURNING id`, fmt.Sprintf("quality-move-%d", i)).Scan(id))
		groupID := *id
		t.Cleanup(func() { _, _ = integrationDB.ExecContext(ctx, `DELETE FROM groups WHERE id=$1`, groupID) })
	}
	f.qualityBPSFixture = newQualityBPSFixture(t, `{"unrelated":"kept","model_rate_limits":{"native":{"reason":"native"}}}`, &service.QualityPolicy{
		Action: service.QualityActionMoveGroups, MoveGroupIDs: []int64{f.existingTarget, f.newTarget}, AutoRestore: autoRestore,
	})
	f.exec(`INSERT INTO account_groups(account_id,group_id,priority,created_at,allowed_models) VALUES
 ($1,$2,7,'2025-01-02T03:04:05.123456Z','["sol","astra"]'),
 ($1,$3,83,'2025-02-03T04:05:06.234567Z',NULL),
 ($1,$4,19,'2025-03-04T05:06:07.345678Z','["existing-target-model"]')`, f.source, f.sourcePeer, f.existingTarget)
	return f
}

func (f *qualityMoveGroupsFixture) memberships() map[int64]qualityGroup {
	f.t.Helper()
	var raw []byte
	require.NoError(f.t, integrationDB.QueryRowContext(context.Background(), `SELECT COALESCE(jsonb_agg(to_jsonb(g) ORDER BY group_id),'[]'::jsonb) FROM account_groups g WHERE account_id=$1`, f.account).Scan(&raw))
	var groups []qualityGroup
	require.NoError(f.t, json.Unmarshal(raw, &groups))
	byID := make(map[int64]qualityGroup, len(groups))
	for _, group := range groups {
		byID[group.GroupID] = group
	}
	return byID
}

func (f *qualityMoveGroupsFixture) state() qualityState {
	f.t.Helper()
	var raw []byte
	require.NoError(f.t, integrationDB.QueryRowContext(context.Background(), `SELECT state FROM account_quality_states WHERE plan_id=$1`, f.plan.ID).Scan(&raw))
	var state qualityState
	require.NoError(f.t, json.Unmarshal(raw, &state))
	return state
}

func (f *qualityMoveGroupsFixture) snapshot() string {
	f.t.Helper()
	var snapshot string
	require.NoError(f.t, integrationDB.QueryRowContext(context.Background(), `SELECT jsonb_build_object(
 'plan',(SELECT to_jsonb(p) FROM scheduled_test_plans p WHERE id=$1),
 'state',(SELECT state FROM account_quality_states WHERE plan_id=$1),
 'account',(SELECT to_jsonb(a) FROM accounts a WHERE id=$2),
 'memberships',(SELECT COALESCE(jsonb_agg(to_jsonb(g) ORDER BY group_id),'[]'::jsonb) FROM account_groups g WHERE account_id=$2),
 'outbox',(SELECT COALESCE(jsonb_agg(to_jsonb(o) ORDER BY id),'[]'::jsonb) FROM scheduler_outbox o WHERE account_id=$2)
 )::text`, f.plan.ID, f.account).Scan(&snapshot))
	return snapshot
}

func (f *qualityMoveGroupsFixture) requireOutboxGroups(ids ...int64) {
	f.t.Helper()
	rows, err := integrationDB.QueryContext(context.Background(), `SELECT payload FROM scheduler_outbox WHERE account_id=$1 AND event_type='account_groups_changed' ORDER BY id`, f.account)
	require.NoError(f.t, err)
	defer func() { _ = rows.Close() }()
	seen := map[int64]bool{}
	for rows.Next() {
		var raw []byte
		require.NoError(f.t, rows.Scan(&raw))
		var payload struct {
			GroupIDs []int64 `json:"group_ids"`
		}
		require.NoError(f.t, json.Unmarshal(raw, &payload))
		for _, id := range payload.GroupIDs {
			seen[id] = true
		}
	}
	require.NoError(f.t, rows.Err())
	for _, id := range ids {
		require.Contains(f.t, seen, id, "invalidate source and destination group caches")
	}
}

func TestQualityMoveGroupsMultiTargetLifecycleAndOwnership(t *testing.T) {
	f := newQualityMoveGroupsFixture(t, true)
	original := f.memberships()
	before := f.snapshot()
	request := beginQualityRequest(t, f.account)
	require.Equal(t, "inconclusive", f.apply("inconclusive"))
	require.JSONEq(t, before, f.snapshot())
	requireQualityRequestActive(t, request)

	require.Equal(t, "groups_moved", f.apply("failed"))
	requireQualityRequestInterrupted(t, request)
	moved := f.memberships()
	require.Len(t, moved, 2)
	require.Contains(t, moved, f.newTarget)
	require.Equal(t, original[f.existingTarget], moved[f.existingTarget], "keep an existing target's priority, timestamp and allowed models")
	state := f.state()
	require.Equal(t, service.QualityActionMoveGroups, state.Action)
	require.ElementsMatch(t, []int64{f.existingTarget, f.newTarget}, state.MoveGroupIDs)
	require.ElementsMatch(t, []qualityGroup{original[f.source], original[f.sourcePeer]}, state.Removed)
	require.Equal(t, []qualityGroup{moved[f.newTarget]}, state.Added, "own only memberships inserted by this rule")
	f.requireOutboxGroups(f.source, f.sourcePeer, f.existingTarget, f.newTarget)

	before = f.snapshot()
	require.Equal(t, "already_quarantined", f.apply("failed"))
	require.JSONEq(t, before, f.snapshot(), "repeat failures cannot replace the original snapshot or emit more invalidations")
	for _, outcome := range []string{"inconclusive", "unknown"} {
		f.apply(outcome)
		require.JSONEq(t, before, f.snapshot(), "uncertain evidence cannot alter memberships or ownership")
	}

	f.exec(`INSERT INTO account_groups(account_id,group_id,priority,created_at,allowed_models) VALUES($1,$2,31,'2025-04-05T06:07:08.456789Z','["manual-model"]')`, f.unrelated)
	original[f.unrelated] = f.memberships()[f.unrelated]
	f.exec(`DELETE FROM scheduler_outbox WHERE account_id=$1`)
	require.Equal(t, "restored", f.apply("passed"))
	require.Equal(t, original, f.memberships(), "restore exact source metadata, keep pre-existing targets and unrelated manual memberships")
	require.Zero(t, f.count(`SELECT count(*) FROM account_quality_states WHERE plan_id=`+strconv.FormatInt(f.plan.ID, 10)+` AND $1::bigint>0`))
	f.requireOutboxGroups(f.source, f.sourcePeer, f.existingTarget, f.newTarget)
}

func TestQualityMoveGroupsEmptySourceAndNoChange(t *testing.T) {
	t.Run("empty source still joins every target and restores to ungrouped", func(t *testing.T) {
		f := newQualityMoveGroupsFixture(t, true)
		f.exec(`DELETE FROM account_groups WHERE account_id=$1`)
		require.Equal(t, "groups_moved", f.apply("failed"))
		require.Len(t, f.memberships(), 2)
		require.Empty(t, f.state().Removed)
		require.Len(t, f.state().Added, 2)
		f.requireOutboxGroups(f.existingTarget, f.newTarget)
		f.exec(`DELETE FROM scheduler_outbox WHERE account_id=$1`)
		require.Equal(t, "restored", f.apply("passed"))
		require.Empty(t, f.memberships())
		f.requireOutboxGroups(f.existingTarget, f.newTarget)
	})
	t.Run("all memberships already are targets", func(t *testing.T) {
		f := newQualityMoveGroupsFixture(t, true)
		f.exec(`DELETE FROM account_groups WHERE account_id=$1 AND group_id IN ($2,$3)`, f.source, f.sourcePeer)
		f.exec(`INSERT INTO account_groups(account_id,group_id,priority,allowed_models) VALUES($1,$2,3,'[]')`, f.newTarget)
		before := f.snapshot()
		request := beginQualityRequest(t, f.account)
		require.Equal(t, "no_change", f.apply("failed"))
		require.JSONEq(t, before, f.snapshot(), "no mutation means no ownership, account version change or outbox event")
		requireQualityRequestActive(t, request)
		require.Equal(t, "no_change", f.apply("failed"))
		require.JSONEq(t, before, f.snapshot())
		requireQualityRequestActive(t, request)
	})
}

func TestQualityMoveGroupsSingleTargetLifecycle(t *testing.T) {
	for _, alreadyPresent := range []bool{true, false} {
		t.Run(fmt.Sprintf("target-already-present=%t", alreadyPresent), func(t *testing.T) {
			f := newQualityMoveGroupsFixture(t, true)
			original := f.memberships()
			target := f.newTarget
			if alreadyPresent {
				target = f.existingTarget
			}
			f.plan.PelicanConfig.Quality.MoveGroupIDs = []int64{target}
			plan, err := f.plans.Update(context.Background(), f.plan)
			require.NoError(t, err)
			f.plan = plan
			require.Equal(t, "groups_moved", f.apply("failed"))
			memberships := f.memberships()
			require.Len(t, memberships, 1)
			require.Contains(t, memberships, target)
			if alreadyPresent {
				require.Equal(t, original[target], memberships[target])
				require.Empty(t, f.state().Added)
			} else {
				require.Len(t, f.state().Added, 1)
			}
			require.Equal(t, "restored", f.apply("passed"))
			require.Equal(t, original, f.memberships())
		})
	}
}

func TestQualityMoveGroupsAutoRestoreDisabled(t *testing.T) {
	f := newQualityMoveGroupsFixture(t, false)
	require.Equal(t, "groups_moved", f.apply("failed"))
	before := f.snapshot()
	f.apply("passed")
	require.JSONEq(t, before, f.snapshot(), "passing must preserve quarantine when auto_restore is disabled")
}

func TestQualityMoveGroupsUnavailableTargetRejectsWholeMove(t *testing.T) {
	for _, mutation := range []struct{ name, query string }{
		{"soft deleted", `UPDATE groups SET deleted_at=clock_timestamp() WHERE id=$1`},
		{"deleted", `DELETE FROM groups WHERE id=$1`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			f := newQualityMoveGroupsFixture(t, true)
			// Delete the last target after the plan was saved; the earlier target
			// remains valid so validation must precede any source removal.
			_, err := integrationDB.ExecContext(context.Background(), mutation.query, f.newTarget)
			require.NoError(t, err)
			before := f.snapshot()
			request := beginQualityRequest(t, f.account)
			require.Equal(t, "action_conflict", f.apply("failed"))
			require.JSONEq(t, before, f.snapshot())
			requireQualityRequestActive(t, request)
		})
	}
}

func TestQualityMoveGroupsStaleRuleAndLeaseCannotMutate(t *testing.T) {
	for _, mutation := range []struct{ name, query string }{
		{"edited rule", `UPDATE scheduled_test_plans SET updated_at=updated_at+interval '1 second' WHERE id=$1`},
		{"paused rule", `UPDATE scheduled_test_plans SET enabled=false WHERE id=$1`},
		{"deleted rule", `DELETE FROM scheduled_test_plans WHERE id=$1`},
		{"missing lease", `UPDATE scheduled_test_plans SET running_until=NULL WHERE id=$1`},
		{"expired lease", `UPDATE scheduled_test_plans SET running_until=NOW()-interval '1 second' WHERE id=$1`},
		{"replaced lease", `UPDATE scheduled_test_plans SET running_until=running_until+interval '1 second' WHERE id=$1`},
	} {
		for _, outcome := range []string{"failed", "passed"} {
			t.Run(mutation.name+"/"+outcome, func(t *testing.T) {
				f := newQualityMoveGroupsFixture(t, true)
				if outcome == "passed" {
					require.Equal(t, "groups_moved", f.apply("failed"))
				}
				_, err := integrationDB.ExecContext(context.Background(), mutation.query, f.plan.ID)
				require.NoError(t, err)
				before := f.snapshot()
				request := beginQualityRequest(t, f.account)
				require.Equal(t, "stale_run", f.apply(outcome))
				require.JSONEq(t, before, f.snapshot())
				requireQualityRequestActive(t, request)
			})
		}
	}
}

func TestQualityMoveGroupsRestoreConflictIsAtomic(t *testing.T) {
	for _, mutation := range []struct {
		name, query string
		target      bool
	}{
		{"target priority changed", `UPDATE account_groups SET priority=priority+1 WHERE account_id=$1 AND group_id=$2`, true},
		{"target timestamp changed", `UPDATE account_groups SET created_at=created_at+interval '1 microsecond' WHERE account_id=$1 AND group_id=$2`, true},
		{"target model restriction changed", `UPDATE account_groups SET allowed_models='["manual-model"]' WHERE account_id=$1 AND group_id=$2`, true},
		{"target unrestricted changed to empty list", `UPDATE account_groups SET allowed_models='[]' WHERE account_id=$1 AND group_id=$2`, true},
		{"target membership rebuilt", `WITH old AS (DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2 RETURNING *)
 INSERT INTO account_groups(account_id,group_id,priority,created_at,allowed_models)
 SELECT account_id,group_id,priority,created_at+interval '1 microsecond',allowed_models FROM old`, true},
		{"target membership removed", `DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, true},
		{"target group soft deleted", `UPDATE groups SET deleted_at=clock_timestamp() WHERE id=$2 AND $1::bigint>0`, true},
		{"target group deleted", `DELETE FROM groups WHERE id=$2 AND $1::bigint>0`, true},
		{"source group soft deleted", `UPDATE groups SET deleted_at=clock_timestamp() WHERE id=$2 AND $1::bigint>0`, false},
		{"source group deleted", `DELETE FROM groups WHERE id=$2 AND $1::bigint>0`, false},
		{"source membership manually readded", `INSERT INTO account_groups(account_id,group_id,priority,allowed_models) VALUES($1,$2,99,'["manual-model"]')`, false},
		{"source membership readded with original metadata", `INSERT INTO account_groups(account_id,group_id,priority,created_at,allowed_models)
 VALUES($1,$2,83,'2025-02-03T04:05:06.234567Z',NULL)`, false},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			f := newQualityMoveGroupsFixture(t, true)
			require.Equal(t, "groups_moved", f.apply("failed"))
			groupID := f.sourcePeer
			if mutation.target {
				groupID = f.newTarget
			}
			f.exec(mutation.query, groupID)
			before := f.snapshot()
			request := beginQualityRequest(t, f.account)
			require.Equal(t, "restore_conflict", f.apply("passed"))
			require.JSONEq(t, before, f.snapshot(), "a conflicting membership must roll back all source inserts, target deletes, ownership and outbox writes")
			requireQualityRequestActive(t, request)
			if mutation.target {
				require.NotNil(t, f.plan.QualityRestoreDiagnostic)
				require.Equal(t, "target_membership_changed", f.plan.QualityRestoreDiagnostic.Reason)
			}
		})
	}
}

func TestQualityMoveGroupsExistingTargetManualEditRemainsUnowned(t *testing.T) {
	f := newQualityMoveGroupsFixture(t, true)
	original := f.memberships()
	require.Equal(t, "groups_moved", f.apply("failed"))
	f.exec(`UPDATE account_groups SET priority=91,allowed_models='["manually-limited"]' WHERE account_id=$1 AND group_id=$2`, f.existingTarget)
	original[f.existingTarget] = f.memberships()[f.existingTarget]
	require.Equal(t, "restored", f.apply("passed"))
	require.Equal(t, original, f.memberships(), "an existing target is not owned by the move and must retain subsequent manual edits")
}

func TestQualityMoveGroupsOrdinaryEditRestoresBeforeReplacingPolicy(t *testing.T) {
	for _, replacement := range []string{"targets", service.QualityActionObserveOnly, "disable_scheduling"} {
		t.Run(replacement, func(t *testing.T) {
			f := newQualityMoveGroupsFixture(t, false)
			original := f.memberships()
			require.Equal(t, "groups_moved", f.apply("failed"))
			f.exec(`DELETE FROM scheduler_outbox WHERE account_id=$1`)
			plan, err := f.plans.GetByID(context.Background(), f.plan.ID)
			require.NoError(t, err)
			if replacement == "targets" {
				plan.PelicanConfig.Quality.MoveGroupIDs = []int64{f.unrelated}
			} else {
				plan.PelicanConfig.Quality.Action = replacement
				plan.PelicanConfig.Quality.MoveGroupIDs = nil
			}
			updated, err := f.plans.Update(context.Background(), plan)
			require.NoError(t, err, "ordinary edits explicitly restore old ownership even when automatic restoration is disabled")
			require.Equal(t, plan.PelicanConfig.Quality.Action, updated.PelicanConfig.Quality.Action)
			require.Equal(t, plan.PelicanConfig.Quality.MoveGroupIDs, updated.PelicanConfig.Quality.MoveGroupIDs)
			require.Equal(t, original, f.memberships())
			require.Zero(t, f.count(`SELECT count(*) FROM account_quality_states WHERE plan_id=`+strconv.FormatInt(f.plan.ID, 10)+` AND $1::bigint>0`))
			f.requireOutboxGroups(f.source, f.sourcePeer, f.existingTarget, f.newTarget)
			require.Equal(t, "stale_run", f.apply("failed"), "a pre-edit run cannot apply the replaced policy")
		})
	}
}

func TestQualityMoveGroupsEditConflictRollsBackPolicyAndMemberships(t *testing.T) {
	for _, replacement := range []string{"targets", service.QualityActionObserveOnly} {
		for _, targetConflict := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/target-conflict=%t", replacement, targetConflict), func(t *testing.T) {
				f := newQualityMoveGroupsFixture(t, true)
				require.Equal(t, "groups_moved", f.apply("failed"))
				if targetConflict {
					f.exec(`UPDATE account_groups SET priority=99 WHERE account_id=$1 AND group_id=$2`, f.newTarget)
				} else {
					f.exec(`INSERT INTO account_groups(account_id,group_id,priority) VALUES($1,$2,99)`, f.sourcePeer)
				}
				plan, err := f.plans.GetByID(context.Background(), f.plan.ID)
				require.NoError(t, err)
				if replacement == "targets" {
					plan.PelicanConfig.Quality.MoveGroupIDs = []int64{f.unrelated}
				} else {
					plan.PelicanConfig.Quality.Action = replacement
					plan.PelicanConfig.Quality.MoveGroupIDs = nil
				}
				before := f.snapshot()
				_, err = f.plans.Update(context.Background(), plan)
				require.Error(t, err)
				require.JSONEq(t, before, f.snapshot(), "failed restoration must also roll back the rule edit")
			})
		}
	}
}

func TestQualityMoveGroupsInvalidEditRollsBackOldRestoration(t *testing.T) {
	f := newQualityMoveGroupsFixture(t, true)
	require.Equal(t, "groups_moved", f.apply("failed"))
	_, err := integrationDB.ExecContext(context.Background(), `DELETE FROM groups WHERE id=$1`, f.unrelated)
	require.NoError(t, err)
	plan, err := f.plans.GetByID(context.Background(), f.plan.ID)
	require.NoError(t, err)
	plan.PelicanConfig.Quality.MoveGroupIDs = []int64{f.unrelated}
	before := f.snapshot()
	_, err = f.plans.Update(context.Background(), plan)
	require.Error(t, err)
	require.JSONEq(t, before, f.snapshot(), "validation of the replacement target must roll back any preceding restoration")
}

func TestQualityMoveGroupsReuseCannotImplicitlyRestore(t *testing.T) {
	for _, replacement := range []string{"targets", service.QualityActionObserveOnly} {
		t.Run(replacement, func(t *testing.T) {
			f := newQualityMoveGroupsFixture(t, true)
			require.Equal(t, "groups_moved", f.apply("failed"))
			before := f.snapshot()
			plan, err := f.plans.GetByID(context.Background(), f.plan.ID)
			require.NoError(t, err)
			plan.PreserveQualityState = true
			if replacement == "targets" {
				plan.PelicanConfig.Quality.MoveGroupIDs = []int64{f.unrelated}
			} else {
				plan.PelicanConfig.Quality.Action = replacement
				plan.PelicanConfig.Quality.MoveGroupIDs = nil
			}
			_, err = f.plans.Update(context.Background(), plan)
			require.Error(t, err, "reuse must reject changes to an owned move scope")
			require.JSONEq(t, before, f.snapshot(), "rejecting incompatible reuse must preserve the rule, ownership, account, memberships and outbox")
		})
	}
}

func TestQualityMoveGroupsTemplateAndPresetPersistTargets(t *testing.T) {
	ctx := context.Background()
	f := newQualityMoveGroupsFixture(t, true)
	templates := NewQualityRuleTemplateRepository(integrationEntClient, integrationDB)
	svc := service.ProvideScheduledTestService(nil, nil, templates, nil)
	preset, err := svc.CreateQualityPreset(ctx, &service.QualityRulePreset{
		Name: "Move to quarantine groups", ModelID: f.plan.ModelID, CronExpression: "*/30 * * * *", Enabled: true, MaxResults: 100, PelicanConfig: f.plan.PelicanConfig,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM quality_rule_presets WHERE id=$1`, preset.ID)
	})
	require.ElementsMatch(t, []int64{f.existingTarget, f.newTarget}, preset.PelicanConfig.Quality.MoveGroupIDs)
	reloaded := service.ProvideScheduledTestService(nil, nil, NewQualityRuleTemplateRepository(integrationEntClient, integrationDB), nil)
	checkPreset := func(want []int64) {
		t.Helper()
		presets, err := reloaded.ListQualityPresets(ctx)
		require.NoError(t, err)
		for _, item := range presets {
			if item.ID == preset.ID {
				require.Equal(t, service.QualityActionMoveGroups, item.PelicanConfig.Quality.Action)
				require.ElementsMatch(t, want, item.PelicanConfig.Quality.MoveGroupIDs)
				return
			}
		}
		t.Fatal("saved move-groups preset was not reloaded")
	}
	checkPreset([]int64{f.existingTarget, f.newTarget})
	preset.PelicanConfig.Quality.MoveGroupIDs = []int64{f.unrelated}
	_, err = svc.UpdateQualityPreset(ctx, preset.ID, preset)
	require.NoError(t, err)
	checkPreset([]int64{f.unrelated})

	template, err := templates.Create(ctx, &service.QualityRuleTemplate{
		AccountFilter: service.QualityRuleAccountFilter{Group: strconv.FormatInt(f.source, 10)},
		ModelID:       f.plan.ModelID, CronExpression: "*/30 * * * *", Enabled: true, MaxResults: 100, PelicanConfig: f.plan.PelicanConfig,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM quality_rule_templates WHERE id=$1`, template.ID)
	})
	require.ElementsMatch(t, []int64{f.existingTarget, f.newTarget}, template.PelicanConfig.Quality.MoveGroupIDs)
	template.PelicanConfig.Quality.MoveGroupIDs = []int64{f.unrelated}
	_, err = templates.Update(ctx, template)
	require.NoError(t, err)
	saved, err := NewQualityRuleTemplateRepository(integrationEntClient, integrationDB).Get(ctx, template.ID)
	require.NoError(t, err)
	require.Equal(t, service.QualityActionMoveGroups, saved.PelicanConfig.Quality.Action)
	require.Equal(t, []int64{f.unrelated}, saved.PelicanConfig.Quality.MoveGroupIDs)
}

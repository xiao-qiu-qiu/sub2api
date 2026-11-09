//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func beginQualityRequest(t *testing.T, accountID int64) context.Context {
	t.Helper()
	ctx, release := accountrequest.Default.Begin(context.Background(), accountID)
	t.Cleanup(release)
	return ctx
}

func requireQualityRequestActive(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("request unexpectedly canceled: %v", context.Cause(ctx))
	default:
	}
	require.NoError(t, ctx.Err())
	require.NoError(t, context.Cause(ctx))
}

func requireQualityRequestInterrupted(t *testing.T, ctx context.Context) {
	t.Helper()
	// Cancellation must be observable as soon as the repository call returns.
	select {
	case <-ctx.Done():
	default:
		t.Fatal("committed quarantine returned before canceling the in-flight request")
	}
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.ErrorIs(t, context.Cause(ctx), accountrequest.ErrQualityDegraded)
}

func TestQualityInterruptCommittedActions(t *testing.T) {
	for _, tc := range []struct{ action, applied string }{
		{service.QualityActionMoveGroups, "groups_moved"},
		{"remove_groups", "groups_removed"},
		{"disable_scheduling", "scheduling_disabled"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			f := newQualityMoveGroupsFixture(t, true)
			if tc.action != service.QualityActionMoveGroups {
				q := f.plan.PelicanConfig.Quality
				q.Action, q.MoveGroupIDs = tc.action, nil
				q.RemoveGroupIDs = []int64{f.source}
				updated, err := f.plans.Update(context.Background(), f.plan)
				require.NoError(t, err)
				f.plan = updated
			}
			original := f.memberships()
			var otherAccount int64
			require.NoError(t, integrationDB.QueryRowContext(context.Background(), `INSERT INTO accounts(name,platform,type,status,schedulable) VALUES('quality-interrupt-other','openai','apikey','active',true) RETURNING id`).Scan(&otherAccount))
			t.Cleanup(func() {
				_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM accounts WHERE id=$1`, otherAccount)
			})
			other := beginQualityRequest(t, otherAccount)
			requests := []context.Context{
				beginQualityRequest(t, f.account),
				beginQualityRequest(t, f.account),
				beginQualityRequest(t, f.account),
			}
			for _, ctx := range requests {
				requireQualityRequestActive(t, ctx)
			}

			require.Equal(t, tc.applied, f.apply("failed"))
			for _, ctx := range requests {
				requireQualityRequestInterrupted(t, ctx)
			}
			requireQualityRequestActive(t, other)
			require.Equal(t, tc.action, f.state().Action)
			switch tc.action {
			case service.QualityActionMoveGroups:
				moved := f.memberships()
				require.Len(t, moved, 2)
				require.Contains(t, moved, f.newTarget)
				require.Equal(t, original[f.existingTarget], moved[f.existingTarget])
			case "remove_groups":
				require.NotContains(t, f.memberships(), f.source)
			case "disable_scheduling":
				var schedulable bool
				require.NoError(t, integrationDB.QueryRowContext(context.Background(), `SELECT schedulable FROM accounts WHERE id=$1`, f.account).Scan(&schedulable))
				require.False(t, schedulable)
			}

			// Requests admitted to the new pool survive a repeated failed probe.
			fresh := beginQualityRequest(t, f.account)
			requireQualityRequestActive(t, fresh)
			before := f.snapshot()
			require.Equal(t, "already_quarantined", f.apply("failed"))
			require.JSONEq(t, before, f.snapshot())
			requireQualityRequestActive(t, fresh)
			requireQualityRequestActive(t, other)

			require.Equal(t, "restored", f.apply("passed"))
			require.Equal(t, original, f.memberships())
			requireQualityRequestActive(t, fresh)
			requireQualityRequestActive(t, other)
			requireQualityRequestActive(t, beginQualityRequest(t, f.account))
		})
	}
}

func TestQualityInterruptPassedMoveKeepsRequestsActive(t *testing.T) {
	f := independentRecoveryFixture(t, service.QualityActionMoveGroups)
	ctx := beginQualityRequest(t, f.account)
	require.Equal(t, "recovery_confirmation_pending", f.apply("passed"))
	requireQualityRequestActive(t, ctx)
	claimRecoveryProbe(t, f, true)
	require.Equal(t, "groups_moved", f.apply("passed"))
	require.Len(t, f.memberships(), 1)
	require.Contains(t, f.memberships(), f.unrelated)
	requireQualityRequestActive(t, ctx)
	requireQualityRequestActive(t, beginQualityRequest(t, f.account))
}

func TestQualityInterruptRolledBackMoveKeepsRequestsActive(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%t", deferred), func(t *testing.T) {
			f := newQualityMoveGroupsFixture(t, true)
			ctx := context.Background()
			name := fmt.Sprintf("quality_interrupt_rollback_%d", f.account)
			_, err := integrationDB.ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'synthetic quality interruption rollback'; END $$`, name))
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := integrationDB.ExecContext(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON scheduler_outbox`, name))
				require.NoError(t, err)
				_, err = integrationDB.ExecContext(ctx, fmt.Sprintf(`DROP FUNCTION %s()`, name))
				require.NoError(t, err)
			})
			trigger := fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON scheduler_outbox FOR EACH ROW WHEN (NEW.account_id=%d) EXECUTE FUNCTION %s()`, name, f.account, name)
			if deferred {
				// A deferred constraint trigger fails tx.Commit after all account,
				// membership and ownership writes have otherwise succeeded.
				trigger = fmt.Sprintf(`CREATE CONSTRAINT TRIGGER %s AFTER INSERT ON scheduler_outbox DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.account_id=%d) EXECUTE FUNCTION %s()`, name, f.account, name)
			}
			_, err = integrationDB.ExecContext(ctx, trigger)
			require.NoError(t, err)
			requests := []context.Context{beginQualityRequest(t, f.account), beginQualityRequest(t, f.account)}
			before := f.snapshot()
			_, err = f.plans.ApplyQualityOutcome(ctx, f.plan, f.until, "failed")
			require.ErrorContains(t, err, "synthetic quality interruption rollback")
			require.JSONEq(t, before, f.snapshot(), "failed writes and failed commits must roll back the entire quarantine")
			for _, request := range requests {
				requireQualityRequestActive(t, request)
			}
			requireQualityRequestActive(t, beginQualityRequest(t, f.account))
		})
	}
}

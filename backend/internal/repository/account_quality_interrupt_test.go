package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestQualityInterruptionOnlyForNewFailedAccountQuarantine(t *testing.T) {
	for i, tc := range []struct {
		name, outcome, action, actionOn string
		interrupted                     bool
	}{
		{name: "move", outcome: "failed", action: "groups_moved", interrupted: true},
		{name: "remove", outcome: "failed", action: "groups_removed", interrupted: true},
		{name: "disable", outcome: "failed", action: "scheduling_disabled", interrupted: true},
		{name: "recover", outcome: "passed", action: "groups_moved"},
		{name: "recovery rule", outcome: "failed", action: "groups_moved", actionOn: "passed"},
		{name: "unknown", outcome: "inconclusive", action: "groups_moved"},
		{name: "already quarantined", outcome: "failed", action: "already_quarantined"},
		{name: "conflict", outcome: "failed", action: "action_conflict"},
		{name: "stale", outcome: "failed", action: "stale_run"},
		{name: "observation", outcome: "failed", action: "observed"},
		{name: "individual model", outcome: "failed", action: "models_isolated"},
		{name: "BPS", outcome: "failed", action: "bps_enabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accountID := int64(9200101 + i*2)
			request, release := accountrequest.Default.Begin(context.Background(), accountID)
			defer release()
			other, releaseOther := accountrequest.Default.Begin(context.Background(), accountID+1)
			defer releaseOther()
			plan := &service.ScheduledTestPlan{ID: 7, AccountID: accountID,
				PelicanConfig: &service.PelicanTestConfig{Quality: &service.QualityPolicy{
					Action: service.QualityActionMoveGroups, ActionOn: tc.actionOn,
				}}}
			// A completed probe's cancellation cannot prevent the stop signal.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			interruptQualityRequests(ctx, plan, tc.outcome, tc.action, time.Now())
			if tc.interrupted {
				require.ErrorIs(t, context.Cause(request), accountrequest.ErrQualityDegraded)
			} else {
				require.NoError(t, request.Err())
			}
			require.NoError(t, other.Err())
		})
	}
}

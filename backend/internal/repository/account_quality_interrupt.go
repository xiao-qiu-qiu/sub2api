package repository

import (
	"context"
	"log/slog"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Only a committed, newly applied quarantine interrupts traffic. In particular,
// a recovery rule also returns groups_moved, and repeated failures in the
// quarantine pool must leave that pool's newly admitted requests alone.
func interruptQualityRequests(ctx context.Context, plan *service.ScheduledTestPlan, outcome, action string, before time.Time) {
	if plan == nil || plan.PelicanConfig == nil || plan.PelicanConfig.Quality == nil ||
		plan.PelicanConfig.Quality.RunsOnPassed() || outcome != "failed" {
		return
	}
	switch action {
	case "groups_moved", "groups_removed", "scheduling_disabled":
	default:
		return
	}
	// Completion of the probe or its save deadline must not suppress a committed
	// quarantine's stop signal. Local cancellation happens before Redis I/O.
	publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	count, err := accountrequest.Default.InterruptAt(publishCtx, plan.AccountID, before)
	if err != nil {
		slog.Warn("quality request interruption broadcast failed", "account_id", plan.AccountID, "plan_id", plan.ID, "error", err)
	}
	slog.Info("quality quarantine interrupted account requests", "account_id", plan.AccountID, "plan_id", plan.ID, "action", action, "local_requests", count)
}

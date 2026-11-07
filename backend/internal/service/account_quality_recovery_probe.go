package service

import (
	"fmt"
	"time"
)

func (q *QualityPolicy) RecoveryProbeActive() bool {
	return q != nil && (q.RunsOnPassed() || q.AutoRestore && q.RecoveryProbeEnabled && supportsQualityRecoveryProbe(q.Action))
}

// Empty action_on preserves the original failed-answer policy contract.
func (q *QualityPolicy) RunsOnPassed() bool { return q != nil && q.ActionOn == "passed" }

func supportsQualityRecoveryProbe(action string) bool {
	return action == QualityActionMoveGroups || action == "remove_groups" || action == "disable_scheduling"
}

func (q *QualityPolicy) RecoveryProbeInterval() time.Duration {
	minutes := q.RecoveryProbeIntervalMinutes
	if minutes == 0 {
		minutes = 60
	}
	return time.Duration(minutes) * time.Minute
}

func (q *QualityPolicy) RecoveryConfirmationInterval() time.Duration {
	minutes := q.RecoveryConfirmationMinutes
	if minutes == 0 {
		minutes = 5
	}
	return time.Duration(minutes) * time.Minute
}

func (q *QualityPolicy) validateRecoveryProbe() error {
	if q.ActionOn != "" && q.ActionOn != "failed" && q.ActionOn != "passed" {
		return fmt.Errorf("quality action_on must be failed or passed")
	}
	if q.RunsOnPassed() {
		if q.Action != QualityActionMoveGroups && q.Action != QualityActionEnableScheduling && q.Action != QualityActionObserveOnly {
			return fmt.Errorf("passed quality actions require move_groups, enable_scheduling or observe_only")
		}
		q.AutoRestore = false
		q.RecoveryProbeEnabled = true
		q.TriggerOnUpstream5xx = false
	}
	if q.RecoveryProbeEnabled && q.AutoRestore && !supportsQualityRecoveryProbe(q.Action) {
		return fmt.Errorf("recovery probes require move_groups, remove_groups or disable_scheduling with auto_restore")
	}
	if q.RecoveryProbeIntervalMinutes < 0 || q.RecoveryProbeIntervalMinutes > 10080 {
		return fmt.Errorf("recovery probe interval must be 0-10080 minutes (0 uses 60)")
	}
	if q.RecoveryConfirmationMinutes < 0 || q.RecoveryConfirmationMinutes > 10080 {
		return fmt.Errorf("recovery confirmation interval must be 0-10080 minutes (0 uses 5)")
	}
	if q.RecoveryProbeEnabled {
		if q.RecoveryProbeIntervalMinutes == 0 {
			q.RecoveryProbeIntervalMinutes = 60
		}
		if q.RecoveryConfirmationMinutes == 0 {
			q.RecoveryConfirmationMinutes = 5
		}
	}
	return nil
}

package service

import (
	"context"
	"errors"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"slices"
)

type AstraSchedulingActionResult struct {
	Changed bool
	Allowed bool
	Action  string
}
type AstraSchedulingActionRepository interface {
	ApplyAstraScheduling(context.Context, int64, config.AstraRoutingSettings, bool) (AstraSchedulingActionResult, error)
}

func applyAstraScheduling(ctx context.Context, repo AccountRepository, a *Account, s config.AstraRoutingSettings, ready bool) (AstraSchedulingActionResult, error) {
	if r, ok := repo.(AstraSchedulingActionRepository); ok {
		return r.ApplyAstraScheduling(ctx, a.ID, s, ready)
	}
	if s.EffectiveSchedulingMode() != "account" {
		return AstraSchedulingActionResult{}, errors.New("astra_scheduling_action_unavailable")
	}
	if a.Schedulable == ready {
		return AstraSchedulingActionResult{}, nil
	}
	err := repo.SetSchedulable(ctx, a.ID, ready)
	return AstraSchedulingActionResult{Changed: err == nil, Allowed: ready, Action: "account"}, err
}
func astraSchedulingAppliesToRequest(s config.AstraRoutingSettings, a *Account, model string, group int64) bool {
	if a.IsSIWCEnabled() {
		return false
	}
	switch s.EffectiveSchedulingMode() {
	case "model":
		return IsAstraSchedulingModel(model) || (a != nil && IsAstraSchedulingModel(a.GetMappedModel(model)))
	case "groups":
		return slices.Contains(s.SchedulingGroupIDs, group)
	default:
		return true
	}
}

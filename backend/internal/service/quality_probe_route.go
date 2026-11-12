package service

import (
	"context"
	"sync"
)

// The dispatcher may consult current persisted ownership before claiming a
// 5xx run. Claim and action still repeat the check under their own locks.
type qualityProbeRouteRepository interface {
	QualityProbeRouteMatches(context.Context, *ScheduledTestPlan) (bool, error)
}

func (s *ScheduledTestRunnerService) qualityProbeRouteMatches(ctx context.Context, plan *ScheduledTestPlan) (bool, error) {
	if repo, ok := s.planRepo.(qualityProbeRouteRepository); ok {
		return repo.QualityProbeRouteMatches(ctx, plan)
	}
	return true, nil
}

// QualityStateProbeMatchesAccount keeps ordinary native probes off effective
// BPS accounts, including models not selected for BPS. An enable_bps owner must
// still probe the native route to decide when its own fallback can be restored.
func QualityStateProbeMatchesAccount(account *Account, cfg *PelicanTestConfig) bool {
	return !isOpenAICodexStateProbePlan(cfg) || !account.IsExcelBPSEnabled() ||
		cfg.Quality != nil && (cfg.Quality.Action == QualityActionEnableBPS || cfg.BPSRecoveryPending)
}

// Observation never gains mutation rights during an upgrade. Only explicitly
// configured BPS actions require an effective, independently enabled BPS account.
func QualityProbeRequiresBPS(cfg *PelicanTestConfig) bool {
	return cfg != nil && cfg.TestChannel == "bps" && cfg.Quality != nil && cfg.Quality.Action != QualityActionObserveOnly
}

func QualityProbeMatchesAccount(account *Account, cfg *PelicanTestConfig) bool {
	if !QualityStateProbeMatchesAccount(account, cfg) {
		return false
	}
	if cfg != nil && cfg.TestChannel == "bps" && !account.IsExcelBPSEnabled() {
		return false
	}
	if !QualityProbeRequiresBPS(cfg) {
		return true
	}
	for _, model := range append(append([]string{cfg.ModelID}, cfg.ModelIDs...), cfg.Quality.RemoveModels...) {
		if model != "" && !account.IsExcelBPSEnabledForModel(model) {
			return false
		}
	}
	return true
}

type qualityTestRouteContextKey struct{}

type qualityTestRouteEvidence struct {
	mu          sync.Mutex
	route       string
	mappedModel string
}

func withQualityTestRouteEvidence(ctx context.Context) (context.Context, *qualityTestRouteEvidence) {
	evidence := &qualityTestRouteEvidence{}
	return context.WithValue(ctx, qualityTestRouteContextKey{}, evidence), evidence
}

func recordQualityTestRoute(ctx context.Context, route string, mappedModels ...string) {
	if evidence, ok := ctx.Value(qualityTestRouteContextKey{}).(*qualityTestRouteEvidence); ok {
		evidence.mu.Lock()
		evidence.route = route
		if len(mappedModels) > 0 {
			evidence.mappedModel = mappedModels[0]
		}
		evidence.mu.Unlock()
	}
}

func (e *qualityTestRouteEvidence) mapped() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mappedModel
}

func setQualityProbeModels(plan *ScheduledTestPlan, results []*ScheduledTestResult) {
	plan.QualityProbeModels = nil
	for _, result := range results {
		if result == nil || result.PelicanConfig == nil || result.PelicanConfig.TestMappedModel == "" {
			continue
		}
		cfg := result.PelicanConfig
		if plan.QualityProbeModels == nil {
			plan.QualityProbeModels = map[string]string{}
		}
		if previous, ok := plan.QualityProbeModels[cfg.ModelID]; ok && previous != cfg.TestMappedModel {
			plan.QualityProbeModels[cfg.ModelID] = "" // Mixed targets cannot authorize an action.
		} else {
			plan.QualityProbeModels[cfg.ModelID] = cfg.TestMappedModel
		}
	}
}

func (e *qualityTestRouteEvidence) value() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.route
}

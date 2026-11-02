//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func quality5xxStaleFailureState(t *testing.T, f *qualityBPSFixture) qualityState {
	t.Helper()
	var raw []byte
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `SELECT state FROM account_quality_states WHERE plan_id=$1`, f.plan.ID).Scan(&raw))
	var state qualityState
	require.NoError(t, json.Unmarshal(raw, &state))
	return state
}

func TestQuality5xxStaleFailureAfterNewEpisodeCoolsModels(t *testing.T) {
	f := newQualityBPSFixture(t, `{"unrelated":"kept"}`, &service.QualityPolicy{
		Action: service.QualityActionRemoveModel, TriggerOnUpstream5xx: true, AutoRestore: true,
		RemoveModels: []string{"gpt-6-astra", "gpt-6.1-sol"}, RecoveryConcurrency: 4,
	})
	f.exec(`UPDATE accounts SET concurrency=20 WHERE id=$1`)
	claimedEpisode := f.plan.Quality5xxEpisode
	require.NoError(t, (&accountRepository{sql: integrationDB}).ApplyQuality5xx(context.Background(), f.account))
	episode := quality5xxStaleFailureState(t, f).Quality5xxEpisode
	require.Greater(t, episode, claimedEpisode)
	require.Equal(t, claimedEpisode, f.plan.Quality5xxEpisode, "keep the probe's original episode; do not reclaim after the 5xx")
	require.NotContains(t, f.extra(), "model_rate_limits", "pending protection only lowers concurrency")

	// A single account-level state_degraded verdict applies to every target.
	require.Equal(t, "model_cooldown_refreshed", f.apply("failed"))
	extra := f.extra()
	limits, ok := extra["model_rate_limits"].(map[string]any)
	require.True(t, ok)
	require.Len(t, limits, 2)
	for _, model := range f.plan.PelicanConfig.Quality.RemoveModels {
		entry, ok := limits[model].(map[string]any)
		require.True(t, ok, "confirmed degradation must cool %s despite the newer episode", model)
		require.Equal(t, fmt.Sprintf("quality_rule:%d", f.plan.ID), entry["reason"])
		require.True(t, service.IsQualityModelCooldown(entry))
	}
	require.Equal(t, "kept", extra["unrelated"])
	state := quality5xxStaleFailureState(t, f)
	require.Equal(t, episode, state.Quality5xxEpisode, "applying an old failure must retain the new episode")
	require.Len(t, state.ModelApplied, 2)
	var concurrency int
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `SELECT concurrency FROM accounts WHERE id=$1`, f.account).Scan(&concurrency))
	require.Equal(t, 4, concurrency)
}

func TestQuality5xxStaleFailureMixedRoundCannotRestorePassedModel(t *testing.T) {
	f := newQualityBPSFixture(t, `{"unrelated":"kept"}`, &service.QualityPolicy{
		Action: service.QualityActionRemoveModel, TriggerOnUpstream5xx: true, AutoRestore: true,
		RemoveModels: []string{"gpt-6-astra", "gpt-6.1-sol"}, RecoveryConcurrency: 4,
	})
	f.exec(`UPDATE accounts SET concurrency=20 WHERE id=$1`)
	// Establish an owned cooldown on the peer that will later report passed.
	f.plan.QualityModelOutcomes = map[string]string{"gpt-6-astra": "skipped", "gpt-6.1-sol": "failed"}
	require.Equal(t, "models_cooled", f.apply("failed"))
	initial, ok := f.extra()["model_rate_limits"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, initial, "gpt-6.1-sol")
	require.NotContains(t, initial, "gpt-6-astra")

	require.NoError(t, (&accountRepository{sql: integrationDB}).ApplyQuality5xx(context.Background(), f.account))
	episode := quality5xxStaleFailureState(t, f).Quality5xxEpisode
	require.Greater(t, episode, f.plan.Quality5xxEpisode)
	// Supply raw passed evidence directly to the real repository: this must be
	// fenced under its locks even if a runner's Redis check saw no newer signal.
	f.plan.QualityModelOutcomes = map[string]string{"gpt-6-astra": "failed", "gpt-6.1-sol": "passed"}
	require.Equal(t, "model_cooldown_refreshed", f.apply("failed"))
	require.Equal(t, map[string]string{"gpt-6-astra": "failed", "gpt-6.1-sol": "inconclusive"}, f.plan.QualityModelOutcomes)
	extra := f.extra()
	limits, ok := extra["model_rate_limits"].(map[string]any)
	require.True(t, ok)
	require.Len(t, limits, 2)
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol"} {
		require.Contains(t, limits, model, "the newly failed model must cool and the stale passed peer must stay cooled")
		require.True(t, service.IsQualityModelCooldown(limits[model]))
	}
	state := quality5xxStaleFailureState(t, f)
	require.Equal(t, episode, state.Quality5xxEpisode)
	require.Contains(t, state.ModelApplied, "gpt-6-astra")
	require.Contains(t, state.ModelApplied, "gpt-6.1-sol", "retain ownership of the peer's cooldown")
	require.Equal(t, "kept", extra["unrelated"])
}

func TestQuality5xxStaleFailureOldPassedRemainsStale(t *testing.T) {
	f := newQualityBPSFixture(t, `{"unrelated":"kept"}`, &service.QualityPolicy{
		Action: service.QualityActionRemoveModel, TriggerOnUpstream5xx: true, AutoRestore: true,
		RemoveModels: []string{"gpt-6-astra"}, RecoveryConcurrency: 4,
	})
	f.exec(`UPDATE accounts SET concurrency=20 WHERE id=$1`)
	require.Equal(t, "models_cooled", f.apply("failed"))
	require.NoError(t, (&accountRepository{sql: integrationDB}).ApplyQuality5xx(context.Background(), f.account))
	beforeState := quality5xxStaleFailureState(t, f)
	require.Greater(t, beforeState.Quality5xxEpisode, f.plan.Quality5xxEpisode)
	beforeExtra := f.extra()
	limits, ok := beforeExtra["model_rate_limits"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, limits, "gpt-6-astra")
	f.plan.QualityModelOutcomes = map[string]string{"gpt-6-astra": "passed"}

	require.Equal(t, "stale_run", f.apply("passed"))
	require.Equal(t, beforeExtra, f.extra(), "an old pass must not release cooldowns or arm concurrency recovery")
	require.Equal(t, beforeState, quality5xxStaleFailureState(t, f), "reject the stale pass without rewriting the new episode or ownership")
	var concurrency int
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `SELECT concurrency FROM accounts WHERE id=$1`, f.account).Scan(&concurrency))
	require.Equal(t, 4, concurrency)
}

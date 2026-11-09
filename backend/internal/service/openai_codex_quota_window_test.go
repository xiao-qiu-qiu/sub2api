package service

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExtractOpenAICodexUsageWindow(t *testing.T) {
	t.Parallel()
	type expected struct {
		used   float64
		source string
	}
	for _, tc := range []struct {
		name  string
		extra map[string]any
		five  expected
		seven expected
	}{
		{name: "missing snapshot"},
		{
			name:  "legacy canonical without durations",
			extra: map[string]any{"codex_5h_used_percent": 11.0, "codex_7d_used_percent": 22.0},
			five:  expected{11, "5h"}, seven: expected{22, "7d"},
		},
		{
			name:  "legacy raw without any durations",
			extra: map[string]any{"codex_primary_used_percent": 22.0, "codex_secondary_used_percent": 11.0},
			five:  expected{11, "secondary"}, seven: expected{22, "primary"},
		},
		{
			name:  "canonical durations decoded from JSON and strings",
			extra: map[string]any{"codex_5h_used_percent": 11.0, "codex_5h_window_minutes": json.Number("300"), "codex_7d_used_percent": 22.0, "codex_7d_window_minutes": "10080"},
			five:  expected{11, "5h"}, seven: expected{22, "7d"},
		},
		{
			name:  "raw current order",
			extra: map[string]any{"codex_primary_used_percent": 11.0, "codex_primary_window_minutes": 300, "codex_secondary_used_percent": 22.0, "codex_secondary_window_minutes": 10080},
			five:  expected{11, "primary"}, seven: expected{22, "secondary"},
		},
		{
			name:  "raw reversed order",
			extra: map[string]any{"codex_primary_used_percent": 22.0, "codex_primary_window_minutes": 10080, "codex_secondary_used_percent": 11.0, "codex_secondary_window_minutes": 300},
			five:  expected{11, "secondary"}, seven: expected{22, "primary"},
		},
		{
			name:  "image daily and zero windows",
			extra: map[string]any{"codex_primary_used_percent": 1.0, "codex_primary_window_minutes": 1440, "codex_secondary_used_percent": 0.0, "codex_secondary_window_minutes": 0},
		},
		{
			name:  "image daily window with missing partner",
			extra: map[string]any{"codex_primary_used_percent": 1.0, "codex_primary_window_minutes": 1440},
		},
		{
			name:  "explicit zero does not activate legacy mapping",
			extra: map[string]any{"codex_primary_used_percent": 22.0, "codex_primary_window_minutes": 0, "codex_secondary_used_percent": 11.0},
		},
		{
			name:  "null is explicit unknown duration",
			extra: map[string]any{"codex_primary_used_percent": 22.0, "codex_primary_window_minutes": nil, "codex_secondary_used_percent": 11.0},
		},
		{
			name:  "malformed duration does not activate legacy mapping",
			extra: map[string]any{"codex_primary_used_percent": 22.0, "codex_primary_window_minutes": "invalid", "codex_secondary_used_percent": 11.0},
		},
		{
			name:  "missing weekly partner is not inferred",
			extra: map[string]any{"codex_primary_used_percent": 11.0, "codex_primary_window_minutes": 300, "codex_secondary_used_percent": 22.0},
			five:  expected{11, "primary"},
		},
		{
			name:  "missing five hour partner is not inferred",
			extra: map[string]any{"codex_primary_used_percent": 11.0, "codex_secondary_used_percent": 22.0, "codex_secondary_window_minutes": 10080},
			seven: expected{22, "secondary"},
		},
		{
			name:  "duplicate five hour slots rejected",
			extra: map[string]any{"codex_primary_used_percent": 11.0, "codex_primary_window_minutes": 300, "codex_secondary_used_percent": 22.0, "codex_secondary_window_minutes": 300},
		},
		{
			name:  "duplicate weekly slots rejected",
			extra: map[string]any{"codex_primary_used_percent": 11.0, "codex_primary_window_minutes": 10080, "codex_secondary_used_percent": 22.0, "codex_secondary_window_minutes": 10080},
		},
		{
			name:  "fractional duration is not truncated",
			extra: map[string]any{"codex_primary_used_percent": 11.0, "codex_primary_window_minutes": 300.5, "codex_secondary_used_percent": 22.0, "codex_secondary_window_minutes": 10080},
			seven: expected{22, "secondary"},
		},
		{
			name:  "invalid canonical cannot fall back to legacy raw",
			extra: map[string]any{"codex_5h_used_percent": 11.0, "codex_5h_window_minutes": 0, "codex_7d_used_percent": 22.0, "codex_7d_window_minutes": 1440, "codex_primary_used_percent": 22.0, "codex_secondary_used_percent": 11.0},
		},
		{
			name:  "invalid canonical cannot fall back to recognized raw",
			extra: map[string]any{"codex_7d_used_percent": 1.0, "codex_7d_window_minutes": 1440, "codex_primary_used_percent": 22.0, "codex_primary_window_minutes": 10080, "codex_secondary_used_percent": 11.0, "codex_secondary_window_minutes": 300},
			five:  expected{11, "secondary"},
		},
		{
			name:  "valid canonical retained despite unrelated image raw fields",
			extra: map[string]any{"codex_5h_used_percent": 11.0, "codex_5h_window_minutes": 300, "codex_7d_used_percent": 22.0, "codex_7d_window_minutes": 10080, "codex_primary_used_percent": 1.0, "codex_primary_window_minutes": 1440, "codex_secondary_used_percent": 0.0, "codex_secondary_window_minutes": 0},
			five:  expected{11, "5h"}, seven: expected{22, "7d"},
		},
		{
			name:  "nonfinite percentages are not quota evidence",
			extra: map[string]any{"codex_5h_used_percent": math.NaN(), "codex_7d_used_percent": math.Inf(1)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for window, want := range map[string]expected{"5h": tc.five, "7d": tc.seven} {
				used, source, ok := extractOpenAICodexUsageWindow(tc.extra, window)
				require.Equal(t, want.source != "", ok, "window %s", window)
				if ok {
					require.Equal(t, want.source, source)
					require.Equal(t, want.used, used)
				}
			}
		})
	}
}

func TestOpenAICodexQuotaReadersValidatePersistedDurations(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name        string
		fiveMinutes any
		weekMinutes any
		valid       bool
	}{
		{"recognized windows", 300, 10080, true},
		{"image windows", 0, 1440, false},
		{"swapped canonical durations", 10080, 300, false},
		{"fractional durations", 300.5, 10080.5, false},
		{"null durations", nil, nil, false},
		{"malformed durations", "bad", "bad", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra := map[string]any{
				"codex_5h_used_percent": 100.0, "codex_5h_window_minutes": tc.fiveMinutes,
				"codex_7d_used_percent": 100.0, "codex_7d_window_minutes": tc.weekMinutes,
				"codex_5h_reset_at":      now.Add(time.Hour).Format(time.RFC3339),
				"codex_7d_reset_at":      now.Add(24 * time.Hour).Format(time.RFC3339),
				"codex_usage_updated_at": now.Format(time.RFC3339),
				// A raw legacy fallback must not restore an invalid canonical window.
				"codex_primary_used_percent": 100.0, "codex_secondary_used_percent": 100.0,
			}
			account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: extra}
			usage := &UsageInfo{FiveHour: &UsageProgress{Utilization: 3}, SevenDay: &UsageProgress{Utilization: 4}}
			applyExtraToUsage(usage, extra, now)
			require.Equal(t, tc.valid, usage.FiveHour != nil)
			require.Equal(t, tc.valid, usage.SevenDay != nil)
			five, seven := openAICanonicalQuotaWindows(extra, now)
			require.Equal(t, tc.valid, five.hasUsed)
			require.Equal(t, tc.valid, seven.hasUsed)
			for _, window := range []string{"5h", "7d"} {
				_, ok := resolveOpenAIQuotaUtilization(extra, window, now)
				require.Equal(t, tc.valid, ok)
				_, ok = openAICodexWindowResetAt(extra, window)
				require.Equal(t, tc.valid, ok)
			}
			_, hasReset := openAISchedulingResetWindowEnd(account, now)
			require.Equal(t, tc.valid, hasReset)
			require.Equal(t, tc.valid, qualityProbeQuotaExhausted(account, now))
			decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenAI: 90}, now)
			require.Equal(t, tc.valid, decision.ShouldPause)
			wantFactor := openAIQuotaHeadroomNeutralFactor
			if tc.valid {
				wantFactor = 0
			}
			require.Equal(t, wantFactor, openAIQuotaHeadroomFactor(account, now))
		})
	}
}

func TestOpenAICodexQuotaReadersPairRawResetWithCorrectWindow(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	extra := map[string]any{
		"codex_primary_used_percent": 100.0, "codex_primary_window_minutes": 300,
		"codex_secondary_used_percent": 100.0, "codex_secondary_window_minutes": 10080,
		"codex_primary_reset_at":   now.Add(-time.Minute).Format(time.RFC3339),
		"codex_secondary_reset_at": now.Add(time.Hour).Format(time.RFC3339),
		"codex_usage_updated_at":   now.Format(time.RFC3339),
	}
	usage := &UsageInfo{}
	applyExtraToUsage(usage, extra, now)
	require.NotNil(t, usage.FiveHour)
	require.Zero(t, usage.FiveHour.Utilization)
	require.NotNil(t, usage.SevenDay)
	require.Equal(t, 100.0, usage.SevenDay.Utilization)
	_, fiveActive := resolveOpenAIQuotaUtilization(extra, "5h", now)
	_, sevenActive := resolveOpenAIQuotaUtilization(extra, "7d", now)
	require.False(t, fiveActive)
	require.True(t, sevenActive)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: extra}
	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenAI: 90}, now)
	require.True(t, decision.ShouldPause)
	require.Equal(t, "7d", decision.Window)
	require.True(t, qualityProbeQuotaExhausted(account, now))
}

func TestGetOpenAIUsageInvalidWindowsDoesNotInventQuotaFromStats(t *testing.T) {
	t.Parallel()
	svc := &AccountUsageService{usageLogRepo: &usageBatchLogRepoStub{}}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		"codex_5h_used_percent": 0.0, "codex_5h_window_minutes": 0,
		"codex_7d_used_percent": 1.0, "codex_7d_window_minutes": 1440,
	}}
	usage, err := svc.getOpenAIUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.Nil(t, usage.FiveHour)
	require.Nil(t, usage.SevenDay)
}

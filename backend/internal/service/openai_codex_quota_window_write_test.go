package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNormalizeCodexQuotaWindowDurations(t *testing.T) {
	ptr := func(n int) *int { return &n }
	primaryUsed, secondaryUsed := 1.0, 96.0
	for _, tt := range []struct {
		name               string
		primary, secondary *int
		fiveHour, sevenDay *float64
	}{
		{name: "image daily and unused slot", primary: ptr(1440), secondary: ptr(0)},
		{name: "image daily only", primary: ptr(1440)},
		{name: "zero is not a five hour window", primary: ptr(0)},
		{name: "negative is unknown", primary: ptr(-300)},
		{name: "weekly primary", primary: ptr(10080), sevenDay: &primaryUsed},
		{name: "standard order", primary: ptr(300), secondary: ptr(10080), fiveHour: &primaryUsed, sevenDay: &secondaryUsed},
		{name: "reversed order", primary: ptr(10080), secondary: ptr(300), fiveHour: &secondaryUsed, sevenDay: &primaryUsed},
		{name: "daily beside real five hour", primary: ptr(1440), secondary: ptr(300), fiveHour: &secondaryUsed},
		{name: "daily beside real weekly", primary: ptr(1440), secondary: ptr(10080), sevenDay: &secondaryUsed},
		{name: "missing duration beside known window", primary: ptr(300), fiveHour: &primaryUsed},
		{name: "duplicate weekly slots are ambiguous", primary: ptr(10080), secondary: ptr(10080)},
		{name: "legacy without durations", fiveHour: &secondaryUsed, sevenDay: &primaryUsed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := &OpenAICodexUsageSnapshot{
				PrimaryUsedPercent: &primaryUsed, PrimaryWindowMinutes: tt.primary,
				SecondaryUsedPercent: &secondaryUsed, SecondaryWindowMinutes: tt.secondary,
			}
			got := snapshot.Normalize()
			require.NotNil(t, got)
			require.Equal(t, tt.fiveHour, got.Used5hPercent)
			require.Equal(t, tt.sevenDay, got.Used7dPercent)
		})
	}
}

func TestCodexWeeklyQuotaSurvivesImageDailyHeaders(t *testing.T) {
	observed := time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC)
	weekly := make(http.Header)
	weekly.Set("x-codex-primary-used-percent", "96")
	weekly.Set("x-codex-primary-window-minutes", "10080")
	weekly.Set("x-codex-primary-reset-after-seconds", "86400")
	snapshot := ParseCodexRateLimitHeaders(weekly)
	snapshot.UpdatedAt = observed.Format(time.RFC3339Nano)
	stored := buildCodexUsageExtraUpdates(snapshot, observed)
	require.Equal(t, 96.0, stored["codex_7d_used_percent"])

	daily := make(http.Header)
	daily.Set("x-codex-primary-used-percent", "1")
	daily.Set("x-codex-primary-window-minutes", "1440")
	daily.Set("x-codex-primary-reset-after-seconds", "72586")
	daily.Set("x-codex-secondary-used-percent", "0")
	daily.Set("x-codex-secondary-window-minutes", "0")
	daily.Set("x-codex-secondary-reset-after-seconds", "0")
	imageSnapshot := ParseCodexRateLimitHeaders(daily)
	imageSnapshot.UpdatedAt = observed.Add(time.Minute).Format(time.RFC3339Nano)
	updates := buildCodexUsageExtraUpdates(imageSnapshot, observed.Add(time.Minute))
	require.Empty(t, updates, "image quota must not replace raw fallback, weekly quota, reset or freshness")
	for key, value := range updates {
		stored[key] = value
	}
	require.Equal(t, 96.0, stored["codex_7d_used_percent"])
	require.Equal(t, 10080, stored["codex_7d_window_minutes"])
	require.Equal(t, observed.Add(24*time.Hour).Format(time.RFC3339), stored["codex_7d_reset_at"])
	require.Equal(t, observed.Format(time.RFC3339Nano), stored["codex_usage_updated_at"])

	weekly.Set("x-codex-primary-used-percent", "97")
	nextSnapshot := ParseCodexRateLimitHeaders(weekly)
	nextSnapshot.UpdatedAt = observed.Add(2 * time.Minute).Format(time.RFC3339Nano)
	next := buildCodexUsageExtraUpdates(nextSnapshot, observed)
	require.Equal(t, 97.0, next["codex_7d_used_percent"], "subsequent real weekly observations must still update")
}

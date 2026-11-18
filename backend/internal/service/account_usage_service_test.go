package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

type accountUsageCodexProbeRepo struct {
	stubOpenAIAccountRepo
	updateExtraCh chan map[string]any
	rateLimitCh   chan time.Time
}

func (r *accountUsageCodexProbeRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	if r.updateExtraCh != nil {
		copied := make(map[string]any, len(updates))
		for k, v := range updates {
			copied[k] = v
		}
		r.updateExtraCh <- copied
	}
	return nil
}

func (r *accountUsageCodexProbeRepo) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	if r.rateLimitCh != nil {
		r.rateLimitCh <- resetAt
	}
	return nil
}

func TestShouldRefreshOpenAICodexSnapshot(t *testing.T) {
	t.Parallel()

	rateLimitedUntil := time.Now().Add(5 * time.Minute)
	now := time.Now()
	usage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 0},
		SevenDay: &UsageProgress{Utilization: 0},
	}

	if !shouldRefreshOpenAICodexSnapshot(&Account{RateLimitResetAt: &rateLimitedUntil}, usage, now) {
		t.Fatal("expected rate-limited account to force codex snapshot refresh")
	}

	if shouldRefreshOpenAICodexSnapshot(&Account{}, usage, now) {
		t.Fatal("expected complete non-rate-limited usage to skip codex snapshot refresh")
	}

	if !shouldRefreshOpenAICodexSnapshot(&Account{}, &UsageInfo{FiveHour: nil, SevenDay: &UsageProgress{}}, now) {
		t.Fatal("expected missing 5h snapshot to require refresh")
	}

	staleAt := now.Add(-(openAIProbeCacheTTL + time.Minute)).Format(time.RFC3339)
	if !shouldRefreshOpenAICodexSnapshot(&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"openai_oauth_responses_websockets_v2_enabled": true,
			"codex_usage_updated_at":                       staleAt,
		},
	}, usage, now) {
		t.Fatal("expected stale ws snapshot to trigger refresh")
	}
}

// TestShouldRefreshOpenAICodexSnapshot_SparkShadowIgnoresWSv2 外审第9轮 P1:spark 影子用量走
// QueryUsage(/wham/usage,与 WSv2 无关),staleness 不得被 WSv2 门控,否则首刷后窗口永久冻结。
func TestShouldRefreshOpenAICodexSnapshot_SparkShadowIgnoresWSv2(t *testing.T) {
	t.Parallel()

	now := time.Now()
	usage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 0},
		SevenDay: &UsageProgress{Utilization: 0},
	}
	staleAt := now.Add(-(openAIProbeCacheTTL + time.Minute)).Format(time.RFC3339)
	freshAt := now.Add(-time.Minute).Format(time.RFC3339)
	parentID := int64(7001)

	// 影子无 WSv2,但首刷后窗口已存在;过期 codex_usage_updated_at 必须触发再刷新。
	shadowStale := &Account{
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
		Extra:           map[string]any{"codex_usage_updated_at": staleAt},
	}
	if !shouldRefreshOpenAICodexSnapshot(shadowStale, usage, now) {
		t.Fatal("expected stale spark shadow (no WSv2) to trigger refresh")
	}

	// 影子时间戳仍新鲜→不刷(TTL 生效)。
	shadowFresh := &Account{
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
		Extra:           map[string]any{"codex_usage_updated_at": freshAt},
	}
	if shouldRefreshOpenAICodexSnapshot(shadowFresh, usage, now) {
		t.Fatal("expected fresh spark shadow to skip refresh (TTL not elapsed)")
	}

	// 反向对照:普通账号无 WSv2 + 过期时间戳→仍不刷(WSv2 门控普通账号的 probe 刷新)。
	normalNoWS := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"codex_usage_updated_at": staleAt},
	}
	if shouldRefreshOpenAICodexSnapshot(normalNoWS, usage, now) {
		t.Fatal("expected non-WSv2 normal account to skip codex probe refresh")
	}
}

func TestExtractOpenAICodexProbeUpdatesAccepts429WithCodexHeaders(t *testing.T) {
	t.Parallel()

	headers := make(http.Header)
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")
	headers.Set("x-codex-secondary-used-percent", "100")
	headers.Set("x-codex-secondary-reset-after-seconds", "18000")
	headers.Set("x-codex-secondary-window-minutes", "300")

	updates, err := extractOpenAICodexProbeUpdates(&http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})
	if err != nil {
		t.Fatalf("extractOpenAICodexProbeUpdates() error = %v", err)
	}
	if len(updates) == 0 {
		t.Fatal("expected codex probe updates from 429 headers")
	}
	if got := updates["codex_5h_used_percent"]; got != 100.0 {
		t.Fatalf("codex_5h_used_percent = %v, want 100", got)
	}
	if got := updates["codex_7d_used_percent"]; got != 100.0 {
		t.Fatalf("codex_7d_used_percent = %v, want 100", got)
	}
}

func TestAccountUsageService_PersistOpenAICodexProbeSnapshotOnlyUpdatesExtra(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{
		updateExtraCh: make(chan map[string]any, 1),
		rateLimitCh:   make(chan time.Time, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	svc.persistOpenAICodexProbeSnapshot(321, map[string]any{
		"codex_7d_used_percent": 100.0,
		"codex_7d_reset_at":     time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339),
	}, nil)

	select {
	case updates := <-repo.updateExtraCh:
		if got := updates["codex_7d_used_percent"]; got != 100.0 {
			t.Fatalf("codex_7d_used_percent = %v, want 100", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待 codex 探测快照写入 extra 超时")
	}

	select {
	case got := <-repo.rateLimitCh:
		t.Fatalf("不应将探测快照写入运行时限流状态: %v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAccountUsageService_GetOpenAIUsage_DoesNotPromoteCodexExtraToRateLimit(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(6 * 24 * time.Hour).UTC().Truncate(time.Second)
	repo := &accountUsageCodexProbeRepo{
		rateLimitCh: make(chan time.Time, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"codex_5h_used_percent": 1.0,
			"codex_5h_reset_at":     time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339),
			"codex_7d_used_percent": 100.0,
			"codex_7d_reset_at":     resetAt.Format(time.RFC3339),
		},
	}

	usage, err := svc.getOpenAIUsage(context.Background(), account, false)
	if err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}
	if usage.SevenDay == nil || usage.SevenDay.Utilization != 100.0 {
		t.Fatalf("预期 7 天用量仍然可见，实际为 %#v", usage.SevenDay)
	}
	if account.RateLimitResetAt != nil {
		t.Fatalf("不应让已耗尽的 codex extra 改写运行时限流状态: %v", account.RateLimitResetAt)
	}
	select {
	case got := <-repo.rateLimitCh:
		t.Fatalf("不应将已耗尽的 codex extra 持久化为运行时限流状态: %v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAccountUsageService_GetOpenAIUsage_PreservesSnapshotObservationTime(t *testing.T) {
	t.Parallel()
	observedAt := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	for _, stamp := range []string{observedAt.Format(time.RFC3339), "", "invalid", time.Time{}.Format(time.RFC3339)} {
		t.Run(stamp, func(t *testing.T) {
			account := &Account{
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Extra: map[string]any{
					"codex_5h_used_percent":  10.0,
					"codex_7d_used_percent":  100.0,
					"codex_usage_updated_at": stamp,
				},
			}
			usage, err := (&AccountUsageService{}).getOpenAIUsage(context.Background(), account, false)
			require.NoError(t, err)
			if stamp == observedAt.Format(time.RFC3339) {
				require.NotNil(t, usage.UpdatedAt)
				require.True(t, usage.UpdatedAt.Equal(observedAt))
			} else {
				require.Nil(t, usage.UpdatedAt)
			}
			require.Equal(t, "passive", usage.Source)
			require.Empty(t, usage.Error)
			require.Empty(t, usage.ErrorCode)
		})
	}
}

func TestAccountUsageService_GetOpenAIUsage_RefreshFailureKeepsCachedDataAndReportsError(t *testing.T) {
	t.Parallel()
	observedAt := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	for _, accountType := range []string{"native", "shadow", "bps"} {
		t.Run(accountType, func(t *testing.T) {
			resetAt := time.Now().Add(time.Hour)
			account := &Account{
				Platform:         PlatformOpenAI,
				Type:             AccountTypeOAuth,
				RateLimitResetAt: &resetAt,
				Extra: map[string]any{
					"codex_7d_used_percent":  100.0,
					"codex_usage_updated_at": observedAt.Format(time.RFC3339),
				},
			}
			if accountType == "shadow" {
				parentID := int64(10)
				account.ParentAccountID = &parentID
				account.QuotaDimension = QuotaDimensionSpark
			} else if accountType == "bps" {
				account.Extra["openai_excel_bps"] = true
			}
			usage, err := (&AccountUsageService{}).getOpenAIUsage(context.Background(), account, true)
			require.NoError(t, err)
			require.Equal(t, errorCodeQuotaRefreshFailed, usage.ErrorCode)
			require.NotEmpty(t, usage.Error)
			require.NotNil(t, usage.SevenDay)
			require.Equal(t, 100.0, usage.SevenDay.Utilization)
			require.NotNil(t, usage.UpdatedAt)
			require.True(t, usage.UpdatedAt.Equal(observedAt))
			require.Equal(t, "passive", usage.Source)
			require.Equal(t, &resetAt, account.RateLimitResetAt)
			require.Equal(t, observedAt.Format(time.RFC3339), account.Extra["codex_usage_updated_at"])
		})
	}
}

type quotaFreshnessStatsRepo struct {
	usageBatchLogRepoStub
	accountIDs []int64
	starts     []time.Time
}

func (repo *quotaFreshnessStatsRepo) GetAccountWindowStats(_ context.Context, accountID int64, start time.Time) (*usagestats.AccountStats, error) {
	repo.accountIDs = append(repo.accountIDs, accountID)
	repo.starts = append(repo.starts, start)
	return &usagestats.AccountStats{Requests: 2, Tokens: 100, Cost: 3, StandardCost: 4, UserCost: 5}, nil
}

func TestAccountUsageService_GetOpenAIUsage_RefreshFailureWithoutSnapshot(t *testing.T) {
	t.Parallel()
	repo := &quotaFreshnessStatsRepo{}
	svc := &AccountUsageService{usageLogRepo: repo}
	before := time.Now()
	usage, err := svc.getOpenAIUsage(context.Background(), &Account{
		ID:       123,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
	}, true)
	after := time.Now()
	require.NoError(t, err)
	require.Equal(t, errorCodeQuotaRefreshFailed, usage.ErrorCode)
	require.Nil(t, usage.UpdatedAt)
	require.Equal(t, []int64{123, 123}, repo.accountIDs)
	for index, window := range []*UsageProgress{usage.FiveHour, usage.SevenDay} {
		require.NotNil(t, window)
		require.True(t, window.UtilizationUnknown)
		require.Equal(t, &WindowStats{Requests: 2, Tokens: 100, Cost: 3, StandardCost: 4, UserCost: 5}, window.WindowStats)
		duration := []time.Duration{5 * time.Hour, 7 * 24 * time.Hour}[index]
		require.False(t, repo.starts[index].Before(before.Add(-duration)))
		require.False(t, repo.starts[index].After(after.Add(-duration)))
	}
}

func TestAccountUsageService_PersistOpenAICodexProbeResponse_IgnoresMetadataOnly(t *testing.T) {
	t.Parallel()
	repo := &accountUsageCodexProbeRepo{updateExtraCh: make(chan map[string]any, 1)}
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}
	response.Header.Set("x-codex-primary-window-minutes", "300")
	response.Header.Set("x-codex-primary-reset-after-seconds", "100")
	updates, err := (&AccountUsageService{accountRepo: repo}).persistOpenAICodexProbeResponse(123, response, nil)
	require.NoError(t, err)
	require.Nil(t, updates)
	select {
	case <-repo.updateExtraCh:
		t.Fatal("metadata without utilization must not persist a fresh observation")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBuildCodexUsageProgressFromExtra_ZerosExpiredWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)

	t.Run("expired 5h window zeroes utilization", func(t *testing.T) {
		extra := map[string]any{
			"codex_5h_used_percent": 42.0,
			"codex_5h_reset_at":     "2026-03-16T10:00:00Z", // 2h ago
		}
		progress := buildCodexUsageProgressFromExtra(extra, "5h", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 0 {
			t.Fatalf("expected Utilization=0 for expired window, got %v", progress.Utilization)
		}
		if progress.RemainingSeconds != 0 {
			t.Fatalf("expected RemainingSeconds=0, got %v", progress.RemainingSeconds)
		}
	})

	t.Run("active 5h window keeps utilization", func(t *testing.T) {
		resetAt := now.Add(2 * time.Hour).Format(time.RFC3339)
		extra := map[string]any{
			"codex_5h_used_percent": 42.0,
			"codex_5h_reset_at":     resetAt,
		}
		progress := buildCodexUsageProgressFromExtra(extra, "5h", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 42.0 {
			t.Fatalf("expected Utilization=42, got %v", progress.Utilization)
		}
	})

	t.Run("expired 7d window zeroes utilization", func(t *testing.T) {
		extra := map[string]any{
			"codex_7d_used_percent": 88.0,
			"codex_7d_reset_at":     "2026-03-15T00:00:00Z", // yesterday
		}
		progress := buildCodexUsageProgressFromExtra(extra, "7d", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 0 {
			t.Fatalf("expected Utilization=0 for expired 7d window, got %v", progress.Utilization)
		}
	})
}

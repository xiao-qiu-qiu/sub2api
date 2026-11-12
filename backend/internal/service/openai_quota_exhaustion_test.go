package service

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenAIExhaustedQuotaWindow(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(24 * time.Hour)
	for _, tc := range []struct {
		name   string
		change func(*Account)
		want   bool
		window string
	}{
		{name: "weekly quota with no pause threshold", want: true, window: "7d"},
		{name: "explicit early pause opt out", change: func(a *Account) { a.Extra["auto_pause_7d_disabled"] = true }, want: true, window: "7d"},
		{name: "below quota", change: func(a *Account) { a.Extra["codex_7d_used_percent"] = 99.0 }},
		{name: "expired reset", change: func(a *Account) { a.Extra["codex_7d_reset_at"] = now.Format(time.RFC3339) }},
		{name: "unknown reset", change: func(a *Account) { delete(a.Extra, "codex_7d_reset_at") }},
		{name: "invalid window duration", change: func(a *Account) { a.Extra["codex_7d_window_minutes"] = 1440 }},
		{name: "invalid utilization", change: func(a *Account) { a.Extra["codex_7d_used_percent"] = math.Inf(1) }},
		{name: "API key relay snapshot", change: func(a *Account) { a.Type = AccountTypeAPIKey }},
		{name: "another platform", change: func(a *Account) { a.Platform = PlatformAnthropic }},
		{name: "quota belongs to another email", change: func(a *Account) {
			a.Credentials = map[string]any{"email": "current@example.com"}
			a.Extra["email"] = "previous@example.com"
		}},
		{name: "quota belongs to another account", change: func(a *Account) {
			a.Credentials = map[string]any{"chatgpt_account_id": "current-account"}
			a.Extra["chatgpt_account_id"] = "previous-account"
		}},
		{name: "quota belongs to another workspace", change: func(a *Account) {
			a.Credentials = map[string]any{"workspace_id": "current-workspace"}
			a.Extra["workspace_id"] = "previous-workspace"
		}},
		{name: "old snapshot with valid future reset", change: func(a *Account) { a.Extra["codex_usage_updated_at"] = now.Add(-24 * time.Hour).Format(time.RFC3339) }, want: true, window: "7d"},
		{name: "reset after anchored at observation", change: func(a *Account) {
			delete(a.Extra, "codex_7d_reset_at")
			a.Extra["codex_7d_reset_after_seconds"] = 24 * 60 * 60
		}, want: true, window: "7d"},
		{name: "expired relative reset does not slide", change: func(a *Account) {
			delete(a.Extra, "codex_7d_reset_at")
			a.Extra["codex_7d_reset_after_seconds"] = 60
			a.Extra["codex_usage_updated_at"] = now.Add(-2 * time.Minute).Format(time.RFC3339)
		}},
		{name: "raw reversed windows keep matching reset", change: func(a *Account) {
			a.Extra = map[string]any{
				"codex_primary_used_percent": 0.0, "codex_primary_window_minutes": 300,
				"codex_secondary_used_percent": 100.0, "codex_secondary_window_minutes": 10080,
				"codex_primary_reset_at":   now.Add(-time.Minute).Format(time.RFC3339),
				"codex_secondary_reset_at": reset.Format(time.RFC3339),
			}
		}, want: true, window: "7d"},
		{name: "five hour quota", change: func(a *Account) {
			a.Extra = map[string]any{"codex_5h_used_percent": 100.0, "codex_5h_window_minutes": 300, "codex_5h_reset_at": reset.Format(time.RFC3339)}
		}, want: true, window: "5h"},
		{name: "both windows choose later reset", change: func(a *Account) {
			a.Extra["codex_5h_used_percent"] = 100.0
			a.Extra["codex_5h_reset_at"] = now.Add(time.Hour).Format(time.RFC3339)
		}, want: true, window: "7d"},
		{name: "shadow owns its own quota", change: func(a *Account) { id := int64(10); a.ParentAccountID = &id; a.QuotaDimension = QuotaDimensionSpark }, want: true, window: "7d"},
		{name: "managed aggregate does not own grant quota", change: func(a *Account) { a.Extra["openai_connections_managed"] = true }},
		{name: "projected grant still inherits aggregate quota", change: func(a *Account) {
			a.Extra["openai_connections_managed"] = true
			a.AuthConnectionID = "grant"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
				"codex_7d_used_percent": 100.0, "codex_7d_window_minutes": 10080,
				"codex_7d_reset_at": reset.Format(time.RFC3339), "codex_usage_updated_at": now.Format(time.RFC3339),
				"codex_primary_used_percent": 0.0,
			}}
			if tc.change != nil {
				tc.change(account)
			}
			window, until, exhausted := openAIExhaustedQuotaWindow(account, now)
			require.Equal(t, tc.want, exhausted)
			require.Equal(t, tc.window, window)
			if tc.want {
				require.Equal(t, reset, until)
			} else {
				require.True(t, until.IsZero())
			}
		})
	}
}

func TestOpenAIQuotaExhaustionBypassesOptionalPauseThresholds(t *testing.T) {
	for _, window := range []string{"5h", "7d"} {
		t.Run(window, func(t *testing.T) {
			account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
				"codex_" + window + "_used_percent":  100.0,
				"codex_" + window + "_reset_at":      time.Now().Add(time.Hour).Format(time.RFC3339),
				"auto_pause_" + window + "_disabled": true,
			}}
			paused, decision := shouldAutoPauseOpenAIAccountByQuota(context.Background(), account)
			require.True(t, paused)
			require.Equal(t, "quota_exhausted_"+window, decision.reason)
		})
	}
}

func TestOpenAIQuotaExhaustionSkipsStickyAccountWithoutThreshold(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		for _, window := range []string{"5h", "7d"} {
			t.Run(strconv.FormatBool(advanced)+"/"+window, func(t *testing.T) {
				resetOpenAIAdvancedSchedulerSettingCacheForTest()
				t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
				groupID := int64(101301)
				accounts := []Account{
					{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 2, GroupIDs: []int64{groupID}, Extra: map[string]any{"openai_excel_bps": true}},
					{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 2, GroupIDs: []int64{groupID}, Extra: map[string]any{"openai_excel_bps": true}},
				}
				cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:quota-session": 1}}
				svc := &OpenAIGatewayService{
					accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cache: cache, cfg: &config.Config{},
					rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(strconv.FormatBool(advanced)),
					concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
				}
				a := &accounts[0]
				a.Extra["codex_"+window+"_used_percent"] = 100.0
				a.Extra["codex_"+window+"_reset_at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
				a.Extra["auto_pause_"+window+"_disabled"] = true
				ctx := WithOpenAIAccountCooldownBudget(t.Context())
				selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "quota-session", "gpt-6-astra", nil, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.NotNil(t, selection)
				if selection.ReleaseFunc != nil {
					defer selection.ReleaseFunc()
				}
				require.Equal(t, int64(2), selection.Account.ID)
				require.True(t, a.Schedulable, "the manual scheduling switch remains independently controlled")
			})
		}
	}
}

func TestOpenAIQuotaExhaustionPreviousResponsePreservesBinding(t *testing.T) {
	ctx := t.Context()
	groupID := int64(23)
	account := Account{ID: 77, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 2, Extra: map[string]any{
		"openai_oauth_responses_websockets_v2_enabled": true,
		"codex_7d_used_percent":                        100.0, "codex_7d_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	}}
	cache := &stubGatewayCache{}
	store := NewOpenAIWSStateStore(cache)
	svc := &OpenAIGatewayService{
		accountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}, cache: cache,
		cfg: newOpenAIWSV2TestConfig(), concurrencyService: NewConcurrencyService(stubConcurrencyCache{}), openaiWSStateStore: store,
	}
	require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_quota_exhausted", account.ID, time.Hour))
	selection, err := svc.SelectAccountByPreviousResponseID(ctx, &groupID, "resp_quota_exhausted", "gpt-6-astra", nil, false)
	require.NoError(t, err)
	require.Nil(t, selection)
	bound, err := store.GetResponseAccount(ctx, groupID, "resp_quota_exhausted")
	require.NoError(t, err)
	require.Equal(t, account.ID, bound)
}

func TestOpenAIQuotaExhaustionDoesNotWaitForShortBPSCooldown(t *testing.T) {
	account := Account{ID: 88, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Extra: map[string]any{
		"openai_excel_bps":      true,
		"codex_7d_used_percent": 100.0, "codex_7d_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	}}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{account}}}
	svc.excelBPSCooldownUntil.Store(account.ID, time.Now().Add(5*time.Second))
	ctx := WithOpenAIAccountCooldownBudget(t.Context())
	require.False(t, svc.QueueOpenAIAccountCooldown(ctx, account.ID, "gpt-6-astra"))
	waited, err := svc.WaitForOpenAIAccountCooldown(ctx)
	require.NoError(t, err)
	require.False(t, waited)
}

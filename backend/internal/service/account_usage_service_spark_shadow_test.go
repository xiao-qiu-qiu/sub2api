package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// sparkShadowUsageTestRepo is a minimal AccountRepository stub for spark shadow
// usage tests.  GetByID serves both shadow and parent accounts from a map;
// UpdateExtra records the persisted updates for assertion.
type sparkShadowUsageTestRepo struct {
	AccountRepository
	accounts      map[int64]*Account
	updateExtraCh chan map[string]any
}

func (r *sparkShadowUsageTestRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if acc, ok := r.accounts[id]; ok {
		return acc, nil
	}
	return nil, fmt.Errorf("account %d not found", id)
}

func (r *sparkShadowUsageTestRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	if r.updateExtraCh != nil {
		copied := make(map[string]any, len(updates))
		for k, v := range updates {
			copied[k] = v
		}
		r.updateExtraCh <- copied
	}
	return nil
}

// TestGetOpenAIUsage_SparkShadow_WritesExtraAndReturnsNonEmptyWindows covers
// two assertions required by Task 3.2:
//
// A) After getOpenAIUsage on a spark shadow account the shadow row's
// Extra["codex_5h_used_percent"] is persisted, and the upstream call carried
// the PARENT account's chatgpt-account-id (not the shadow's empty one).
//
// B) (P1-b regression guard) The UsageInfo RETURNED by the same call has
// non-nil FiveHour AND SevenDay windows — proving that the rebuild happened
// and not just the DB write.
func TestGetOpenAIUsage_SparkShadow_WritesExtraAndReturnsNonEmptyWindows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	pid := int64(100)
	shadow := &Account{
		ID:              200,
		ParentAccountID: &pid,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		Status:          StatusActive,
		QuotaDimension:  QuotaDimensionSpark,
	}
	parent := &Account{
		ID:       100,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			"chatgpt_account_id": "org-spark-parent",
		},
	}

	// Repo shared by both the OpenAIQuotaService (needs shadow+parent for resolve)
	// and the AccountUsageService (needs UpdateExtra for persist).
	updateExtraCh := make(chan map[string]any, 1)
	repo := &sparkShadowUsageTestRepo{
		accounts:      map[int64]*Account{200: shadow, 100: parent},
		updateExtraCh: updateExtraCh,
	}

	// Token cache: return a fake token for the parent account key.
	tokenCache := &stubQuotaTokenCache{tokens: map[string]string{
		OpenAITokenCacheKey(parent): "fake-access-token",
	}}
	tokenProvider := NewOpenAITokenProvider(repo, tokenCache, nil)

	// httptest server: records the chatgpt-account-id header and returns a
	// synthetic OpenAIQuotaUsage with codex_bengalfox 5h+7d windows.
	var capturedAccountID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAccountID = r.Header.Get("chatgpt-account-id")
		w.Header().Set("content-type", "application/json")
		resp := OpenAIQuotaUsage{
			AdditionalRateLimits: []OpenAIAdditionalRateLimit{
				{
					MeteredFeature: "codex_bengalfox",
					RateLimit: &OpenAIRateLimit{
						// Primary window → 5h (18000 s = 300 min)
						PrimaryWindow: &OpenAIRateLimitWindow{
							UsedPercent:        42.5,
							ResetAfterSeconds:  3600,
							LimitWindowSeconds: 18000,
						},
						// Secondary window → 7d (604800 s = 10080 min)
						SecondaryWindow: &OpenAIRateLimitWindow{
							UsedPercent:        10.0,
							ResetAfterSeconds:  86400,
							LimitWindowSeconds: 604800,
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	quotaService := NewOpenAIQuotaService(repo, nil, tokenProvider, newQuotaRedirectingFactory(srv), nil)
	svc := &AccountUsageService{
		accountRepo:        repo,
		openAIQuotaService: quotaService,
	}

	usage, err := svc.getOpenAIUsage(ctx, shadow, true /*force*/)
	require.NoError(t, err)

	// Assertion A-1: upstream received the PARENT's chatgpt-account-id.
	require.Equal(t, "org-spark-parent", capturedAccountID,
		"QueryUsage must use parent's chatgpt-account-id for spark shadow accounts")

	// Assertion A-2: shadow Extra was persisted with codex_5h_used_percent.
	select {
	case updates := <-updateExtraCh:
		require.Contains(t, updates, "codex_5h_used_percent",
			"persisted extra must contain codex_5h_used_percent")
		require.InDelta(t, 42.5, updates["codex_5h_used_percent"], 0.01,
			"codex_5h_used_percent must match the upstream value")
	case <-time.After(2 * time.Second):
		t.Fatal("UpdateExtra was not called within timeout — spark shadow persist did not happen")
	}

	// Assertion B (P1-b regression guard): returned UsageInfo must have
	// non-nil windows. This FAILS if the code only writes Extra without
	// rebuilding the returned UsageInfo.
	require.NotNil(t, usage.FiveHour,
		"returned UsageInfo.FiveHour must be non-nil (rebuild from merged Extra must happen)")
	require.NotNil(t, usage.SevenDay,
		"returned UsageInfo.SevenDay must be non-nil (rebuild from merged Extra must happen)")
	require.Equal(t, "active", usage.Source)
	require.Empty(t, usage.Error)
	require.NotNil(t, usage.UpdatedAt)
	observedAt, err := time.Parse(time.RFC3339Nano, shadow.Extra["codex_usage_updated_at"].(string))
	require.NoError(t, err)
	require.True(t, usage.UpdatedAt.Equal(observedAt))
}

func TestGetOpenAIUsage_QuotaEndpoint_FailedOrEmptyQueryDoesNotRefreshSnapshot(t *testing.T) {
	t.Parallel()
	for _, accountType := range []string{"shadow", "bps"} {
		for _, scenario := range []struct {
			name   string
			status int
			body   string
		}{
			{"expired token", http.StatusUnauthorized, `{"error":{"code":"token_expired"}}`},
			{"empty quota", http.StatusOK, `{}`},
			{"empty windows", http.StatusOK, `{"additional_rate_limits":[{"metered_feature":"codex_bengalfox","rate_limit":{}}]}`},
		} {
			t.Run(accountType+"/"+scenario.name, func(t *testing.T) {
				t.Parallel()
				observedAt := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
				parentID := int64(100)
				parent := &Account{ID: parentID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive}
				account := &Account{
					ID: 200, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
					Extra: map[string]any{
						"codex_7d_used_percent":  100.0,
						"codex_usage_updated_at": observedAt.Format(time.RFC3339),
					},
				}
				tokenAccount := account
				if accountType == "shadow" {
					account.ParentAccountID = &parentID
					account.QuotaDimension = QuotaDimensionSpark
					tokenAccount = parent
				} else {
					account.Extra["openai_excel_bps"] = true
				}
				repo := &sparkShadowUsageTestRepo{
					accounts:      map[int64]*Account{100: parent, 200: account},
					updateExtraCh: make(chan map[string]any, 1),
				}
				tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(tokenAccount): "fake-access-token"}}
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
					writer.Header().Set("Content-Type", "application/json")
					writer.WriteHeader(scenario.status)
					_, _ = writer.Write([]byte(scenario.body))
				}))
				defer server.Close()
				svc := &AccountUsageService{
					accountRepo:        repo,
					openAIQuotaService: NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, tokens, nil), newQuotaRedirectingFactory(server), nil),
				}
				usage, err := svc.getOpenAIUsage(context.Background(), account, true)
				require.NoError(t, err)
				require.Equal(t, errorCodeQuotaRefreshFailed, usage.ErrorCode)
				require.NotEmpty(t, usage.Error)
				require.Equal(t, "passive", usage.Source)
				require.NotNil(t, usage.UpdatedAt)
				require.True(t, usage.UpdatedAt.Equal(observedAt))
				require.NotNil(t, usage.SevenDay)
				require.Equal(t, 100.0, usage.SevenDay.Utilization)
				require.Equal(t, observedAt.Format(time.RFC3339), account.Extra["codex_usage_updated_at"])
				select {
				case <-repo.updateExtraCh:
					t.Fatal("failed or empty queries must not persist a fresh snapshot")
				case <-time.After(100 * time.Millisecond):
				}
			})
		}
	}
}

func TestGetOpenAIUsage_BPS_UsesQuotaOAuthAndReturnsObservationTime(t *testing.T) {
	t.Parallel()
	account := &Account{
		ID: 201, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{"chatgpt_account_id": "bps-quota-account"},
		Extra:       map[string]any{"openai_excel_bps": true},
	}
	repo := &sparkShadowUsageTestRepo{
		accounts:      map[int64]*Account{201: account},
		updateExtraCh: make(chan map[string]any, 1),
	}
	tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "quota-oauth-token"}}
	var authorization, upstreamAccount string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		upstreamAccount = request.Header.Get("chatgpt-account-id")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":12,"reset_after_seconds":3600,"limit_window_seconds":18000},"secondary_window":{"used_percent":34,"reset_after_seconds":86400,"limit_window_seconds":604800}}}`))
	}))
	defer server.Close()
	svc := &AccountUsageService{
		accountRepo:        repo,
		openAIQuotaService: NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, tokens, nil), newQuotaRedirectingFactory(server), nil),
	}
	usage, err := svc.getOpenAIUsage(context.Background(), account, true)
	require.NoError(t, err)
	require.Equal(t, "Bearer quota-oauth-token", authorization)
	require.Equal(t, "bps-quota-account", upstreamAccount)
	require.Equal(t, "active", usage.Source)
	require.Empty(t, usage.ErrorCode)
	require.NotNil(t, usage.FiveHour)
	require.NotNil(t, usage.SevenDay)
	require.Equal(t, 12.0, usage.FiveHour.Utilization)
	require.Equal(t, 34.0, usage.SevenDay.Utilization)
	observedAt, err := time.Parse(time.RFC3339Nano, account.Extra["codex_usage_updated_at"].(string))
	require.NoError(t, err)
	require.NotNil(t, usage.UpdatedAt)
	require.True(t, usage.UpdatedAt.Equal(observedAt))
}

package service

import (
	"context"
	"maps"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIQualityAdmissionTurnFreshDB(t *testing.T) {
	for _, mode := range []string{"business pending", "false marker pending", "internal probe pending", "internal probe rate limited", "fresh checked evidence"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			selected := ticketTestAccount(42001)
			require.True(t, selected.IsSchedulable(), "the selected account represents an earlier eligible snapshot")
			latest := *selected
			latest.Extra = maps.Clone(selected.Extra)
			if latest.Extra == nil {
				latest.Extra = map[string]any{}
			}
			latest.Extra[QualityProbeAdmissionKey] = map[string]any{"enabled": true, "required_after": now.Add(-time.Minute).Format(time.RFC3339Nano)}
			ctx := context.Background()
			wantAllowed := false
			switch mode {
			case "false marker pending":
				ctx = context.WithValue(ctx, qualityProbeContextKey{}, false)
			case "internal probe pending":
				ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
				wantAllowed = true
			case "internal probe rate limited":
				ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
				until := now.Add(time.Hour)
				latest.RateLimitResetAt = &until
			case "fresh checked evidence":
				qualityAdmissionTestChecked(&latest, now.Add(-30*time.Second), now.Add(-time.Second))
				wantAllowed = true
			}
			repo := &turnAdmissionRepo{account: &latest}
			svc := &OpenAIGatewayService{accountRepo: repo}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			admitted, err := svc.admitOpenAITurn(ctx, c, selected, "gpt-6-astra")
			if wantAllowed {
				require.NoError(t, err)
				require.NotNil(t, admitted)
				require.Equal(t, latest.ID, admitted.ID)
			} else {
				require.True(t, IsOpenAITurnAdmissionError(err), "pending admission must reject before the upstream send: %v", err)
				require.Nil(t, admitted)
			}
			require.Equal(t, 1, repo.reads, "pre-send admission must read authoritative DB state")
			require.NotContains(t, selected.Extra, QualityProbeAdmissionKey, "admission must not mutate the earlier selected snapshot")
		})
	}
}

func qualityGateSchedulerTestService(t *testing.T, accounts []Account, advanced, snapshot bool) (*OpenAIGatewayService, *schedulerTestGatewayCache) {
	t.Helper()
	enabled := "false"
	if advanced {
		enabled = "true"
	}
	limits := newOpenAIAdvancedSchedulerRateLimitService(enabled)
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	cache := &schedulerTestGatewayCache{}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:       cache, cfg: cfg, rateLimitService: limits,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	if snapshot {
		stale := &openAISnapshotCacheStub{accountsByID: make(map[int64]*Account)}
		for i := range accounts {
			account := accounts[i]
			// The stale snapshot predates admission; never share Extra with DB.
			account.Extra = nil
			stale.snapshotAccounts = append(stale.snapshotAccounts, &account)
			stale.accountsByID[account.ID] = &account
		}
		svc.schedulerSnapshot = &SchedulerSnapshotService{cache: stale}
	}
	return svc, cache
}

func TestOpenAIAccountSchedulerQualityGate(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		strategy := "legacy"
		if advanced {
			strategy = "advanced"
		}
		t.Run(strategy, func(t *testing.T) {
			for _, snapshot := range []bool{false, true} {
				source := "repository"
				if snapshot {
					source = "stale snapshot"
				}
				t.Run(source, func(t *testing.T) {
					for _, sticky := range []bool{false, true} {
						path := "fresh selection"
						if sticky {
							path = "session sticky"
						}
						t.Run(path, func(t *testing.T) {
							ctx := context.Background()
							groupID := int64(42000)
							now := time.Now()
							primary := qualityAdmissionTestAccount(now.Add(-time.Minute))
							primary.GroupIDs = []int64{groupID}
							backup := *primary
							backup.ID, backup.Priority, backup.Extra = primary.ID+1, 5, nil
							svc, _ := qualityGateSchedulerTestService(t, []Account{*primary, backup}, advanced, snapshot)
							hash := ""
							if sticky {
								hash = "quality-admission-pending"
								require.NoError(t, svc.setStickySessionAccountID(ctx, &groupID, hash, primary.ID, time.Hour))
							}
							selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", hash, "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
							require.NoError(t, err)
							require.NotNil(t, selection)
							if selection.ReleaseFunc != nil {
								defer selection.ReleaseFunc()
							}
							require.NotNil(t, selection.Account)
							require.Equal(t, backup.ID, selection.Account.ID, "pending admission must survive sticky lookup and stale snapshot DB recheck")

							qualityAdmissionTestChecked(primary, now.Add(-30*time.Second), now.Add(-time.Second))
							selection, _, err = svc.SelectAccountWithScheduler(ctx, &groupID, "", "", "gpt-5.1", map[int64]struct{}{backup.ID: {}}, OpenAIUpstreamTransportAny, false)
							require.NoError(t, err)
							require.NotNil(t, selection)
							if selection.ReleaseFunc != nil {
								defer selection.ReleaseFunc()
							}
							require.NotNil(t, selection.Account)
							require.Equal(t, primary.ID, selection.Account.ID, "fresh completed evidence should restore eligibility")
						})
					}
				})
			}
		})
	}
}

func TestOpenAIAccountSchedulerQualityGateNoEligibleAccount(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		name := "legacy"
		if advanced {
			name = "advanced"
		}
		t.Run(name, func(t *testing.T) {
			groupID := int64(42000)
			account := qualityAdmissionTestAccount(time.Now().Add(-time.Minute))
			account.GroupIDs = []int64{groupID}
			svc, _ := qualityGateSchedulerTestService(t, []Account{*account}, advanced, false)
			selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
			if selection != nil && selection.ReleaseFunc != nil {
				defer selection.ReleaseFunc()
			}
			require.Error(t, err, "an all-pending pool must not fail open")
			require.Nil(t, selection)
		})
	}
}

func TestOpenAIAccountSchedulerQualityGatePreviousResponse(t *testing.T) {
	ctx := context.Background()
	groupID := int64(42000)
	account := qualityAdmissionTestAccount(time.Now().Add(-time.Minute))
	account.GroupIDs = []int64{groupID}
	svc, _ := qualityGateSchedulerTestService(t, []Account{*account}, true, true)
	svc.cfg = newSchedulerTestOpenAIWSV2Config()
	const responseID = "resp_quality_admission_pending"
	require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(ctx, groupID, responseID, account.ID, time.Hour))
	selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, responseID, "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	if selection != nil && selection.ReleaseFunc != nil {
		defer selection.ReleaseFunc()
	}
	require.Error(t, err, "previous_response_id must not bypass pending quality admission")
	require.Nil(t, selection)
}

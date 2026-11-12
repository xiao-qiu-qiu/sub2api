package service

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func manualAdmissionAccountState(t *testing.T, account *Account) string {
	t.Helper()
	state, err := json.Marshal(account)
	require.NoError(t, err)
	return string(state)
}

func TestManualAccountTestContextIsExplicitAndScoped(t *testing.T) {
	ordinary := context.Background()
	probe := context.WithValue(ordinary, qualityProbeContextKey{}, true)
	manual := WithManualAccountTest(ordinary)
	require.False(t, isManualAccountTest(ordinary))
	require.False(t, isManualAccountTest(probe))
	require.True(t, isManualAccountTest(manual))
	require.True(t, isManualAccountTest(context.WithValue(manual, qualityProbeContextKey{}, true)))
	require.Nil(t, manual.Value(qualityProbeContextKey{}), "manual admission is independent of the quality-probe marker")

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/accounts/1/test", nil)
	c.Request.Header.Set("X-Manual-Account-Test", "true")
	require.False(t, isManualAccountTest(c.Request.Context()), "client headers cannot create the server-owned marker")
}

func TestManualAccountTestAdmissionBypassesOnlyLocalGates(t *testing.T) {
	const model = "gpt-6-astra"
	future, past := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name             string
		change           func(*Account, *OpenAIGatewayService)
		wantReason       string
		qualityProbePass bool
	}{
		{"unschedulable", func(a *Account, _ *OpenAIGatewayService) { a.Schedulable = false }, "account_ineligible", false},
		{"status_error", func(a *Account, _ *OpenAIGatewayService) { a.Status = StatusError }, "account_ineligible", false},
		{"status_disabled", func(a *Account, _ *OpenAIGatewayService) { a.Status = StatusDisabled }, "account_ineligible", false},
		{"expired", func(a *Account, _ *OpenAIGatewayService) {
			a.ExpiresAt, a.AutoPauseOnExpired = &past, true
		}, "account_ineligible", false},
		{"quota_exhausted", func(a *Account, _ *OpenAIGatewayService) {
			a.Extra["quota_limit"], a.Extra["quota_used"] = 10.0, 10.0
		}, "account_ineligible", false},
		{"global_rate_limit", func(a *Account, _ *OpenAIGatewayService) { a.RateLimitResetAt = &future }, "account_ineligible", false},
		{"overloaded", func(a *Account, _ *OpenAIGatewayService) { a.OverloadUntil = &future }, "account_ineligible", false},
		{"temporary_stop", func(a *Account, _ *OpenAIGatewayService) {
			a.TempUnschedulableUntil, a.TempUnschedulableReason = &future, "upstream_transport"
		}, "account_ineligible", false},
		{"performance_paused", func(a *Account, _ *OpenAIGatewayService) { a.PerformancePaused = true }, "account_ineligible", false},
		{"account_runtime_block", func(a *Account, s *OpenAIGatewayService) {
			s.openaiAccountRuntimeBlockUntil.Store(a.ID, future)
		}, "account_runtime_blocked", false},
		{"model_transient_block", func(a *Account, s *OpenAIGatewayService) {
			s.recordOpenAIAccountModelTransientFailure(a, model, time.Now())
			s.recordOpenAIAccountModelTransientFailure(a, model, time.Now())
		}, "model_runtime_blocked", false},
		{"persisted_model_rate_limit", func(a *Account, _ *OpenAIGatewayService) {
			a.Extra[modelRateLimitsKey] = map[string]any{model: map[string]any{"rate_limit_reset_at": future.Format(time.RFC3339)}}
		}, "model_rate_limited", false},
		{"quality_isolation", func(a *Account, _ *OpenAIGatewayService) {
			a.Extra[QualityModelIsolationKey] = map[string]any{model: map[string]any{"reason": "quality_rule:7"}}
		}, "model_runtime_blocked", true},
		{"quality_pending", func(a *Account, _ *OpenAIGatewayService) {
			a.Extra[QualityProbeAdmissionKey] = map[string]any{"enabled": true, "required_after": past.Format(time.RFC3339)}
		}, "account_ineligible", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := ticketTestAccount(9918201)
			if tc.name == "quota_exhausted" {
				selected.Type = AccountTypeAPIKey
			}
			selected.Extra = map[string]any{}
			latest := *selected
			latest.Extra = maps.Clone(selected.Extra)
			repo := &turnAdmissionRepo{account: &latest}
			s := &OpenAIGatewayService{accountRepo: repo}
			tc.change(&latest, s)
			selectedBefore := manualAdmissionAccountState(t, selected)
			latestBefore := manualAdmissionAccountState(t, &latest)

			for _, kind := range []string{"manual", "ordinary", "quality_probe_only"} {
				t.Run(kind, func(t *testing.T) {
					ctx := context.Background()
					switch kind {
					case "manual":
						ctx = WithManualAccountTest(ctx)
					case "quality_probe_only":
						ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
					}
					got, err := s.AdmitOpenAITurn(ctx, nil, selected, model)
					if kind == "manual" || kind == "quality_probe_only" && tc.qualityProbePass {
						require.NoError(t, err)
						require.Same(t, &latest, got)
					} else {
						var denied *OpenAITurnAdmissionError
						require.ErrorAs(t, err, &denied)
						require.Equal(t, tc.wantReason, denied.Reason)
						require.Nil(t, got)
					}
					require.JSONEq(t, selectedBefore, manualAdmissionAccountState(t, selected))
					require.JSONEq(t, latestBefore, manualAdmissionAccountState(t, &latest), "diagnostics must not clear persistent gates")
				})
			}
			require.Equal(t, 3, repo.reads, "manual tests still read authoritative state; ordinary requests remain blocked afterwards")
		})
	}
}

func TestManualAccountTestAdmissionRetainsAuthoritativeIdentityChecks(t *testing.T) {
	for _, name := range []string{"selected_nil", "deleted", "db_error", "wrong_account", "parent_binding", "platform", "type", "route", "group_membership"} {
		t.Run(name, func(t *testing.T) {
			selected := ticketTestAccount(9918202)
			selected.GroupIDs = []int64{9}
			latest := *selected
			latest.Credentials = maps.Clone(selected.Credentials)
			repo := &turnAdmissionRepo{account: &latest}
			wantReason, wantReads := "account_binding_changed", 1
			switch name {
			case "selected_nil":
				selected = nil
				wantReason, wantReads = "account_unavailable", 0
			case "deleted":
				// The authoritative repository omits soft-deleted accounts.
				repo.account = nil
				wantReason = "latest_state_unavailable"
			case "db_error":
				repo.err = errors.New("database unavailable")
				wantReason = "latest_state_unavailable"
			case "wrong_account":
				latest.ID++
				wantReason = "latest_state_unavailable"
			case "parent_binding":
				repo.parent = ticketTestAccount(9918203)
				latest.ParentAccountID = &repo.parent.ID
			case "platform":
				latest.Platform = PlatformKimi
			case "type":
				latest.Type = AccountTypeAPIKey
			case "route":
				latest.Credentials["base_url"] = "https://changed.example.invalid"
			case "group_membership":
				latest.GroupIDs = []int64{10}
				wantReason = "group_membership_changed"
			}
			s := &OpenAIGatewayService{accountRepo: repo}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			groupID := int64(9)
			c.Set("api_key", &APIKey{GroupID: &groupID})
			got, err := s.AdmitOpenAITurn(WithManualAccountTest(context.Background()), c, selected, "gpt-6-astra")
			var denied *OpenAITurnAdmissionError
			require.ErrorAs(t, err, &denied)
			require.Equal(t, wantReason, denied.Reason)
			require.Nil(t, got)
			require.Equal(t, wantReads, repo.reads)
		})
	}
}

func TestManualAccountTestAdmissionUsesLatestCredentialsWithoutMutatingState(t *testing.T) {
	selected := ticketTestAccount(9918204)
	selected.Schedulable = false
	selected.Status, selected.ErrorMessage = StatusError, "retain diagnostic state"
	latest := *selected
	latest.Credentials = maps.Clone(selected.Credentials)
	latest.Credentials["access_token"] = "fresh-token"
	latest.Credentials["refresh_token"] = "fresh-refresh"
	latest.Credentials["_token_version"] = int64(2)
	repo := &turnAdmissionRepo{account: &latest}
	s := &OpenAIGatewayService{accountRepo: repo}
	ctx := WithManualAccountTest(context.Background())
	selectedBefore, latestBefore := manualAdmissionAccountState(t, selected), manualAdmissionAccountState(t, &latest)

	got, err := s.AdmitOpenAITurn(ctx, nil, selected, "gpt-6-astra")
	require.NoError(t, err)
	require.Same(t, &latest, got)
	require.Equal(t, "fresh-token", got.GetOpenAIAccessToken())
	require.JSONEq(t, selectedBefore, manualAdmissionAccountState(t, selected))
	require.JSONEq(t, latestBefore, manualAdmissionAccountState(t, &latest))

	rotated := latest
	rotated.Credentials = maps.Clone(latest.Credentials)
	rotated.Credentials["access_token"] = "rotated-again"
	rotated.Credentials["_token_version"] = int64(3)
	repo.account = &rotated
	got, err = s.AdmitOpenAITurn(ctx, nil, selected, "gpt-6-astra")
	require.NoError(t, err)
	require.Same(t, &rotated, got)
	require.Equal(t, "rotated-again", got.GetOpenAIAccessToken())
	require.Equal(t, 2, repo.reads)
}

func TestManualAccountTestAdmissionHonorsCanceledContext(t *testing.T) {
	for _, markerFirst := range []bool{false, true} {
		name := "marker_after_cancel_context"
		if markerFirst {
			name = "marker_before_cancel_context"
		}
		t.Run(name, func(t *testing.T) {
			base := context.Background()
			if markerFirst {
				base = WithManualAccountTest(base)
			}
			ctx, cancel := context.WithCancel(base)
			defer cancel()
			if !markerFirst {
				ctx = WithManualAccountTest(ctx)
			}
			cancel()
			account := ticketTestAccount(9918205)
			repo := &turnAdmissionRepo{account: account}
			s := &OpenAIGatewayService{accountRepo: repo}
			got, err := s.AdmitOpenAITurn(ctx, nil, account, "gpt-6-astra")
			require.ErrorIs(t, err, context.Canceled)
			require.Nil(t, got)
			require.Zero(t, repo.reads)
		})
	}
}

func TestManualAccountTestBypassesImageModelCooldown(t *testing.T) {
	account := ticketTestAccount(9918208)
	account.Extra = map[string]any{modelRateLimitsKey: map[string]any{
		"gpt-image-2": map[string]any{"rate_limit_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339)},
	}}
	ordinary := WithOpenAIImageGenerationIntent(context.Background())
	manual := WithManualAccountTest(ordinary)
	require.Equal(t, "image_model_rate_limited", openAIImageRequestRejection(ordinary, account, "gpt-image-2"))
	require.Empty(t, openAIImageRequestRejection(manual, account, "gpt-image-2"))
	require.Equal(t, "image_model_rate_limited", openAIImageRequestRejection(ordinary, account, "gpt-image-2"), "the cooldown remains installed for business requests")
}

type manualAdmissionPriorityRepo struct {
	*turnAdmissionRepo
	listReads int
	listErr   error
}

func (r *manualAdmissionPriorityRepo) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]Account, error) {
	r.listReads++
	return nil, r.listErr
}

func TestManualAccountTestSkipsPriorityLatencyAdmissionReads(t *testing.T) {
	cfg := DefaultPrioritySchedulingConfig()
	cfg.Enabled, cfg.LatencyPenaltyMode = true, PriorityLatencyStop
	groupID := int64(9)
	model := "gpt-6-astra"
	require.True(t, priorityHardStopApplies(cfg, OpenAIAccountScheduleRequest{
		Platform: PlatformOpenAI, GroupID: &groupID, RequestedModel: model, UseUpstreamTokenCost: true,
	}))
	for _, kind := range []string{"manual", "ordinary", "quality_probe_only"} {
		t.Run(kind, func(t *testing.T) {
			readErr := errors.New("latency pool read failed")
			account := ticketTestAccount(9918206)
			repo := &manualAdmissionPriorityRepo{turnAdmissionRepo: &turnAdmissionRepo{account: account}, listErr: readErr}
			s := priorityGateway(cfg, &priorityReaderStub{})
			s.accountRepo = repo
			ctx := WithPrioritySchedulingObservation(context.Background(), &groupID, model, "/v1/responses")
			switch kind {
			case "manual":
				ctx = WithManualAccountTest(ctx)
			case "quality_probe_only":
				ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
			}
			got, err := s.beginPriorityLatencyTurn(ctx, nil, account, model)
			if kind == "manual" {
				require.NoError(t, err)
				require.Same(t, ctx, got)
				require.Nil(t, got.Value(priorityLatencyTurnKey{}))
				require.Zero(t, repo.listReads)
			} else {
				require.ErrorIs(t, err, readErr)
				require.Equal(t, 1, repo.listReads)
			}
			require.Zero(t, repo.reads)
		})
	}
}

func TestManualAccountTestIgnoresQualityInterruptButHonorsClientCancel(t *testing.T) {
	account := ticketTestAccount(9918207)
	client, cancel := context.WithCancel(context.Background())
	defer cancel()
	manual, releaseManual := beginAccountGatewayRequest(WithManualAccountTest(client), account)
	defer releaseManual()
	ordinary, releaseOrdinary := beginAccountGatewayRequest(context.Background(), account)
	defer releaseOrdinary()
	probeBase := context.WithValue(context.Background(), qualityProbeContextKey{}, true)
	probe, releaseProbe := beginAccountGatewayRequest(probeBase, account)
	defer releaseProbe()
	require.False(t, gatewayRequestStartedAt(manual).IsZero(), "manual requests still retain request timing")

	require.Equal(t, 2, accountrequest.Default.Cancel(account.ID, time.Now()), "only ordinary and quality-probe requests register for interruption")
	for _, ctx := range []context.Context{ordinary, probe} {
		require.ErrorIs(t, ctx.Err(), context.Canceled)
		require.ErrorIs(t, context.Cause(ctx), accountrequest.ErrQualityDegraded)
		require.ErrorIs(t, accountRequestQualityError(ctx), accountrequest.ErrQualityDegraded)
	}
	require.NoError(t, manual.Err())
	require.NoError(t, accountRequestQualityError(manual))
	require.False(t, accountrequest.QualityDegraded(manual))

	cancel()
	require.ErrorIs(t, manual.Err(), context.Canceled)
	require.ErrorIs(t, context.Cause(manual), context.Canceled)
	require.NoError(t, accountRequestQualityError(manual), "client cancellation is not a quality failure")
}

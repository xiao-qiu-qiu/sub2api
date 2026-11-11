package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func bpsObservationConfig() *PelicanTestConfig {
	return &PelicanTestConfig{QuestionKind: "candy", TestChannel: "bps", Prompt: "Return 21", ReasoningEffort: "high", ParallelCount: 1,
		Quality: &QualityPolicy{Action: QualityActionObserveOnly, ExpectedAnswer: "21", Judge: &QualityJudgeConfig{GroupID: 9, ModelID: "judge", Prompt: "grade"}}}
}

func TestQualityBPSObservationValidation(t *testing.T) {
	for _, kind := range []string{"candy", QualityRandomQuestionKind} {
		t.Run(kind, func(t *testing.T) {
			plan := pelicanPlan()
			plan.PelicanConfig = bpsObservationConfig()
			plan.PelicanConfig.QuestionKind = kind
			q := plan.PelicanConfig.Quality
			q.AutoRestore, q.RemoveGroupIDs, q.BPS = true, []int64{3}, &QualityBPSPolicy{FailureThreshold: 2}
			_, err := nextPlanRun(plan, time.Now())
			require.NoError(t, err)
			require.False(t, q.AutoRestore)
			require.Empty(t, q.RemoveGroupIDs)
			require.Nil(t, q.BPS)
			for _, action := range []string{QualityActionEnableBPS, "remove_groups", "disable_scheduling"} {
				q.Action = action
				_, err := nextPlanRun(plan, time.Now())
				require.ErrorContains(t, err, "observation-only")
			}
			q.Action = QualityActionObserveOnly
			plan.PelicanConfig.QuestionKind = OpenAICodexStateProbeQuestionKind
			_, err = nextPlanRun(plan, time.Now())
			require.ErrorContains(t, err, "answer question")
			plan.PelicanConfig = bpsObservationConfig()
			plan.PelicanConfig.QuestionKind = kind
			plan.PelicanConfig.TestChannel = "unknown"
			_, err = nextPlanRun(plan, time.Now())
			require.ErrorContains(t, err, "invalid test channel")
		})
	}
}

func TestQualityBPSObservationNeverFallsBackToNative(t *testing.T) {
	for _, transport := range []string{"sse", "ws"} {
		for _, tc := range []struct {
			name   string
			change func(*Account)
		}{
			{"disabled", func(a *Account) { a.Extra["openai_excel_bps"] = false }},
			{"authorization_pending", func(a *Account) { a.Extra[ExcelBPSAuthorizationPendingKey] = true }},
			{"model_not_allowed", func(a *Account) { a.Extra["openai_excel_bps_models"] = []string{"gpt-5.6-sol"} }},
			{"api_key", func(a *Account) { a.Type = AccountTypeAPIKey }},
		} {
			t.Run(transport+"/"+tc.name, func(t *testing.T) {
				account := excelAccount()
				account.Extra[ExcelBPSTransportKey] = transport
				tc.change(account)
				upstream := &excelBPSWSTestUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{}}
				gateway := openAIClientToolsTestService(upstream.httpUpstreamRecorder)
				gateway.httpUpstream = upstream
				svc := &AccountTestService{accountRepo: &stateProbeAccountRepo{account: account}, httpUpstream: upstream, openaiGatewayService: gateway}
				result, err := svc.RunPelicanBackground(context.Background(), account.ID, "gpt-6-astra", bpsObservationConfig())
				require.NoError(t, err)
				require.Equal(t, "failed", result.Status)
				require.Contains(t, result.ErrorMessage, "BPS observation unavailable")
				require.Empty(t, upstream.requests)
				require.Zero(t, upstream.httpCalls+upstream.wsCalls)
				require.Equal(t, "inconclusive", qualityOutcome([]*ScheduledTestResult{result}))
			})
		}
	}
}

func TestQualityBPSObservationRequiresAvailableGateway(t *testing.T) {
	for _, mode := range []string{"missing_gateway", "globally_disabled", "settings_unavailable"} {
		t.Run(mode, func(t *testing.T) {
			account := excelAccount()
			upstream := &httpUpstreamRecorder{}
			gateway := openAIClientToolsTestService(upstream)
			if mode == "missing_gateway" {
				gateway = nil
			} else {
				repo := &excelBPSImageSettingsRepo{values: map[string]string{SettingKeyExcelBPSEnabled: "false"}}
				if mode == "settings_unavailable" {
					repo.err = fmt.Errorf("settings unavailable")
				}
				gateway.settingService = NewSettingService(repo, gateway.cfg)
			}
			svc := &AccountTestService{accountRepo: &stateProbeAccountRepo{account: account}, httpUpstream: upstream, openaiGatewayService: gateway}
			result, err := svc.RunPelicanBackground(context.Background(), account.ID, "gpt-6-astra", bpsObservationConfig())
			require.NoError(t, err)
			require.Equal(t, "failed", result.Status)
			require.Contains(t, result.ErrorMessage, "BPS observation unavailable")
			require.Empty(t, upstream.requests)
			require.True(t, account.IsExcelBPSEnabled(), "observation never changes account settings")
			require.Equal(t, "inconclusive", qualityOutcome([]*ScheduledTestResult{result}))
		})
	}
}

func TestQualityBPSObservationUsesBPSWithoutAccountActions(t *testing.T) {
	for _, tc := range []struct {
		transport string
		status    int
	}{
		{"sse", http.StatusOK}, {"sse", http.StatusForbidden}, {"sse", http.StatusTooManyRequests},
		{"ws", http.StatusOK}, {"ws", http.StatusForbidden}, {"ws", http.StatusTooManyRequests},
	} {
		status := tc.status
		t.Run(tc.transport+"/"+fmt.Sprint(status), func(t *testing.T) {
			wire := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"model\":\"gpt-6-astra\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}]}}\n\n"
			wire = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"21\"}\n\n" + wire
			if status != http.StatusOK {
				wire = `{"error":{"code":"permission_denied"}}`
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}}
			account := excelAccount()
			account.Extra[ExcelBPSTransportKey] = tc.transport
			account.Extra["openai_excel_bps_auto_disable_on_403"] = true
			account.Extra[ExcelBPSAutoMoveOn403Key], account.Extra[ExcelBPS403TargetGroupIDKey] = true, 7
			account.Credentials["model_mapping"] = map[string]any{"test-alias": "gpt-6-astra"}
			account.Extra["openai_excel_bps_models"] = []string{"gpt-6-astra"}
			gateway := openAIClientToolsTestService(upstream)
			transport := &excelBPSWSTestUpstream{httpUpstreamRecorder: upstream}
			gateway.httpUpstream = transport
			mutations := 0
			mutate := func(context.Context, *Account) (bool, error) { mutations++; return true, nil }
			gateway.accountRepo = &excelBPSGroupActionRepo{excelBPSAutoDisableRepo: excelBPSAutoDisableRepo{disable: mutate}, move: mutate}
			svc := &AccountTestService{accountRepo: &stateProbeAccountRepo{account: account}, httpUpstream: upstream, openaiGatewayService: gateway}
			result, err := svc.RunPelicanBackground(context.Background(), account.ID, "test-alias", bpsObservationConfig())
			require.NoError(t, err)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, basispoints.ResponsesURL, upstream.requests[0].URL.String())
			require.Equal(t, "gpt-6-astra", gjson.GetBytes(upstream.lastBody, "model").String(), "BPS receives the configured mapped model")
			if tc.transport == "ws" {
				require.Equal(t, 1, transport.wsCalls)
				require.Zero(t, transport.httpCalls)
			} else {
				require.Equal(t, 1, transport.httpCalls)
				require.Zero(t, transport.wsCalls)
			}
			require.Zero(t, mutations, "observations must not disable BPS or move groups")
			require.False(t, gateway.isExcelBPSCoolingDown(account, "test-alias"))
			require.True(t, account.IsExcelBPSEnabled())
			require.True(t, account.IsExcelBPSAutoDisableOn403Enabled(), "shared account settings are preserved")
			require.Equal(t, true, account.Extra[ExcelBPSAutoMoveOn403Key])
			if status == http.StatusOK {
				require.Equal(t, "success", result.Status)
				require.Equal(t, "21", result.ResponseText)
			} else {
				require.Equal(t, "failed", result.Status)
			}
		})
	}
}

func TestQualityBPSObservationDoesNotDisableSchedulingOn401(t *testing.T) {
	for _, transport := range []string{"sse", "ws"} {
		t.Run(transport, func(t *testing.T) {
			upstream := &excelBPSWSTestUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}}}
			gateway := openAIClientToolsTestService(upstream.httpUpstreamRecorder)
			gateway.httpUpstream = upstream
			authRepo, invalidator := setupExcelBPSAuth(gateway)
			account := excelAccount()
			account.Extra[ExcelBPSTransportKey] = transport
			svc := &AccountTestService{accountRepo: &stateProbeAccountRepo{account: account}, httpUpstream: upstream, openaiGatewayService: gateway}
			result, err := svc.RunPelicanBackground(context.Background(), account.ID, "gpt-6-astra", bpsObservationConfig())
			require.NoError(t, err)
			require.Equal(t, "failed", result.Status)
			require.Len(t, upstream.requests, 1)
			require.Zero(t, authRepo.errorCalls+authRepo.tempCalls)
			require.Empty(t, invalidator.ids)
			require.False(t, gateway.isOpenAIAccountRuntimeBlocked(account))
			require.True(t, account.IsExcelBPSEnabled())
			require.True(t, account.Schedulable)
		})
	}
}

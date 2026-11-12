package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestQualityStateProbeMatchesEffectiveBPSAccount(t *testing.T) {
	parent := int64(99)
	for _, tc := range []struct {
		name   string
		change func(*Account)
		bps    bool
	}{
		{"enabled", func(*Account) {}, true},
		{"models_do_not_cover_probe", func(a *Account) { a.Extra["openai_excel_bps_models"] = []any{"other-model"} }, true},
		{"pending_grant", func(a *Account) { a.Extra[ExcelBPSAuthorizationPendingKey] = true }, false},
		{"free", func(a *Account) { a.Credentials["plan_type"] = " free " }, false},
		{"shadow", func(a *Account) { a.ParentAccountID = &parent }, false},
		{"api_key", func(a *Account) { a.Type = AccountTypeAPIKey }, false},
		{"disabled", func(a *Account) { a.Extra["openai_excel_bps"] = false }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := stateProbeAccount()
			account.Extra = map[string]any{"openai_excel_bps": true}
			tc.change(account)
			require.Equal(t, tc.bps, account.IsExcelBPSEnabled())
			for _, passed := range []bool{false, true} {
				cfg := stateProbePlanConfig()
				cfg.Quality = &QualityPolicy{Action: QualityActionMoveGroups}
				if passed {
					cfg.Quality.ActionOn = "passed"
				}
				require.Equal(t, !tc.bps, QualityStateProbeMatchesAccount(account, cfg))
				copy := *cfg
				copy.BPSRecoveryPending = true
				require.True(t, QualityStateProbeMatchesAccount(account, &copy))
				cfg.Quality.Action = QualityActionEnableBPS
				require.True(t, QualityStateProbeMatchesAccount(account, cfg))
			}
			answer := &PelicanTestConfig{QuestionKind: "candy", TestChannel: "account"}
			require.True(t, QualityStateProbeMatchesAccount(account, answer), "account-routed candy must retain its own semantics")
		})
	}
}

func TestQualityTemplateExcludesBPSFromOrdinaryStateProbe(t *testing.T) {
	for _, enableBPS := range []bool{false, true} {
		plans := newTemplatePlanRepo()
		repo := newFakeTemplateRepo(plans, QualityTemplateAccount{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_excel_bps": true}})
		svc := NewScheduledTestService(plans, &pelicanResults{})
		svc.templateRepo = repo
		tpl := probeTemplate()
		if enableBPS {
			tpl.PelicanConfig.Quality.Action = QualityActionEnableBPS
			tpl.PelicanConfig.Quality.BPS = &QualityBPSPolicy{FailureThreshold: 1, AllModels: true}
		}
		_, count, err := svc.CreateQualityTemplate(context.Background(), tpl)
		require.NoError(t, err)
		if enableBPS {
			require.Equal(t, 1, count)
		} else {
			require.Zero(t, count)
		}
	}
}

func TestQualityBPSStateProbeRunnerDoesNotDispatchOrApply(t *testing.T) {
	for _, triggered := range []bool{false, true} {
		for _, recovery := range []bool{false, true} {
			plans, saved := &qualityTriggerPlans{}, &pelicanResults{}
			account := stateProbeAccount()
			account.Extra = map[string]any{"openai_excel_bps": true}
			plan := pelicanPlan()
			plan.PelicanConfig = stateProbePlanConfig()
			plan.PelicanConfig.Quality = &QualityPolicy{Action: QualityActionMoveGroups, MoveGroupIDs: []int64{3}, TriggerOnUpstream5xx: true}
			if recovery {
				plan.PelicanConfig.Quality.ActionOn = "passed"
			}
			if triggered {
				plan.TriggerSource = quality5xxSource
			}
			runner := &ScheduledTestRunnerService{planRepo: plans, scheduledSvc: NewScheduledTestService(plans, saved), accountTestSvc: &AccountTestService{accountRepo: &stateProbeAccountRepo{account: account}}}
			runner.runPelican = func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
				t.Fatal("ordinary BPS state probe was dispatched")
				return nil, nil
			}
			require.True(t, runner.runOnePlan(context.Background(), plan))
			require.True(t, plans.finished)
			require.Zero(t, plans.outcomes)
			require.Empty(t, saved.results)
		}
	}
}

func TestQualityRuntimeRouteCannotConfigureRuleButHistoryDecodes(t *testing.T) {
	for _, key := range []string{"test_route", "Test_Route"} {
		plan := pelicanPlan()
		require.NoError(t, json.Unmarshal([]byte(`{"`+key+`":"openai:bps"}`), plan.PelicanConfig))
		_, err := nextPlanRun(plan, time.Now())
		require.ErrorContains(t, err, "test_route is runtime result evidence")
	}
	plan := pelicanPlan()
	plan.PelicanConfig.TestMappedModel = "forged-upstream"
	_, err := nextPlanRun(plan, time.Now())
	require.ErrorContains(t, err, "runtime result evidence")
	var cfg PelicanTestConfig
	require.NoError(t, json.Unmarshal([]byte(`{"test_route":"openai:account","bps_recovery_pending":true}`), &cfg))
	require.Equal(t, "openai:account", cfg.TestRoute)
	require.False(t, cfg.BPSRecoveryPending)
	require.NoError(t, json.Unmarshal([]byte(`{"question_kind":"candy"}`), &PelicanTestConfig{}))
}

func TestQualityRuntimeRouteIndependentOfRandomBatch(t *testing.T) {
	ctx, evidence := withQualityTestRouteEvidence(context.Background())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	account := stateProbeAccount()
	recordQualityBatchRoute(c, account, "gpt-6-astra", "openai:account")
	recordQualityBatchRoute(c, account, "gpt-6-astra", "openai:bps")
	require.Equal(t, "openai:bps", evidence.value())
	_, isolated := withQualityTestRouteEvidence(ctx)
	require.Empty(t, isolated.value())
}

func TestQualityRuntimeRouteSurvivesScheduledSampleSnapshot(t *testing.T) {
	plans, saved := &pelicanPlanRepo{}, &pelicanResults{}
	runner := &ScheduledTestRunnerService{planRepo: plans, scheduledSvc: NewScheduledTestService(plans, saved)}
	runner.runPelican = func(_ context.Context, _ int64, _ string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
		snapshot := *cfg
		snapshot.TestRoute = "openai:bps"
		return &ScheduledTestResult{Status: "success", ResponseText: "<html></html>", PelicanConfig: &snapshot}, nil
	}
	plan := pelicanPlan()
	require.True(t, runner.runOnePlan(context.Background(), plan))
	require.Len(t, saved.results, 2)
	for _, result := range saved.results {
		require.Equal(t, "openai:bps", result.PelicanConfig.TestRoute)
	}
	require.Empty(t, plan.PelicanConfig.TestRoute)
}

func TestQualityRuntimeRouteNativeProbeAndSkippedProbe(t *testing.T) {
	upstream := &stateProbeUpstream{replies: []stateProbeReply{stateProbeMint("ticket"), {status: http.StatusOK, body: stateProbeCompletedStream}}}
	svc := stateProbeTestService(upstream)
	cfg := stateProbePlanConfig()
	cfg.TestRoute = "forged"
	result, err := svc.RunPelicanBackground(context.Background(), 7, "gpt-6-astra", cfg)
	require.NoError(t, err)
	require.Equal(t, "openai:account", result.PelicanConfig.TestRoute)
	account := stateProbeAccount()
	account.Extra = map[string]any{"openai_excel_bps": true, "openai_excel_bps_models": []any{"other-model"}}
	svc.accountRepo = &stateProbeAccountRepo{account: account}
	result, err = svc.RunPelicanBackground(context.Background(), 7, "gpt-6-astra", cfg)
	require.NoError(t, err)
	require.Empty(t, result.PelicanConfig.TestRoute, "a skipped probe cannot claim native route evidence")
	require.Len(t, upstream.calls, 2)
}

func TestQualityRuntimeRouteAccountCandyUsesBPS(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"basispoints_model_access_changed"}}`))}}
	svc := &AccountTestService{accountRepo: &stateProbeAccountRepo{account: excelAccount()}, openaiGatewayService: openAIClientToolsTestService(upstream)}
	cfg := &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt, TestChannel: "account", TestRoute: "forged", ReasoningEffort: "low", ParallelCount: 1}
	result, err := svc.RunPelicanBackground(context.Background(), 300, "gpt-6-astra", cfg)
	require.NoError(t, err)
	require.Equal(t, "openai:bps", result.PelicanConfig.TestRoute)
	require.Equal(t, "gpt-6-astra", result.PelicanConfig.TestMappedModel)
	require.Nil(t, result.PelicanConfig.QualityBatch)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "account", result.PelicanConfig.TestChannel)
}

func TestQualityRuntimeRouteGatewayNativeWSIsNotBPS(t *testing.T) {
	for _, requested := range []string{"gpt-6.1-sol", "public-sol", ""} {
		t.Run("model="+requested, func(t *testing.T) {
			model := requested
			if model == "" {
				model = openai.DefaultTestModel
			}
			account := groupAdmissionAccount(7)
			account.Extra["openai_excel_bps"] = true
			account.Extra["openai_excel_bps_models"] = []any{"different-bps-model"}
			if requested == "public-sol" {
				model = "gpt-6.1-sol"
				account.Credentials["model_mapping"] = map[string]any{requested: model}
			}
			completed := map[string]any{"type": "response.completed", "response": map[string]any{"id": "native-route-test", "model": model, "status": "completed",
				"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "21"}}}},
				"usage":  map[string]any{"input_tokens": 1, "output_tokens": 1}}}
			final, err := json.Marshal(completed)
			require.NoError(t, err)
			conn := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.output_text.delta","delta":"21"}`), final}}
			gateway, _ := newGroupAdmissionWSv2Service(t, &groupAdmissionRepo{first: account}, conn)
			svc := &AccountTestService{accountRepo: &stateProbeAccountRepo{account: account}, openaiGatewayService: gateway}
			cfg := &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt, TestChannel: "account", ReasoningEffort: "medium", ParallelCount: 1}
			result, err := svc.RunPelicanBackground(context.Background(), account.ID, requested, cfg)
			require.NoError(t, err)
			require.Equal(t, "success", result.Status, result.ErrorMessage)
			require.Equal(t, "21", result.ResponseText)
			require.NotNil(t, conn.lastWrite, "the request must reach native WS")
			require.Equal(t, "openai:account", result.PelicanConfig.TestRoute)
			require.Equal(t, model, result.PelicanConfig.TestMappedModel)
			require.Nil(t, result.PelicanConfig.QualityBatch)
		})
	}
}

func TestQualityReusableConfigDoesNotCopyRuntimeRoute(t *testing.T) {
	cfg := bpsObservationConfig()
	cfg.TestRoute, cfg.TestMappedModel = "openai:bps", "upstream-model"
	copy, err := reusableQualityConfig(cfg)
	require.NoError(t, err)
	require.Empty(t, copy.TestRoute)
	require.Empty(t, copy.TestMappedModel)
	require.Equal(t, QualityActionObserveOnly, copy.Quality.Action)
}

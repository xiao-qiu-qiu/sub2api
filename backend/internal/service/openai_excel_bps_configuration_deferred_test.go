package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const deferredExcelBPSServiceCompletion = "data: " + `{"type":"response.completed","response":{"id":"resp_deferred","status":"completed","model":"gpt-5.6-sol","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"allowed current effort"}]}],"usage":{"input_tokens":10,"output_tokens":2}}}` + "\n\n"

func deferredExcelBPSServiceEffort(value any) map[string]any {
	return map[string]any{"type": "configuration_update", "reasoning": map[string]any{"effort": value}}
}

func deferredExcelBPSServiceBody(t *testing.T, updates ...any) []byte {
	t.Helper()
	input := []any{map[string]any{"role": "user", "content": "original task"}}
	input = append(input, updates...)
	body, err := json.MarshalIndent(map[string]any{
		"model": "gpt-5.6-sol", "reasoning": map[string]any{"effort": "high"}, "input": input,
	}, "", "  ")
	require.NoError(t, err)
	return body
}

func deferredExcelBPSServiceContext(t *testing.T, body []byte) (context.Context, error) {
	t.Helper()
	ctx := WithRequestedReasoningEffort(t.Context(), "high")
	ctx = WithAPIKeyOpenAIReasoningEffortPolicy(ctx, "", "low", nil, ReasoningEffortOverLimitDeny, "gpt-5.6-sol")
	_, _, denied := ApplyOpenAIReasoningEffortPolicyFromContext(ctx, body)
	require.Error(t, denied)
	require.True(t, IsReasoningEffortPolicyDenied(denied))
	return WithDeferredExcelBPSReasoningPolicyError(ctx, denied), denied
}

func TestExcelBPSConfigurationDeferredContextIsLocal(t *testing.T) {
	body := deferredExcelBPSServiceBody(t, deferredExcelBPSServiceEffort("low"))
	ctx, denied := deferredExcelBPSServiceContext(t, body)
	base := WithRequestedReasoningEffort(t.Context(), "high")
	marked := WithDeferredExcelBPSReasoningPolicyError(base, denied)
	require.Same(t, denied, DeferredExcelBPSReasoningPolicyError(marked))
	require.Same(t, denied, DeferredExcelBPSReasoningPolicyError(ctx))
	require.Nil(t, DeferredExcelBPSReasoningPolicyError(base))
	require.Nil(t, DeferredExcelBPSReasoningPolicyError(nil))
	require.Equal(t, "high", *RequestedReasoningEffortFromContext(base))
	for _, err := range []error{nil, errors.New("ordinary request error")} {
		got := WithDeferredExcelBPSReasoningPolicyError(base, err)
		require.Same(t, base, got, "only policy denials may create a deferred marker")
		require.Nil(t, DeferredExcelBPSReasoningPolicyError(got))
	}
	nilBase := WithDeferredExcelBPSReasoningPolicyError(nil, denied)
	require.NotNil(t, nilBase)
	require.Same(t, denied, DeferredExcelBPSReasoningPolicyError(nilBase))
}

func TestExcelBPSConfigurationDeferredHelperRequiresExplicitAllowedEffort(t *testing.T) {
	for _, tc := range []struct {
		name         string
		updates      []any
		wantEffort   string
		originalDeny bool
		invalid      bool
	}{
		{"low_replaces_initial_denial", []any{deferredExcelBPSServiceEffort("low")}, "low", false, false},
		{"latest_low_wins", []any{deferredExcelBPSServiceEffort("high"), deferredExcelBPSServiceEffort("low")}, "low", false, false},
		{"null_after_low_keeps_new_preference", []any{deferredExcelBPSServiceEffort("low"), deferredExcelBPSServiceEffort(nil)}, "low", false, false},
		{"same_high_is_rechecked_and_denied", []any{deferredExcelBPSServiceEffort("high")}, "", false, false},
		{"latest_high_is_denied", []any{deferredExcelBPSServiceEffort("low"), deferredExcelBPSServiceEffort("high")}, "", false, false},
		{"no_update_keeps_original_denial", nil, "", true, false},
		{"empty_update_keeps_original_denial", []any{map[string]any{"type": "configuration_update"}}, "", true, false},
		{"text_update_keeps_original_denial", []any{map[string]any{"type": "configuration_update", "text": map[string]any{"format": map[string]any{"type": "text"}}}}, "", true, false},
		{"null_reasoning_keeps_original_denial", []any{map[string]any{"type": "configuration_update", "reasoning": nil}}, "", true, false},
		{"null_effort_keeps_original_denial", []any{deferredExcelBPSServiceEffort(nil)}, "", true, false},
		{"all_noops_keep_original_denial", []any{map[string]any{"type": "configuration_update"}, deferredExcelBPSServiceEffort(nil)}, "", true, false},
		{"invalid_effort_is_not_a_policy_denial", []any{deferredExcelBPSServiceEffort(7)}, "", false, true},
		{"invalid_earlier_update_is_not_hidden", []any{deferredExcelBPSServiceEffort(7), deferredExcelBPSServiceEffort("low")}, "", false, true},
		{"unknown_field_is_not_a_policy_denial", []any{map[string]any{"type": "configuration_update", "future_option": true}}, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := deferredExcelBPSServiceBody(t, tc.updates...)
			original := append([]byte(nil), body...)
			ctx, denied := deferredExcelBPSServiceContext(t, body)
			gotCtx, got, err := prepareExcelBPSConfiguration(ctx, body)
			require.Equal(t, original, body)
			require.Same(t, denied, DeferredExcelBPSReasoningPolicyError(ctx))
			require.Equal(t, "high", *RequestedReasoningEffortFromContext(ctx), "BPS resolution must not change the native attempt context")
			if tc.wantEffort != "" {
				require.NoError(t, err)
				require.Equal(t, tc.wantEffort, gjson.GetBytes(got, "reasoning.effort").String())
				require.Equal(t, tc.wantEffort, *RequestedReasoningEffortFromContext(gotCtx))
				require.NotContains(t, string(got), "configuration_update")
				return
			}
			require.Error(t, err)
			if tc.invalid {
				require.False(t, IsReasoningEffortPolicyDenied(err))
				require.Contains(t, err.Error(), "configuration_update")
				require.Equal(t, original, got)
				require.Same(t, ctx, gotCtx)
			} else {
				require.True(t, IsReasoningEffortPolicyDenied(err))
				if tc.originalDeny {
					require.Same(t, denied, err, "text and noop events must return the original deferred denial")
					require.Same(t, ctx, gotCtx)
				}
			}
		})
	}
}

func TestExcelBPSConfigurationDeferredHelperCannotUnlockWithoutPolicyContext(t *testing.T) {
	body := deferredExcelBPSServiceBody(t, deferredExcelBPSServiceEffort("low"))
	_, denied := deferredExcelBPSServiceContext(t, body)
	ctx := WithDeferredExcelBPSReasoningPolicyError(WithRequestedReasoningEffort(t.Context(), "high"), denied)
	gotCtx, got, err := prepareExcelBPSConfiguration(ctx, body)
	require.Same(t, denied, err)
	require.Same(t, ctx, gotCtx)
	require.Equal(t, body, got, "a new preference cannot release a denial without the original policy snapshot")
	require.Equal(t, "high", *RequestedReasoningEffortFromContext(gotCtx))
}

func TestExcelBPSConfigurationDeferredForwardAllowsCurrentLow(t *testing.T) {
	for _, transport := range []string{"sse", "ws"} {
		t.Run(transport, func(t *testing.T) {
			for _, tc := range []struct {
				name   string
				stream bool
			}{
				{"unary", false}, {"stream", true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					body := deferredExcelBPSServiceBody(t, deferredExcelBPSServiceEffort("low"))
					var source map[string]any
					require.NoError(t, json.Unmarshal(body, &source))
					source["stream"] = tc.stream
					body, err := json.Marshal(source)
					require.NoError(t, err)
					original := append([]byte(nil), body...)
					ctx, denied := deferredExcelBPSServiceContext(t, body)
					u := &excelBPSWSTestUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{resp: bpsCompletionResponse(http.StatusOK, deferredExcelBPSServiceCompletion)}}
					svc := openAIClientToolsTestService(u.httpUpstreamRecorder)
					svc.httpUpstream = u
					account := excelAccount()
					account.Extra[ExcelBPSTransportKey] = transport
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(ctx)
					result, err := svc.Forward(ctx, c, account, body)
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, http.StatusOK, rec.Code)
					require.Len(t, u.requests, 1)
					require.Equal(t, "bps.openai.com", u.lastReq.URL.Host)
					require.Equal(t, "/basispoints/api/responses", u.lastReq.URL.Path)
					require.Equal(t, "low", gjson.GetBytes(u.lastBody, "reasoning_effort").String())
					require.Equal(t, "low", optionalStringValue(result.ReasoningEffort))
					require.Equal(t, "low", optionalStringValue(result.RequestedReasoningEffort))
					require.NotContains(t, string(u.lastBody), "configuration_update")
					require.Contains(t, rec.Body.String(), "allowed current effort")
					require.Equal(t, transport, rec.Header().Get("X-Sub2API-BPS-Transport"))
					if transport == "ws" {
						require.Equal(t, 1, u.wsCalls)
						require.Zero(t, u.httpCalls)
					} else {
						require.Equal(t, 1, u.httpCalls)
						require.Zero(t, u.wsCalls)
					}
					require.Equal(t, original, body)
					require.Equal(t, "high", *RequestedReasoningEffortFromContext(ctx))
					require.Same(t, denied, DeferredExcelBPSReasoningPolicyError(ctx))
				})
			}
		})
	}
}

func TestExcelBPSConfigurationDeferredForwardRejectsBeforeUpstream(t *testing.T) {
	for _, transport := range []string{"sse", "ws"} {
		t.Run(transport, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				updates []any
				status  int
			}{
				{"high", []any{deferredExcelBPSServiceEffort("high")}, http.StatusForbidden},
				{"latest_high", []any{deferredExcelBPSServiceEffort("low"), deferredExcelBPSServiceEffort("high")}, http.StatusForbidden},
				{"no_update", nil, http.StatusForbidden},
				{"noop", []any{map[string]any{"type": "configuration_update"}}, http.StatusForbidden},
				{"text_only", []any{map[string]any{"type": "configuration_update", "text": map[string]any{"format": map[string]any{"type": "text"}}}}, http.StatusForbidden},
				{"null_reasoning", []any{map[string]any{"type": "configuration_update", "reasoning": nil}}, http.StatusForbidden},
				{"null_effort", []any{deferredExcelBPSServiceEffort(nil)}, http.StatusForbidden},
				{"invalid_effort", []any{deferredExcelBPSServiceEffort(7)}, http.StatusBadRequest},
				{"invalid_before_valid", []any{deferredExcelBPSServiceEffort(7), deferredExcelBPSServiceEffort("low")}, http.StatusBadRequest},
				{"unknown_field", []any{map[string]any{"type": "configuration_update", "future_option": true}}, http.StatusBadRequest},
			} {
				t.Run(tc.name, func(t *testing.T) {
					body := deferredExcelBPSServiceBody(t, tc.updates...)
					ctx, denied := deferredExcelBPSServiceContext(t, body)
					u := &excelBPSWSTestUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{resp: bpsCompletionResponse(http.StatusOK, deferredExcelBPSServiceCompletion)}}
					svc := openAIClientToolsTestService(u.httpUpstreamRecorder)
					svc.httpUpstream = u
					account := excelAccount()
					account.Extra[ExcelBPSTransportKey] = transport
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(ctx)
					result, err := svc.Forward(ctx, c, account, body)
					require.Error(t, err)
					require.Nil(t, result)
					require.Equal(t, tc.status, rec.Code)
					if tc.status == http.StatusForbidden {
						require.Equal(t, denied.Error(), gjson.Get(rec.Body.String(), "error.message").String())
					} else {
						require.Equal(t, "basispoints_request_invalid", gjson.Get(rec.Body.String(), "error.code").String())
						require.Contains(t, gjson.Get(rec.Body.String(), "error.message").String(), "configuration_update")
					}
					require.Empty(t, u.requests)
					require.Zero(t, u.httpCalls)
					require.Zero(t, u.wsCalls)
				})
			}
		})
	}
}

func TestExcelBPSConfigurationDeferredForwardCannotUnlockWithoutPolicyContext(t *testing.T) {
	body := deferredExcelBPSServiceBody(t, deferredExcelBPSServiceEffort("low"))
	_, denied := deferredExcelBPSServiceContext(t, body)
	ctx := WithDeferredExcelBPSReasoningPolicyError(t.Context(), denied)
	upstream := &httpUpstreamRecorder{resp: bpsCompletionResponse(http.StatusOK, deferredExcelBPSServiceCompletion)}
	svc := openAIClientToolsTestService(upstream)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(ctx)
	result, err := svc.Forward(ctx, c, excelAccount(), body)
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, denied.Error(), gjson.Get(rec.Body.String(), "error.message").String())
	require.Empty(t, upstream.requests)
}

func TestExcelBPSConfigurationDeferredNativeRoutesRemainForbidden(t *testing.T) {
	for _, route := range []string{"native_oauth", "native_api_key", "model_outside_bps_scope", "bps_globally_disabled", "siwc", "prism_browser"} {
		t.Run(route, func(t *testing.T) {
			for _, tc := range []struct {
				name   string
				update any
			}{
				{"new_low", deferredExcelBPSServiceEffort("low")},
				{"noop", map[string]any{"type": "configuration_update"}},
				{"invalid_update", deferredExcelBPSServiceEffort(7)},
			} {
				t.Run(tc.name, func(t *testing.T) {
					body := deferredExcelBPSServiceBody(t, tc.update)
					original := append([]byte(nil), body...)
					ctx, denied := deferredExcelBPSServiceContext(t, body)
					u := &excelBPSWSTestUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{resp: bpsCompletionResponse(http.StatusOK, deferredExcelBPSServiceCompletion)}}
					svc := openAIClientToolsTestService(u.httpUpstreamRecorder)
					svc.httpUpstream = u
					account := excelAccount()
					switch route {
					case "native_oauth":
						account.Extra["openai_excel_bps"] = false
					case "native_api_key":
						account.Type = AccountTypeAPIKey
						account.Credentials = map[string]any{"api_key": "synthetic-test-key"}
					case "model_outside_bps_scope":
						account.Extra["openai_excel_bps_models"] = []string{"gpt-6-astra"}
					case "bps_globally_disabled":
						repo := &excelBPSImageSettingsRepo{values: map[string]string{SettingKeyExcelBPSEnabled: "false"}}
						svc.settingService = NewSettingService(repo, &config.Config{})
					case "siwc":
						account.Extra[OpenAISIWCEnabledKey] = true
						require.True(t, account.IsSIWCEnabled())
					case "prism_browser":
						account.Extra["openai_prism_browser"] = true
						svc.cfg.Gateway.PrismBrowser.Enabled = true
						require.True(t, account.IsPrismBrowserEnabledForModel("gpt-5.6-sol"))
					}
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(ctx)
					result, err := svc.Forward(ctx, c, account, body)
					require.Same(t, denied, err, "non-BPS branches must return the original ingress denial")
					require.Nil(t, result)
					require.Equal(t, http.StatusForbidden, rec.Code)
					require.Equal(t, denied.Error(), gjson.Get(rec.Body.String(), "error.message").String())
					require.Equal(t, "permission_error", gjson.Get(rec.Body.String(), "error.type").String())
					require.Empty(t, u.requests)
					require.Zero(t, u.httpCalls)
					require.Zero(t, u.wsCalls)
					require.Equal(t, original, body)
					require.Same(t, denied, DeferredExcelBPSReasoningPolicyError(ctx))
					require.Equal(t, "high", *RequestedReasoningEffortFromContext(ctx))
				})
			}
		})
	}
}

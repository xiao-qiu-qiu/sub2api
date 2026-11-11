package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func excelBPSConfigurationPolicyBody(t *testing.T, updates ...any) []byte {
	t.Helper()
	input := append(append([]any{}, updates...), map[string]any{"role": "user", "content": "hello"})
	body, err := json.Marshal(map[string]any{"model": "upstream-model", "reasoning": map[string]any{"effort": "medium"}, "input": input})
	require.NoError(t, err)
	return body
}

func excelBPSConfigurationEffortUpdate(effort any) map[string]any {
	return map[string]any{"type": "configuration_update", "reasoning": map[string]any{"effort": effort}}
}

func TestExcelBPSConfigurationReappliesPolicyOnlyForEffortUpdates(t *testing.T) {
	chain := []ReasoningEffortMapping{{From: "high", To: "medium"}, {From: "medium", To: "low"}}
	formatOnly := map[string]any{"type": "configuration_update", "text": map[string]any{"format": map[string]any{"type": "text"}}}
	for _, tc := range []struct {
		name, forced, ceiling, requestModel, wantEffort, wantRequested, wantWire string
		updates                                                                  []any
		mappings                                                                 []ReasoningEffortMapping
	}{
		{name: "key preference wins over history", forced: "low", updates: []any{excelBPSConfigurationEffortUpdate("ultra")}, wantEffort: "low", wantRequested: "ultra", wantWire: "low"},
		{name: "key retains ceiling and skips mappings", forced: "high", ceiling: "medium", mappings: chain, updates: []any{excelBPSConfigurationEffortUpdate("low")}, wantEffort: "medium", wantRequested: "low", wantWire: "medium"},
		{name: "group ceiling caps history", ceiling: "low", updates: []any{excelBPSConfigurationEffortUpdate("ultra")}, wantEffort: "low", wantRequested: "ultra", wantWire: "low"},
		{name: "history maps exactly once", mappings: chain, updates: []any{excelBPSConfigurationEffortUpdate("high")}, wantEffort: "medium", wantRequested: "high", wantWire: "medium"},
		{name: "same value remains a new preference", mappings: chain, updates: []any{excelBPSConfigurationEffortUpdate("medium")}, wantEffort: "low", wantRequested: "medium", wantWire: "low"},
		{name: "format only does not remap", mappings: chain, updates: []any{formatOnly}, wantEffort: "medium", wantRequested: "high", wantWire: "medium"},
		{name: "null effort does not remap", mappings: chain, updates: []any{excelBPSConfigurationEffortUpdate(nil)}, wantEffort: "medium", wantRequested: "high", wantWire: "medium"},
		{name: "null reasoning does not remap", mappings: chain, updates: []any{map[string]any{"type": "configuration_update", "reasoning": nil}}, wantEffort: "medium", wantRequested: "high", wantWire: "medium"},
		{name: "latest nonnull effort wins", updates: []any{excelBPSConfigurationEffortUpdate("high"), excelBPSConfigurationEffortUpdate("low"), excelBPSConfigurationEffortUpdate(nil), formatOnly}, wantEffort: "low", wantRequested: "low", wantWire: "low"},
		{name: "client max is distinct from billing tier", updates: []any{excelBPSConfigurationEffortUpdate("max")}, wantEffort: "max", wantRequested: "max", wantWire: "xhigh"},
		{name: "none replaces stale requested value", updates: []any{excelBPSConfigurationEffortUpdate("none")}, wantEffort: "none", wantRequested: "none", wantWire: "low"},
		{name: "mapping uses client model", requestModel: "client-model", mappings: []ReasoningEffortMapping{{From: "high", To: "low", Model: "client-model"}}, updates: []any{excelBPSConfigurationEffortUpdate("high")}, wantEffort: "low", wantRequested: "high", wantWire: "low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Simulate a top-level effort already transformed by the HTTP ingress.
			body := excelBPSConfigurationPolicyBody(t, tc.updates...)
			base := WithRequestedReasoningEffort(context.Background(), "high")
			ctx := WithAPIKeyOpenAIReasoningEffortPolicy(base, tc.forced, tc.ceiling, tc.mappings, ReasoningEffortOverLimitDowngrade, tc.requestModel)
			resolvedCtx, resolved, err := prepareExcelBPSConfiguration(ctx, body)
			require.NoError(t, err)
			require.Equal(t, tc.wantEffort, gjson.GetBytes(resolved, "reasoning.effort").String())
			require.Equal(t, tc.wantRequested, optionalStringValue(RequestedReasoningEffortFromContext(resolvedCtx)))
			require.Equal(t, "high", optionalStringValue(RequestedReasoningEffortFromContext(ctx)), "native failover must retain the ingress context")
			require.NotContains(t, string(resolved), "configuration_update")
			wire, bridge, err := basispoints.Prepare(resolved, "", nil)
			require.NoError(t, err)
			require.Equal(t, tc.wantWire, bridge.Effort)
			require.Equal(t, tc.wantWire, gjson.GetBytes(wire, "reasoning_effort").String())
		})
	}
}

func TestExcelBPSConfigurationNoUpdatePreservesBodyAndContext(t *testing.T) {
	body := excelBPSConfigurationPolicyBody(t)
	ctx := WithAPIKeyOpenAIReasoningEffortPolicy(context.Background(), "", "", []ReasoningEffortMapping{{From: "medium", To: "low"}}, "")
	gotCtx, got, err := prepareExcelBPSConfiguration(ctx, body)
	require.NoError(t, err)
	require.Equal(t, body, got)
	require.Same(t, ctx, gotCtx)
	require.Nil(t, RequestedReasoningEffortFromContext(gotCtx))
}

func TestExcelBPSConfigurationUnboundPolicyPreservesUpdatedClientEffort(t *testing.T) {
	body := excelBPSConfigurationPolicyBody(t, excelBPSConfigurationEffortUpdate("max"))
	ctx, resolved, err := prepareExcelBPSConfiguration(nil, body)
	require.NoError(t, err)
	require.Equal(t, "max", gjson.GetBytes(resolved, "reasoning.effort").String())
	require.Equal(t, "max", optionalStringValue(RequestedReasoningEffortFromContext(ctx)))
}

func TestExcelBPSConfigurationPolicyRejectsHistory(t *testing.T) {
	for _, tc := range []struct {
		name, forced, ceiling, overLimit string
		mappings                         []ReasoningEffortMapping
	}{
		{name: "group ceiling deny", ceiling: "low", overLimit: ReasoningEffortOverLimitDeny},
		{name: "group mapping deny", mappings: []ReasoningEffortMapping{{From: "high", To: ReasoningEffortMappingDeny}}},
		{name: "key still respects deny", forced: "high", ceiling: "low", overLimit: ReasoningEffortOverLimitDeny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := excelBPSConfigurationPolicyBody(t, excelBPSConfigurationEffortUpdate("high"))
			ctx := WithAPIKeyOpenAIReasoningEffortPolicy(context.Background(), tc.forced, tc.ceiling, tc.mappings, tc.overLimit)
			_, got, err := prepareExcelBPSConfiguration(ctx, body)
			require.Error(t, err)
			require.True(t, IsReasoningEffortPolicyDenied(err))
			require.Equal(t, body, got)
		})
	}
}

func TestExcelBPSConfigurationLatestEffortReplacesLegacyAliasesBeforeDeny(t *testing.T) {
	var source map[string]any
	require.NoError(t, json.Unmarshal(excelBPSConfigurationPolicyBody(t, excelBPSConfigurationEffortUpdate("low")), &source))
	source["reasoning_effort"] = "max"
	source["output_config"] = map[string]any{"effort": "max"}
	body, err := json.Marshal(source)
	require.NoError(t, err)
	ctx := WithAPIKeyOpenAIReasoningEffortPolicy(context.Background(), "", "high", []ReasoningEffortMapping{{From: "max", To: ReasoningEffortMappingDeny}}, ReasoningEffortOverLimitDeny)
	ctx, resolved, err := prepareExcelBPSConfiguration(ctx, body)
	require.NoError(t, err)
	for _, path := range []string{"reasoning.effort", "reasoning_effort", "output_config.effort"} {
		require.Equal(t, "low", gjson.GetBytes(resolved, path).String())
	}
	require.Equal(t, "low", optionalStringValue(RequestedReasoningEffortFromContext(ctx)))
}

func TestExcelBPSConfigurationInvalidUpdateDoesNotApplyPolicy(t *testing.T) {
	body := excelBPSConfigurationPolicyBody(t, excelBPSConfigurationEffortUpdate(7))
	ctx := WithRequestedReasoningEffort(context.Background(), "low")
	gotCtx, got, err := prepareExcelBPSConfiguration(ctx, body)
	require.Error(t, err)
	require.False(t, IsReasoningEffortPolicyDenied(err))
	require.Equal(t, body, got)
	require.Same(t, ctx, gotCtx)
}

func TestExcelBPSConfigurationPolicyDenyStopsBeforeUpstream(t *testing.T) {
	upstream := &httpUpstreamRecorder{}
	svc := openAIClientToolsTestService(upstream)
	ctx := WithAPIKeyOpenAIReasoningEffortPolicy(context.Background(), "", "low", nil, ReasoningEffortOverLimitDeny)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	body := excelBPSConfigurationPolicyBody(t, excelBPSConfigurationEffortUpdate("high"))
	result, err := svc.Forward(ctx, c, excelAccount(), body)
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "permission_error", gjson.Get(rec.Body.String(), "error.code").String())
	require.Empty(t, upstream.requests)
}

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type deferredExcelBPSHandlerContextKey struct{}

func deferredExcelBPSHandlerBody(t *testing.T, input any) []byte {
	t.Helper()
	body, err := json.MarshalIndent(map[string]any{
		"model": "gpt-6-sol", "reasoning": map[string]any{"effort": "high"}, "input": input,
	}, "", "  ")
	require.NoError(t, err)
	return body
}

func deferredExcelBPSHandlerKey() *service.APIKey {
	return &service.APIKey{Group: &service.Group{
		Platform: service.PlatformOpenAI, MaxReasoningEffort: "low", MaxReasoningEffortOverLimit: service.ReasoningEffortOverLimitDeny,
	}}
}

func TestOpenAIHTTPConfigurationDeferredInitialDenyPreservesRequest(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		t.Run(path, func(t *testing.T) {
			body := deferredExcelBPSHandlerBody(t, []any{
				map[string]any{"role": "user", "content": "hello"},
				map[string]any{"type": "configuration_update", "reasoning": map[string]any{"effort": "low"}},
			})
			original := append([]byte(nil), body...)
			base := context.WithValue(t.Context(), deferredExcelBPSHandlerContextKey{}, "native-context-marker")
			base = service.WithRequestedReasoningEffort(base, "high")
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)).WithContext(base)
			key := deferredExcelBPSHandlerKey()
			_, _, originalErr := service.ApplyAPIKeyOpenAIReasoningEffortPolicy(body, "", "low", nil, service.ReasoningEffortOverLimitDeny, "gpt-6-sol")
			require.Error(t, originalErr)
			got, changed, err := applyOpenAIReasoningEffortPolicyForRequest(c, key, body)
			require.NoError(t, err, "the account route must decide whether this deny can be resolved by BPS")
			require.False(t, changed)
			require.Equal(t, original, got)
			require.Equal(t, original, body, "the native body must remain byte-for-byte unchanged")
			require.Equal(t, "high", gjson.GetBytes(got, "reasoning.effort").String())
			require.True(t, gjson.GetBytes(got, "input.1.reasoning.effort").Exists())
			deferred := service.DeferredExcelBPSReasoningPolicyError(c.Request.Context())
			require.Error(t, deferred)
			require.Equal(t, originalErr.Error(), deferred.Error())
			require.True(t, service.IsReasoningEffortPolicyDenied(deferred))
			require.Nil(t, service.DeferredExcelBPSReasoningPolicyError(base), "the parent/native context must not be mutated")
			require.Equal(t, "native-context-marker", c.Request.Context().Value(deferredExcelBPSHandlerContextKey{}))
			require.Equal(t, "high", *service.RequestedReasoningEffortFromContext(base))
			require.Equal(t, "high", *service.RequestedReasoningEffortFromContext(c.Request.Context()), "do not promote the history effort before selecting BPS")
			_, _, policyErr := service.ApplyOpenAIReasoningEffortPolicyFromContext(c.Request.Context(), body)
			require.Error(t, policyErr, "deferring a denial must retain the ingress policy snapshot")
			require.Equal(t, originalErr.Error(), policyErr.Error())
			require.Empty(t, rec.Body.String())
		})
	}
}

func TestOpenAIHTTPConfigurationDeferredDenyRequiresActualInputEvent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		input    any
		deferred bool
	}{
		{"effort_event", []any{map[string]any{"type": "configuration_update", "reasoning": map[string]any{"effort": "low"}}}, true},
		{"text_only_event", []any{map[string]any{"type": "configuration_update", "text": map[string]any{"format": map[string]any{"type": "text"}}}}, true},
		{"noop_event", []any{map[string]any{"type": "configuration_update"}}, true},
		{"null_effort_event", []any{map[string]any{"type": "configuration_update", "reasoning": map[string]any{"effort": nil}}}, true},
		{"invalid_effort_event_is_validated_later", []any{map[string]any{"type": "configuration_update", "reasoning": map[string]any{"effort": 7}}}, true},
		{"no_event", []any{map[string]any{"role": "user", "content": "hello"}}, false},
		{"empty_input", []any{}, false},
		{"null_input", nil, false},
		{"literal_text", "configuration_update", false},
		{"array_literal_text", []any{"configuration_update"}, false},
		{"message_mentions_event", []any{map[string]any{"role": "user", "content": "configuration_update"}}, false},
		{"nested_event_is_not_a_control_item", []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "configuration_update"}}}}, false},
		{"similar_type_is_not_an_event", []any{map[string]any{"type": "configuration_update_extra"}}, false},
		{"non_array_input_is_not_an_event", map[string]any{"type": "configuration_update"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := deferredExcelBPSHandlerBody(t, tc.input)
			original := append([]byte(nil), body...)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			got, changed, err := applyOpenAIReasoningEffortPolicyForRequest(c, deferredExcelBPSHandlerKey(), body)
			require.False(t, changed)
			require.Equal(t, original, body)
			require.Equal(t, original, got)
			if tc.deferred {
				require.NoError(t, err)
				require.Error(t, service.DeferredExcelBPSReasoningPolicyError(c.Request.Context()))
			} else {
				require.Error(t, err)
				require.True(t, service.IsReasoningEffortPolicyDenied(err))
				require.Nil(t, service.DeferredExcelBPSReasoningPolicyError(c.Request.Context()))
			}
		})
	}
}

func TestOpenAIHTTPConfigurationDeferredMarkerIsNotSetWithoutDeny(t *testing.T) {
	for _, tc := range []struct {
		name, initial, overLimit, want string
		changed                        bool
	}{
		{"allowed_top_level", "low", service.ReasoningEffortOverLimitDeny, "low", false},
		{"ordinary_downgrade", "high", service.ReasoningEffortOverLimitDowngrade, "low", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"model": "gpt-6-sol", "reasoning": map[string]any{"effort": tc.initial},
				"input": []any{map[string]any{"type": "configuration_update", "reasoning": map[string]any{"effort": "high"}}},
			})
			require.NoError(t, err)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			key := deferredExcelBPSHandlerKey()
			key.Group.MaxReasoningEffortOverLimit = tc.overLimit
			got, changed, err := applyOpenAIReasoningEffortPolicyForRequest(c, key, body)
			require.NoError(t, err)
			require.Equal(t, tc.changed, changed)
			require.Equal(t, tc.want, gjson.GetBytes(got, "reasoning.effort").String())
			require.Nil(t, service.DeferredExcelBPSReasoningPolicyError(c.Request.Context()))
			require.Equal(t, tc.initial, *service.RequestedReasoningEffortFromContext(c.Request.Context()))
		})
	}
}

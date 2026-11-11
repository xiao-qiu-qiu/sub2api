package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIHTTPPolicyBindsBPSKeyPreferenceAndCeiling(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions"} {
		t.Run(path, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, path, nil)
			key := &service.APIKey{ReasoningEffort: "high", Group: &service.Group{Platform: service.PlatformOpenAI, MaxReasoningEffort: "medium"}}
			body, err := json.Marshal(map[string]any{"model": "gpt-5.6-sol", "reasoning": map[string]any{"effort": "low"}})
			require.NoError(t, err)
			got, _, err := applyOpenAIReasoningEffortPolicyForRequest(c, key, body)
			require.NoError(t, err)
			require.Equal(t, "medium", gjson.GetBytes(got, "reasoning.effort").String())
			key.ReasoningEffort, key.Group.MaxReasoningEffort = "ultra", "ultra"
			got, _, err = service.ApplyOpenAIReasoningEffortPolicyFromContext(c.Request.Context(), body)
			require.NoError(t, err)
			require.Equal(t, "medium", gjson.GetBytes(got, "reasoning.effort").String(), "BPS must use the ingress policy snapshot")
			require.Equal(t, "low", *service.RequestedReasoningEffortFromContext(c.Request.Context()))
		})
	}
}

func TestOpenAIHTTPPolicyBindsBPSMappingsAgainstClientModel(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.RequestedPublicModel, "client-model"))
	mappings := []service.ReasoningEffortMapping{{From: "high", To: "medium", Model: "client-model"}, {From: "medium", To: "low"}}
	key := &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI, ReasoningEffortMappings: mappings}}
	body, err := json.Marshal(map[string]any{"model": "upstream-model", "reasoning": map[string]any{"effort": "high"}})
	require.NoError(t, err)
	got, _, err := applyOpenAIReasoningEffortPolicyForRequest(c, key, body)
	require.NoError(t, err)
	require.Equal(t, "medium", gjson.GetBytes(got, "reasoning.effort").String())
	mappings[0].To = "low"
	got, _, err = service.ApplyOpenAIReasoningEffortPolicyFromContext(c.Request.Context(), body)
	require.NoError(t, err)
	require.Equal(t, "medium", gjson.GetBytes(got, "reasoning.effort").String(), "snapshot must copy mappings and preserve the client model")
}

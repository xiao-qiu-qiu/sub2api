package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAstraModelDisabledCoversGPT62AndMappedAliases(t *testing.T) {
	for _, mapping := range []map[string]any{
		nil,
		{"*": "*"},
		{"gpt-6.2-*": openai.GPT62AstraModel, openai.GPT62SolModel: openai.GPT62SolModel},
		{"alias": openai.GPT62AstraModel, openai.GPT62AstraModel: openai.GPT62AstraModel, openai.GPT62SolModel: openai.GPT62SolModel},
	} {
		a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": mapping}, Extra: map[string]any{"astra_model_disabled": true}}
		for _, model := range []string{"gpt-6-astra", openai.GPT62AstraModel, "openai/gpt-6.2-astra", "gpt-6.2-astra-high", "gpt-6.2-astra-2026-11-08"} {
			require.False(t, a.IsModelSupported(model), "%s with %v", model, mapping)
		}
		require.True(t, a.IsModelSupported(openai.GPT62SolModel), "%v", mapping)
		if mapping["alias"] != nil {
			require.False(t, a.IsModelSupported("alias"))
		}
	}

	// Passthrough must respect the scheduling block before bypassing allowlists.
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{"astra_model_disabled": true, "openai_passthrough": true}}
	require.False(t, a.IsModelSupported(openai.GPT62AstraModel))
	require.True(t, a.IsModelSupported(openai.GPT62SolModel))
}

func TestCodexWSAnchorRemainsLegacyAstraOnly(t *testing.T) {
	s, a := anchorTestService()
	c := anchorTestContext(1, "generation-isolation")
	completeAnchorSeed(t, s, a, c)
	for _, model := range []string{openai.GPT62AstraModel, openai.GPT62SolModel} {
		for _, previous := range []string{"", "resp_seed"} {
			body, err := json.Marshal(map[string]string{"model": model, "previous_response_id": previous})
			require.NoError(t, err)
			ctx, finish, err := s.prepareCodexWSAnchor(t.Context(), c, a, body)
			require.NoError(t, err)
			require.Nil(t, finish)
			require.Nil(t, codexWSAnchorFromContext(ctx))
			require.Len(t, s.codexWSAnchors.entries, 1)
			require.Empty(t, s.codexWSAnchors.busy)
		}
	}
	ctx, finish, err := s.prepareCodexWSAnchor(t.Context(), c, a, []byte(`{"model":"gpt-6-astra","previous_response_id":"resp_seed"}`))
	require.NoError(t, err)
	require.Equal(t, "resp_seed", codexWSAnchorFromContext(ctx).previousID)
	codexWSAnchorFromContext(ctx).qualified = true
	finish(&OpenAIForwardResult{ResponseID: "resp_wrong_generation", UpstreamResponseModel: openai.GPT62AstraModel}, nil)
	require.Empty(t, s.codexWSAnchors.entries, "a GPT-6.2 response cannot renew a legacy anchor")
}

func TestAstraCompletionQualificationDoesNotCrossGenerations(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", openai.GPT62AstraModel, openai.GPT62SolModel} {
		for _, event := range []string{"response.completed", "response.done"} {
			for _, output := range []string{`[]`, `[{"content":[{"type":"output_text","text":"OK"}]}]`} {
				body := []byte(fmt.Sprintf(`{"response":{"model":%q,"status":"completed","output":%s}}`, model, output))
				require.Equal(t, model == "gpt-6-astra" && output != `[]`, codexAstraCompleted(gjson.GetBytes(body, "response")))
				observer := &upstreamResponseModelObserver{}
				observer.ObserveOpenAI([]byte(`{"delta":"OK"}`), "response.output_text.delta")
				observer.ObserveOpenAI(body, event)
				require.Equal(t, model == "gpt-6-astra", observer.astraCompleted)
				require.Equal(t, model, observer.Model())
			}
		}
	}
	observer := &upstreamResponseModelObserver{}
	observer.ObserveOpenAI([]byte(`{"response":{"model":"gpt-6-astra"}}`), "response.created")
	observer.ObserveOpenAI([]byte(`{"response":{"model":"gpt-6.2-astra","status":"completed","output":[{"content":[{"type":"output_text","text":"OK"}]}]}}`), "response.completed")
	require.True(t, observer.Conflict())
	require.False(t, observer.astraCompleted)
}

func TestAstraGPT62TicketsRemainIndependentAndOptIn(t *testing.T) {
	s := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, FailClosed: true}, nil)
	a := ticketTestAccount(41)
	attachReadyCodexTicket(a, "gpt-6-astra")
	require.NotNil(t, s.lookupOpenAICodexTicket(a, "gpt-6-astra"))
	for _, model := range []string{openai.GPT62AstraModel, openai.GPT62SolModel} {
		require.Nil(t, s.lookupOpenAICodexTicket(a, model))
		require.False(t, s.openAICodexTicketGatedModel(model), "new models are not added to the default ticket list")
	}

	s.cfg.Gateway.OpenAICodexTicket.Models = []string{"gpt-6-astra", openai.GPT62AstraModel, openai.GPT62SolModel}
	for _, model := range []string{openai.GPT62AstraModel, openai.GPT62SolModel} {
		require.True(t, s.openAICodexTicketBlocksAccount(a, model), "a legacy ticket cannot satisfy an explicit GPT-6.2 gate")
		require.ErrorIs(t, s.applyOpenAICodexTicket(t.Context(), a, model, http.Header{}), ErrOpenAICodexTicketUnavailable)
	}
	attachReadyCodexTicket(a, openai.GPT62AstraModel)
	require.False(t, s.openAICodexTicketBlocksAccount(a, openai.GPT62AstraModel))
	require.True(t, s.openAICodexTicketBlocksAccount(a, openai.GPT62SolModel))
	require.Equal(t, openai.GPT62AstraModel, s.lookupOpenAICodexTicket(a, openai.GPT62AstraModel).Model)
	require.Equal(t, "gpt-6-astra", s.lookupOpenAICodexTicket(a, "gpt-6-astra").Model)

	// The legacy model-level readiness flag must not accept a new model's ticket.
	s.cfg.Gateway.OpenAICodexTicket.Models = []string{openai.GPT62AstraModel}
	require.False(t, s.ticketPoolSchedulingReady(a, config.AstraRoutingSettings{SchedulingMode: "model"}))
}

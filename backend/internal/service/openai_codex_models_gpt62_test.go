package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func TestGPT62CodexFallbackKeepsIndependentIdentityAndGenericLimits(t *testing.T) {
	for _, modelID := range []string{
		openai.GPT62SolModel, openai.GPT62AstraModel,
		"OpenAI/GPT-6.2-SOL", "vendor/gpt-6.2-astra-max",
		"gpt-6.2-sol-openai-compact", "gpt-6.2-astra-2026-11-07",
	} {
		t.Run(modelID, func(t *testing.T) {
			descriptor := newConfiguredCodexModelDescriptor(modelID)
			require.Equal(t, modelID, descriptor.Slug)
			require.Empty(t, descriptor.officialMetadata)
			require.Equal(t, openai.CodexBaseInstructionsForModel("gpt-6-astra"), descriptor.ModelMessages.InstructionsTemplate,
				"GPT-6 prompt is a compatibility template, not GPT-6.2 official metadata")
			require.EqualValues(t, configuredCodexFallbackContext, descriptor.ContextWindow)
			require.EqualValues(t, configuredCodexFallbackContext, descriptor.MaxContextWindow)
			require.Nil(t, descriptor.MultiAgentReasoningEffort)
			require.Empty(t, descriptor.MultiAgentVersion)
			require.Empty(t, descriptor.ServiceTiers)
			require.Equal(t, []string{"text", "image"}, descriptor.InputModalities)
			require.Equal(t, "medium", *descriptor.DefaultReasoningLevel)
			require.True(t, isOpenAICodexReasoningGPTModel(modelID))
			require.True(t, isOpenAICodexImageInputModel(modelID))
			encoded, err := json.Marshal(descriptor)
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, json.Unmarshal(encoded, &fields))
			require.Equal(t, []string{"low", "medium", "high", "xhigh", "max"}, effortsFromManifestModel(t, fields))
			require.NotContains(t, fields, "max_output_tokens")
		})
	}
	for _, modelID := range []string{
		"gpt-6.2-sol-preview", "gpt-6.2-astra-ultra", "gpt-6.2-astra-2026-02-30", "gpt-6.3-sol",
	} {
		t.Run(modelID, func(t *testing.T) {
			require.False(t, isOpenAICodexReasoningGPTModel(modelID))
			require.False(t, isOpenAICodexImageInputModel(modelID))
			descriptor := newConfiguredCodexModelDescriptor(modelID)
			require.Equal(t, modelID, descriptor.Slug)
			require.Equal(t, "none", *descriptor.DefaultReasoningLevel)
		})
	}
	body, err := BuildCodexModelsManifest([]string{openai.GPT62SolModel, openai.GPT62AstraModel})
	require.NoError(t, err)
	models := decodeCodexManifestModels(t, body)
	require.Len(t, models, 2)
	require.Equal(t, openai.GPT62SolModel, models[0]["slug"])
	require.Equal(t, "GPT-6.2 Sol", models[0]["display_name"])
	require.Equal(t, openai.GPT62AstraModel, models[1]["slug"])
	require.Equal(t, "GPT-6.2 Astra", models[1]["display_name"])
}

func TestGPT62CodexAPIKeyPreservesNativeMetadata(t *testing.T) {
	for _, baseURL := range []string{"https://api.openai.com/v1", "https://relay.example/v1"} {
		for _, modelID := range []string{openai.GPT62SolModel, openai.GPT62AstraModel} {
			t.Run(baseURL+"/"+modelID, func(t *testing.T) {
				account := newCodexModelsAPIKeyTestAccount(baseURL)
				reasoning := true
				account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{
					modelID: {ID: modelID, ContextWindow: 999_000, Reasoning: &reasoning,
						SupportedReasoningLevels: []string{"low"}, InputModalities: []string{"text", "image"}},
				}})
				manifest := &OpenAIModelsResponse{Body: []byte(`{"models":[{
					"slug":"` + modelID + `","display_name":"Upstream display","description":"Upstream description",
					"default_reasoning_level":"max","supported_reasoning_levels":[{"effort":"high"},{"effort":"max"}],
					"context_window":64000,"max_context_window":128000,"max_output_tokens":777,
					"input_modalities":["text"],"supports_image_detail_original":false,
					"supports_search_tool":false,"tool_mode":"upstream","comp_hash":"upstream-hash",
					"use_responses_lite":true,"service_tiers":[],"multi_agent_version":"upstream-v3"
				}]}`)}
				svc := &OpenAIGatewayService{}
				require.NoError(t, svc.CompleteAPIKeyCodexModelsManifestForClient(manifest, account))
				model := decodeCodexManifestModels(t, manifest.Body)[0]
				require.Equal(t, modelID, model["slug"])
				require.Equal(t, "Upstream display", model["display_name"])
				require.Equal(t, "Upstream description", model["description"])
				require.Equal(t, "max", model["default_reasoning_level"])
				require.Equal(t, []string{"high", "max"}, effortsFromManifestModel(t, model))
				require.EqualValues(t, 64_000, model["context_window"])
				require.EqualValues(t, 128_000, model["max_context_window"])
				require.EqualValues(t, 777, model["max_output_tokens"])
				require.Equal(t, []any{"text"}, model["input_modalities"])
				require.Equal(t, false, model["supports_image_detail_original"])
				require.Equal(t, false, model["supports_search_tool"])
				require.Equal(t, false, model["use_responses_lite"])
				require.Equal(t, "upstream", model["tool_mode"])
				require.Equal(t, "upstream-hash", model["comp_hash"])
				require.Equal(t, "upstream-v3", model["multi_agent_version"])
				require.Empty(t, model["service_tiers"])
			})
		}
	}
}

func TestGPT62CodexAPIKeyListUsesOwnLiveMetadataBeforeSnapshot(t *testing.T) {
	account := newCodexModelsAPIKeyTestAccount("https://api.openai.com/v1")
	reasoning := true
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{
		"gpt-6-astra": {ID: "gpt-6-astra", ContextWindow: 9_999_999},
		"gpt-6.1-sol": {ID: "gpt-6.1-sol", ContextWindow: 8_888_888},
		openai.GPT62SolModel: {ID: openai.GPT62SolModel, DisplayName: "Snapshot Sol", ContextWindow: 777_000,
			Reasoning: &reasoning, SupportedReasoningLevels: []string{"low"}, InputModalities: []string{"text", "image"}},
		openai.GPT62AstraModel: {ID: openai.GPT62AstraModel, DisplayName: "Snapshot Astra", ContextWindow: 888_000,
			MaxContextWindow: 999_000, Reasoning: &reasoning, DefaultReasoningLevel: "max",
			SupportedReasoningLevels: []string{"high", "max"}, InputModalities: []string{"text"}},
	}})
	source := []byte(`{"data":[
		{"id":"gpt-6.2-sol","display_name":"Live Sol","description":"Live description",
		 "context_window":64000,"max_context_window":128000,"input_modalities":["text"],
		 "default_reasoning_level":"max","supported_reasoning_levels":[{"effort":"high"},{"effort":"max"}],
		 "supports_search_tool":false,"use_responses_lite":true},
		{"id":"gpt-6.2-astra"}
	]}`)
	manifest := &OpenAIModelsResponse{
		Body:               convertOpenAIModelListToCodexManifestForAccount(source, account),
		upstreamSourceBody: source, convertedFromOpenAIModelList: true,
	}
	svc := &OpenAIGatewayService{}
	require.NoError(t, svc.CompleteAPIKeyCodexModelsManifestForClient(manifest, account))
	models := decodeCodexManifestModels(t, manifest.Body)
	require.Len(t, models, 2)
	sol, astra := models[0], models[1]
	require.Equal(t, openai.GPT62SolModel, sol["slug"])
	require.Equal(t, "Live Sol", sol["display_name"])
	require.Equal(t, "Live description", sol["description"])
	require.EqualValues(t, 64_000, sol["context_window"])
	require.EqualValues(t, 128_000, sol["max_context_window"])
	require.Equal(t, []any{"text"}, sol["input_modalities"])
	require.Equal(t, "max", sol["default_reasoning_level"])
	require.Equal(t, []string{"high", "max"}, effortsFromManifestModel(t, sol))
	require.Equal(t, false, sol["supports_search_tool"])
	require.Equal(t, false, sol["use_responses_lite"])
	require.Equal(t, openai.GPT62AstraModel, astra["slug"])
	require.Equal(t, "Snapshot Astra", astra["display_name"])
	require.EqualValues(t, 888_000, astra["context_window"])
	require.EqualValues(t, 999_000, astra["max_context_window"])
	require.Equal(t, []any{"text"}, astra["input_modalities"])
	require.Equal(t, []string{"high", "max"}, effortsFromManifestModel(t, astra))
	require.Equal(t, "max", astra["default_reasoning_level"])
	require.Equal(t, true, astra["supports_search_tool"])
	require.Equal(t, false, astra["use_responses_lite"])

	// A refreshed snapshot only fills fields that the live source omitted.
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{
		openai.GPT62AstraModel: {ID: openai.GPT62AstraModel, ContextWindow: 456_000},
	}})
	require.NoError(t, svc.CompleteAPIKeyCodexModelsManifestForClient(manifest, account))
	models = decodeCodexManifestModels(t, manifest.Body)
	require.EqualValues(t, 64_000, models[0]["context_window"])
	require.EqualValues(t, 456_000, models[1]["context_window"])
}

func TestGPT62CodexGroupDoesNotBorrowPreviousGenerationMetadata(t *testing.T) {
	account := Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{
		"gpt-6-astra": {ID: "gpt-6-astra", ContextWindow: 9_999_999},
		"gpt-6.1-sol": {ID: "gpt-6.1-sol", ContextWindow: 8_888_888},
	}})
	body, err := buildCodexModelsManifestForAccounts(PlatformOpenAI,
		[]string{openai.GPT62SolModel, openai.GPT62AstraModel}, []Account{account}, nil, nil, true)
	require.NoError(t, err)
	for _, model := range decodeCodexManifestModels(t, body) {
		require.EqualValues(t, configuredCodexFallbackContext, model["context_window"])
		require.EqualValues(t, configuredCodexFallbackContext, model["max_context_window"])
		require.Contains(t, effortsFromManifestModel(t, model), "max")
		require.Equal(t, []any{"text", "image"}, model["input_modalities"])
		require.Nil(t, model["multi_agent_reasoning_effort"])
		require.Empty(t, model["multi_agent_version"])
	}
}

func TestGPT62CodexResponsesLiteGuardRecognizesOnlySupportedSpellings(t *testing.T) {
	for _, tc := range []struct {
		modelID string
		disable bool
	}{
		{openai.GPT62SolModel, true}, {openai.GPT62AstraModel, true},
		{"vendor/GPT-6.2-SOL-max", true}, {"gpt-6.2-astra-2026-11-07", true},
		{"gpt-6.2-astra-openai-compact", true}, {"gpt-6.2-sol-preview", false},
		{"gpt-6.2-astra-ultra", false}, {"gpt-6.3-sol", false},
	} {
		t.Run(tc.modelID, func(t *testing.T) {
			account := newCodexModelsAPIKeyTestAccount("https://relay.example/v1")
			account.Credentials["model_mapping"] = map[string]any{"public-model": tc.modelID}
			body, err := adjustAPIKeyCodexModelsManifest([]byte(`{"models":[{"slug":"public-model","use_responses_lite":true}]}`), account)
			require.NoError(t, err)
			require.Equal(t, !tc.disable, decodeCodexManifestModels(t, body)[0]["use_responses_lite"])
			capabilities := accountCodexToolCapabilities(account, tc.modelID)
			if tc.disable {
				require.JSONEq(t, "false", string(capabilities["use_responses_lite"]))
			} else {
				require.NotContains(t, capabilities, "use_responses_lite")
			}
		})
	}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{
		openai.GPT62SolModel: {ID: openai.GPT62SolModel, CodexToolCapabilities: map[string]json.RawMessage{
			"use_responses_lite": json.RawMessage("true"), "supports_search_tool": json.RawMessage("false"),
		}},
	}})
	capabilities := accountCodexToolCapabilities(account, openai.GPT62SolModel)
	require.JSONEq(t, "true", string(capabilities["use_responses_lite"]))
	require.JSONEq(t, "false", string(capabilities["supports_search_tool"]))
	require.NotContains(t, capabilities, "comp_hash")
	require.NotContains(t, capabilities, "tool_mode")
}

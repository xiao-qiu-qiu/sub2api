package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const testCodexV2Session = "019a2984-e900-7abc-8def-0123456789ab"

func newCodexIdentityV2TestAccount(id int64, mode string) *Account {
	account := newTestOAuthAccount(id, map[string]any{
		codexIdentitySchemeExtraKey:  "v2",
		codexFingerprintModeExtraKey: mode,
	})
	account.AuthConnectionID = fmt.Sprintf("test-connection-%d", id)
	return account
}

func TestCodexIdentityV2OptInPreservesLegacyDerivationAndWire(t *testing.T) {
	for _, scheme := range []any{nil, "", "v1", "V2", " v2 ", true} {
		account := newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "session", codexIdentitySchemeExtraKey: scheme})
		account.AuthConnectionID = "legacy-connection"
		require.False(t, codexIdentityV2Enabled(account))
		for _, kind := range []string{"session", "thread", "window", "prompt-cache"} {
			raw := testCodexV2Session
			if kind == "window" {
				raw += ":9"
			}
			expected := deriveStableUUIDv4(fmt.Sprintf("sub2api:codex-account-identity:v1:user:7:account:connection:legacy-connection:kind:%s:value:%s", kind, raw))
			require.Equal(t, expected, scopeCodexAccountIdentityValue(account, 7, kind, raw))
		}
		metadata := gjson.Parse(`{"session_id":"body-only-session","window_number":7}`)
		ids := resolveCodexFingerprintIDsWithBody(nil, account, nil, metadata)
		require.NotNil(t, ids)
		require.Equal(t, ids.sessionID, ids.threadID, "v1 must ignore body-only session evidence")
		require.Equal(t, ids.threadID+":0", ids.windowID)
		metadataHeader := http.Header{}
		metadataHeader.Set(openAIWSTurnMetadataHeader, ` { "session_id" : "original", "z" : 1 } `)
		applyCodexFingerprintHeaders(metadataHeader, ids)
		require.False(t, gjson.Get(metadataHeader.Get(openAIWSTurnMetadataHeader), "window_number").Exists())
		ws := []byte(`{"client_metadata":{"session_id":"keep-v1"}}`)
		out, err := applyCodexFingerprintToWSPayload(nil, account, ws, ids)
		require.NoError(t, err)
		require.Equal(t, ws, out)
	}
}

func TestCodexIdentityV2RelationshipsAndAccountIsolation(t *testing.T) {
	first := newCodexIdentityV2TestAccount(1, "off")
	second := newCodexIdentityV2TestAccount(2, "off")
	session := scopeCodexAccountIdentityValue(first, 7, "session", testCodexV2Session)
	parsed, err := uuid.Parse(session)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), parsed.Version())
	require.Equal(t, testCodexV2Session[:13], session[:13], "v7 timestamp must survive scoping")
	for _, kind := range []string{"thread", "prompt-cache", "request"} {
		require.Equal(t, session, scopeCodexAccountIdentityValue(first, 7, kind, testCodexV2Session))
	}
	require.Equal(t, session+":0009", scopeCodexAccountIdentityValue(first, 7, "window", testCodexV2Session+":0009"))
	require.Equal(t, "guardian:"+session, scopeCodexAccountIdentityValue(first, 7, "prompt-cache", "guardian:"+testCodexV2Session))
	require.NotEqual(t, session, scopeCodexAccountIdentityValue(second, 7, "session", testCodexV2Session))
	require.NotEqual(t, session, scopeCodexAccountIdentityValue(first, 8, "session", testCodexV2Session))
	metadata := map[string]any{
		"session_id": testCodexV2Session, "thread_id": testCodexV2Session,
		"turn_id": testCodexV2Session, "root_turn_id": testCodexV2Session,
		"parent_turn_id": testCodexV2Session, "parent_thread_id": testCodexV2Session,
		"forked_from_thread_id": testCodexV2Session, "context_window_id": testCodexV2Session,
	}
	require.True(t, applyCodexAccountIdentityFields(metadata, first, 7))
	require.Equal(t, metadata["turn_id"], metadata["root_turn_id"])
	require.Equal(t, metadata["turn_id"], metadata["parent_turn_id"])
	require.Equal(t, session, metadata["parent_thread_id"])
	require.Equal(t, session, metadata["forked_from_thread_id"])
	require.NotEqual(t, session, metadata["context_window_id"])
	require.NotEqual(t, metadata["turn_id"], metadata["context_window_id"])
}

func TestCodexIdentityV2NestedRawAndMapPreserveNumbersAndOpaqueMetadata(t *testing.T) {
	account := newCodexIdentityV2TestAccount(3, "device")
	embedded := ` { "session_id" : "` + testCodexV2Session + `", "installation_id":"raw-install", "opaque" : [9007199254740993,1.2300e+42,"\u0061<>&"], "turn_id":"raw-turn", "root_turn_id":"raw-turn" } `
	bodyMap := map[string]any{
		"type": "response.create",
		"response": map[string]any{
			"prompt_cache_key": testCodexV2Session,
			"client_metadata": map[string]any{
				"session_id": testCodexV2Session, "thread_id": testCodexV2Session,
				"unknown_number":           json.Number("9007199254740993"),
				openAIWSTurnMetadataHeader: embedded,
			},
		},
	}
	// Decode the raw fixture with UseNumber before exercising the map path.
	raw, err := marshalOpenAIUpstreamJSON(bodyMap)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, decodeOpenAIJSONUseNumber(raw, &decoded))
	ids := resolveCodexFingerprintIDsWithBody(nil, account, nil, gjson.ParseBytes(raw))
	require.NotNil(t, ids)
	require.True(t, applyCodexAccountIdentityClientMetadataMap(decoded, account, 7))
	require.True(t, applyCodexFingerprintClientMetadata(decoded, ids))
	fromMap, err := marshalOpenAIUpstreamJSON(decoded)
	require.NoError(t, err)
	fromRaw, changed, err := applyCodexAccountIdentityClientMetadataRaw(raw, account, 7)
	require.NoError(t, err)
	require.True(t, changed)
	fromRaw, changed, err = applyCodexFingerprintClientMetadataRaw(fromRaw, ids)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, string(fromMap), string(fromRaw))
	result := gjson.ParseBytes(fromRaw)
	require.False(t, result.Get("client_metadata").Exists(), "nested envelope must remain nested")
	require.Equal(t, "9007199254740993", result.Get("response.client_metadata.unknown_number").Raw)
	require.Equal(t, result.Get("response.prompt_cache_key").String(), result.Get("response.client_metadata.thread_id").String())
	updatedEmbedded := result.Get("response.client_metadata." + openAIWSTurnMetadataHeader).String()
	require.Contains(t, updatedEmbedded, `"opaque" : [9007199254740993,1.2300e+42,"\u0061<>&"]`)
	require.True(t, strings.HasPrefix(updatedEmbedded, ` { "session_id" : `))
	require.True(t, strings.HasSuffix(updatedEmbedded, " } "))
}

func TestCodexIdentityV2DeviceBodyInstallationMatchesCompactHeader(t *testing.T) {
	for _, deviceID := range []string{"", "22222222-2222-4222-8222-222222222222"} {
		t.Run("device_"+deviceID, func(t *testing.T) {
			c := newFingerprintStageTestContext(t)
			account := newCodexIdentityV2TestAccount(30, "device")
			account.Extra["openai_device_id"] = deviceID
			body := map[string]any{"client_metadata": map[string]any{
				"installation_id": "client-installation", "x-codex-installation-id": "client-installation",
				openAIWSTurnMetadataHeader: `{"installation_id":"client-installation"}`,
			}}
			ids := resolveCodexFingerprintIDsWithBody(c, account, nil, body)
			require.NotNil(t, ids)
			require.True(t, applyCodexAccountIdentityClientMetadataMap(body, account, 0))
			require.True(t, applyCodexFingerprintClientMetadata(body, ids))
			metadata := body["client_metadata"].(map[string]any)
			require.Equal(t, ids.installationID, metadata["installation_id"])
			require.Equal(t, ids.installationID, metadata["x-codex-installation-id"])
			require.Equal(t, ids.installationID, gjson.Get(metadata[openAIWSTurnMetadataHeader].(string), "installation_id").String())

			// Compact has no client_metadata; its independently resolved header
			// must reuse the exact installation identity carried in Responses.
			c.Request.URL.Path = "/v1/responses/compact"
			stageCodexFingerprintIDs(c, resolveCodexFingerprintIDsWithBody(c, account, nil, nil))
			stageCodexConvergenceBodyIdentityMap(c, account, nil)
			svc := &OpenAIGatewayService{}
			req, err := svc.buildUpstreamRequest(context.Background(), c, account,
				[]byte(`{"model":"gpt-6-astra","input":[]}`), "test-token", false, "", true)
			require.NoError(t, err)
			require.True(t, strings.HasSuffix(req.URL.Path, "/responses/compact"))
			require.Equal(t, ids.installationID, req.Header.Get("x-codex-installation-id"))
		})
	}
}

func TestCodexIdentityV2RawMapCompositeParentThreadRelationships(t *testing.T) {
	account := newCodexIdentityV2TestAccount(31, "device")
	for _, prefix := range []string{"", "response."} {
		root := `{"prompt_cache_key":"guardian:` + testCodexV2Session + `","client_metadata":{"session_id":"` + testCodexV2Session + `","thread_id":"` + testCodexV2Session + `","parent_thread_id":"` + testCodexV2Session + `","x-codex-parent-thread-id":"` + testCodexV2Session + `","x-codex-window-id":"` + testCodexV2Session + `:12"}}`
		if prefix != "" {
			root = `{"type":"response.create","response":` + root + `}`
		}
		var decoded map[string]any
		require.NoError(t, decodeOpenAIJSONUseNumber([]byte(root), &decoded))
		ids := resolveCodexFingerprintIDsWithBody(nil, account, nil, gjson.Parse(root))
		require.NotNil(t, ids)
		require.True(t, applyCodexAccountIdentityClientMetadataMap(decoded, account, 7))
		require.True(t, applyCodexFingerprintClientMetadata(decoded, ids))
		fromMap, err := marshalOpenAIUpstreamJSON(decoded)
		require.NoError(t, err)
		fromRaw, _, err := applyCodexAccountIdentityClientMetadataRaw([]byte(root), account, 7)
		require.NoError(t, err)
		fromRaw, _, err = applyCodexFingerprintClientMetadataRaw(fromRaw, ids)
		require.NoError(t, err)
		require.JSONEq(t, string(fromMap), string(fromRaw))
		result := gjson.ParseBytes(fromRaw)
		thread := result.Get(prefix + "client_metadata.thread_id").String()
		require.Equal(t, thread, result.Get(prefix+"client_metadata.parent_thread_id").String())
		require.Equal(t, thread, result.Get(prefix+"client_metadata.x-codex-parent-thread-id").String())
		require.Equal(t, thread+":12", result.Get(prefix+"client_metadata.x-codex-window-id").String())
		require.Equal(t, "guardian:"+thread, result.Get(prefix+"prompt_cache_key").String())
		c := newFingerprintStageTestContext(t)
		stageCodexConvergenceBodyIdentityRaw(c, account, fromRaw)
		headers := http.Header{}
		applyCodexFingerprintConvergenceHeaders(c, account, headers)
		require.Equal(t, thread, headers.Get("thread-id"))
		require.Equal(t, thread, headers.Get("x-codex-parent-thread-id"))
		require.Equal(t, thread, headers.Get("x-client-request-id"))
	}
}

func TestCodexIdentityV2BodyEvidenceWindowRootAndWSProjection(t *testing.T) {
	c := newFingerprintStageTestContext(t)
	c.Set("api_key", &APIKey{ID: 7})
	account := newCodexIdentityV2TestAccount(4, "session")
	metadata := `{"session_id":"` + testCodexV2Session + `","turn_id":"root-turn","root_turn_id":"root-turn","window_number":18446744073709551615,"opaque":9007199254740993}`
	body, err := marshalOpenAIUpstreamJSON(map[string]any{
		"response": map[string]any{
			"prompt_cache_key": testCodexV2Session,
			"client_metadata":  map[string]any{openAIWSTurnMetadataHeader: metadata},
		},
	})
	require.NoError(t, err)
	c.Request.Header.Set("x-codex-window-id", testCodexV2Session+":1")
	ids := resolveCodexFingerprintIDsWithBody(c, account, nil, gjson.ParseBytes(body))
	require.NotNil(t, ids)
	require.Equal(t, resolveConvergedThreadID(testCodexFingerprintSeed, testCodexV2Session), ids.threadID)
	require.Equal(t, ids.threadID+":18446744073709551615", ids.windowID)
	scoped, _, err := applyCodexAccountIdentityClientMetadataRaw(body, account, 7)
	require.NoError(t, err)
	out, err := applyCodexFingerprintToWSPayload(c, account, scoped, ids)
	require.NoError(t, err)
	require.Equal(t, ids.sessionID, gjson.GetBytes(out, "response.prompt_cache_key").String())
	turn := gjson.Parse(gjson.GetBytes(out, "response.client_metadata."+openAIWSTurnMetadataHeader).String())
	require.Equal(t, ids.turnID, turn.Get("turn_id").String())
	require.Equal(t, ids.turnID, turn.Get("root_turn_id").String())
	require.Equal(t, "18446744073709551615", turn.Get("window_number").Raw)
	require.Equal(t, "9007199254740993", turn.Get("opaque").Raw)
	headers := http.Header{}
	applyStagedCodexFingerprintHeaders(c, account, headers)
	applyCodexFingerprintConvergenceHeaders(c, account, headers)
	require.Equal(t, ids.sessionID, headers.Get("session-id"))
	require.Equal(t, ids.threadID, headers.Get("thread-id"))
	require.Equal(t, ids.threadID, headers.Get("x-client-request-id"))
	require.Empty(t, headers.Get("session_id"))
	c.Set("codex_wire_skip", true)
	pinned := []byte(`{"client_metadata":{"session_id":"harvest-pin"}}`)
	out, err = applyCodexFingerprintToWSPayload(c, account, pinned, ids)
	require.NoError(t, err)
	require.Equal(t, pinned, out)
	require.Nil(t, stagedCodexFingerprintIDs(c, account))
}

func TestCodexIdentityV2StagesFollowShadowCredentialAndClearOnFailover(t *testing.T) {
	c := newFingerprintStageTestContext(t)
	parent := newCodexIdentityV2TestAccount(5, "session")
	shadow := newCodexIdentityV2TestAccount(15, "session")
	shadow.ParentAccountID = &parent.ID
	svc := &OpenAIGatewayService{accountRepo: &codexAccountIdentityRepoStub{account: parent}}
	source, err := svc.prepareCodexAccountIdentitySource(context.Background(), c, shadow)
	require.NoError(t, err)
	require.Same(t, parent, source)
	ids := resolveCodexFingerprintIDsWithBody(c, shadow, nil, map[string]any{"session_id": testCodexV2Session})
	require.NotNil(t, ids)
	require.Equal(t, codexAccountIdentityNamespace(parent), ids.credentialNamespace)
	require.Equal(t, shadow.ID, ids.accountID)
	stageCodexFingerprintIDs(c, ids)
	stageCodexConvergenceBodyIdentityMap(c, source, map[string]any{"response": map[string]any{
		"client_metadata": map[string]any{"session_id": "derived-session", "thread_id": "derived-thread", "parent_thread_id": "derived-parent"},
	}})
	headers := http.Header{}
	applyCodexFingerprintConvergenceHeaders(c, source, headers)
	require.Equal(t, "derived-session", headers.Get("session-id"))
	require.Equal(t, "derived-thread", headers.Get("x-client-request-id"))
	require.Equal(t, "derived-parent", headers.Get("x-codex-parent-thread-id"))
	// Even the same row ID must not reuse identity from a different credential.
	changedCredential := *parent
	changedCredential.AuthConnectionID = "replacement-credential"
	require.Nil(t, stagedCodexConvergenceBodyIdentity(c, &changedCredential))
	c.Set(codexAccountIdentitySourceContextKey, &changedCredential)
	require.Nil(t, stagedCodexFingerprintIDs(c, shadow))
	_, err = svc.prepareCodexAccountIdentitySource(context.Background(), c, newCodexIdentityV2TestAccount(6, "off"))
	require.NoError(t, err)
	require.Nil(t, stagedCodexConvergenceBodyIdentity(c, parent))
	require.Nil(t, stagedCodexFingerprintIDs(c, shadow))
}

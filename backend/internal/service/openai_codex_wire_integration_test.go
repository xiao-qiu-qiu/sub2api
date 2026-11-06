package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexWireHTTPTransportBoundary(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := []byte(`{ "input":[],"model":"mapped-model","client_metadata":{"sequence":9007199254740993,"x-codex-turn-metadata":"{\"model\":\"client-model\", \"unknown\":9007199254740993}"}}`)
	account := newTestOAuthAccount(901, map[string]any{
		"codex_identity_scheme": "v2",
		"codex_wire_options":    map[string]any{"body_order": true, "request_compression": true},
	})
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Version", "0.156.1")
	svc := &OpenAIGatewayService{}
	req, err = svc.prepareCodexHTTPWireRequest(c, account, req)
	require.NoError(t, err)
	plain, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Empty(t, req.Header.Get("Content-Encoding"), "local policy and ticket observers still receive JSON")
	require.True(t, bytes.HasPrefix(plain, []byte(`{"model":`)))
	require.Equal(t, "mapped-model", gjson.Parse(gjson.GetBytes(plain, "client_metadata.x-codex-turn-metadata").Str).Get("model").Str)
	require.Equal(t, "9007199254740993", gjson.GetBytes(plain, "client_metadata.sequence").Raw)
	wire, err := EncodeCodexNativeHTTPRequest(req)
	require.NoError(t, err)
	require.NotSame(t, req, wire)
	require.Equal(t, "zstd", wire.Header.Get("Content-Encoding"))
	require.Empty(t, req.Header.Get("Content-Encoding"))
	encoded, err := io.ReadAll(wire.Body)
	require.NoError(t, err)
	require.Equal(t, int64(len(encoded)), wire.ContentLength)
	decoder, err := zstd.NewReader(nil)
	require.NoError(t, err)
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(encoded, nil)
	require.NoError(t, err)
	require.Equal(t, plain, decoded)
	replay, err := wire.GetBody()
	require.NoError(t, err)
	defer func() { _ = replay.Close() }()
	copy, err := io.ReadAll(replay)
	require.NoError(t, err)
	require.Equal(t, encoded, copy)
	legacy := newTestOAuthAccount(902, nil)
	original, _ := http.NewRequest(http.MethodPost, req.URL.String(), bytes.NewReader(body))
	unchanged, err := svc.prepareCodexHTTPWireRequest(c, legacy, original)
	require.NoError(t, err)
	require.Same(t, original, unchanged)
	originalBytes, _ := io.ReadAll(unchanged.Body)
	require.Equal(t, body, originalBytes)
}

func TestCodexWireWSBodyHeaderAndNestedIdentity(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	account := newTestOAuthAccount(903, map[string]any{"codex_identity_scheme": "v2", "codex_fingerprint_mode": "device"})
	raw := []byte(`{"type":"response.create","model":"mapped","client_metadata":{"session_id":"019a3400-0000-7000-8000-000000000001","thread_id":"019a3400-0000-7000-8000-000000000001"},"response":{"client_metadata":{"session_id":"019a3400-0000-7000-8000-000000000001","sequence":9007199254740993}}}`)
	svc := &OpenAIGatewayService{}
	scoped, err := svc.scopeCodexWSIdentity(context.Background(), c, account, "mapped", raw)
	require.NoError(t, err)
	headers := http.Header{}
	applyCodexFingerprintConvergenceHeaders(c, account, headers)
	thread := gjson.GetBytes(scoped, "client_metadata.thread_id").Str
	require.NotEmpty(t, thread)
	require.Equal(t, thread, gjson.GetBytes(scoped, "client_metadata.session_id").Str)
	require.Equal(t, thread, headers.Get("session-id"))
	require.Equal(t, thread, headers.Get("thread-id"))
	require.Equal(t, thread, gjson.GetBytes(scoped, "response.client_metadata.session_id").Str)
	require.Equal(t, "9007199254740993", gjson.GetBytes(scoped, "response.client_metadata.sequence").Raw)
	frame := svc.prepareCodexWSFrame(context.Background(), c, account, scoped, "client-turn-state")
	require.Equal(t, "client-turn-state", gjson.GetBytes(frame, "client_metadata.x-codex-turn-state").Str)
	require.NotEmpty(t, gjson.GetBytes(frame, "client_metadata.x-codex-ws-stream-request-start-ms").Str)
	require.NotEmpty(t, gjson.GetBytes(frame, "response.client_metadata.x-codex-ws-stream-request-start-ms").Str)
	other := newTestOAuthAccount(904, map[string]any{"codex_identity_scheme": "v2", "codex_fingerprint_mode": "device"})
	other.Extra[codexFingerprintSeedExtraKey] = "22222222-2222-4222-8222-222222222222"
	changed, err := svc.scopeCodexWSIdentity(context.Background(), c, other, "mapped", raw)
	require.NoError(t, err)
	require.NotEqual(t, thread, gjson.GetBytes(changed, "client_metadata.thread_id").Str)
	require.Nil(t, stagedCodexFingerprintIDs(c, account), "previous account IDs cannot survive failover")
}

func TestCodexWireSettingsValidation(t *testing.T) {
	account := newTestOAuthAccount(905, nil)
	for _, extra := range []map[string]any{
		{"codex_identity_scheme": "v3"},
		{"codex_wire_options": map[string]any{"body_order": "true"}},
		{"codex_wire_options": map[string]any{"unknown": true}},
		{"codex_wire_timezone": "invalid/timezone"},
		{"codex_wire_user_location": map[string]any{"timezone": "invalid/timezone"}},
	} {
		require.Error(t, validateCodexFingerprintSettings(account, extra))
	}
	require.NoError(t, validateCodexFingerprintSettings(account, map[string]any{"codex_identity_scheme": "v2", "codex_wire_options": map[string]any{"settings_user": false}}))
	require.Error(t, validateCodexFingerprintSettings(&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, map[string]any{"codex_identity_scheme": "v2"}))
}

func TestCodexWireCompactRestoresOnlyVersionedCacheKey(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	c.Set(openAICompactSessionSeedKey, "019a3400-0000-7000-8000-000000000001")
	body := []byte(`{"model":"mapped","input":[]}`)
	for _, scheme := range []string{"v1", "v2"} {
		account := newTestOAuthAccount(906, map[string]any{"codex_identity_scheme": scheme, codexFingerprintSeedExtraKey: testCodexFingerprintSeed})
		req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses/compact", bytes.NewReader(body))
		require.NoError(t, err)
		svc := &OpenAIGatewayService{}
		req, err = svc.prepareCodexHTTPWireRequest(c, account, req)
		require.NoError(t, err)
		got, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		if scheme == "v1" {
			require.Equal(t, body, got)
		} else {
			require.Equal(t, scopeCodexAccountIdentityValue(account, 0, "prompt-cache", c.GetString(openAICompactSessionSeedKey)), gjson.GetBytes(got, "prompt_cache_key").Str)
			require.False(t, gjson.GetBytes(got, "client_metadata").Exists())
		}
		require.Empty(t, req.Header.Get("Content-Encoding"))
	}
}

func TestCodexWireDeferredImagesEndpoint(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	account := newTestOAuthAccount(907, map[string]any{
		"codex_identity_scheme": "v2",
		"codex_wire_options":    map[string]any{"body_order": true, "request_compression": true, "guardian_credits": true},
	})
	body := []byte(`{ "prompt":"fixture", "model":"image-model" }`)
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), codexWireTargetURLKey{}, "https://chatgpt.com/backend-api/images/generations"))
	svc := &OpenAIGatewayService{}
	req, err := svc.prepareCodexHTTPWireRequest(c, account, req)
	require.NoError(t, err)
	encoded, err := EncodeCodexNativeHTTPRequest(req)
	require.NoError(t, err)
	require.Same(t, req, encoded)
	got, _ := io.ReadAll(req.Body)
	require.Equal(t, body, got, "the Responses builder must not decorate a future Images request")
	require.Empty(t, req.Header.Get("Content-Encoding"))
}

func TestCodexWireProfileMatchesModelsAndQuota(t *testing.T) {
	withCodexClientProfileIdentityState(t)
	account := newTestOAuthAccount(908, map[string]any{"codex_client_profile": map[string]any{
		"client": "desktop", "platform": "windows", "versionMode": "fixed",
		"codexVersion": "0.156.1", "desktopVersion": "26.1029.10000",
	}})
	account.Credentials = map[string]any{"access_token": "fixture-token", "chatgpt_account_id": "fixture-workspace"}
	identity := codexClientIdentityFor(account, account)
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	quota := &OpenAIQuotaService{accountRepo: repo}
	headers, _, err := quota.buildCodexQuotaHeaders(context.Background(), account.ID, "fixture-token", "fixture-workspace", false)
	require.NoError(t, err)
	require.Equal(t, identity.userAgent, headers["user-agent"])
	require.Equal(t, identity.originator, headers["originator"])
	require.Equal(t, identity.version, headers["version"])
	probe := &AccountTestService{accountRepo: repo}
	req, err := probe.buildOpenAIOAuthUpstreamModelsRequest(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, identity.userAgent, req.Header.Get("User-Agent"))
	require.Equal(t, identity.originator, req.Header.Get("Originator"))
	require.Equal(t, identity.version, req.Header.Get("Version"))
	require.Equal(t, identity.version, req.URL.Query().Get("client_version"))
}

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexWSIdentityDeviceScopesHandshakeAndPayloadOnce(t *testing.T) {
	c := newFingerprintStageTestContext(t)
	rawMetadata := `{"installation_id":"client-install","session_id":"session","thread_id":"thread","turn_id":"turn","parent_thread_id":"parent","root_turn_id":"turn","context_window_id":"context","window_number":1,"opaque":9007199254740993}`
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.156.1")
	c.Request.Header.Set("originator", "codex_cli_rs")
	c.Request.Header.Set(openAIWSTurnMetadataHeader, rawMetadata)
	cfg := newOpenAIWSExecutionScopeTestConfig()
	conn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.completed","response":{"id":"resp_v2_device_once","model":"gpt-5.2","usage":{"input_tokens":1,"output_tokens":1}}}`),
	}}
	dialer := &openAIWSCaptureDialer{conn: conn}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(dialer)
	defer pool.Close()
	svc := &OpenAIGatewayService{
		cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool,
	}
	account := newTestOAuthAccount(4811, map[string]any{
		codexIdentitySchemeExtraKey: "v2", codexFingerprintModeExtraKey: "device",
		"responses_websockets_v2_enabled": true,
	})
	account.Status, account.Schedulable, account.Concurrency = StatusActive, true, 1
	account.Credentials = map[string]any{"access_token": "test-token", "chatgpt_account_id": "ws-device-account"}
	result, err := svc.Forward(context.Background(), c, account,
		[]byte(`{"model":"gpt-5.2","stream":false,"input":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	header := gjson.Parse(dialer.lastHeaders.Get(openAIWSTurnMetadataHeader))
	body := gjson.Parse(gjson.Get(requestToJSONString(conn.lastWrite), "client_metadata."+openAIWSTurnMetadataHeader).String())
	for name, kind := range map[string]string{
		"session_id": "session", "thread_id": "thread", "turn_id": "turn",
		"parent_thread_id": "thread", "root_turn_id": "turn", "context_window_id": "context_window",
	} {
		expected := scopeCodexAccountIdentityValue(account, 0, kind, gjson.Get(rawMetadata, name).String())
		require.Equal(t, expected, header.Get(name).String(), "handshake %s", name)
		require.Equal(t, expected, body.Get(name).String(), "body %s", name)
	}
	require.Equal(t, body.Get("installation_id").String(), header.Get("installation_id").String())
	require.Equal(t, "9007199254740993", header.Get("opaque").Raw)
	require.Equal(t, "9007199254740993", body.Get("opaque").Raw)
}

func TestCodexWSIdentityIngressPreservesV2FrameMetadataAndLegacyFallback(t *testing.T) {
	for _, tc := range []struct {
		name, scheme, body, path, window string
	}{
		{"v2_current_frame", "v2", `{"client_metadata":{"x-codex-turn-metadata":"{\"window_number\":1}"}}`, "client_metadata", "1"},
		{"v2_nested_frame", "v2", `{"response":{"client_metadata":{"x-codex-turn-metadata":"{\"window_number\":1}"}}}`, "response.client_metadata", "1"},
		{"v2_missing_frame", "v2", `{"type":"response.create"}`, "client_metadata", "0"},
		{"v1_handshake_precedence", "v1", `{"client_metadata":{"x-codex-turn-metadata":"{\"window_number\":1}"}}`, "client_metadata", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFingerprintStageTestContext(t)
			c.Request.Header.Set(openAIWSTurnMetadataHeader, `{"window_number":0}`)
			account := newTestOAuthAccount(4812, map[string]any{codexIdentitySchemeExtraKey: tc.scheme, codexFingerprintModeExtraKey: "device"})
			var body map[string]any
			require.NoError(t, decodeOpenAIJSONUseNumber([]byte(tc.body), &body))
			if fallback := openAIWSIngressTurnMetadataFallback(c, account, []byte(tc.body)); fallback != "" {
				setOpenAIWSTurnMetadata(body, fallback)
			}
			raw, err := marshalOpenAIUpstreamJSON(body)
			require.NoError(t, err)
			svc := &OpenAIGatewayService{}
			scoped, err := svc.scopeCodexWSIdentity(context.Background(), c, account, "gpt-5.2", raw)
			require.NoError(t, err)
			metadata := gjson.Parse(gjson.GetBytes(scoped, tc.path+"."+openAIWSTurnMetadataHeader).String())
			require.Equal(t, tc.window, metadata.Get("window_number").Raw)
		})
	}
}

func TestCodexWSIdentitySourceEntryClearsSkipAndStagingOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		c := newFingerprintStageTestContext(t)
		previous := newCodexIdentityV2TestAccount(4813, "device")
		c.Set(codexAccountIdentitySourceContextKey, previous)
		c.Set("codex_wire_skip", true)
		stageCodexFingerprintIDs(c, resolveCodexFingerprintIDs(previous, "session", codexFingerprintDevice))
		stageCodexConvergenceBodyIdentityMap(c, previous, map[string]any{"client_metadata": map[string]any{"session_id": "old-derived-session"}})
		next := newCodexIdentityV2TestAccount(4814, "device")
		if fail {
			next.ParentAccountID = &previous.ID
		}
		svc := &OpenAIGatewayService{accountRepo: &codexAccountIdentityRepoStub{}}
		_, err := svc.prepareCodexAccountIdentitySource(context.Background(), c, next)
		if fail {
			require.Error(t, err)
			source, _ := c.Get(codexAccountIdentitySourceContextKey)
			require.Nil(t, source)
		} else {
			require.NoError(t, err)
			require.Same(t, next, codexAccountIdentitySource(c, next))
		}
		skip, _ := c.Get("codex_wire_skip")
		require.Equal(t, false, skip)
		require.Nil(t, stagedCodexFingerprintIDs(c, previous))
		require.Nil(t, stagedCodexConvergenceBodyIdentity(c, previous))
	}
}

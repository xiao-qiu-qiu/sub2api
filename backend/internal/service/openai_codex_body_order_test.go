package service

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func codexWireHelpersTestContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	// Deliberately differs from the outbound Responses endpoint.
	c.Request = httptest.NewRequest("POST", "/v1/images/generations", nil)
	return c
}

func codexWireHelpersTestAccount(order, compression bool) *Account {
	return &Account{
		Platform: "openai",
		Type:     "oauth",
		Extra: map[string]any{
			"codex_wire_options": map[string]any{
				"body_order":          order,
				"request_compression": compression,
			},
		},
	}
}

func TestCodexWireHelpersDisabledBytesAndIndependentOptions(t *testing.T) {
	c := codexWireHelpersTestContext()
	body := []byte(" { \"input\": [9007199254740993], \"model\": \"m\" } \n")
	ws := []byte(`{ "input":[], "type":"response.create", "model":"m" }`)
	for _, account := range []*Account{nil, {Platform: "openai", Type: "oauth"}, codexWireHelpersTestAccount(false, false)} {
		require.Equal(t, body, applyCodexBodyFieldOrder(c, account, "/v1/responses", body))
		require.Equal(t, ws, prepareCodexWSWireBytes(c, account, ws))
		wire, encoding, err := compressCodexRequestBody(c, account, "/v1/responses", body)
		require.NoError(t, err)
		require.Empty(t, encoding)
		require.Equal(t, body, wire)
	}
	require.Equal(t, body, applyCodexBodyFieldOrder(c, codexWireHelpersTestAccount(false, true), "/responses", body))
	require.Equal(t, ws, prepareCodexWSWireBytes(c, codexWireHelpersTestAccount(false, true), ws))
	wire, encoding, err := compressCodexRequestBody(c, codexWireHelpersTestAccount(true, false), "/responses", body)
	require.NoError(t, err)
	require.Empty(t, encoding)
	require.Equal(t, body, wire)
}

func TestCodexWireHelpersOutboundEndpointScope(t *testing.T) {
	c := codexWireHelpersTestContext()
	account := codexWireHelpersTestAccount(true, true)
	body := []byte(`{"stream":true,"input":[],"instructions":"i","model":"m"}`)
	responses := `{"model":"m","instructions":"i","input":[],"stream":true}`
	compact := `{"model":"m","input":[],"instructions":"i","stream":true}`
	for _, tc := range []struct {
		target, ordered string
		compressed      bool
	}{
		{"/responses", responses, true},
		{"https://upstream.invalid/backend-api/codex/responses/?q=/compact", responses, true},
		{"/v1/responses/compact", compact, false},
		{"https://upstream.invalid/responses/compact/?q=/responses", compact, false},
		{"/v1/images/generations", string(body), false},
		{"/v1/notresponses", string(body), false},
		{"/v1/responses-extra", string(body), false},
		{"/v1/responses/compact/child", string(body), false},
		{"/v1/models?target=/responses", string(body), false},
		{"wss://upstream.invalid/responses", string(body), false},
		{"://bad url", string(body), false},
		{"https://upstream.invalid/%ZZ/responses", string(body), false},
	} {
		t.Run(tc.target, func(t *testing.T) {
			require.Equal(t, tc.ordered, string(applyCodexBodyFieldOrder(c, account, tc.target, body)))
			wire, encoding, err := compressCodexRequestBody(c, account, tc.target, body)
			require.NoError(t, err)
			if tc.compressed {
				require.Equal(t, "zstd", encoding)
			} else {
				require.Empty(t, encoding)
				require.Equal(t, body, wire)
			}
		})
	}
}

func TestReorderCodexTopLevelFieldsPreservesRawValuesAndDuplicateKeys(t *testing.T) {
	body := []byte(` {"unknown_z":1e+900,"input":[ {"n":90071992547409931234567890,"n":-0,"text":"\u4e2d\/\\\""} ],"unknown_a":"\u003c<&>","instructions":"i","mo\u0064el":"m","client_metadata":{"b": 1e-900, "a":"\u4e2d"}} `)
	original := bytes.Clone(body)
	want := `{"mo\u0064el":"m","instructions":"i","input":[ {"n":90071992547409931234567890,"n":-0,"text":"\u4e2d\/\\\""} ],"client_metadata":{"b": 1e-900, "a":"\u4e2d"},"unknown_z":1e+900,"unknown_a":"\u003c<&>"}`
	out := reorderCodexTopLevelFields(body, codexResponsesFieldOrder)
	require.Equal(t, want, string(out))
	require.Equal(t, original, body, "caller retains its original plaintext")
	require.Same(t, &out[0], &reorderCodexTopLevelFields(out, codexResponsesFieldOrder)[0])
	for _, in := range []string{
		`{"input":[],"model":"a","model":"b"}`,
		`{"model":"a","mo\u0064el":"b"}`,
		`{"unknown":1,"model":"m","unknown":2}`,
		`{"model":"m","input":[]`, `{"model":"m"} trailing`, `{broken`, `{"input":[1,]}`,
		`[]`, `null`, `{}`, ``,
	} {
		require.Equal(t, in, string(reorderCodexTopLevelFields([]byte(in), codexResponsesFieldOrder)), in)
	}
}

func TestPrepareCodexWSWireBytesOnlyResponseCreate(t *testing.T) {
	c := codexWireHelpersTestContext()
	account := codexWireHelpersTestAccount(true, true)
	payload := []byte(`{"unknown":1e+900,"input":[{"n":9007199254740993,"text":"\u4e2d"}],"previous_response_id":"r","model":"m","type":"response.create"}`)
	want := `{"type":"response.create","model":"m","previous_response_id":"r","input":[{"n":9007199254740993,"text":"\u4e2d"}],"unknown":1e+900}`
	require.Equal(t, want, string(prepareCodexWSWireBytes(c, account, payload)))
	for _, in := range []string{
		`{ "input":[], "type":"response.cancel", "model":"m" }`,
		`{"input":[],"model":"m"}`, `{"type":true,"model":"m"}`,
		`{"input":[],"type":"response.create","type":"response.cancel"}`,
		`{"input":[],"type":"response.create","ty\u0070e":"response.create"}`,
		`{"type":"response.create",`, `[{"type":"response.create"}]`,
	} {
		require.Equal(t, in, string(prepareCodexWSWireBytes(c, account, []byte(in))), in)
	}
}

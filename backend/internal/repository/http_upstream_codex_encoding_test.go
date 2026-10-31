package repository

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

type codexEncodingRoundTripper func(*http.Request) (*http.Response, error)

func (f codexEncodingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

const codexEncodingTestBody = `{"model":"gpt-6-astra","input":[{"role":"user","content":"hello \\ \u4e2d"}],"client_metadata":{"n":9007199254740993}}`

func newCodexEncodingTestRequest(t *testing.T, enabled bool) *http.Request {
	t.Helper()
	ctx := service.WithCodexRequestCompression(t.Context(), enabled)
	ctx = service.WithHTTPUpstreamProfile(ctx, service.HTTPUpstreamProfileOpenAI)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewBufferString(codexEncodingTestBody))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestHTTPUpstreamCodexEncodingKeepsRoutingJSONUntilNativeSend(t *testing.T) {
	req := newCodexEncodingTestRequest(t, true)
	defer req.Body.Close()
	// Exercise the actual repository routing predicate before the native send.
	// The compression marker must not hide model JSON from Astra ticket routing.
	require.True(t, codexGatewayPinRequest(req))
	require.True(t, codexGatewayPinAstra(req))
	decoder, err := zstd.NewReader(nil)
	require.NoError(t, err)
	defer decoder.Close()
	calls := 0
	var firstWire []byte
	client := &http.Client{Transport: codexEncodingRoundTripper(func(sent *http.Request) (*http.Response, error) {
		calls++
		defer sent.Body.Close()
		require.Equal(t, "zstd", sent.Header.Get("Content-Encoding"))
		require.Equal(t, "application/json", sent.Header.Get("Content-Type"))
		wire, err := io.ReadAll(sent.Body)
		require.NoError(t, err)
		require.NotEqual(t, []byte(codexEncodingTestBody), wire)
		require.Equal(t, int64(len(wire)), sent.ContentLength)
		plain, err := decoder.DecodeAll(wire, nil)
		require.NoError(t, err)
		require.Equal(t, codexEncodingTestBody, string(plain), "transport receives exactly one layer of zstd")
		require.NotNil(t, sent.GetBody)
		replay, err := sent.GetBody()
		require.NoError(t, err)
		replayed, err := io.ReadAll(replay)
		require.NoError(t, replay.Close())
		require.NoError(t, err)
		require.Equal(t, wire, replayed, "HTTP replay bytes agree with encoding and length")
		if calls == 1 {
			firstWire = bytes.Clone(wire)
		} else {
			require.Equal(t, firstWire, wire)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: sent}, nil
	})}
	for range 2 {
		resp, err := doUpstreamRequest(client, req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Empty(t, req.Header.Get("Content-Encoding"), "source request stays plaintext across attempts")
		require.Equal(t, int64(len(codexEncodingTestBody)), req.ContentLength)
		require.True(t, codexGatewayPinAstra(req), "routing can still inspect the source request after an attempt")
		require.NoError(t, req.Context().Err(), "closing an attempt must not cancel the retry source")
	}
	require.Equal(t, 2, calls)
	plain, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, codexEncodingTestBody, string(plain), "encoding never consumes the caller's body")
}

func TestHTTPUpstreamCodexEncodingUsesFinalEndpointMethodAndProfile(t *testing.T) {
	for _, tc := range []struct {
		name, path, method string
		harvest, disabled  bool
	}{
		{name: "images_generations", path: "/backend-api/codex/images/generations"},
		{name: "images_edits", path: "/backend-api/codex/images/edits"},
		{name: "compact", path: "/backend-api/codex/responses/compact"},
		{name: "get", path: "/backend-api/codex/responses", method: http.MethodGet},
		{name: "harvest", path: "/backend-api/codex/responses", harvest: true},
		{name: "disabled", path: "/backend-api/codex/responses", disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := newCodexEncodingTestRequest(t, !tc.disabled)
			// Keep the original Responses compression marker while replacing
			// the actual URL, as the self-built OAuth images path does.
			req.URL.Path = tc.path
			if tc.method != "" {
				req.Method = tc.method
			}
			if tc.harvest {
				req = req.WithContext(service.WithHTTPUpstreamProfile(req.Context(), service.HTTPUpstreamProfileOpenAIHarvest))
			}
			calls := 0
			client := &http.Client{Transport: codexEncodingRoundTripper(func(sent *http.Request) (*http.Response, error) {
				calls++
				defer sent.Body.Close()
				require.Equal(t, tc.path, sent.URL.Path)
				require.Empty(t, sent.Header.Get("Content-Encoding"))
				plain, err := io.ReadAll(sent.Body)
				require.NoError(t, err)
				require.Equal(t, codexEncodingTestBody, string(plain))
				require.Equal(t, int64(len(plain)), sent.ContentLength)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: sent}, nil
			})}
			resp, err := doUpstreamRequest(client, req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, 1, calls)
		})
	}
}

func TestHTTPUpstreamCodexEncodingGetBodyFailureReturnsError(t *testing.T) {
	req := newCodexEncodingTestRequest(t, true)
	defer req.Body.Close()
	getBodyErr := errors.New("codex encoding fixture cannot rewind body")
	req.GetBody = func() (io.ReadCloser, error) { return nil, getBodyErr }
	calls := 0
	client := &http.Client{Transport: codexEncodingRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("transport must not run after an encoding failure")
	})}
	var resp *http.Response
	var err error
	require.NotPanics(t, func() { resp, err = doUpstreamRequest(client, req) })
	require.Nil(t, resp)
	require.ErrorIs(t, err, getBodyErr)
	require.Zero(t, calls)
	require.Empty(t, req.Header.Get("Content-Encoding"))
	require.NoError(t, req.Context().Err())
}

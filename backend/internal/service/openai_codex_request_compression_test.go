package service

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func TestCompressCodexRequestBodyRoundTripAndFrameMetadata(t *testing.T) {
	c := codexWireHelpersTestContext()
	account := codexWireHelpersTestAccount(false, true)
	decoder, err := zstd.NewReader(nil)
	require.NoError(t, err)
	defer decoder.Close()
	// Cover small frames, content-size header variants and the 2 MiB window boundary.
	for _, size := range []int{1, 300, 70000, (1 << 21) + 17} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			body := []byte(`{"model":"m","input":"` + strings.Repeat("abc ", size/4+1) + `","client_metadata":{"n":9007199254740993,"text":"\u4e2d"}}`)
			original := bytes.Clone(body)
			wire, encoding, err := compressCodexRequestBody(c, account, "/v1/responses", body)
			require.NoError(t, err)
			require.Equal(t, "zstd", encoding)
			require.Equal(t, []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x58}, wire[:6])
			var header zstd.Header
			require.NoError(t, header.Decode(wire))
			require.False(t, header.SingleSegment)
			require.False(t, header.HasFCS)
			require.False(t, header.HasCheckSum)
			require.Zero(t, header.DictionaryID)
			require.Equal(t, uint64(1<<21), header.WindowSize)
			plain, err := decoder.DecodeAll(wire, nil)
			require.NoError(t, err)
			require.Equal(t, original, plain)
			require.Equal(t, original, body)
		})
	}
	for _, body := range [][]byte{nil, {}} {
		wire, encoding, err := compressCodexRequestBody(c, account, "/responses", body)
		require.NoError(t, err)
		require.Empty(t, encoding)
		require.Equal(t, body, wire)
	}
}

func TestFinalizeCodexHTTPRequestBodyMetadataAndReplay(t *testing.T) {
	c := codexWireHelpersTestContext()
	body := []byte(`{"model":"final-mapped-model","input":[],"client_metadata":{"text":"\u4e2d"}}`)
	original := bytes.Clone(body)
	req, err := http.NewRequest(http.MethodPost, "https://upstream.invalid/responses", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", "99999")
	req.Header.Set("X-Ticket", "already-consumed")
	req.TransferEncoding = []string{"chunked"}
	require.NoError(t, finalizeCodexHTTPRequestBody(c, codexWireHelpersTestAccount(false, true), req, body))
	defer func() { _ = req.Body.Close() }()
	wire, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, "zstd", req.Header.Get("Content-Encoding"))
	require.Equal(t, "application/json", req.Header.Get("Content-Type"))
	require.Equal(t, "already-consumed", req.Header.Get("X-Ticket"))
	require.Empty(t, req.Header.Get("Content-Length"))
	require.Equal(t, int64(len(wire)), req.ContentLength)
	require.Empty(t, req.TransferEncoding)
	require.NotNil(t, req.GetBody)
	replay, err := req.GetBody()
	require.NoError(t, err)
	defer func() { _ = replay.Close() }()
	replayed, err := io.ReadAll(replay)
	require.NoError(t, err)
	require.Equal(t, wire, replayed)
	decoder, err := zstd.NewReader(nil)
	require.NoError(t, err)
	defer decoder.Close()
	plain, err := decoder.DecodeAll(wire, nil)
	require.NoError(t, err)
	require.Equal(t, original, plain)
	require.Equal(t, original, body)
}

func TestFinalizeCodexHTTPRequestBodyNoopKeepsMetadata(t *testing.T) {
	for _, tc := range []struct {
		target  string
		enabled bool
	}{{"/responses", false}, {"/responses/compact", true}} {
		req, err := http.NewRequest(http.MethodPost, tc.target, strings.NewReader(`{ "input": [], "model": "m" }`))
		require.NoError(t, err)
		beforeBody, beforeLength := req.Body, req.ContentLength
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Length", "preserved-while-disabled")
		req.TransferEncoding = []string{"chunked"}
		beforeHeader := req.Header.Clone()
		require.NoError(t, finalizeCodexHTTPRequestBody(codexWireHelpersTestContext(), codexWireHelpersTestAccount(true, tc.enabled), req, []byte(`{ "input": [], "model": "m" }`)))
		require.Equal(t, beforeBody, req.Body)
		require.Equal(t, beforeLength, req.ContentLength)
		require.Equal(t, beforeHeader, req.Header)
		require.Equal(t, []string{"chunked"}, req.TransferEncoding)
		require.NoError(t, req.Body.Close())
	}
}

func TestNormalizeOwnedCodexZstdFrameHeaderRejectsInvalidMetadata(t *testing.T) {
	for _, frame := range [][]byte{
		nil, {0x28, 0xb5, 0x2f, 0xfd, 0x80},
		{0x28, 0xb5, 0x2f, 0xfd, 0x01, 0x58}, // Dictionary ID.
		{0x28, 0xb5, 0x2f, 0xfd, 0x04, 0x58}, // Checksum.
		{0x28, 0xb5, 0x2f, 0xfd, 0x08, 0x58}, // Reserved bit.
		{0x28, 0xb5, 0x2f, 0xfd, 0x10, 0x58}, // Unused bit.
	} {
		original := bytes.Clone(frame)
		_, err := normalizeOwnedCodexZstdFrameHeader(frame)
		require.Error(t, err)
		require.Equal(t, original, frame, "validation precedes mutation")
	}
}

package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

const (
	codexRequestZstdContentEncoding = "zstd"
	codexRequestZstdWindowSize      = 1 << 21
)

var codexRequestZstdEncoders = sync.Pool{New: func() any {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderCRC(false),
		zstd.WithWindowSize(codexRequestZstdWindowSize),
		zstd.WithSingleSegment(false),
		zstd.WithEncoderConcurrency(1),
		zstd.WithLowerEncoderMem(true),
	)
	if err != nil {
		panic(fmt.Sprintf("codex request zstd encoder: %v", err))
	}
	return enc
}}

// compressCodexRequestBody produces separate wire bytes, retaining body as
// plaintext for routing, billing and plugins. Call only after the HTTP request,
// local model mappings and ticket consumption have been finalized. Compression
// is independently gated by codex_wire_options.request_compression; compact and
// WebSocket requests are excluded.
func compressCodexRequestBody(c *gin.Context, account *Account, targetURL string, body []byte) (wire []byte, encoding string, err error) {
	if len(body) == 0 || !codexWireOptionEnabled(c, account, "request_compression") || codexWireEndpoint(targetURL) != "/responses" {
		return body, "", nil
	}
	wire, err = encodeCodexZstdRequestBody(body)
	if err != nil {
		return nil, "", err
	}
	return wire, codexRequestZstdContentEncoding, nil
}

// finalizeCodexHTTPRequestBody optionally installs compressed wire bytes on a
// finalized request. body must be its final plaintext body, after local model
// mappings and ticket consumption; callers keep that slice for local consumers.
// Body ordering belongs before request construction. Disabled compression leaves
// the entire request untouched. Call once, immediately before transport dispatch.
func finalizeCodexHTTPRequestBody(c *gin.Context, account *Account, req *http.Request, body []byte) error {
	if req == nil || req.URL == nil {
		return errors.New("codex request missing outbound URL")
	}
	wire, encoding, err := compressCodexRequestBody(c, account, req.URL.String(), body)
	if err != nil || encoding == "" {
		return err
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	req.Body = io.NopCloser(bytes.NewReader(wire))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(wire)), nil
	}
	req.ContentLength = int64(len(wire))
	req.TransferEncoding = nil
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Content-Encoding", encoding)
	// net/http derives the actual Content-Length from Request.ContentLength.
	req.Header.Del("Content-Length")
	return nil
}

func encodeCodexZstdRequestBody(body []byte) ([]byte, error) {
	enc, ok := codexRequestZstdEncoders.Get().(*zstd.Encoder)
	if !ok {
		return nil, errors.New("codex request zstd encoder pool returned unexpected type")
	}
	defer codexRequestZstdEncoders.Put(enc)
	frame := enc.EncodeAll(body, make([]byte, 0, len(body)/3+64))
	wire, err := normalizeOwnedCodexZstdFrameHeader(frame)
	if err != nil {
		return nil, fmt.Errorf("zstd compress codex request body: %w", err)
	}
	return wire, nil
}

var (
	codexZstdFrameMagic        = []byte{0x28, 0xb5, 0x2f, 0xfd}
	codexZstdStreamFrameHeader = []byte{0x00, 0x58}
)

// normalizeOwnedCodexZstdFrameHeader uses libzstd's streaming level-3 metadata:
// no content size, checksum, dictionary or single segment, and a 2 MiB window.
// The encoder above bounds match offsets to that window. Only its fresh output
// may be changed in place; plaintext and shared frames must never be passed in.
func normalizeOwnedCodexZstdFrameHeader(frame []byte) ([]byte, error) {
	magic := len(codexZstdFrameMagic)
	if len(frame) < magic+1 || !bytes.Equal(frame[:magic], codexZstdFrameMagic) {
		return nil, errors.New("unexpected zstd frame magic")
	}
	fhd := frame[magic]
	if fhd&0x1f != 0 {
		return nil, errors.New("unexpected zstd frame header descriptor flags")
	}
	single := fhd&(1<<5) != 0
	headerLen := 1
	if !single {
		headerLen++ // Window_Descriptor.
	}
	switch fhd >> 6 { // Frame_Content_Size_flag.
	case 0:
		if single {
			headerLen++
		}
	case 1:
		headerLen += 2
	case 2:
		headerLen += 4
	default:
		headerLen += 8
	}
	if len(frame) < magic+headerLen {
		return nil, errors.New("short zstd frame header")
	}
	if headerLen == len(codexZstdStreamFrameHeader) && bytes.Equal(frame[magic:magic+headerLen], codexZstdStreamFrameHeader) {
		return frame, nil
	}
	// The normalized header is never longer. Move only the block payload.
	length := magic + len(codexZstdStreamFrameHeader) + copy(frame[magic+len(codexZstdStreamFrameHeader):], frame[magic+headerLen:])
	copy(frame[magic:], codexZstdStreamFrameHeader)
	return frame[:length], nil
}

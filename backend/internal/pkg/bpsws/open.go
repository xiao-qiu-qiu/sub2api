package bpsws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

const (
	handshakeTimeout = 60 * time.Second
	writeTimeout     = 30 * time.Second
	maxRejectionBody = 64 << 10
	maxSSEEvent      = 16 << 20
	maxWSMessage     = maxSSEEvent - 1024
)

type preparedCreateKey struct{}

var errPreparedCreateConsumed = errors.New("prepared BPS WebSocket request already consumed")

type preparedCreate struct {
	payload   []byte
	requestID string
}

// PrepareRequest measures the exact response.create bytes Open will send,
// including control fields and JSON escaping. The original HTTP body stays
// available for capture or an HTTP/SSE send. Discard the returned request to
// release the encoded payload when choosing HTTP instead. Call before adding
// submission tracking: reading this replayable body is local preparation.
// Open consumes the cached bytes once; a consumed request cannot be prepared
// or opened again.
func PrepareRequest(req *http.Request) (*http.Request, int, error) {
	if req == nil || req.GetBody == nil {
		return nil, 0, errors.New("BPS WebSocket preparation requires a replayable body")
	}
	if pending, ok := req.Context().Value(preparedCreateKey{}).(*atomic.Pointer[preparedCreate]); ok {
		create := pending.Load()
		if create == nil {
			return nil, 0, errPreparedCreateConsumed
		}
		return req, len(create.payload), nil
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, 0, safeError(req.Context(), err, "could not read prepared BPS WebSocket JSON")
	}
	defer func() { _ = body.Close() }()
	create, err := encodeCreate(req.Context(), body)
	if err != nil {
		return nil, 0, err
	}
	pending := new(atomic.Pointer[preparedCreate])
	pending.Store(create)
	return req.WithContext(context.WithValue(req.Context(), preparedCreateKey{}, pending)), len(create.payload), nil
}

func encodeCreate(ctx context.Context, body io.Reader) (*preparedCreate, error) {
	var doc map[string]json.RawMessage
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(&doc); err != nil || doc == nil {
		return nil, safeError(ctx, err, "invalid prepared BPS WebSocket JSON")
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, safeError(ctx, err, "invalid prepared BPS WebSocket JSON")
	}
	requestID := uuid.NewString()
	doc["type"] = json.RawMessage(`"response.create"`)
	doc["basispoints_request_id"], _ = json.Marshal(requestID)
	doc["basispoints_resume_token"], _ = json.Marshal("resume_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	payload, err := json.Marshal(doc)
	if err != nil {
		return nil, errors.New("could not encode prepared BPS WebSocket JSON")
	}
	return &preparedCreate{payload: payload, requestID: requestID}, nil
}

// Open sends req's prepared JSON body once, using the supplied HTTP/1 client
// (including its proxy, transport wrappers and tracing). It consumes and closes
// req.Body. A successful handshake becomes a 200 SSE response; a rejected HTTP
// handshake retains its status, headers and up to 64 KiB of body with nil error.
// The caller must close the returned response body. idleTimeout <= 0 disables
// the per-message idle deadline; the parent context always bounds the stream.
func Open(ctx context.Context, client *http.Client, req *http.Request, idleTimeout time.Duration) (*http.Response, error) {
	if ctx == nil || client == nil || req == nil || req.URL == nil || req.Body == nil || req.Method != http.MethodPost {
		return nil, errors.New("invalid BPS WebSocket request")
	}
	var create *preparedCreate
	if pending, ok := req.Context().Value(preparedCreateKey{}).(*atomic.Pointer[preparedCreate]); ok {
		// Only this Open owns the encoded bytes. Inherited stream contexts keep
		// an empty slot rather than retaining the payload until stream closure.
		create = pending.Swap(nil)
		if create == nil {
			return nil, errPreparedCreateConsumed
		}
	}
	defer func() { _ = req.Body.Close() }()
	if err := ctx.Err(); err != nil {
		return nil, safeError(ctx, err, "BPS WebSocket request cancelled")
	}
	u := *req.URL
	if u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid BPS WebSocket URL")
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return nil, errors.New("invalid BPS WebSocket URL scheme")
	}
	auth := strings.Fields(req.Header.Get("Authorization"))
	if len(auth) != 2 || !strings.EqualFold(auth[0], "Bearer") || !validProtocolToken(auth[1]) {
		return nil, errors.New("invalid BPS WebSocket bearer credentials")
	}
	var err error
	if create == nil {
		create, err = encodeCreate(ctx, req.Body)
		if err != nil {
			return nil, err
		}
	} else if _, err := io.Copy(io.Discard, req.Body); err != nil {
		// Capture wrappers still need to observe the original prepared POST.
		return nil, safeError(ctx, err, "could not read prepared BPS WebSocket JSON")
	}
	requestID := create.requestID
	profile := excelProfile()
	profileJSON, _ := json.Marshal(profile)
	query := u.Query()
	query.Set("bps_client_info", string(profileJSON))
	query.Set("bps_auth_mode", "chatgpt")
	query.Set("bps_ws_affinity", requestID)
	query.Set("bps_control_frames", "upstream_sent,heartbeat")
	u.RawQuery = query.Encode()
	headers := req.Header.Clone()
	for name := range headers {
		lower := strings.ToLower(name)
		if lower == "cookie" || lower == "cookie2" || lower == "proxy-authorization" ||
			lower == "originator" || lower == "openai-beta" || strings.HasPrefix(lower, "x-codex-") ||
			strings.HasPrefix(lower, "sec-websocket-") {
			delete(headers, name)
		}
	}
	headers.Del("Content-Type")
	headers.Del("Content-Length")
	headers.Set("Accept", "*/*")
	headers.Set("Origin", "https://bps.openai.com")
	headers.Set("X-Basispoints-Auth-Mode", "chatgpt")
	for name, value := range profile {
		headers.Set(name, value)
	}
	// The request may carry timing hooks added after its parent ctx was made.
	if trace := httptrace.ContextClientTrace(req.Context()); trace != nil && trace != httptrace.ContextClientTrace(ctx) {
		copyTrace := *trace
		ctx = httptrace.WithClientTrace(ctx, &copyTrace)
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	scoped, dialParent, release := scopedClient(streamCtx, client)
	rejected := &rejectionTransport{base: scoped.Transport}
	scoped.Transport = rejected
	dialCtx, cancelDial := context.WithTimeout(dialParent, handshakeTimeout)
	// A timeout must also close sockets detached by net/http during CONNECT/TLS.
	stopDial := context.AfterFunc(dialCtx, release)
	conn, response, dialErr := websocket.Dial(dialCtx, u.String(), &websocket.DialOptions{
		HTTPClient: scoped, HTTPHeader: headers,
		Subprotocols: []string{"responses", "openai-bearer." + auth[1]},
	})
	stopDial()
	if dialErr != nil {
		err = safeError(dialCtx, dialErr, "BPS WebSocket handshake failed")
	}
	cancelDial()
	if dialErr != nil {
		parentErr := ctx.Err()
		cancelStream()
		release()
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if parentErr == nil && rejected.response != nil {
			if rejected.response.StatusCode >= 300 {
				return rejected.response, nil
			}
			_ = rejected.response.Body.Close()
			return nil, errors.New("BPS WebSocket endpoint did not upgrade the connection")
		}
		return nil, err
	}
	conn.SetReadLimit(maxWSMessage)
	stopParent := context.AfterFunc(streamCtx, func() {
		_ = conn.CloseNow()
		release()
	})
	writeCtx, cancelWrite := context.WithTimeout(streamCtx, writeTimeout)
	err = conn.Write(writeCtx, websocket.MessageText, create.payload)
	if err != nil {
		err = safeError(writeCtx, err, "BPS WebSocket response.create write failed; request was not retried")
	}
	cancelWrite()
	if err != nil {
		stopParent()
		cancelStream()
		_ = conn.CloseNow()
		release()
		return nil, err
	}
	result := *response
	result.StatusCode = http.StatusOK
	result.Status = "200 OK"
	result.Header = response.Header.Clone()
	result.Header.Set("Content-Type", "text/event-stream")
	result.ContentLength = -1
	result.TransferEncoding = nil
	result.Body = &eventBody{
		conn: conn, ctx: streamCtx, cancel: cancelStream, release: release,
		stopParent: stopParent, idleTimeout: idleTimeout,
	}
	return &result, nil
}

func validProtocolToken(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c <= 32 || c >= 127 || strings.ContainsRune(`()<>@,;:\"/[]?={}`, c) {
			return false
		}
	}
	return true
}

// coder/websocket only retains 1024 rejection bytes. Capture a bounded body
// before it runs validation so model-access errors remain available to callers.
type rejectionTransport struct {
	base     http.RoundTripper
	response *http.Response
}

func (t *rejectionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if err != nil || response == nil || response.StatusCode == http.StatusSwitchingProtocols {
		return response, err
	}
	var data []byte
	if response.Body != nil {
		body := response.Body
		stop := context.AfterFunc(req.Context(), func() { _ = body.Close() })
		data, _ = io.ReadAll(io.LimitReader(body, maxRejectionBody))
		stop()
		_ = body.Close()
	}
	copyResponse := *response
	copyResponse.Header = response.Header.Clone()
	copyResponse.Body = io.NopCloser(bytes.NewReader(data))
	copyResponse.ContentLength = int64(len(data))
	copyResponse.TransferEncoding = nil
	t.response = &copyResponse
	response.Body = io.NopCloser(bytes.NewReader(data))
	return response, nil
}

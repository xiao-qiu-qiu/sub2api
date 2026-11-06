package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexWireTargetURLKey struct{}

func setCodexRequestBody(req *http.Request, body []byte) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	req.ContentLength = int64(len(body))
	req.TransferEncoding = nil
	req.Header.Del("Content-Length")
}

func (s *OpenAIGatewayService) skipCodexWireIdentity(ctx context.Context, c *gin.Context, account *Account, model string) bool {
	skip := account == nil || !account.IsOpenAIOAuthLike() || s.harvestPinsCodexIdentity(ctx, account, model)
	if c != nil {
		c.Set("codex_wire_skip", skip)
	}
	return skip
}

// Run after model mapping, ticket binding and compatibility transformations.
// Keep the request plaintext for local policy and accounting; compression is
// installed on an attempt-local copy immediately before the transport.
func (s *OpenAIGatewayService) prepareCodexHTTPWireRequest(c *gin.Context, account *Account, req *http.Request) (*http.Request, error) {
	if req == nil || req.URL == nil || account == nil {
		return req, nil
	}
	if c != nil {
		c.Set("codex_wire_skip", false)
	}
	source := codexAccountIdentitySource(c, account)
	targetURL := req.URL.String()
	if finalTarget, ok := req.Context().Value(codexWireTargetURLKey{}).(string); ok && finalTarget != "" {
		targetURL = finalTarget
	}
	endpoint := codexWireEndpoint(targetURL)
	active := codexIdentityV2Enabled(source)
	for option := range codexWireOptionNames {
		active = active || codexWireOptionEnabled(c, account, option)
	}
	if !active {
		return req, nil
	}
	if s.skipCodexWireIdentity(req.Context(), c, account, extractOpenAICodexTicketModelFromRequest(req)) || s.codexTicketRequestBound(req, account) {
		return req.WithContext(context.WithValue(req.Context(), codexRequestCompressionKey{}, false)), nil
	}
	if req.GetBody != nil {
		reader, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			return nil, err
		}
		original := body
		// The compact handler retained the original explicit key for account
		// stickiness before pruning its legacy schema. Restore it only for v2.
		if codexIdentityV2Enabled(source) && endpoint == "/responses/compact" {
			key := gjson.GetBytes(body, "prompt_cache_key").Str
			if key == "" && c != nil {
				key = c.GetString(openAICompactSessionSeedKey)
			}
			if strings.TrimSpace(key) != "" {
				if next, err := sjson.SetBytes(body, "prompt_cache_key", scopeCodexAccountIdentityValue(source, getAPIKeyIDFromContext(c), "prompt-cache", key)); err == nil {
					body = next
				}
			}
		}
		next := applyCodexOptionalBodyFeatures(c, source, targetURL, body)
		if codexIdentityV2Enabled(source) && (endpoint == "/responses" || endpoint == "/responses/compact") {
			next = alignCodexWireBodyMetadata(next, req.Header)
			alignCodexWireHeaderMetadata(req.Header, next)
			applyCodexWireIdentityHeaders(c, account, req.Header, endpoint == "/responses/compact", false)
		}
		next = applyCodexBodyFieldOrder(c, account, targetURL, next)
		if !bytes.Equal(original, next) {
			setCodexRequestBody(req, next)
		}
	}
	enabled := codexWireOptionEnabled(c, account, "request_compression") && endpoint == "/responses"
	req = req.WithContext(context.WithValue(req.Context(), codexRequestCompressionKey{}, enabled))
	return s.prepareCodexOptionalTraffic(c, account, req), nil
}

// This small reader is used only before compression; no middleware needs to
// decode zstd just to identify the request's already-resolved model.
func extractOpenAICodexTicketModelFromRequest(req *http.Request) string {
	if req == nil || req.GetBody == nil {
		return ""
	}
	r, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	if err != nil {
		return ""
	}
	return extractOpenAICodexTicketModel(b)
}

// EncodeCodexNativeHTTPRequest is the final native transport step, after proxy,
// model/ticket routing and egress timezone processing. Plugins keep plaintext
// according to their existing request contract and own their transport encoding.
func EncodeCodexNativeHTTPRequest(req *http.Request) (*http.Request, error) {
	if req == nil || req.URL == nil || req.Method != http.MethodPost || codexWireEndpoint(req.URL.String()) != "/responses" ||
		HTTPUpstreamProfileFromContext(req.Context()) == HTTPUpstreamProfileOpenAIHarvest || IsBoundAstraTicketRequest(req.Context()) ||
		req.Context().Value(codexRequestCompressionKey{}) != true || req.GetBody == nil || req.Header.Get("Content-Encoding") != "" {
		return req, nil
	}
	reader, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		return nil, err
	}
	wire, err := encodeCodexZstdRequestBody(body)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	// Do not close the original attempt's body when installing the wire copy.
	clone.Body = nil
	setCodexRequestBody(clone, wire)
	clone.Header.Set("Content-Encoding", codexRequestZstdContentEncoding)
	return clone, nil
}

func alignCodexWireBodyMetadata(body []byte, headers http.Header) []byte {
	for _, prefix := range []string{"", "response."} {
		path := prefix + "client_metadata." + openAIWSTurnMetadataHeader
		raw := gjson.GetBytes(body, path)
		if raw.Type != gjson.String {
			continue
		}
		model := gjson.GetBytes(body, prefix+"model").String()
		if model == "" {
			model = gjson.GetBytes(body, "model").String()
		}
		next := alignCodexWireTurnMetadata(raw.Str, model, headers.Get("version"))
		if next != raw.Str {
			if value, err := marshalOpenAIUpstreamJSON(next); err == nil {
				if changed, err := sjson.SetRawBytes(body, path, value); err == nil {
					body = changed
				}
			}
		}
	}
	return body
}

func alignCodexWireTurnMetadata(raw, model, version string) string {
	return rewriteCodexTurnMetadataJSON(raw, false, func(metadata map[string]any) map[string]any {
		updates := map[string]any{}
		for name, value := range map[string]string{"model": model, "codex_version": version} {
			if _, ok := metadata[name]; ok && value != "" {
				updates[name] = value
			}
		}
		return updates
	})
}

func alignCodexWireHeaderMetadata(headers http.Header, body []byte) {
	if raw := headers.Get(openAIWSTurnMetadataHeader); raw != "" {
		headers.Set(openAIWSTurnMetadataHeader, alignCodexWireTurnMetadata(raw, gjson.GetBytes(body, "model").String(), headers.Get("version")))
	}
}

func applyCodexWireIdentityHeaders(c *gin.Context, account *Account, headers http.Header, compact, websocket bool) {
	source := codexAccountIdentitySource(c, account)
	if !codexIdentityV2Enabled(source) {
		return
	}
	if websocket {
		headers.Del(openAICodexTurnStateHeader)
	} else {
		stripOpenAILegacyResponsesBeta(headers)
		if compact {
			headers.Del("x-client-request-id")
		}
		if !compact && headers.Get("x-codex-inference-call-id") != "" {
			headers.Set("x-codex-inference-call-id", uuid.NewString())
		}
	}
	if !compact {
		if ids := stagedCodexFingerprintIDs(c, account); ids != nil && ids.mode == codexFingerprintDevice && ids.installationID != "" {
			headers.Del("x-codex-installation-id")
		}
	}
}

func (s *OpenAIGatewayService) prepareCodexWSFrame(ctx context.Context, c *gin.Context, account *Account, payload []byte, clientTurnState string) []byte {
	if account == nil || !account.IsOpenAIOAuthLike() || s.skipCodexWireIdentity(ctx, c, account, extractOpenAICodexTicketModel(payload)) {
		return payload
	}
	source := codexAccountIdentitySource(c, account)
	if codexIdentityV2Enabled(source) && gjson.GetBytes(payload, "type").String() == "response.create" {
		for _, prefix := range []string{"", "response."} {
			metadataPath := prefix + "client_metadata"
			metadata := gjson.GetBytes(payload, metadataPath)
			if metadata.Exists() && !metadata.IsObject() {
				continue
			}
			if prefix != "" && !gjson.GetBytes(payload, "response").IsObject() {
				continue
			}
			if value := strings.TrimSpace(clientTurnState); value != "" && gjson.GetBytes(payload, metadataPath+"."+openAICodexTurnStateHeader).String() == "" {
				payload, _ = sjson.SetBytes(payload, metadataPath+"."+openAICodexTurnStateHeader, value)
			}
			payload, _ = sjson.SetBytes(payload, metadataPath+".x-codex-ws-stream-request-start-ms", fmt.Sprint(time.Now().UnixMilli()))
		}
		payload = alignCodexWireBodyMetadata(payload, http.Header{})
	}
	payload = applyCodexOptionalWSFeatures(c, source, payload)
	return prepareCodexWSWireBytes(c, account, payload)
}

func (s *OpenAIGatewayService) writeCodexWSFrame(ctx context.Context, c *gin.Context, account *Account, lease *openAIWSConnLease, value any, clientTurnState string, timeout time.Duration, maxPayloadBytes ...int64) error {
	if !codexIdentityV2Enabled(codexAccountIdentitySource(c, account)) && !codexAnyWSWireOption(c, account) {
		return lease.WriteJSONWithContextTimeout(ctx, value, timeout)
	}
	var raw []byte
	switch v := value.(type) {
	case json.RawMessage:
		raw = v
	case openAIWSPreparedJSON:
		raw = v
	case []byte:
		raw = v
	default:
		var err error
		raw, err = json.Marshal(value)
		if err != nil {
			return err
		}
	}
	if s.skipCodexWireIdentity(ctx, c, account, extractOpenAICodexTicketModel(raw)) {
		return lease.WriteJSONWithContextTimeout(ctx, value, timeout)
	}
	raw = s.prepareCodexWSFrame(ctx, c, account, raw, clientTurnState)
	if len(maxPayloadBytes) > 0 && maxPayloadBytes[0] > 0 && int64(len(raw)) > maxPayloadBytes[0] {
		return errOpenAIWSSSEPayloadTooLarge
	}
	return lease.WriteJSONWithContextTimeout(ctx, openAIWSPreparedJSON(raw), timeout)
}

func codexAnyWSWireOption(c *gin.Context, account *Account) bool {
	for _, option := range []string{"body_order", "environment_timezone", "web_search_location", "guardian_credits"} {
		if codexWireOptionEnabled(c, account, option) {
			return true
		}
	}
	return false
}

// Account-scoping always starts from the original client frame. Optional device
// convergence then projects that same frame's IDs onto the scoped copy.
func (s *OpenAIGatewayService) scopeCodexWSIdentity(ctx context.Context, c *gin.Context, account *Account, model string, raw []byte) ([]byte, error) {
	stageCodexFingerprintIDs(c, nil)
	stageCodexConvergenceBodyIdentityRaw(c, nil, nil)
	source := codexAccountIdentitySource(c, account)
	skip := s.skipCodexWireIdentity(ctx, c, account, model)
	var ids *codexFingerprintIDs
	if !skip && codexIdentityV2Enabled(source) {
		ids = resolveCodexFingerprintIDsWithBody(c, account, nil, gjson.ParseBytes(raw))
	}
	next, err := s.applyCodexAccountIdentityOrHarvestPinRaw(ctx, account, source, getAPIKeyIDFromContext(c), model, raw)
	if err != nil {
		return nil, err
	}
	if !skip && codexIdentityV2Enabled(source) {
		if ids != nil {
			next, _, err = applyCodexFingerprintClientMetadataRaw(next, ids)
			if err != nil {
				return nil, err
			}
		}
		stageCodexFingerprintIDs(c, ids)
		stageCodexConvergenceBodyIdentityRaw(c, source, next)
	} else {
		stageCodexFingerprintIDs(c, nil)
		stageCodexConvergenceBodyIdentityRaw(c, nil, nil)
	}
	return next, nil
}

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// ErrOpenAIWSOAuthCredentialChanged asks ingress to reselect and establish a
// fresh upstream connection, without starting another login or replaying an
// earlier completed turn. OpenAIWSCurrentTurnRetryPayload carries this turn.
var ErrOpenAIWSOAuthCredentialChanged = errors.New("OpenAI websocket OAuth credentials changed")

// Keep the transport's cancellation owner, but use the budget installed by the
// current turn's handler hook rather than the connection's original budget.
func openAIWSOAuthRecoveryContext(ctx context.Context, c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		if budget := recoveryBudget(c.Request.Context()); budget != nil {
			return context.WithValue(ctx, oauthRecoveryBudgetKey{}, budget)
		}
	}
	return ctx
}

func (s *OpenAIGatewayService) freshOpenAIWSOAuthToken(ctx context.Context, account *Account, fallback string) (string, error) {
	if s == nil || s.oauthRecovery == nil || s.accountRepo == nil || !oauth401CanRecover(account, nil) {
		return fallback, nil
	}
	latest, err := s.accountRepo.GetByID(ctx, account.ID)
	if err != nil {
		return "", err
	}
	if !oauth401CanRecover(latest, nil) {
		return "", denyOpenAITurn("authorization_binding_changed")
	}
	token, _, err := s.GetAccessToken(ctx, latest)
	return token, err
}

func openAIWSOAuthFailureStatus(account *Account, body []byte, message string) int {
	status := openAIStreamFailureStatus(body, message)
	body = openAIOAuthErrorEnvelope(body)
	if oauth401CanRecover(account, body) && credentialOperationEnvelope(body) == "requires_relogin" {
		return http.StatusUnauthorized
	}
	return status
}

func openAIOAuthErrorEnvelope(body []byte) []byte {
	// response.failed has an authoritative response.error envelope. Do not
	// inspect input, output, message, or other arbitrary nested content.
	if gjson.GetBytes(body, "type").String() == "response.failed" && !gjson.GetBytes(body, "error").Exists() {
		if failure := gjson.GetBytes(body, "response.error"); failure.IsObject() {
			return []byte(`{"error":` + failure.Raw + `}`)
		}
	}
	return body
}

// Starting the event and deciding whether the client can replay are separate:
// a rejected credential still needs repair after output/usage was observed.
func (s *OpenAIGatewayService) openAIWSOAuth401(ctx context.Context, account *Account, token string, headers http.Header, body []byte, model string) *UpstreamFailoverError {
	body = openAIOAuthErrorEnvelope(body)
	if s == nil || s.oauthRecovery == nil || recoveryBudget(ctx) == nil || !oauth401CanRecover(account, body) {
		return nil
	}
	recordOAuthRecoveryToken(ctx, account, token)
	s.handleOpenAIAccountUpstreamError(ctx, account, http.StatusUnauthorized, headers, body, model)
	return newOpenAIUpstreamFailoverError(http.StatusUnauthorized, headers, body, "Upstream authentication rejected", false)
}

// Passthrough has one reader in each direction. Retain only portable request
// input and completed output so recovery can move the current, unbilled turn.
type openAIWSOAuthReplay struct {
	mu         sync.Mutex
	enabled    bool
	portable   bool
	payload    []byte
	model      string
	input      []json.RawMessage
	hasInput   bool
	history    []json.RawMessage
	hasHistory bool
	collector  openAIWSToolCallReplayCollector
	usage      bool
	wrote      bool
}

func newOpenAIWSOAuthReplay(enabled bool, payload []byte, model string) *openAIWSOAuthReplay {
	r := &openAIWSOAuthReplay{enabled: enabled, portable: true}
	r.begin(payload, model)
	return r
}

func (r *openAIWSOAuthReplay) begin(payload []byte, model string) {
	if !r.enabled {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.payload = append([]byte(nil), payload...)
	r.model = model
	r.input, r.hasInput, _ = buildOpenAIWSReplayInputSequence(r.history, r.hasHistory, payload, gjson.GetBytes(payload, "previous_response_id").String() != "")
	r.collector = openAIWSToolCallReplayCollector{}
	r.usage, r.wrote = false, false
}

func (r *openAIWSOAuthReplay) invalidate() {
	r.mu.Lock()
	r.portable = false // Out-of-band session/item mutations are not reconstructible.
	r.mu.Unlock()
}

func (r *openAIWSOAuthReplay) observe(payload []byte) {
	if !r.enabled {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	event := gjson.GetBytes(payload, "type").String()
	if _, observed := extractOpenAIUsageFromJSONBytes(payload); observed {
		r.usage = true
	}
	r.collector.AddEvent(event, payload)
	if event == "response.completed" || event == "response.done" {
		r.history = append(append([]json.RawMessage(nil), r.input...), r.collector.AllItems()...)
		r.hasHistory = r.hasInput
	}
}

func (r *openAIWSOAuthReplay) clientWrote(payload []byte) {
	if !r.enabled {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if openAIStreamDataStartsClientOutput(string(payload), gjson.GetBytes(payload, "type").String()) {
		r.wrote = true
	}
}

func (r *openAIWSOAuthReplay) canReplay() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enabled && !r.usage && !r.wrote
}

func (r *openAIWSOAuthReplay) retry(cause error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var payload []byte
	if r.enabled && r.portable && !r.usage && !r.wrote {
		candidate, safe, err := buildOpenAIWSCurrentTurnRetryPayload(r.payload, r.input, r.hasInput, r.model)
		if err == nil && safe {
			payload = candidate
		}
	}
	return newOpenAIWSCurrentTurnFailoverError(cause, payload)
}

// Lifecycle metadata is held until semantic output or a terminal commits the
// turn. A pre-output 401 can then discard it without exposing two response IDs.
type openAIWSOAuthStager struct {
	enabled   bool
	pending   [][]byte
	bytes     int64
	committed bool
}

func (s *openAIWSOAuthStager) write(payload []byte, write func([]byte) error) error {
	event := gjson.GetBytes(payload, "type").String()
	keepalive := event == "keepalive"
	if s.enabled && !s.committed && !keepalive && !isOpenAIWSTerminalEvent(event) && !openAIStreamDataStartsClientOutput(string(payload), event) {
		if s.bytes+int64(len(payload)) > openAIFirstOutputStageMaxBytes {
			return errors.New("upstream websocket event staging limit exceeded")
		}
		s.pending = append(s.pending, append([]byte(nil), payload...))
		s.bytes += int64(len(payload))
		return nil
	}
	if !keepalive {
		for _, pending := range s.pending {
			if err := write(pending); err != nil {
				return err
			}
		}
		s.pending, s.bytes = nil, 0
	}
	if err := write(payload); err != nil {
		return err
	}
	if !keepalive {
		s.committed = true
	}
	if isOpenAIWSTerminalEvent(event) {
		s.committed = false // The next response.create starts a fresh turn.
	}
	return nil
}

type openAIWSOAuthFrameConn interface {
	ReadFrame(context.Context) (coderws.MessageType, []byte, error)
	WriteFrame(context.Context, coderws.MessageType, []byte) error
	Close() error
}

type openAIWSOAuthStagedConn struct {
	openAIWSOAuthFrameConn
	stager openAIWSOAuthStager
}

func (c *openAIWSOAuthStagedConn) WriteFrame(ctx context.Context, kind coderws.MessageType, payload []byte) error {
	if kind != coderws.MessageText || !c.stager.enabled {
		return c.openAIWSOAuthFrameConn.WriteFrame(ctx, kind, payload)
	}
	return c.stager.write(payload, func(data []byte) error {
		return c.openAIWSOAuthFrameConn.WriteFrame(ctx, kind, data)
	})
}

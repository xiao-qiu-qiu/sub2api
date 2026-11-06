package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/pkg/upstreamroute"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

type codexOptionalTestCall struct {
	req       *http.Request
	proxy     string
	accountID int64
}
type codexOptionalTestUpstream struct {
	calls chan codexOptionalTestCall
	hold  <-chan struct{}
}

func (u *codexOptionalTestUpstream) Do(req *http.Request, proxy string, accountID int64, _ int) (*http.Response, error) {
	u.calls <- codexOptionalTestCall{req.Clone(req.Context()), proxy, accountID}
	if u.hold != nil {
		select {
		case <-u.hold:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
}
func (u *codexOptionalTestUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func codexOptionalTestAccount(id int64, cookies, settings bool) *Account {
	return &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 3,
		Credentials: map[string]any{"chatgpt_account_id": fmt.Sprintf("workspace-%d", id), "chatgpt_user_id": fmt.Sprintf("user-%d", id)},
		Extra:       map[string]any{"codex_identity_scheme": "v2", codexWireOptionsExtraKey: map[string]any{"cookie_replay": cookies, "settings_user": settings}},
	}
}
func codexOptionalTestRequest(s *OpenAIGatewayService, account *Account, raw string) *http.Request {
	req, _ := http.NewRequest(http.MethodPost, raw, nil)
	req.Header.Set("Authorization", "Bearer test-only")
	req.Header.Set("Thread-Id", "thread-one")
	req.Header.Set("Chatgpt-Account-Id", account.GetChatGPTAccountID())
	req.Header.Set("User-Agent", "test-codex")
	return s.prepareCodexOptionalTraffic(nil, account, req)
}
func codexOptionalTestResponse(status int, value string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Set-Cookie": {"__oailb=" + value + "; Secure; Path=/", "session-token=never-store; Secure; Path=/"}}}
}
func codexOptionalReceive(t *testing.T, calls <-chan codexOptionalTestCall) codexOptionalTestCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(time.Second):
		t.Fatal("optional request not dispatched")
		return codexOptionalTestCall{}
	}
}

func TestCodexOptionalTrafficDefaultOff(t *testing.T) {
	u := &codexOptionalTestUpstream{calls: make(chan codexOptionalTestCall, 1)}
	s := &OpenAIGatewayService{httpUpstream: u}
	a := codexOptionalTestAccount(1, false, false)
	req := codexOptionalTestRequest(s, a, "https://chatgpt.com/backend-api/codex/responses")
	require.Nil(t, codexOptionalTrafficFromContext(req.Context()), "v2 must not enable optional traffic")
	s.observeCodexOptionalTraffic(req, codexOptionalTestResponse(200, "off"), a, "")
	require.Same(t, req, s.applyCodexOptionalTraffic(req, a, ""))
	s.scheduleCodexOptionalTraffic(req, a, "")
	s.scheduleCodexWSOptionalTraffic(req.Context(), a, req.Header, "", []byte(`{"type":"response.create"}`))
	require.Empty(t, u.calls)
	require.Empty(t, s.codexOptionalTraffic.jars)
	require.Zero(t, s.codexOptionalTraffic.running)
}

func TestCodexOptionalTrafficThreadEvidenceWithoutV2(t *testing.T) {
	upstream := &codexOptionalTestUpstream{calls: make(chan codexOptionalTestCall, 2)}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := codexOptionalTestAccount(910, false, true)
	account.Extra["codex_identity_scheme"] = "v1"
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(`{"client_metadata":{"thread_id":"scoped-body-thread"}}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer fixture-only")
	req = svc.prepareCodexOptionalTraffic(nil, account, req)
	svc.scheduleCodexOptionalTraffic(req, account, "")
	call := codexOptionalReceive(t, upstream.calls)
	require.Equal(t, http.MethodGet, call.req.Method)
	require.Equal(t, codexOptionalSettingsPath, call.req.URL.Path)
	require.Empty(t, req.Header.Get("thread-id"), "deduplication must not mutate inference headers")
}

func TestCodexOptionalTrafficCredentialAndProxyIsolation(t *testing.T) {
	s := &OpenAIGatewayService{}
	a, other := codexOptionalTestAccount(1, true, false), codexOptionalTestAccount(2, true, false)
	req := codexOptionalTestRequest(s, a, "https://chatgpt.com/backend-api/codex/responses")
	s.observeCodexOptionalTraffic(req, codexOptionalTestResponse(200, "route-a"), a, "http://proxy-a:8080")
	sent := s.applyCodexOptionalTraffic(req, a, "http://proxy-a:8080")
	require.Equal(t, "__oailb=route-a", sent.Header.Get("Cookie"))
	require.Empty(t, req.Header.Get("Cookie"), "caller header must remain independent")
	require.Empty(t, s.applyCodexOptionalTraffic(req, a, "http://proxy-b:8080").Header.Get("Cookie"))
	require.Empty(t, s.applyCodexOptionalTraffic(sent, a, "http://proxy-b:8080").Header.Get("Cookie"), "retry must shed our previous proxy's cookie")
	otherReq := codexOptionalTestRequest(s, other, req.URL.String())
	require.Empty(t, s.applyCodexOptionalTraffic(otherReq, other, "http://proxy-a:8080").Header.Get("Cookie"))
	// Reauthorization changes the namespace, while a request already prepared keeps its immutable snapshot.
	a.Credentials["chatgpt_user_id"] = "replacement-user"
	require.Equal(t, "__oailb=route-a", s.applyCodexOptionalTraffic(req, a, "http://proxy-a:8080").Header.Get("Cookie"))
	require.Empty(t, s.applyCodexOptionalTraffic(codexOptionalTestRequest(s, a, req.URL.String()), a, "http://proxy-a:8080").Header.Get("Cookie"))
	// Re-preparing a context for a disabled failover account clears the old binding.
	other.Extra[codexWireOptionsExtraKey] = map[string]any{}
	require.Nil(t, codexOptionalTrafficFromContext(s.prepareCodexOptionalTrafficContext(req.Context(), nil, other)))
}

func TestCodexOptionalTrafficExistingCookieTicketAndConnectionPriority(t *testing.T) {
	s := &OpenAIGatewayService{}
	a := codexOptionalTestAccount(1, true, true)
	req := codexOptionalTestRequest(s, a, "https://chatgpt.com/backend-api/codex/responses")
	s.observeCodexOptionalTraffic(req, codexOptionalTestResponse(200, "jar"), a, "")
	withCookie := req.Clone(req.Context())
	withCookie.Header["cookie"] = []string{"__oailb=existing"}
	require.Equal(t, []string{"__oailb=existing"}, s.applyCodexOptionalTraffic(withCookie, a, "").Header["cookie"]) //nolint:staticcheck // SA1008: verify the deliberately noncanonical raw key is preserved without adding a canonical Cookie header.
	require.Empty(t, s.applyCodexOptionalTraffic(withCookie, a, "").Header.Get("Cookie"))
	bound := req.WithContext(context.WithValue(req.Context(), boundAstraTicketContextKey{}, true))
	require.Empty(t, s.applyCodexOptionalTraffic(bound, a, "").Header.Get("Cookie"))
	s.openaiCodexTickets.Store(openAICodexTicketKey(a.ID, openAICodexTicketDefaultModel), &openAICodexTicket{AccountID: a.ID, State: "ticket-state"})
	bound = req.Clone(req.Context())
	bound.Header.Set(openAICodexTurnStateHeader, "ticket-state")
	require.Empty(t, applyCodexOptionalTrafficWS(bound.Context(), "wss://chatgpt.com/backend-api/codex/responses", bound.Header, "").Get("Cookie"))
	s.observeCodexOptionalTraffic(bound, codexOptionalTestResponse(200, "ticket-cookie"), a, "")
	require.Equal(t, "__oailb=jar", s.applyCodexOptionalTraffic(req, a, "").Header.Get("Cookie"))
	a.AuthConnectionID = "managed-connection"
	require.Empty(t, s.applyCodexOptionalTraffic(req, a, "").Header.Get("Cookie"))
	require.Nil(t, codexOptionalTrafficFromContext(s.prepareCodexOptionalTrafficContext(req.Context(), nil, a)))
}

func TestCodexOptionalTrafficHostsSuccessAndWS(t *testing.T) {
	s := &OpenAIGatewayService{}
	a := codexOptionalTestAccount(1, true, false)
	req := codexOptionalTestRequest(s, a, "https://chatgpt.com/backend-api/codex/responses")
	s.observeCodexOptionalTraffic(req, codexOptionalTestResponse(403, "failed"), a, "")
	require.Empty(t, s.applyCodexOptionalTraffic(req, a, "").Header.Get("Cookie"))
	for _, raw := range []string{"https://chatgpt.com.evil.test/responses", "http://chatgpt.com/responses", "https://api.openai.com/v1/responses"} {
		bad := codexOptionalTestRequest(s, a, raw)
		s.observeCodexOptionalTraffic(bad, codexOptionalTestResponse(200, "wrong-host"), a, "")
		require.Empty(t, s.applyCodexOptionalTraffic(bad, a, "").Header.Get("Cookie"))
	}
	wsURL := "wss://chatgpt.com/backend-api/codex/responses"
	observeCodexOptionalTrafficWS(req.Context(), wsURL, req.Header, 101, codexOptionalTestResponse(101, "ws").Header, "http://actual:8080")
	require.Equal(t, "__oailb=ws", applyCodexOptionalTrafficWS(req.Context(), wsURL, req.Header, "http://actual:8080").Get("Cookie"))
	require.Empty(t, applyCodexOptionalTrafficWS(req.Context(), wsURL, req.Header, "").Get("Cookie"))
	require.Empty(t, req.Header.Get("Cookie"))
	spoof := req.Clone(req.Context())
	spoof.Host = "evil.test"
	require.Empty(t, s.applyCodexOptionalTraffic(spoof, a, "http://actual:8080").Header.Get("Cookie"))
}

func TestCodexOptionalTrafficSettingsDedupAndSnapshot(t *testing.T) {
	u := &codexOptionalTestUpstream{calls: make(chan codexOptionalTestCall, 8)}
	s := &OpenAIGatewayService{httpUpstream: u}
	a := codexOptionalTestAccount(1, false, true)
	req := codexOptionalTestRequest(s, a, "https://chatgpt.com/backend-api/codex/responses")
	req.Header.Set("Originator", "must-not-copy")
	for i := 0; i < 10; i++ {
		s.scheduleCodexOptionalTraffic(req, a, "http://actual:8080")
	}
	call := codexOptionalReceive(t, u.calls)
	require.Equal(t, "https://chatgpt.com/backend-api/wham/settings/user", call.req.URL.String())
	require.Equal(t, http.MethodGet, call.req.Method)
	require.Equal(t, "http://actual:8080", call.proxy)
	require.Equal(t, "Bearer test-only", call.req.Header.Get("Authorization"))
	require.Equal(t, "no-cache, no-store", call.req.Header.Get("Cache-Control"))
	require.Empty(t, call.req.Header.Get("Originator"))
	require.Nil(t, call.req.Body)
	require.Equal(t, true, call.req.Context().Value(codexOptionalSideCallKey{}))
	deadline, ok := call.req.Context().Deadline()
	require.True(t, ok)
	require.LessOrEqual(t, time.Until(deadline), codexOptionalTimeout)
	// Same credential/thread over WS or another egress is still only one GET.
	s.scheduleCodexWSOptionalTraffic(req.Context(), a, req.Header, "http://different:8080", []byte(`{"type":"response.create","client_metadata":{"thread_id":"thread-one"}}`))
	s.scheduleCodexOptionalTraffic(call.req, a, call.proxy) // no recursive scheduling
	require.Empty(t, u.calls)
	for _, path := range []string{"/backend-api/codex/responses/compact", "/backend-api/models"} {
		bad := req.Clone(req.Context())
		bad.URL.Path = path
		s.scheduleCodexOptionalTraffic(bad, a, "")
	}
	require.Empty(t, u.calls)
	other := codexOptionalTestAccount(2, false, true)
	s.scheduleCodexOptionalTraffic(codexOptionalTestRequest(s, other, req.URL.String()), other, call.proxy)
	require.Equal(t, other.ID, codexOptionalReceive(t, u.calls).accountID)
	// Preparation captures options and namespace, rather than retaining their maps.
	a.Extra[codexWireOptionsExtraKey] = map[string]any{}
	a.Credentials["chatgpt_user_id"] = "new-user"
	s.scheduleCodexWSOptionalTraffic(req.Context(), a, req.Header, call.proxy, []byte(`{"type":"response.create","response":{"client_metadata":{"thread_id":"thread-two"}}}`))
	require.Equal(t, a.ID, codexOptionalReceive(t, u.calls).accountID)
}

func TestCodexOptionalTrafficWorkerAndStorageBounds(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	u := &codexOptionalTestUpstream{calls: make(chan codexOptionalTestCall, codexOptionalMaxRunning), hold: release}
	s := &OpenAIGatewayService{httpUpstream: u}
	a := codexOptionalTestAccount(1, true, true)
	req := codexOptionalTestRequest(s, a, "https://chatgpt.com/backend-api/codex/responses")
	for i := 0; i < codexOptionalMaxRunning+10; i++ {
		next := req.Clone(req.Context())
		next.Header.Set("Thread-Id", fmt.Sprintf("thread-%d", i))
		s.scheduleCodexOptionalTraffic(next, a, "")
	}
	for i := 0; i < codexOptionalMaxRunning; i++ {
		codexOptionalReceive(t, u.calls)
	}
	r := &s.codexOptionalTraffic
	r.mu.Lock()
	require.Equal(t, codexOptionalMaxRunning, r.running)
	require.Len(t, r.seen, codexOptionalMaxRunning)
	r.mu.Unlock()
	b := codexOptionalTrafficFromContext(req.Context())
	for i := 0; i < codexOptionalCookieMaxJars+3; i++ {
		b.cookieJar(fmt.Sprintf("http://proxy-%d:8080", i), true)
	}
	r.mu.Lock()
	require.Len(t, r.jars, codexOptionalCookieMaxJars)
	for key, entry := range r.jars {
		entry.expires = time.Now().Add(-time.Second)
		r.jars[key] = entry
	}
	r.mu.Unlock()
	b.cookieJar("", false)
	r.mu.Lock()
	require.Empty(t, r.jars)
	r.mu.Unlock()
}

type codexOptionalTestRoundTripper func(*http.Request) (*http.Response, error)

func (f codexOptionalTestRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCodexOptionalTrafficWSSchedulesAfterSendWithActualRegionalProxy(t *testing.T) {
	handshakes := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		handshakes <- req.Header.Clone()
		w.Header().Add("Set-Cookie", "__cf_bm=handshake; Secure; Path=/")
		conn, err := coderws.Accept(w, req, &coderws.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			kind, payload, err := conn.Read(req.Context())
			if err != nil {
				return
			}
			if conn.Write(req.Context(), kind, payload) != nil {
				return
			}
		}
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	actualProxy := "http://regional-proxy.test:8080"
	cfg := regionalWSConfig(t, actualProxy, "chatgpt.com")
	dialer := newConfiguredOpenAIWSClientDialer(cfg).(*coderOpenAIWSClientDialer)
	// Keep the real coder/websocket handshake and frames, routing this test's
	// selected proxy client to the local fixture without external networking.
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	dialer.proxyClients[actualProxy] = &openAIWSProxyClientEntry{
		client: &http.Client{Transport: codexOptionalTestRoundTripper(func(req *http.Request) (*http.Response, error) {
			clone := req.Clone(req.Context())
			clone.URL.Scheme, clone.URL.Host = "http", target.Host
			return transport.RoundTrip(clone)
		})}, lastUsedUnixNano: time.Now().UnixNano(),
	}
	u := &codexOptionalTestUpstream{calls: make(chan codexOptionalTestCall, 4)}
	s := &OpenAIGatewayService{httpUpstream: u}
	a := codexOptionalTestAccount(1, true, true)
	req := codexOptionalTestRequest(s, a, "https://chatgpt.com/backend-api/codex/responses")
	s.observeCodexOptionalTraffic(req, codexOptionalTestResponse(200, "regional"), a, actualProxy)
	s.observeCodexOptionalTraffic(req, codexOptionalTestResponse(200, "direct"), a, "")
	type privateContextKey struct{}
	ctx := context.WithValue(upstreamroute.WithAccountID(req.Context(), a.ID), privateContextKey{}, "do-not-retain")
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, _, _, err := dialer.Dial(ctx, "wss://chatgpt.com/backend-api/codex/responses", req.Header, "")
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	wrapped := conn.(*coderOpenAIWSClientConn)
	require.Equal(t, actualProxy, wrapped.optionalTrafficProxy)
	require.Nil(t, wrapped.optionalTrafficContext.Value(privateContextKey{}))
	require.Equal(t, "__oailb=regional", (<-handshakes).Get("Cookie"))
	require.Contains(t, applyCodexOptionalTrafficWS(req.Context(), "wss://chatgpt.com/backend-api/codex/responses", req.Header, actualProxy).Get("Cookie"), "__cf_bm=handshake")
	req.Header.Set("Authorization", "Bearer caller-mutated")
	require.NoError(t, conn.WriteJSON(ctx, map[string]any{"type": "response.create", "client_metadata": map[string]any{"thread_id": "ws-first"}}))
	_, err = conn.ReadMessage(ctx)
	require.NoError(t, err)
	call := codexOptionalReceive(t, u.calls)
	require.Equal(t, actualProxy, call.proxy)
	require.Equal(t, "Bearer test-only", call.req.Header.Get("Authorization"), "handshake headers must be copied")
	require.NoError(t, wrapped.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","client_metadata":{"thread_id":"ws-second"}}`)))
	_, err = conn.ReadMessage(ctx)
	require.NoError(t, err)
	require.Equal(t, actualProxy, codexOptionalReceive(t, u.calls).proxy)
	require.NoError(t, wrapped.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","client_metadata":{"thread_id":"ws-second"}}`)))
	_, err = conn.ReadMessage(ctx)
	require.NoError(t, err)
	require.Empty(t, u.calls, "a repeated frame must not duplicate settings traffic")
	require.NoError(t, conn.Close())
	require.Error(t, wrapped.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","client_metadata":{"thread_id":"failed-write"}}`)))
	require.Empty(t, u.calls, "failed sends must not schedule optional traffic")
}

func TestCodexOptionalTrafficPluginSelectionUsesMinimalSnapshot(t *testing.T) {
	manager := &PluginManager{}
	manager.route.Store(&pluginRoute{rolloutPercent: 100})
	s := &OpenAIGatewayService{}
	a := codexOptionalTestAccount(1, false, true)
	a.Extra["unrelated_private_config"] = map[string]any{"nested": "do-not-copy"}
	req := codexOptionalTestRequest(s, a, "https://chatgpt.com/backend-api/codex/responses")
	snapshot := codexOptionalTrafficFromContext(req.Context()).snapshot.account()
	require.True(t, manager.ShouldRouteOpenAIOAuth(a))
	require.True(t, manager.ShouldRouteOpenAIOAuth(snapshot))
	require.Nil(t, snapshot.Extra)
	require.Nil(t, snapshot.Credentials)
}

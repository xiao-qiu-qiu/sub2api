package service

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/chatgptcookies"
	"github.com/Wei-Shaw/sub2api/internal/pkg/upstreamroute"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	codexOptionalCookieMaxJars    = 512
	codexOptionalCookieTTL        = 2 * time.Hour
	codexOptionalThreadMaxEntries = 4096
	codexOptionalThreadTTL        = 2 * time.Hour
	codexOptionalMaxRunning       = 8
	codexOptionalTimeout          = 10 * time.Second
	codexOptionalResponseLimit    = 1 << 20
	codexOptionalSettingsPath     = "/backend-api/wham/settings/user"
)

type codexOptionalTrafficContextKey struct{}
type codexOptionalSideCallKey struct{}
type codexOptionalReplayKey struct{}
type codexOptionalReplay struct{ cookie string }

// No gin context, account pointer, bearer, or shared credential/extra map escapes
// into this immutable request snapshot.
type codexOptionalTrafficSnapshot struct {
	namespace                  [32]byte
	accountID                  int64
	concurrency                int
	platform, accountType      string
	cookieReplay, settingsUser bool
	ticketStates               [][32]byte
}

func (snapshot codexOptionalTrafficSnapshot) account() *Account {
	return &Account{ID: snapshot.accountID, Concurrency: snapshot.concurrency, Platform: snapshot.platform, Type: snapshot.accountType}
}

type codexOptionalTrafficBinding struct {
	runtime  *codexOptionalTrafficRuntime
	snapshot codexOptionalTrafficSnapshot
}

type codexOptionalCookieEntry struct {
	jar     *chatgptcookies.Jar
	expires time.Time
}

// Zero value is ready to use; all storage and worker admission belong to one service.
type codexOptionalTrafficRuntime struct {
	mu      sync.Mutex
	jars    map[[32]byte]codexOptionalCookieEntry
	seen    map[[32]byte]time.Time
	running int
	service *OpenAIGatewayService
	once    sync.Once
}

func codexOptionalAccountFenced(account *Account) bool {
	return account == nil || account.AuthConnectionID != "" || account.HasManagedOpenAIConnections()
}

func (s *OpenAIGatewayService) prepareCodexOptionalTraffic(c *gin.Context, account *Account, req *http.Request) *http.Request {
	if req == nil {
		return nil
	}
	ctx := s.prepareCodexOptionalTrafficContext(req.Context(), c, account)
	if ctx == req.Context() {
		return req
	}
	return req.WithContext(ctx)
}

func (s *OpenAIGatewayService) prepareCodexOptionalTrafficContext(ctx context.Context, c *gin.Context, account *Account) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	// Re-preparation after failover must also clear an earlier enabled binding.
	clear := func() context.Context {
		if ctx.Value(codexOptionalTrafficContextKey{}) != nil {
			return context.WithValue(ctx, codexOptionalTrafficContextKey{}, (*codexOptionalTrafficBinding)(nil))
		}
		return ctx
	}
	if s == nil || codexOptionalAccountFenced(account) || ctx.Value(codexOptionalSideCallKey{}) == true {
		return clear()
	}
	source := codexAccountIdentitySource(c, account)
	if codexOptionalAccountFenced(source) {
		return clear()
	}
	cookieReplay := codexWireOptionEnabled(c, account, "cookie_replay")
	settingsUser := codexWireOptionEnabled(c, account, "settings_user")
	namespace := codexAccountIdentityNamespace(source)
	if (!cookieReplay && !settingsUser) || namespace == "" {
		return clear()
	}
	snapshot := codexOptionalTrafficSnapshot{
		namespace: sha256.Sum256([]byte(namespace)), accountID: account.ID,
		concurrency: account.Concurrency, platform: account.Platform, accountType: account.Type,
		cookieReplay: cookieReplay, settingsUser: settingsUser,
	}
	// WS dialers have no Account. Copy only hashes of currently bound ticket
	// states, so stored account tickets receive the same precedence as HTTP.
	if s.openAICodexTicketEnabledContext(ctx) {
		for _, model := range s.openAICodexTicketConfig().Models {
			if ticket := s.lookupOpenAICodexTicket(account, model); ticket != nil && !ticket.Revoked && ticket.State != "" {
				snapshot.ticketStates = append(snapshot.ticketStates, sha256.Sum256([]byte(ticket.State)))
			}
		}
	}
	runtime := &s.codexOptionalTraffic
	runtime.once.Do(func() { runtime.service = s })
	return context.WithValue(ctx, codexOptionalTrafficContextKey{}, &codexOptionalTrafficBinding{runtime: runtime, snapshot: snapshot})
}

func codexOptionalTrafficFromContext(ctx context.Context) *codexOptionalTrafficBinding {
	if ctx == nil {
		return nil
	}
	binding, _ := ctx.Value(codexOptionalTrafficContextKey{}).(*codexOptionalTrafficBinding)
	return binding
}

// A socket may outlive its dial request. Retain only our immutable binding, never
// the dial context's gin, timing, cancellation, or billing state.
func codexOptionalTrafficWSDialContext(ctx context.Context, wsURL string) context.Context {
	b := codexOptionalTrafficFromContext(ctx)
	u := codexOptionalCookieURL(wsURL)
	if b == nil || !b.snapshot.settingsUser || u == nil || !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/responses") {
		return nil
	}
	clean := context.WithValue(context.Background(), codexOptionalTrafficContextKey{}, b)
	if IsBoundAstraTicketRequest(ctx) {
		clean = context.WithValue(clean, boundAstraTicketContextKey{}, true)
	}
	return clean
}

func (b *codexOptionalTrafficBinding) guarded(ctx context.Context, headers http.Header) bool {
	if b == nil || b.runtime == nil || IsBoundAstraTicketRequest(ctx) {
		return true
	}
	state := headers.Get(openAICodexTurnStateHeader)
	if state == "" {
		return false
	}
	digest := sha256.Sum256([]byte(state))
	for _, candidate := range b.snapshot.ticketStates {
		if digest == candidate {
			return true
		}
	}
	// Also catch a ticket acquired after preparation, without retaining Account maps.
	s := b.runtime.service
	if s == nil {
		return true
	}
	models := s.openAICodexTicketConfig().Models
	s.openaiCodexTicketStateMu.Lock()
	defer s.openaiCodexTicketStateMu.Unlock()
	for _, model := range models {
		value, _ := s.openaiCodexTickets.Load(openAICodexTicketKey(b.snapshot.accountID, model))
		ticket, _ := value.(*openAICodexTicket)
		if ticket != nil && ticket.AccountID == b.snapshot.accountID && !ticket.Revoked && ticket.State == state {
			return true
		}
	}
	return false
}

func codexOptionalCookieURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return nil
	}
	if u.Scheme == "wss" {
		u.Scheme = "https"
	}
	if !chatgptcookies.IsChatGPTURL(u) {
		return nil
	}
	return u
}

func codexOptionalRequestURL(req *http.Request) *url.URL {
	if req == nil || req.URL == nil {
		return nil
	}
	u := codexOptionalCookieURL(req.URL.String())
	if u == nil || (req.Host != "" && !strings.EqualFold(req.Host, req.URL.Host)) {
		return nil
	}
	return u
}

func (b *codexOptionalTrafficBinding) cookieJar(proxyURL string, create bool) *chatgptcookies.Jar {
	key := sha256.Sum256([]byte(string(b.snapshot.namespace[:]) + "\x00" + strings.TrimSpace(proxyURL)))
	r := b.runtime
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for candidate, entry := range r.jars {
		if !now.Before(entry.expires) {
			delete(r.jars, candidate)
		}
	}
	if entry, ok := r.jars[key]; ok {
		return entry.jar
	}
	if !create {
		return nil
	}
	if r.jars == nil {
		r.jars = make(map[[32]byte]codexOptionalCookieEntry)
	}
	if len(r.jars) >= codexOptionalCookieMaxJars {
		var oldestKey [32]byte
		var oldest time.Time
		for candidate, entry := range r.jars {
			if oldest.IsZero() || entry.expires.Before(oldest) {
				oldestKey, oldest = candidate, entry.expires
			}
		}
		delete(r.jars, oldestKey)
	}
	jar := chatgptcookies.NewJar()
	r.jars[key] = codexOptionalCookieEntry{jar: jar, expires: now.Add(codexOptionalCookieTTL)}
	return jar
}

func codexOptionalHasCookie(headers http.Header) bool {
	// Header overrides and third-party dialers may supply non-canonical casing.
	for name := range headers {
		if strings.EqualFold(name, "Cookie") {
			return true
		}
	}
	return false
}

func (s *OpenAIGatewayService) applyCodexOptionalTraffic(req *http.Request, account *Account, proxyURL string) *http.Request {
	if s == nil || req == nil || codexOptionalAccountFenced(account) {
		return req
	}
	b := codexOptionalTrafficFromContext(req.Context())
	if b == nil || b.runtime != &s.codexOptionalTraffic || b.snapshot.accountID != account.ID || !b.snapshot.cookieReplay ||
		b.guarded(req.Context(), req.Header) || s.boundCodexTicket(req, account) != nil {
		return req
	}
	u := codexOptionalRequestURL(req)
	if u == nil {
		return req
	}
	clone := req.Clone(req.Context())
	if clone.Header == nil {
		clone.Header = make(http.Header)
	}
	if replay, ok := req.Context().Value(codexOptionalReplayKey{}).(codexOptionalReplay); ok && clone.Header.Get("Cookie") == replay.cookie {
		clone.Header.Del("Cookie") // a proxy retry must not reuse our earlier egress cookie
	}
	if codexOptionalHasCookie(clone.Header) {
		return clone
	}
	jar := b.cookieJar(proxyURL, false)
	if jar == nil {
		return clone
	}
	for _, cookie := range jar.Cookies(u) {
		clone.AddCookie(cookie)
	}
	if cookie := clone.Header.Get("Cookie"); cookie != "" {
		clone = clone.WithContext(context.WithValue(clone.Context(), codexOptionalReplayKey{}, codexOptionalReplay{cookie: cookie}))
	}
	return clone
}

func (s *OpenAIGatewayService) observeCodexOptionalTraffic(req *http.Request, resp *http.Response, account *Account, proxyURL string) {
	if s == nil || req == nil || codexOptionalAccountFenced(account) {
		return
	}
	b := codexOptionalTrafficFromContext(req.Context())
	if b == nil || b.runtime != &s.codexOptionalTraffic || b.snapshot.accountID != account.ID || s.boundCodexTicket(req, account) != nil {
		return
	}
	observeCodexOptionalTrafficResponse(b, req, resp, proxyURL)
}

func observeCodexOptionalTrafficResponse(b *codexOptionalTrafficBinding, req *http.Request, resp *http.Response, proxyURL string) {
	if b == nil || !b.snapshot.cookieReplay || req == nil || resp == nil || b.guarded(req.Context(), req.Header) ||
		(resp.StatusCode != http.StatusSwitchingProtocols && (resp.StatusCode < 200 || resp.StatusCode >= 300)) {
		return
	}
	u := codexOptionalRequestURL(req)
	if u == nil {
		return
	}
	if resp.Request != nil {
		if b.guarded(resp.Request.Context(), resp.Request.Header) {
			return
		}
		finalURL := codexOptionalRequestURL(resp.Request)
		if finalURL == nil || !strings.EqualFold(finalURL.Host, u.Host) {
			return
		}
		u = finalURL
	}
	var kept []*http.Cookie
	for _, cookie := range resp.Cookies() {
		if chatgptcookies.IsAllowedName(cookie.Name) {
			kept = append(kept, cookie)
		}
	}
	if len(kept) != 0 {
		b.cookieJar(proxyURL, true).SetCookies(u, kept)
	}
}

// All dialers call these AFTER choosing their actual proxy. The returned header
// is independent of the caller's map, including when no cookie is available.
func applyCodexOptionalTrafficWS(ctx context.Context, wsURL string, headers http.Header, actualProxyURL string) http.Header {
	clone := headers.Clone()
	if clone == nil {
		clone = make(http.Header)
	}
	b := codexOptionalTrafficFromContext(ctx)
	u := codexOptionalCookieURL(wsURL)
	if b == nil || !b.snapshot.cookieReplay || u == nil || b.guarded(ctx, headers) || codexOptionalHasCookie(clone) {
		return clone
	}
	jar := b.cookieJar(actualProxyURL, false)
	if jar == nil {
		return clone
	}
	req := &http.Request{Header: clone}
	for _, cookie := range jar.Cookies(u) {
		req.AddCookie(cookie)
	}
	return req.Header
}

func observeCodexOptionalTrafficWS(ctx context.Context, wsURL string, headers http.Header, statusCode int, responseHeaders http.Header, actualProxyURL string) {
	u := codexOptionalCookieURL(wsURL)
	if u == nil {
		return
	}
	req := (&http.Request{URL: u, Header: headers}).WithContext(ctx)
	observeCodexOptionalTrafficResponse(codexOptionalTrafficFromContext(ctx), req, &http.Response{StatusCode: statusCode, Header: responseHeaders}, actualProxyURL)
}

func (s *OpenAIGatewayService) scheduleCodexOptionalTraffic(req *http.Request, account *Account, proxyURL string) {
	if s == nil || req == nil || req.Method != http.MethodPost || codexOptionalAccountFenced(account) ||
		req.URL == nil || !strings.HasSuffix(strings.TrimRight(req.URL.Path, "/"), "/responses") {
		return
	}
	b := codexOptionalTrafficFromContext(req.Context())
	if b == nil || b.runtime != &s.codexOptionalTraffic || b.snapshot.accountID != account.ID || !b.snapshot.settingsUser || s.boundCodexTicket(req, account) != nil {
		return
	}
	// Legacy header forwarding can omit thread-id while keeping scoped body
	// metadata. This option is independent of v2 and may use that same evidence
	// for deduplication without changing the actual inference headers.
	if req.Header.Get("thread-id") == "" && req.GetBody != nil {
		if reader, err := req.GetBody(); err == nil {
			body, readErr := io.ReadAll(reader)
			_ = reader.Close()
			if readErr == nil {
				thread := gjson.GetBytes(body, "client_metadata.thread_id").Str
				if thread == "" {
					thread = gjson.GetBytes(body, "response.client_metadata.thread_id").Str
				}
				if thread != "" {
					req = req.Clone(req.Context())
					req.Header.Set("thread-id", thread)
				}
			}
		}
	}
	s.scheduleCodexOptionalSettings(req, b, proxyURL)
}

// Call only after the main WS frame was sent. It uses this turn's identity;
// callers supply the actual dial proxy rather than re-reading Account.Proxy.
func (s *OpenAIGatewayService) scheduleCodexWSOptionalTraffic(ctx context.Context, account *Account, headers http.Header, proxyURL string, payload []byte) {
	if s == nil || codexOptionalAccountFenced(account) || !gjson.ValidBytes(payload) || gjson.GetBytes(payload, "type").Str != "response.create" {
		return
	}
	b := codexOptionalTrafficFromContext(ctx)
	if b == nil || b.runtime != &s.codexOptionalTraffic || b.snapshot.accountID != account.ID || s.boundCodexTicketFromHeader(ctx, headers, account) != nil {
		return
	}
	clone := headers.Clone()
	if clone == nil {
		clone = make(http.Header)
	}
	for _, path := range []string{"client_metadata.thread_id", "response.client_metadata.thread_id"} {
		if thread := gjson.GetBytes(payload, path); thread.Exists() {
			if thread.Type != gjson.String {
				return
			}
			clone.Set("thread-id", thread.Str)
			break
		}
	}
	u, _ := url.Parse("https://chatgpt.com/backend-api/codex/responses")
	req := (&http.Request{Method: http.MethodPost, URL: u, Header: clone}).WithContext(ctx)
	s.scheduleCodexOptionalSettings(req, b, proxyURL)
}

func (s *OpenAIGatewayService) scheduleCodexOptionalSettings(main *http.Request, b *codexOptionalTrafficBinding, proxyURL string) {
	if !b.snapshot.settingsUser || main.Context().Value(codexOptionalSideCallKey{}) == true ||
		b.guarded(main.Context(), main.Header) || s.httpUpstream == nil {
		return
	}
	u := codexOptionalRequestURL(main)
	thread := strings.TrimSpace(main.Header.Get("thread-id"))
	if u == nil || thread == "" || len(thread) > 1024 || main.Header.Get("Authorization") == "" {
		return
	}
	headers := make(http.Header)
	for _, name := range []string{"Authorization", "Chatgpt-Account-Id", "User-Agent", "X-Openai-Fedramp"} {
		if value := main.Header.Get(name); value != "" {
			headers.Set(name, value)
		}
	}
	headers.Set("Cache-Control", "no-cache, no-store")
	headers.Set("Accept", "*/*")
	// Fixed two-hour deduplication per credential/thread, independent of proxy.
	key := sha256.Sum256([]byte(string(b.snapshot.namespace[:]) + "\x00" + thread))
	r := b.runtime
	now := time.Now()
	r.mu.Lock()
	for candidate, expiry := range r.seen {
		if !now.Before(expiry) {
			delete(r.seen, candidate)
		}
	}
	if _, exists := r.seen[key]; exists || len(r.seen) >= codexOptionalThreadMaxEntries || r.running >= codexOptionalMaxRunning {
		r.mu.Unlock()
		return
	}
	if r.seen == nil {
		r.seen = make(map[[32]byte]time.Time)
	}
	r.seen[key], r.running = now.Add(codexOptionalThreadTTL), r.running+1
	r.mu.Unlock()
	// Keep the same host and actual egress; discard inference path/query/identity.
	u.Path, u.RawPath, u.RawQuery, u.Fragment = codexOptionalSettingsPath, "", "", ""
	go func() {
		defer func() {
			r.mu.Lock()
			r.running--
			r.mu.Unlock()
			_ = recover() // optional traffic must never take down inference
		}()
		ctx, cancel := context.WithTimeout(context.Background(), codexOptionalTimeout)
		defer cancel()
		ctx = context.WithValue(ctx, codexOptionalTrafficContextKey{}, b)
		ctx = context.WithValue(ctx, codexOptionalSideCallKey{}, true)
		ctx = WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(upstreamroute.WithAccountID(ctx, b.snapshot.accountID), HTTPUpstreamProfileOpenAI))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return
		}
		req.Header = headers
		account := b.snapshot.account()
		req = s.applyCodexOptionalTraffic(req, account, proxyURL)
		// Bypass inference admission, ticket acquisition, RPM and billing. GET does
		// not satisfy the repository's POST /responses ticket routing predicate.
		var resp *http.Response
		if s.pluginManager != nil {
			var handled bool
			resp, handled, err = s.pluginManager.RoundTripOpenAIOAuth(ctx, req, proxyURL, account)
			if !handled {
				resp, err = s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
			}
		} else {
			resp, err = s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
		}
		if resp != nil && resp.Body != nil {
			defer resp.Body.Close()
		}
		if err != nil || resp == nil {
			return
		}
		observeCodexOptionalTrafficResponse(b, req, resp, proxyURL)
		if resp.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, codexOptionalResponseLimit))
		}
	}()
}

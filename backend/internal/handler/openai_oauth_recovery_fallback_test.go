//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type oauthRecoveryHandlerBindings struct {
	service.GatewayCache
	mu     sync.Mutex
	owners map[string]int64
}

func (b *oauthRecoveryHandlerBindings) GetSessionAccountID(_ context.Context, _ int64, hash string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id := b.owners[hash]; id > 0 {
		return id, nil
	}
	return 0, service.ErrStickySessionNotFound
}

func (b *oauthRecoveryHandlerBindings) SetSessionAccountID(_ context.Context, _ int64, hash string, id int64, _ time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.owners == nil {
		b.owners = make(map[string]int64)
	}
	b.owners[hash] = id
	return nil
}

func (*oauthRecoveryHandlerBindings) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}

func (b *oauthRecoveryHandlerBindings) DeleteSessionAccountID(_ context.Context, _ int64, hash string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.owners, hash)
	return nil
}

func addOAuthRecoveryHealthyBackup(guard *oauthRecoveryHandlerGuard) {
	guard.repo.mu.Lock()
	defer guard.repo.mu.Unlock()
	backup := guard.repo.account
	backup.ID, backup.Name, backup.Priority = 2, "healthy-backup", 10
	backup.Credentials = map[string]any{"access_token": "healthy-token", "refresh_token": "healthy-refresh"}
	guard.repo.alternatives = []service.Account{backup}
}

func TestOpenAIOAuthRecoveryHTTP_FailedRecoveryBorrowsHealthyBackup(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			ready := make(chan struct{})
			close(ready)
			bindings := &oauthRecoveryHandlerBindings{}
			upstream := &oauthRecoveryHandlerUpstream{}
			h, guard, _, usage := newOAuthRecoveryHandlerWithBindings(t, upstream, ready, bindings)
			guard.failed = true
			addOAuthRecoveryHealthyBackup(guard)
			c, rec := oauthRecoveryHandlerRequest(t, context.Background(), endpoint)
			c.Request.Header.Set("session_id", "borrow-without-rebinding")
			runOAuthRecoveryHandler(h, c, endpoint)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			ids, tokens, _ := upstream.calls()
			require.Equal(t, []int64{1, 2}, ids)
			require.Equal(t, []string{"Bearer old-token", "Bearer healthy-token"}, tokens)
			require.EqualValues(t, 1, usage.count.Load())
			owner, err := bindings.GetSessionAccountID(c.Request.Context(), guard.repo.account.GroupIDs[0], "openai:"+h.gatewayService.GenerateSessionHash(c, nil))
			require.NoError(t, err)
			require.EqualValues(t, 1, owner, "the backup must not replace the original session owner")
		})
	}
}

type oauthRecoverySlotCache interface {
	service.ConcurrencyCache
	service.APIKeySlotAdmissionCache
	service.APIKeySlotQueueCache
	service.APIKeyConcurrencyCache
}

type oauthRecoveryObservedSlots struct {
	oauthRecoverySlotCache
	admissions atomic.Int32
}

var _ service.APIKeySlotAdmissionCache = (*oauthRecoveryObservedSlots)(nil)
var _ service.APIKeySlotQueueCache = (*oauthRecoveryObservedSlots)(nil)

func (s *oauthRecoveryObservedSlots) AcquireUserSlot(ctx context.Context, id int64, max int, request string) (bool, error) {
	ok, err := s.oauthRecoverySlotCache.AcquireUserSlot(ctx, id, max, request)
	if ok {
		s.admissions.Add(1)
	}
	return ok, err
}

func TestOpenAIOAuthRecoveryHTTP_BoundSelectionParksWithoutSlots(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancel"} {
		for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
			t.Run(endpoint+"/"+outcome, func(t *testing.T) {
				ready := make(chan struct{})
				var readyOnce sync.Once
				t.Cleanup(func() { readyOnce.Do(func() { close(ready) }) })
				upstream := &oauthRecoveryHandlerUpstream{}
				h, guard, _, _ := newOAuthRecoveryHandlerWithBindings(t, upstream, ready, &oauthRecoveryHandlerBindings{})
				guard.failed = outcome == "failure"
				addOAuthRecoveryHealthyBackup(guard)
				cache := testutil.NewRedisConcurrencyCache(t)
				observed := &oauthRecoveryObservedSlots{oauthRecoverySlotCache: cache.(oauthRecoverySlotCache)}
				concurrency := service.NewConcurrencyService(observed)
				concurrency.SetAPIKeyQueuePolicy(service.APIKeyQueuePolicy{MaxWaiting: 4, Timeout: time.Second})
				h.concurrencyHelper = NewConcurrencyHelper(concurrency, SSEPingFormatNone, time.Second)
				newRequest := func(session string) (*gin.Context, *httptest.ResponseRecorder, context.CancelFunc) {
					ctx, cancel := service.WithAPIKeyAdmissionOwner(context.Background())
					t.Cleanup(cancel)
					c, rec := oauthRecoveryHandlerRequest(t, ctx, endpoint)
					c.Request.Header.Set("session_id", session)
					key, _ := middleware2.GetAPIKeyFromContext(c)
					key.ConcurrencyLimit = 1
					return c, rec, cancel
				}
				first, firstRec, _ := newRequest("bound-recovery")
				firstDone := make(chan struct{})
				go func() { runOAuthRecoveryHandler(h, first, endpoint); close(firstDone) }()
				select {
				case <-guard.started:
				case <-firstDone:
					t.Fatalf("request returned before recovery: status=%d body=%s", firstRec.Code, firstRec.Body.String())
				case <-time.After(3 * time.Second):
					t.Fatal("recovery did not start")
				}
				second, secondRec, cancelSecond := newRequest("bound-recovery")
				secondDone := make(chan struct{})
				go func() { runOAuthRecoveryHandler(h, second, endpoint); close(secondDone) }()
				require.Eventually(t, func() bool {
					active, waiting, err := observed.GetAPIKeyQueueStats(context.Background(), 99)
					users, userErr := cache.GetUserConcurrency(context.Background(), 100)
					return observed.admissions.Load() >= 2 && err == nil && userErr == nil && active == 0 && waiting == 0 && users == 0
				}, 2*time.Second, time.Millisecond, "scheduler interruption must release both user and key generation slots")
				ids, _, _ := upstream.calls()
				require.Equal(t, []int64{1}, ids, "a bound request must wait even when a healthy backup exists")
				// A new, unbound session may use that healthy account immediately.
				fresh, freshRec, _ := newRequest("new-session")
				runOAuthRecoveryHandler(h, fresh, endpoint)
				require.Equal(t, http.StatusOK, freshRec.Code, freshRec.Body.String())
				if outcome == "cancel" {
					cancelSecond()
					select {
					case <-secondDone:
					case <-time.After(time.Second):
						t.Fatal("canceled bound request kept waiting")
					}
					require.Empty(t, secondRec.Body.String())
				}
				readyOnce.Do(func() { close(ready) })
				for _, done := range []<-chan struct{}{firstDone, secondDone} {
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Fatal("request did not resume after recovery")
					}
				}
				require.Equal(t, http.StatusOK, firstRec.Code, firstRec.Body.String())
				ids, _, _ = upstream.calls()
				if outcome == "cancel" {
					require.Equal(t, []int64{1, 2, 1}, ids, "canceled request must never send a replay")
				} else {
					require.Equal(t, http.StatusOK, secondRec.Code, secondRec.Body.String())
					want := int64(1)
					if outcome == "failure" {
						want = 2
					}
					require.Equal(t, []int64{1, 2, want, want}, ids)
				}
				require.EqualValues(t, 1, guard.requests.Load(), "bound waiters must share the original recovery round")
			})
		}
	}
}

func TestOpenAIOAuthRecoveryHTTP_BackupErrorKeepsItsOwnResponse(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	upstream := &oauthRecoveryHandlerUpstream{responseForCall: func(_ int, id int64) *http.Response {
		if id != 2 {
			return nil
		}
		return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"healthy backup encountered another upstream failure"}}`))}
	}}
	h, guard, _, _ := newOAuthRecoveryHandler(t, upstream, ready)
	guard.failed = true
	addOAuthRecoveryHealthyBackup(guard)
	c, rec := oauthRecoveryHandlerRequest(t, context.Background(), "responses")
	h.Responses(c)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "upstream_auth_recovery")
	ids, _, _ := upstream.calls()
	require.Equal(t, []int64{1, 2}, ids)
}

func TestOpenAIOAuthRecoveryHTTP_EncryptedOwnerNeverBorrowsBackup(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	upstream := &oauthRecoveryHandlerUpstream{}
	h, guard, _, _ := newOAuthRecoveryHandlerWithBindings(t, upstream, ready, &oauthRecoveryHandlerBindings{})
	guard.failed = true
	addOAuthRecoveryHealthyBackup(guard)
	second, secondRec := oauthRecoveryHandlerRequest(t, context.Background(), "responses")
	key, _ := middleware2.GetAPIKeyFromContext(second)
	key.AllowLongContext = true
	body := `{"model":"gpt-5.1","input":[{"type":"reasoning","encrypted_content":"opaque-owner-state"}]}`
	second.Request.Body = io.NopCloser(strings.NewReader(body))
	second.Request.ContentLength = int64(len(body))
	h.Responses(second)
	require.Equal(t, http.StatusServiceUnavailable, secondRec.Code, secondRec.Body.String())
	require.Contains(t, secondRec.Body.String(), "upstream_auth_recovery_failed")
	ids, _, _ := upstream.calls()
	require.Equal(t, []int64{1}, ids, "an owner-bound response must never replay on the healthy backup")
}

func TestOpenAIOAuthRecoveryWS_FailedRecoveryBorrowsHealthyBackupForCurrentTurn(t *testing.T) {
	for _, laterTurn := range []bool{false, true} {
		name := "first_turn"
		if laterTurn {
			name = "later_turn"
		}
		t.Run(name, func(t *testing.T) {
			ready := make(chan struct{})
			close(ready)
			failCall := 1
			if laterTurn {
				failCall = 2
			}
			upstream := &oauthRecoveryHandlerUpstream{failCalls: map[int]bool{failCall: true}}
			bindings := &oauthRecoveryHandlerBindings{}
			h, guard, _, usage := newOAuthRecoveryHandlerWithBindings(t, upstream, ready, bindings)
			guard.failed = true
			h.cfg.Gateway.OpenAIWS.Enabled = true
			h.cfg.Gateway.OpenAIWS.OAuthEnabled = true
			h.cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			h.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			h.cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
			h.cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
			h.cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 3
			guard.repo.mutate(func(a *service.Account) {
				a.Extra = map[string]any{"openai_oauth_responses_websockets_v2_mode": service.OpenAIWSIngressModeHTTPBridge}
			})
			addOAuthRecoveryHealthyBackup(guard)
			router := gin.New()
			router.GET("/v1/responses", func(c *gin.Context) {
				fixture, _ := oauthRecoveryHandlerRequest(t, c.Request.Context(), "responses")
				key, _ := middleware2.GetAPIKeyFromContext(fixture)
				key.AllowLongContext = true
				c.Set(string(middleware2.ContextKeyAPIKey), key)
				c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 2})
				h.ResponsesWebSocket(c)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", &coderws.DialOptions{HTTPHeader: http.Header{"Session_id": {"ws-recovery-backup"}}})
			require.NoError(t, err)
			defer conn.CloseNow()
			complete := func() {
				for {
					_, frame, err := conn.Read(ctx)
					require.NoError(t, err, string(frame))
					require.NotEqual(t, "error", gjson.GetBytes(frame, "type").String(), string(frame))
					if gjson.GetBytes(frame, "type").String() == "response.completed" {
						return
					}
				}
			}
			require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"first portable turn"}`)))
			complete()
			if laterTurn {
				require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_recovered","input":"second portable turn"}`)))
				complete()
			}
			ids, _, bodies := upstream.calls()
			if laterTurn {
				require.Equal(t, []int64{1, 1, 2}, ids)
				require.Contains(t, string(bodies[2]), "second portable turn", "only the failed current turn is replayed")
				require.Empty(t, gjson.GetBytes(bodies[2], "previous_response_id").String(), "portable fallback must not carry the old owner's response ID")
			} else {
				require.Equal(t, []int64{1, 2}, ids)
			}
			bindings.mu.Lock()
			for _, owner := range bindings.owners {
				require.EqualValues(t, 1, owner)
			}
			bindings.mu.Unlock()
			require.EqualValues(t, 1, guard.requests.Load())
			wantUsage := int32(1)
			if laterTurn {
				wantUsage = 2
			}
			require.Eventually(t, func() bool { return usage.count.Load() == wantUsage }, time.Second, time.Millisecond)
		})
	}
}

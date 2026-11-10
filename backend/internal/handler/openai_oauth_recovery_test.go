//go:build unit

package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type oauthRecoveryHandlerRepo struct {
	service.AccountRepository
	mu               sync.Mutex
	account          service.Account
	alternatives     []service.Account
	denyAdmission    atomic.Bool
	admissionDenials atomic.Int32
}

func (r *oauthRecoveryHandlerRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.account
	for _, alternative := range r.alternatives {
		if alternative.ID == id {
			a = alternative
			break
		}
	}
	credentials := a.Credentials
	a.Credentials = make(map[string]any, len(credentials))
	for k, v := range credentials {
		a.Credentials[k] = v
	}
	return &a, nil
}

func (r *oauthRecoveryHandlerRepo) GetOpenAITurnAdmission(ctx context.Context, id int64) (*service.Account, *service.Account, error) {
	a, err := r.GetByID(ctx, id)
	if r.denyAdmission.Load() {
		r.admissionDenials.Add(1)
		a.Status = service.StatusDisabled
	}
	return a, nil, err
}

func (r *oauthRecoveryHandlerRepo) ListSchedulableByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	accounts, err := r.ListAllWithFilters(ctx, "", "", "", "", 0, "")
	var result []service.Account
	for _, account := range accounts {
		if account.Platform == platform && account.IsSchedulable() {
			result = append(result, account)
		}
	}
	return result, err
}

func (r *oauthRecoveryHandlerRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, _ int64, platform string) ([]service.Account, error) {
	return r.ListSchedulableByPlatform(ctx, platform)
}

func (r *oauthRecoveryHandlerRepo) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	return r.ListSchedulableByPlatform(ctx, platform)
}

func (r *oauthRecoveryHandlerRepo) ListAllWithFilters(ctx context.Context, _, _, _, _ string, _ int64, _ string) ([]service.Account, error) {
	a, err := r.GetByID(ctx, 0)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]service.Account{*a}, r.alternatives...), nil
}

func (r *oauthRecoveryHandlerRepo) SetError(_ context.Context, id int64, message string) error {
	r.mu.Lock()
	if id == r.account.ID {
		r.account.Status = service.StatusError
		r.account.ErrorMessage = message
	}
	r.mu.Unlock()
	return nil
}

func (r *oauthRecoveryHandlerRepo) mutate(fn func(*service.Account)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.account)
}

type oauthRecoveryHandlerGuard struct {
	repo      *oauthRecoveryHandlerRepo
	ready     <-chan struct{}
	started   chan struct{}
	startOnce sync.Once
	requests  atomic.Int32
	verified  atomic.Int32
	failed    bool
	onVerify  func()
	onRequest func()
}

func (g *oauthRecoveryHandlerGuard) RequestImmediateReauth(context.Context, *service.Account) (*service.OpenAIOAuthReauthTask, error) {
	g.requests.Add(1)
	g.repo.mutate(func(a *service.Account) {
		a.Status = service.StatusError
		a.ErrorMessage = service.ImmediateOAuthRecoveryErrorMessage
	})
	if g.onRequest != nil {
		g.onRequest()
	}
	g.startOnce.Do(func() { close(g.started) })
	return &service.OpenAIOAuthReauthTask{ID: 13, AccountID: 1, Status: "pending"}, nil
}

func (g *oauthRecoveryHandlerGuard) GetImmediateReauthTask(ctx context.Context, _ int64) (*service.OpenAIOAuthReauthTask, error) {
	if g.requests.Load() == 0 {
		return nil, nil
	}
	return g.GetImmediateReauthTaskByID(ctx, 13)
}

func (g *oauthRecoveryHandlerGuard) GetImmediateReauthTaskByID(context.Context, int64) (*service.OpenAIOAuthReauthTask, error) {
	status := "running"
	select {
	case <-g.ready:
		status = service.OpenAIOAuthReauthStatusSucceeded
		if g.failed {
			status = service.OpenAIOAuthReauthStatusFailed
		}
	default:
	}
	return &service.OpenAIOAuthReauthTask{ID: 13, AccountID: 1, Status: status}, nil
}

func (g *oauthRecoveryHandlerGuard) VerifyImmediateReauth(context.Context, int64) error {
	g.verified.Add(1)
	g.repo.mutate(func(a *service.Account) {
		a.Status = service.StatusActive
		a.ErrorMessage = ""
		a.Credentials["access_token"] = "recovered-token"
		if count := g.requests.Load(); count > 1 {
			a.Credentials["access_token"] = fmt.Sprintf("recovered-token-%d", count)
		}
	})
	if g.onVerify != nil {
		g.onVerify()
	}
	return nil
}

type oauthRecoveryHandlerUpstream struct {
	service.HTTPUpstream
	mu              sync.Mutex
	ids             []int64
	tokens          []string
	bodies          [][]byte
	always401       bool
	failCalls       map[int]bool
	onFirst         func()
	firstResponse   func() *http.Response
	responseForCall func(int, int64) *http.Response
}

func (u *oauthRecoveryHandlerUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	u.mu.Lock()
	u.ids = append(u.ids, accountID)
	u.tokens = append(u.tokens, req.Header.Get("Authorization"))
	u.bodies = append(u.bodies, append([]byte(nil), body...))
	call := len(u.ids)
	first := call == 1
	u.mu.Unlock()
	if first && u.onFirst != nil {
		u.onFirst()
	}
	if u.responseForCall != nil {
		if response := u.responseForCall(call, accountID); response != nil {
			return response, nil
		}
	}
	if first && u.firstResponse != nil {
		return u.firstResponse(), nil
	}
	fail := first || u.always401
	if u.failCalls != nil {
		fail = u.failCalls[call]
	}
	if fail {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"token_invalidated","message":"fixture-private-detail"}}`)),
		}, nil
	}
	wire := `event: response.completed` + "\ndata: " + `{"type":"response.completed","response":{"id":"resp_recovered","model":"gpt-5.1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recovered"}]}],"usage":{"input_tokens":3,"output_tokens":1}}}` + "\n\n"
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}, nil
}

func (u *oauthRecoveryHandlerUpstream) calls() ([]int64, []string, [][]byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.ids...), append([]string(nil), u.tokens...), append([][]byte(nil), u.bodies...)
}

type oauthRecoveryUsageRepo struct {
	service.UsageLogRepository
	count atomic.Int32
}

func (r *oauthRecoveryUsageRepo) Create(context.Context, *service.UsageLog) (bool, error) {
	r.count.Add(1)
	return true, nil
}

func newOAuthRecoveryHandler(t *testing.T, upstream service.HTTPUpstream, ready <-chan struct{}) (*OpenAIGatewayHandler, *oauthRecoveryHandlerGuard, *concurrencyCacheMock, *oauthRecoveryUsageRepo) {
	return newOAuthRecoveryHandlerWithBindings(t, upstream, ready, nil)
}

func newOAuthRecoveryHandlerWithBindings(t *testing.T, upstream service.HTTPUpstream, ready <-chan struct{}, bindings service.GatewayCache) (*OpenAIGatewayHandler, *oauthRecoveryHandlerGuard, *concurrencyCacheMock, *oauthRecoveryUsageRepo) {
	t.Helper()
	repo := &oauthRecoveryHandlerRepo{account: service.Account{
		ID: 1, Name: "oauth-recovery", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true, Concurrency: 2, GroupIDs: []int64{3131},
		Credentials: map[string]any{"access_token": "old-token", "refresh_token": "refresh-token"},
	}}
	guard := &oauthRecoveryHandlerGuard{repo: repo, ready: ready, started: make(chan struct{})}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	usage := &oauthRecoveryUsageRepo{}
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	rateLimit := service.NewRateLimitService(repo, nil, cfg, nil, nil)
	cache := &concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	}
	concurrency := service.NewConcurrencyService(cache)
	gateway := service.NewOpenAIGatewayService(repo, nil, usage, nil, nil, nil, nil, bindings, cfg, nil, concurrency, service.NewBillingService(cfg, nil), rateLimit, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	gateway.SetOpenAIOAuthRecovery(guard)
	t.Cleanup(gateway.CloseOpenAIWSPool)
	h := NewOpenAIGatewayHandler(gateway, concurrency, billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	return h, guard, cache, usage
}

func oauthRecoveryHandlerRequest(t *testing.T, ctx context.Context, endpoint string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, rec := newOpenAIResponsesFailoverTestContext(t, ctx)
	c.Request.URL.Path = "/v1/" + endpoint
	if endpoint != "responses" {
		body := `{"model":"gpt-5.1","stream":false,"messages":[{"role":"user","content":"preserve this input"}],"max_tokens":20}`
		c.Request.Body = io.NopCloser(strings.NewReader(body))
		c.Request.ContentLength = int64(len(body))
	}
	apiKey, _ := middleware2.GetAPIKeyFromContext(c)
	apiKey.Group.AllowMessagesDispatch = true
	apiKey.User.Status = service.StatusActive
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 2})
	return c, rec
}

func runOAuthRecoveryHandler(h *OpenAIGatewayHandler, c *gin.Context, endpoint string) {
	switch endpoint {
	case "responses":
		h.Responses(c)
	case "chat/completions":
		h.ChatCompletions(c)
	case "messages":
		h.Messages(c)
	}
}

func TestOpenAIOAuthRecoveryHTTP_RetrySameAccountWithFreshCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			ready := make(chan struct{})
			upstream := &oauthRecoveryHandlerUpstream{}
			h, guard, slots, usage := newOAuthRecoveryHandler(t, upstream, ready)
			guard.onVerify = func() {
				if atomic.LoadInt32(&slots.releaseAccountCalled) <= 0 || atomic.LoadInt32(&slots.releaseUserCalled) <= 0 {
					t.Error("account and user/key slots must be released before recovery resumes")
				}
			}
			c, rec := oauthRecoveryHandlerRequest(t, context.Background(), endpoint)
			done := make(chan struct{})
			go func() { runOAuthRecoveryHandler(h, c, endpoint); close(done) }()
			select {
			case <-guard.started:
			case <-time.After(3 * time.Second):
				t.Fatal("native 401 did not start recovery")
			}
			require.Eventually(t, func() bool { return atomic.LoadInt32(&slots.releaseAccountCalled) == 1 }, time.Second, time.Millisecond)
			close(ready)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("handler did not resume after recovery")
			}
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			ids, tokens, bodies := upstream.calls()
			require.Equal(t, []int64{1, 1}, ids)
			require.Equal(t, []string{"Bearer old-token", "Bearer recovered-token"}, tokens)
			require.JSONEq(t, string(bodies[0]), string(bodies[1]), "replay must preserve request body and conversation")
			require.EqualValues(t, 1, guard.requests.Load())
			require.EqualValues(t, 1, guard.verified.Load())
			require.EqualValues(t, 1, usage.count.Load(), "only the successful attempt is billed")
		})
	}
}

func TestOpenAIOAuthRecoveryHTTP_FailedAndRepeated401ReturnStable503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		for _, repeated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/repeated=%v", endpoint, repeated), func(t *testing.T) {
				ready := make(chan struct{})
				close(ready)
				upstream := &oauthRecoveryHandlerUpstream{always401: repeated}
				h, guard, _, usage := newOAuthRecoveryHandler(t, upstream, ready)
				guard.failed = !repeated
				c, rec := oauthRecoveryHandlerRequest(t, context.Background(), endpoint)
				runOAuthRecoveryHandler(h, c, endpoint)
				require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "upstream_auth_recovery_failed")
				require.NotContains(t, rec.Body.String(), "fixture-private-detail")
				ids, _, _ := upstream.calls()
				wantCalls := 1
				if repeated {
					wantCalls = 2
				}
				require.Len(t, ids, wantCalls)
				require.EqualValues(t, 1, guard.requests.Load(), "a repeated 401 cannot start an unbounded re-login loop")
				require.Zero(t, usage.count.Load())
			})
		}
	}
}

func TestOpenAIOAuthRecoveryHTTP_ClientCancellationDoesNotReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ready := make(chan struct{})
	upstream := &oauthRecoveryHandlerUpstream{}
	h, guard, _, usage := newOAuthRecoveryHandler(t, upstream, ready)
	ctx, cancel := context.WithCancel(context.Background())
	c, rec := oauthRecoveryHandlerRequest(t, ctx, "responses")
	done := make(chan struct{})
	go func() { h.Responses(c); close(done) }()
	select {
	case <-guard.started:
	case <-time.After(3 * time.Second):
		t.Fatal("recovery did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled request kept waiting")
	}
	ids, _, _ := upstream.calls()
	require.Equal(t, []int64{1}, ids)
	require.Equal(t, statusClientClosedRequest, c.Writer.Status())
	require.Empty(t, rec.Body.String())
	require.Zero(t, usage.count.Load())
	close(ready)
}

func TestOpenAIOAuthRecoveryHTTP_NewRequestsWaitBeforeUserAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ready := make(chan struct{})
	upstream := &oauthRecoveryHandlerUpstream{}
	h, guard, slots, usage := newOAuthRecoveryHandler(t, upstream, ready)
	var admitted atomic.Int32
	slots.acquireUserSlotFn = func(context.Context, int64, int, string) (bool, error) {
		admitted.Add(1)
		return true, nil
	}
	first, firstRec := oauthRecoveryHandlerRequest(t, context.Background(), "responses")
	firstDone := make(chan struct{})
	go func() { h.Responses(first); close(firstDone) }()
	select {
	case <-guard.started:
	case <-time.After(3 * time.Second):
		t.Fatal("recovery did not start")
	}
	second, secondRec := oauthRecoveryHandlerRequest(t, context.Background(), "responses")
	secondDone := make(chan struct{})
	go func() { h.Responses(second); close(secondDone) }()
	require.Never(t, func() bool { return admitted.Load() > 1 }, 100*time.Millisecond, time.Millisecond,
		"a new request must not consume a user/key slot while the only account recovers")
	close(ready)
	for _, done := range []<-chan struct{}{firstDone, secondDone} {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("request did not resume")
		}
	}
	require.Equal(t, http.StatusOK, firstRec.Code, firstRec.Body.String())
	require.Equal(t, http.StatusOK, secondRec.Code, secondRec.Body.String())
	require.EqualValues(t, 3, admitted.Load(), "the triggering request must re-enter user/key admission after recovery")
	require.EqualValues(t, 1, guard.requests.Load())
	require.EqualValues(t, 2, usage.count.Load())
}

func TestOpenAIOAuthRecoveryHTTP_KeyQueueTimeoutDoesNotShortenRecoveryWait(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ready := make(chan struct{})
	h, guard, _, _ := newOAuthRecoveryHandler(t, &oauthRecoveryHandlerUpstream{}, ready)
	cache := testutil.NewRedisConcurrencyCache(t)
	concurrency := service.NewConcurrencyService(cache)
	concurrency.SetAPIKeyQueuePolicy(service.APIKeyQueuePolicy{MaxWaiting: 4, Timeout: 400 * time.Millisecond})
	h.concurrencyHelper = NewConcurrencyHelper(concurrency, SSEPingFormatNone, time.Second)
	newRequest := func() (*gin.Context, *httptest.ResponseRecorder) {
		ctx, cancel := service.WithAPIKeyAdmissionOwner(context.Background())
		t.Cleanup(cancel)
		c, rec := oauthRecoveryHandlerRequest(t, ctx, "responses")
		key, _ := middleware2.GetAPIKeyFromContext(c)
		key.ConcurrencyLimit = 1
		return c, rec
	}
	first, firstRec := newRequest()
	firstDone := make(chan struct{})
	go func() { h.Responses(first); close(firstDone) }()
	select {
	case <-guard.started:
	case <-time.After(3 * time.Second):
		t.Fatal("recovery did not start")
	}
	second, secondRec := newRequest()
	secondDone := make(chan struct{})
	go func() { h.Responses(second); close(secondDone) }()
	require.Never(t, func() bool {
		select {
		case <-secondDone:
			return true
		default:
			return false
		}
	}, 800*time.Millisecond, time.Millisecond, "recovery waiting must outlive a shorter key-queue timeout")
	queueCache := cache.(service.APIKeySlotQueueCache)
	_, waiting, err := queueCache.GetAPIKeyQueueStats(context.Background(), 99)
	require.NoError(t, err)
	require.Zero(t, waiting, "the parked request must not enter the key queue yet")
	close(ready)
	for _, done := range []<-chan struct{}{firstDone, secondDone} {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("request did not finish after recovery")
		}
	}
	require.Equal(t, http.StatusOK, firstRec.Code, firstRec.Body.String())
	require.Equal(t, http.StatusOK, secondRec.Code, secondRec.Body.String())
}

func TestOpenAIOAuthRecoveryHTTP_AlreadyQueuedRequestsLeaveAdmissionDuringRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, queue := range []string{"user", "key"} {
		t.Run(queue, func(t *testing.T) {
			ready, upstreamEntered, allow401 := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var readyOnce, allowOnce sync.Once
			upstream := &oauthRecoveryHandlerUpstream{onFirst: func() { close(upstreamEntered); <-allow401 }}
			h, guard, _, usage := newOAuthRecoveryHandler(t, upstream, ready)
			t.Cleanup(func() { readyOnce.Do(func() { close(ready) }); allowOnce.Do(func() { close(allow401) }) })
			cache := testutil.NewRedisConcurrencyCache(t)
			concurrency := service.NewConcurrencyService(cache)
			concurrency.SetAPIKeyQueuePolicy(service.APIKeyQueuePolicy{MaxWaiting: 4, Timeout: 400 * time.Millisecond})
			h.concurrencyHelper = NewConcurrencyHelper(concurrency, SSEPingFormatNone, time.Second)
			newRequest := func() (*gin.Context, *httptest.ResponseRecorder) {
				ctx, cancel := service.WithAPIKeyAdmissionOwner(context.Background())
				t.Cleanup(cancel)
				c, rec := oauthRecoveryHandlerRequest(t, ctx, "responses")
				if queue == "key" {
					key, _ := middleware2.GetAPIKeyFromContext(c)
					key.ConcurrencyLimit = 1
				} else {
					c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 1})
				}
				return c, rec
			}
			first, firstRec := newRequest()
			firstDone := make(chan struct{})
			go func() { h.Responses(first); close(firstDone) }()
			select {
			case <-upstreamEntered:
			case <-time.After(3 * time.Second):
				t.Fatal("first request did not reach upstream")
			}
			second, secondRec := newRequest()
			secondDone := make(chan struct{})
			go func() { h.Responses(second); close(secondDone) }()
			queueCache := cache.(service.APIKeySlotQueueCache)
			require.Eventually(t, func() bool {
				if queue == "key" {
					_, waiting, err := queueCache.GetAPIKeyQueueStats(context.Background(), 99)
					return err == nil && waiting == 1
				}
				users, err := cache.GetUsersLoadBatch(context.Background(), []service.UserWithConcurrency{{ID: 100, MaxConcurrency: 1}})
				return err == nil && users[100] != nil && users[100].WaitingCount == 1
			}, time.Second, time.Millisecond, "request B must already be queued before A receives its 401")
			allowOnce.Do(func() { close(allow401) })
			select {
			case <-guard.started:
			case <-time.After(3 * time.Second):
				t.Fatal("401 did not start recovery")
			}
			require.Eventually(t, func() bool {
				users, err := cache.GetUsersLoadBatch(context.Background(), []service.UserWithConcurrency{{ID: 100, MaxConcurrency: 1}})
				if err != nil || users[100] == nil || users[100].WaitingCount != 0 || users[100].CurrentConcurrency != 0 {
					return false
				}
				active, waiting, err := queueCache.GetAPIKeyQueueStats(context.Background(), 99)
				return err == nil && active == 0 && waiting == 0
			}, time.Second, time.Millisecond, "both requests must release user/key slots and all old queue entries while recovering")
			require.Never(t, func() bool {
				select {
				case <-secondDone:
					return true
				default:
					return false
				}
			}, 800*time.Millisecond, time.Millisecond,
				"the recovery budget replaces the old, shorter queue timeout")
			readyOnce.Do(func() { close(ready) })
			for _, done := range []<-chan struct{}{firstDone, secondDone} {
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("request did not re-enter admission after recovery")
				}
			}
			require.Equal(t, 200, firstRec.Code, firstRec.Body.String())
			require.Equal(t, 200, secondRec.Code, secondRec.Body.String())
			require.EqualValues(t, 1, guard.requests.Load())
			require.EqualValues(t, 2, usage.count.Load())
		})
	}
}

func TestOpenAIOAuthRecoveryHTTP_DeadlineAndLatestAdmissionPreventReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Run("request deadline", func(t *testing.T) {
		ready := make(chan struct{})
		h, _, _, usage := newOAuthRecoveryHandler(t, &oauthRecoveryHandlerUpstream{}, ready)
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		c, rec := oauthRecoveryHandlerRequest(t, ctx, "responses")
		h.Responses(c)
		require.Equal(t, statusClientClosedRequest, c.Writer.Status())
		require.Empty(t, rec.Body.String())
		require.Zero(t, usage.count.Load())
		close(ready)
	})
	t.Run("latest admission", func(t *testing.T) {
		ready := make(chan struct{})
		close(ready)
		upstream := &oauthRecoveryHandlerUpstream{}
		h, guard, _, usage := newOAuthRecoveryHandler(t, upstream, ready)
		guard.onVerify = func() { guard.repo.denyAdmission.Store(true) }
		c, rec := oauthRecoveryHandlerRequest(t, context.Background(), "responses")
		h.Responses(c)
		ids, _, _ := upstream.calls()
		require.Equal(t, []int64{1}, ids, "recovery must not bypass latest turn admission")
		require.Positive(t, guard.repo.admissionDenials.Load())
		require.NotEqual(t, http.StatusOK, rec.Code)
		require.Zero(t, usage.count.Load())
	})
}

func TestOpenAIOAuthRecoveryHTTP_SemanticOutputIsNeverReplayed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			ready := make(chan struct{})
			close(ready)
			upstream := &oauthRecoveryHandlerUpstream{firstResponse: func() *http.Response {
				stream := "data: " + `{"type":"response.created","response":{"id":"resp_partial","model":"gpt-5.1","status":"in_progress"}}` + "\n\n" +
					"data: " + `{"type":"response.output_text.delta","delta":"already-delivered","output_index":0,"content_index":0}` + "\n\n" +
					"data: " + `{"type":"error","error":{"type":"authentication_error","code":"token_invalidated","message":"fixture expired"}}` + "\n\n"
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
			}}
			h, _, _, _ := newOAuthRecoveryHandler(t, upstream, ready)
			c, rec := oauthRecoveryHandlerRequest(t, context.Background(), endpoint)
			body, err := io.ReadAll(c.Request.Body)
			require.NoError(t, err)
			body = []byte(strings.Replace(string(body), `"stream":false`, `"stream":true`, 1))
			c.Request.Body = io.NopCloser(strings.NewReader(string(body)))
			c.Request.ContentLength = int64(len(body))
			runOAuthRecoveryHandler(h, c, endpoint)
			ids, _, _ := upstream.calls()
			require.Equal(t, []int64{1}, ids)
			require.Contains(t, rec.Body.String(), "already-delivered", "the test must really deliver semantic output before the 401")
		})
	}
}

type oauthRecoveryGatewayStub struct {
	called int
}

func (g *oauthRecoveryGatewayStub) RecoverOpenAIOAuth401(context.Context, *service.Account, []byte) (bool, error) {
	g.called++
	return true, nil
}

func (*oauthRecoveryGatewayStub) WaitForOpenAIOAuthRecovery(context.Context, *int64, string) (bool, error) {
	return false, nil
}

func TestOpenAIOAuthRecovery_DoesNotReplaySemanticOrUnrelatedFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		account *service.Account
		result  *service.OpenAIForwardResult
		failure *service.UpstreamFailoverError
		safe    bool
	}{
		{name: "semantic output", account: &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}, failure: &service.UpstreamFailoverError{StatusCode: 401}, safe: false},
		{name: "observed usage", account: &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}, result: &service.OpenAIForwardResult{ObservedUsageOnError: true}, failure: &service.UpstreamFailoverError{StatusCode: 401}, safe: true},
		{name: "api key", account: &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}, failure: &service.UpstreamFailoverError{StatusCode: 401}, safe: true},
		{name: "other platform", account: &service.Account{Platform: service.PlatformGrok, Type: service.AccountTypeOAuth}, failure: &service.UpstreamFailoverError{StatusCode: 401}, safe: true},
		{name: "other status", account: &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}, failure: &service.UpstreamFailoverError{StatusCode: 403}, safe: true},
		{name: "local credentials", account: &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}, failure: &service.UpstreamFailoverError{StatusCode: 401, Stage: service.GatewayFailureStageAccountAuth}, safe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &oauthRecoveryGatewayStub{}
			handled, err := recoverOpenAIOAuth401(context.Background(), stub, tc.account, tc.result, tc.failure, tc.safe)
			require.NoError(t, err)
			require.False(t, handled)
			require.Zero(t, stub.called)
		})
	}
}

func TestOpenAIOAuthRecovery_HTTPErrorFormatsAndTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, anthropic := range []bool{false, true} {
		for _, err := range []error{service.ErrOAuthRecoveryTimeout, service.ErrOAuthRecoveryQueueFull, fmt.Errorf("wrapped: %w", service.ErrOAuthRecoveryFailed)} {
			c, rec := oauthRecoveryHandlerRequest(t, context.Background(), "responses")
			h := &OpenAIGatewayHandler{}
			h.handleOpenAIOAuthRecoveryError(c, err, false, anthropic)
			require.Equal(t, 503, rec.Code)
			code, _ := openAIOAuthRecoveryErrorDetails(err)
			require.Contains(t, rec.Body.String(), code)
			require.NotContains(t, rec.Body.String(), "wrapped")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, rec := oauthRecoveryHandlerRequest(t, ctx, "responses")
	(&OpenAIGatewayHandler{}).handleOpenAIOAuthRecoveryError(c, errors.Join(service.ErrOAuthRecoveryFailed, context.Canceled), false, false)
	require.Equal(t, statusClientClosedRequest, c.Writer.Status())
	require.Empty(t, rec.Body.String())
}

func TestOpenAIOAuthRecovery_WSStable503Frame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, recoveryErr := range []error{service.ErrOAuthRecoveryFailed, service.ErrOAuthRecoveryTimeout, service.ErrOAuthRecoveryQueueFull} {
		t.Run(recoveryErr.Error(), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = r
				closeOpenAIOAuthWSRecoveryError(r.Context(), c, conn, recoveryErr)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer conn.CloseNow()
			_, payload, err := conn.Read(ctx)
			require.NoError(t, err)
			code, _ := openAIOAuthRecoveryErrorDetails(recoveryErr)
			require.Equal(t, code, gjson.GetBytes(payload, "error.code").String())
			require.EqualValues(t, 503, gjson.GetBytes(payload, "status").Int())
			_, _, err = conn.Read(ctx)
			require.Equal(t, coderws.StatusTryAgainLater, coderws.CloseStatus(err))
		})
	}
}

func TestOpenAIOAuthRecoveryWS_HTTPBridgeRetriesCurrentTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name      string
		failCalls map[int]bool
		turns     int
		logins    int
	}{
		{name: "first_turn", failCalls: map[int]bool{1: true}, turns: 1, logins: 1},
		{name: "later_turn", failCalls: map[int]bool{2: true}, turns: 2, logins: 1},
		{name: "fresh_budget_per_turn", failCalls: map[int]bool{1: true, 3: true}, turns: 2, logins: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ready := make(chan struct{})
			close(ready)
			upstream := &oauthRecoveryHandlerUpstream{failCalls: tc.failCalls}
			h, guard, _, usage := newOAuthRecoveryHandler(t, upstream, ready)
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
			router := gin.New()
			router.GET("/v1/responses", func(c *gin.Context) {
				fixture, _ := oauthRecoveryHandlerRequest(t, c.Request.Context(), "responses")
				key, _ := middleware2.GetAPIKeyFromContext(fixture)
				// Later turns use previous_response_id to exercise OAuth recovery.
				key.AllowLongContext = true
				c.Set(string(middleware2.ContextKeyAPIKey), key)
				c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 2})
				h.ResponsesWebSocket(c)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
			require.NoError(t, err)
			defer conn.CloseNow()
			readCompleted := func() {
				for {
					_, payload, readErr := conn.Read(ctx)
					require.NoError(t, readErr, string(payload))
					require.NotEqual(t, "error", gjson.GetBytes(payload, "type").String(), string(payload))
					if gjson.GetBytes(payload, "type").String() == "response.completed" {
						return
					}
				}
			}
			require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"first-turn"}`)))
			readCompleted()
			if tc.turns > 1 {
				require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"second-turn","previous_response_id":"resp_recovered"}`)))
				readCompleted()
			}
			ids, tokens, bodies := upstream.calls()
			require.Len(t, ids, tc.turns+tc.logins)
			for _, id := range ids {
				require.EqualValues(t, 1, id)
			}
			wantToken := "Bearer recovered-token"
			if tc.logins > 1 {
				wantToken = fmt.Sprintf("Bearer recovered-token-%d", tc.logins)
			}
			require.Equal(t, wantToken, tokens[len(tokens)-1])
			if tc.turns > 1 {
				require.Contains(t, string(bodies[len(bodies)-1]), "second-turn", "resume the failed turn rather than replay the initial frame")
			} else {
				require.JSONEq(t, string(bodies[0]), string(bodies[1]))
			}
			require.EqualValues(t, tc.logins, guard.requests.Load())
			require.Eventually(t, func() bool { return usage.count.Load() == int32(tc.turns) }, time.Second, time.Millisecond)
		})
	}
}

var _ service.OpenAIOAuthRecoveryService = (*oauthRecoveryHandlerGuard)(nil)

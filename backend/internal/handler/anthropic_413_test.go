//go:build unit

package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const anthropic413ClientMessage = "Request body is too large. Anthropic Messages requests are limited to 32 MiB. Reduce the request size and try again."

type anthropic413HTTPUpstream struct {
	client   *http.Client
	target   *url.URL
	mu       sync.Mutex
	accounts []int64
}

func (upstream *anthropic413HTTPUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	upstream.mu.Lock()
	upstream.accounts = append(upstream.accounts, accountID)
	upstream.mu.Unlock()
	local := req.Clone(req.Context())
	local.URL.Scheme = upstream.target.Scheme
	local.URL.Host = upstream.target.Host
	local.Host = upstream.target.Host
	return upstream.client.Do(local)
}

func (upstream *anthropic413HTTPUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return upstream.Do(req, proxy, accountID, concurrency)
}

type anthropic413AccountRepo struct {
	service.AccountRepository
	mu        sync.Mutex
	mutations []string
}

func (repo *anthropic413AccountRepo) record(kind string) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	repo.mutations = append(repo.mutations, kind)
	return nil
}

func (repo *anthropic413AccountRepo) SetError(context.Context, int64, string) error {
	return repo.record("error")
}

func (repo *anthropic413AccountRepo) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	return repo.record("temp_unschedulable")
}

func (repo *anthropic413AccountRepo) SetModelRateLimit(context.Context, int64, string, time.Time, ...string) error {
	return repo.record("model_rate_limit")
}

type anthropic413PassthroughRepo struct {
	service.ErrorPassthroughRepository
	rules []*model.ErrorPassthroughRule
}

func (repo *anthropic413PassthroughRepo) List(context.Context) ([]*model.ErrorPassthroughRule, error) {
	return repo.rules, nil
}

func anthropic413PassthroughRule(status int, message string) *model.ErrorPassthroughRule {
	return &model.ErrorPassthroughRule{
		ID: 1, Name: "synthetic-413-rule", Enabled: true, MatchMode: model.MatchModeAny,
		ErrorCodes: []int{http.StatusRequestEntityTooLarge}, Platforms: []string{service.PlatformAnthropic},
		ResponseCode: &status, CustomMessage: &message, SkipMonitoring: true,
	}
}

func TestAnthropic413MessagesHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, variant := range []struct {
		name        string
		accountType string
		passthrough bool
		omit413     bool
		cooldown    bool
		override    string
	}{
		{name: "apikey", accountType: service.AccountTypeAPIKey},
		{name: "passthrough", accountType: service.AccountTypeAPIKey, passthrough: true},
		{name: "apikey_custom_codes", accountType: service.AccountTypeAPIKey, omit413: true},
		{name: "passthrough_custom_codes", accountType: service.AccountTypeAPIKey, passthrough: true, omit413: true},
		{name: "oauth", accountType: service.AccountTypeOAuth},
		{name: "setup_token", accountType: service.AccountTypeSetupToken},
		{name: "apikey_cooldown_rule", accountType: service.AccountTypeAPIKey, cooldown: true},
		{name: "oauth_cooldown_rule", accountType: service.AccountTypeOAuth, cooldown: true},
		{name: "apikey_custom_override", accountType: service.AccountTypeAPIKey, override: "custom"},
		{name: "passthrough_body_override", accountType: service.AccountTypeAPIKey, passthrough: true, override: "body"},
	} {
		t.Run(variant.name, func(t *testing.T) {
			for _, boundary := range []string{"nonstream", "stream", "headers", "ping", "partial"} {
				t.Run(boundary, func(t *testing.T) {
					stream := boundary != "nonstream"
					committed := boundary == "headers" || boundary == "ping" || boundary == "partial"
					provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
						body, err := io.ReadAll(req.Body)
						if err != nil {
							t.Error(err)
							return
						}
						if len(body) > 256*1024 || !strings.Contains(string(body), "synthetic-413-input") || req.URL.Path != "/v1/messages" {
							t.Errorf("unexpected synthetic request: path=%s bytes=%d", req.URL.Path, len(body))
						}
						writer.Header().Set("Content-Type", "text/html")
						writer.Header().Set("X-Request-Id", "fixture-must-not-leak")
						writer.Header().Set("Retry-After", "999")
						writer.WriteHeader(http.StatusRequestEntityTooLarge)
						_, _ = io.WriteString(writer, "<html>Request body exceeds the 32 MiB limit for Messages requests; fixture-must-not-leak</html>")
					}))
					t.Cleanup(provider.Close)
					target, err := url.Parse(provider.URL)
					require.NoError(t, err)
					upstream := &anthropic413HTTPUpstream{client: provider.Client(), target: target}
					cfg := &config.Config{RunMode: config.RunModeSimple}
					groupID := int64(4130)
					group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
					var accounts []*service.Account
					for _, accountID := range []int64{4131, 4132} {
						account := &service.Account{
							ID: accountID, Name: "synthetic-413", Platform: service.PlatformAnthropic, Type: variant.accountType,
							Status: service.StatusActive, Schedulable: true, Concurrency: 1,
							Credentials:   map[string]any{"api_key": "fixture-key", "access_token": "fixture-token"},
							Extra:         map[string]any{"anthropic_passthrough": variant.passthrough},
							AccountGroups: []service.AccountGroup{{AccountID: accountID, GroupID: groupID}},
						}
						if variant.omit413 {
							account.Credentials["custom_error_codes_enabled"] = true
							account.Credentials["custom_error_codes"] = []any{float64(401), float64(429), float64(500)}
						}
						if variant.cooldown {
							account.Credentials["temp_unschedulable_enabled"] = true
							account.Credentials["temp_unschedulable_rules"] = []any{map[string]any{
								"error_code": float64(413), "keywords": []any{"32 MiB"}, "duration_minutes": float64(10),
							}}
						}
						accounts = append(accounts, account)
					}
					snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: accounts}, nil, nil, nil, nil)
					repo := &anthropic413AccountRepo{}
					rateLimits := service.NewRateLimitService(repo, nil, cfg, nil, nil)
					gateway := service.NewGatewayService(
						nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, cfg,
						snapshot, nil, nil, rateLimits, nil, nil, upstream,
						nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
					)
					billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
					t.Cleanup(billing.Stop)
					handler := &GatewayHandler{
						gatewayService: gateway, billingCacheService: billing, cfg: cfg, maxAccountSwitches: 1,
						concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0),
					}
					if variant.override != "" {
						rule := anthropic413PassthroughRule(http.StatusOK, "fixture-override-must-not-leak")
						rule.PassthroughBody = variant.override == "body"
						handler.errorPassthroughService = service.NewErrorPassthroughService(&anthropic413PassthroughRepo{rules: []*model.ErrorPassthroughRule{rule}}, nil)
					}
					recorder := httptest.NewRecorder()
					body := fmt.Sprintf(`{"model":"claude-sonnet-4-5","max_tokens":16,"stream":%t,"messages":[{"role":"user","content":"synthetic-413-input"}]}`, stream)
					req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
					apiKey := &service.APIKey{ID: 4133, UserID: 4134, GroupID: &groupID, Group: group, Status: service.StatusActive,
						User: &service.User{ID: 4134, Balance: 100, Concurrency: 10}}
					prefix := ""
					setupOpsErrorLogTestQueue(t, 1)
					ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
					router := gin.New()
					router.Use(OpsErrorLoggerMiddleware(ops))
					var requestContext *gin.Context
					router.POST("/v1/messages", func(handlerContext *gin.Context) {
						requestContext = handlerContext
						handlerContext.Set(string(middleware.ContextKeyAPIKey), apiKey)
						handlerContext.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
						if committed {
							handlerContext.Header("Content-Type", "text/event-stream")
							switch boundary {
							case "ping":
								prefix = "event: ping\ndata: {\"type\":\"ping\"}\n\n"
							case "partial":
								prefix = partialMessageStartSSE
							}
							if prefix != "" {
								_, writeErr := io.WriteString(handlerContext.Writer, prefix)
								require.NoError(t, writeErr)
							}
							handlerContext.Writer.Flush()
						}
						handler.Messages(handlerContext)
					})
					router.ServeHTTP(recorder, req)
					if !committed {
						require.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code)
						require.Contains(t, recorder.Result().Header.Get("Content-Type"), "application/json")
						require.JSONEq(t, fmt.Sprintf(`{"type":"error","error":{"type":"request_too_large","message":%q}}`, anthropic413ClientMessage), recorder.Body.String())
					} else {
						require.Equal(t, http.StatusOK, recorder.Code)
						require.Equal(t, "text/event-stream", recorder.Result().Header.Get("Content-Type"))
						require.True(t, strings.HasPrefix(recorder.Body.String(), prefix))
						terminal := strings.TrimPrefix(recorder.Body.String(), prefix)
						var data []string
						for _, line := range strings.Split(terminal, "\n") {
							if strings.HasPrefix(line, "data: ") {
								data = append(data, strings.TrimPrefix(line, "data: "))
							} else {
								require.True(t, line == "" || line == "event: error", "unexpected terminal SSE line: %s", line)
							}
						}
						require.Len(t, data, 1)
						require.JSONEq(t, fmt.Sprintf(`{"type":"error","error":{"type":"request_too_large","message":%q}}`, anthropic413ClientMessage), data[0])
					}
					require.NotContains(t, recorder.Body.String(), "fixture-must-not-leak")
					require.Empty(t, recorder.Result().Header.Get("X-Request-Id"))
					require.Empty(t, recorder.Result().Header.Get("Retry-After"))
					require.Equal(t, int64(1), OpsErrorLogQueueLength())
					entry := (<-opsErrorLogQueue).entry
					require.Equal(t, http.StatusRequestEntityTooLarge, entry.StatusCode)
					require.Equal(t, "request_too_large", entry.ErrorType)
					require.Equal(t, "request", entry.ErrorPhase)
					require.Equal(t, "client", entry.ErrorOwner)
					require.Equal(t, "client_request", entry.ErrorSource)
					require.Equal(t, "P3", entry.Severity)
					require.True(t, entry.IsBusinessLimited)
					require.Nil(t, entry.AccountID)
					require.Nil(t, entry.UpstreamStatusCode)
					require.Empty(t, entry.UpstreamErrors)
					require.NotContains(t, entry.ErrorMessage, "must-not-leak")
					phase, businessLimited, owner, _ := classifyOpsErrorLog(requestContext, "request_too_large", anthropic413ClientMessage, "", http.StatusRequestEntityTooLarge)
					require.Equal(t, "request", phase)
					require.Equal(t, "client", owner)
					require.True(t, businessLimited)
					upstream.mu.Lock()
					defer upstream.mu.Unlock()
					require.Len(t, upstream.accounts, 1, "413 must not replay the payload or fail over")
					repo.mu.Lock()
					defer repo.mu.Unlock()
					require.Empty(t, repo.mutations, "request size rejection must not change account health")
				})
			}
		})
	}
}

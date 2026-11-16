//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type accountSessionAffinityHTTPUpstream struct {
	service.HTTPUpstream
	t        *testing.T
	affinity service.AccountSessionAffinity
	called   bool
	native   bool
}

func (u *accountSessionAffinityHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.called = true
	var ok bool
	u.affinity, ok = service.AccountSessionAffinityFromContext(req.Context())
	require.True(u.t, ok, "the real HTTP handler must carry its affinity through account selection and upstream construction")
	require.False(u.t, u.affinity.PersistentRoute)
	wire := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_affinity\",\"model\":\"gpt-5.4\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}}\n\n"
	if u.native {
		wire = compatStreamUsagePartialStream + compatStreamUsageCompletion
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}, nil
}

func (u *accountSessionAffinityHTTPUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func TestOpenAIHTTPHandlersCarryAccountSessionAffinity(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		t.Run(endpoint, func(t *testing.T) {
			upstream := &accountSessionAffinityHTTPUpstream{t: t}
			h := newStickyMigrationHTTPHandler(t, &stickyMigrationHTTPBindings{}, upstream)
			c, rec := newExcelBPSFailoverTestContext(context.Background(), true)
			body := `{"model":"gpt-5.4","stream":true,"input":"hello"}`
			if endpoint != "responses" {
				body = `{"model":"gpt-5.4","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`
			}
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, strings.NewReader(body))
			c.Request.Header.Set("session-id", "session-affinity-handler-fixture")
			keyValue, _ := c.Get(string(middleware2.ContextKeyAPIKey))
			keyValue.(*service.APIKey).Group.AllowMessagesDispatch = true
			switch endpoint {
			case "responses":
				h.Responses(c)
			case "chat/completions":
				h.ChatCompletions(c)
			default:
				h.Messages(c)
			}
			require.True(t, upstream.called, rec.Body.String())
			affinity, ok := service.AccountSessionAffinityFromContext(c.Request.Context())
			require.True(t, ok)
			require.Equal(t, affinity, upstream.affinity)
		})
	}
}

func TestGatewayHTTPCompatibilityHandlersCarryAccountSessionAffinity(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions"} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := &config.Config{Gateway: config.GatewayConfig{StreamDataIntervalTimeout: 1}}
			groupID := int64(9300)
			group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive, RateMultiplier: 1}
			account := &service.Account{ID: 9301, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
				Status: service.StatusActive, Schedulable: true, Concurrency: 1,
				Credentials: map[string]any{"api_key": "fixture-key"}, AccountGroups: []service.AccountGroup{{AccountID: 9301, GroupID: groupID}}}
			snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{account}}, nil, nil, nil, nil)
			billing := service.NewBillingCacheService(newHandlerInflightCache(100), nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billing.Stop)
			upstream := &accountSessionAffinityHTTPUpstream{t: t, native: true}
			usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 2)}
			gateway := service.NewGatewayService(nil, &fakeGroupRepo{group: group}, usage, &compatStreamUsageBillingRepo{}, nil, nil, nil, nil, cfg,
				snapshot, nil, service.NewBillingService(cfg, nil), nil, nil, nil, upstream,
				&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			h := &GatewayHandler{gatewayService: gateway, billingCacheService: billing, cfg: cfg, maxAccountSwitches: 1,
				concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0)}
			body := `{"model":"claude-sonnet-4.5","stream":true,"input":"hello"}`
			if endpoint == "chat/completions" {
				body = `{"model":"claude-sonnet-4.5","stream":true,"messages":[{"role":"user","content":"hello"}]}`
			}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, strings.NewReader(body))
			c.Request.Header.Set("session-id", "native-compat-conversation")
			key := &service.APIKey{ID: 9302, UserID: 9303, GroupID: &groupID, Group: group, User: &service.User{ID: 9303, Balance: 100}}
			c.Set(string(middleware2.ContextKeyAPIKey), key)
			c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 9303})
			if endpoint == "responses" {
				h.Responses(c)
			} else {
				h.ChatCompletions(c)
			}
			require.True(t, upstream.called, rec.Body.String())
			affinity, ok := service.AccountSessionAffinityFromContext(c.Request.Context())
			require.True(t, ok)
			require.Equal(t, affinity, upstream.affinity)
		})
	}
}

func TestOpenAIWebSocketTurnsCarryCurrentAccountSessionAffinity(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeCtxPool} {
		for _, tc := range []struct {
			name, second string
			wantSame     bool
		}{
			{name: "thread changes", second: `{"type":"response.create","model":"gpt-5.1","input":"next","client_metadata":{"thread_id":"second-thread"}}`},
			{name: "thread inherited", second: `{"type":"response.create","model":"gpt-5.1","input":"next"}`, wantSame: true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				observed := make(chan service.AccountSessionAffinity, 8)
				runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
					ingressMode:   mode,
					firstPayload:  `{"type":"response.create","model":"gpt-5.1","input":"hello","client_metadata":{"thread_id":"first-thread"}}`,
					secondPayload: tc.second,
					observeAccountSlot: func(ctx context.Context) {
						affinity, ok := service.AccountSessionAffinityFromContext(ctx)
						require.True(t, ok, "every real turn admission must retain explicit session affinity")
						require.True(t, affinity.PersistentRoute)
						observed <- affinity
					},
				})
				require.GreaterOrEqual(t, len(observed), 2)
				first := <-observed
				last := first
				for len(observed) > 0 {
					last = <-observed
				}
				if tc.wantSame {
					require.Equal(t, first.Key, last.Key)
				} else {
					require.NotEqual(t, first.Key, last.Key, "a later frame must not retain the previous thread's capacity identity")
				}
			})
		}
	}
}

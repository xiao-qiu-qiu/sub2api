//go:build unit

package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type stickyMigrationHTTPBindings struct {
	testutil.StubGatewayCache
	mu            sync.Mutex
	owner         int64
	revision      string
	sessionKey    string
	casCalls      int
	sets, deletes int
	onCAS         func()
}

func (b *stickyMigrationHTTPBindings) GetSessionAccountID(_ context.Context, _ int64, key string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.owner == 0 || key != "" && key != b.sessionKey {
		return 0, service.ErrStickySessionNotFound
	}
	return b.owner, nil
}

func (b *stickyMigrationHTTPBindings) SetSessionAccountID(_ context.Context, _ int64, key string, owner int64, _ time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if key != b.sessionKey {
		return nil
	}
	b.owner, b.revision = owner, b.revision+"s"
	b.sets++
	return nil
}

func (b *stickyMigrationHTTPBindings) DeleteSessionAccountID(_ context.Context, _ int64, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if key != b.sessionKey {
		return nil
	}
	b.owner, b.revision = 0, b.revision+"d"
	b.deletes++
	return nil
}

func (*stickyMigrationHTTPBindings) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}

func (b *stickyMigrationHTTPBindings) ReadSessionAccountBinding(context.Context, int64, string, string) (int64, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.owner == 0 {
		return 0, "", service.ErrStickySessionNotFound
	}
	return b.owner, b.revision, nil
}

func (b *stickyMigrationHTTPBindings) CompareAndSwapSessionAccountID(_ context.Context, _ int64, _, _ string, expected int64, revision string, owner int64, _, _ time.Duration) (bool, error) {
	if b.onCAS != nil {
		b.onCAS()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.casCalls++
	if b.owner != expected || b.revision != revision {
		return false, nil
	}
	b.owner, b.revision = owner, b.revision+"c"
	return true, nil
}

type stickyMigrationHTTPUpstream struct {
	service.HTTPUpstream
	t        *testing.T
	bindings *stickyMigrationHTTPBindings
	wire     string
	accounts []int64
}

func (u *stickyMigrationHTTPUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	owner, err := u.bindings.GetSessionAccountID(context.Background(), 0, "")
	require.NoError(u.t, err)
	require.EqualValues(u.t, 1, owner, "selection and admission must preserve the original binding")
	u.accounts = append(u.accounts, accountID)
	if accountID == 1 {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"temporary upstream failure"}}`))}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(u.wire))}, nil
}

type stickyMigrationHTTPWriter struct {
	gin.ResponseWriter
	t          *testing.T
	bindings   *stickyMigrationHTTPBindings
	disconnect bool
	completed  bool
}

func (w *stickyMigrationHTTPWriter) observe(p []byte) error {
	owner, err := w.bindings.GetSessionAccountID(context.Background(), 0, "")
	require.NoError(w.t, err)
	require.EqualValues(w.t, 1, owner, "the binding must not move before the downstream terminal is delivered")
	if w.disconnect {
		return errors.New("downstream write failed")
	}
	if bytes.Contains(p, []byte("response.completed")) || bytes.Contains(p, []byte("[DONE]")) || bytes.Contains(p, []byte("message_stop")) {
		w.completed = true
	}
	return nil
}

func (w *stickyMigrationHTTPWriter) Write(p []byte) (int, error) {
	if err := w.observe(p); err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(p)
}

func (w *stickyMigrationHTTPWriter) WriteString(p string) (int, error) {
	if err := w.observe([]byte(p)); err != nil {
		return 0, err
	}
	return w.ResponseWriter.WriteString(p)
}

func newStickyMigrationHTTPHandler(t *testing.T, bindings *stickyMigrationHTTPBindings, upstream service.HTTPUpstream) *OpenAIGatewayHandler {
	t.Helper()
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	settings := service.NewSettingService(&contentModerationHandlerSettingRepo{}, cfg)
	priority := service.DefaultPrioritySchedulingConfig()
	priority.Enabled, priority.StickyMigrationOnSuccess = true, true
	require.NoError(t, settings.SavePrioritySchedulingConfig(context.Background(), priority))
	var accounts []service.Account
	for _, id := range []int64{1, 2} {
		accounts = append(accounts, service.Account{ID: id, Name: fmt.Sprintf("sticky-account-%d", id), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{3131}, Priority: int(id - 1),
			Credentials: map[string]any{"api_key": "sk-fixture", "base_url": "http://upstream.example"}, Extra: map[string]any{"openai_responses_supported": true}})
	}
	repo := &excelBPSFailoverStateAccountRepo{excelBPSFailoverAccountRepo: excelBPSFailoverAccountRepo{openAIImagesFailoverAccountRepo{accounts: accounts}}}
	concurrency := service.NewConcurrencyService(nil)
	gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, bindings, cfg, nil, nil,
		service.NewBillingService(cfg, nil), service.NewRateLimitService(repo, nil, cfg, nil, nil), nil, upstream,
		nil, nil, nil, nil, nil, nil, settings, nil)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	return NewOpenAIGatewayHandler(gateway, concurrency, billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
}

func TestOpenAIStickyMigration_HTTPHandlerCommitsOnlySuccessfulCompletion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		for _, tc := range []struct {
			name, terminal, status string
			disconnect, wantMove   bool
		}{
			{name: "success", terminal: "response.completed", status: "completed", wantMove: true},
			{name: "partial failure", terminal: "response.failed", status: "failed"},
			{name: "incomplete", terminal: "response.incomplete", status: "incomplete"},
			{name: "cancelled", terminal: "response.cancelled", status: "cancelled"},
			{name: "write disconnect", terminal: "response.completed", status: "completed", disconnect: true},
		} {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				bindings := &stickyMigrationHTTPBindings{owner: 1, revision: "initial"}
				wire := `data: {"type":"response.output_text.delta","delta":"ok"}` + "\n\n" + fmt.Sprintf(`data: {"type":%q,"response":{"id":"resp_sticky","model":"gpt-5.4","status":%q,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":7,"output_tokens":2},"error":null}}`, tc.terminal, tc.status) + "\n\n"
				upstream := &stickyMigrationHTTPUpstream{t: t, bindings: bindings, wire: wire}
				h := newStickyMigrationHTTPHandler(t, bindings, upstream)
				c, rec := newExcelBPSFailoverTestContext(context.Background(), true)
				body := `{"model":"gpt-5.4","stream":true,"input":"hello"}`
				if endpoint != "responses" {
					body = `{"model":"gpt-5.4","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`
				}
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, strings.NewReader(body))
				c.Request.Header.Set("session_id", "sticky-migration-fixture")
				bindings.sessionKey = "openai:" + h.gatewayService.GenerateSessionHash(c, []byte(body))
				keyValue, _ := c.Get(string(middleware2.ContextKeyAPIKey))
				keyValue.(*service.APIKey).Group.AllowMessagesDispatch = true
				writer := &stickyMigrationHTTPWriter{ResponseWriter: c.Writer, t: t, bindings: bindings, disconnect: tc.disconnect}
				c.Writer = writer
				bindings.onCAS = func() { require.True(t, writer.completed, "CAS requires a delivered downstream terminal") }
				switch endpoint {
				case "responses":
					h.Responses(c)
				case "chat/completions":
					h.ChatCompletions(c)
				default:
					h.Messages(c)
				}
				require.Equal(t, []int64{1, 2}, upstream.accounts, rec.Body.String())
				owner, err := bindings.GetSessionAccountID(context.Background(), 0, "")
				require.NoError(t, err)
				wantOwner := int64(1)
				if tc.wantMove {
					wantOwner = 2
				}
				require.Equal(t, wantOwner, owner)
				require.Zero(t, bindings.sets, "migration must not fall back to an eager SET")
				require.Zero(t, bindings.deletes, "the previous owner must remain until successful CAS")
				if tc.wantMove {
					require.Equal(t, 1, bindings.casCalls)
				} else {
					require.Zero(t, bindings.casCalls)
				}
			})
		}
	}
}

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

type excelBPSAuthRepo struct {
	excelBPSQuotaRepo
	errorCalls, tempCalls int
	accountID             int64
	reason                string
	until                 time.Time
	ctxErr                error
	deadline              time.Time
	writeErr              error
}

func (r *excelBPSAuthRepo) record(ctx context.Context, id int64, reason string) error {
	r.accountID, r.reason, r.ctxErr = id, reason, ctx.Err()
	r.deadline, _ = ctx.Deadline()
	return r.writeErr
}

func (r *excelBPSAuthRepo) SetError(ctx context.Context, id int64, reason string) error {
	r.errorCalls++
	return r.record(ctx, id, reason)
}

func (r *excelBPSAuthRepo) SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error {
	r.tempCalls++
	r.until = until
	return r.record(ctx, id, reason)
}

type excelBPSAuthInvalidator struct{ ids []int64 }

func (r *excelBPSAuthInvalidator) InvalidateToken(_ context.Context, account *Account) error {
	r.ids = append(r.ids, account.ID)
	return nil
}

func setupExcelBPSAuth(svc *OpenAIGatewayService) (*excelBPSAuthRepo, *excelBPSAuthInvalidator) {
	repo := &excelBPSAuthRepo{excelBPSQuotaRepo: excelBPSQuotaRepo{writes: make(chan excelBPSQuotaWrite, 4)}}
	invalidator := &excelBPSAuthInvalidator{}
	svc.accountRepo = repo
	svc.cfg.RateLimit.OAuth401CooldownMinutes = 7
	svc.rateLimitService = NewRateLimitService(repo, nil, svc.cfg, nil, nil)
	svc.rateLimitService.SetTokenCacheInvalidator(invalidator)
	svc.rateLimitService.SetAccountRuntimeBlocker(svc)
	return repo, invalidator
}

func TestExcelBPSAccessStateIsolation(t *testing.T) {
	const rejected = `{"error":{"code":"deactivated_workspace","message":"PRIVATE_REQUEST test-token refresh-secret"}}`
	for _, transport := range []string{"http", "ws"} {
		for _, tc := range []struct {
			name, event, raw, route                            string
			observe, writeFailure, canceled, keepalive, output bool
			wantBlocked                                        bool
		}{
			{name: "initial rejection", raw: rejected, wantBlocked: true},
			{name: "write failure", raw: rejected, writeFailure: true, wantBlocked: true},
			{name: "canceled client", raw: rejected, canceled: true, wantBlocked: true},
			{name: "compact keepalive", raw: rejected, keepalive: true, wantBlocked: true},
			{name: "observation", raw: rejected, observe: true},
			{name: "ordinary payment required", raw: `{"error":{"message":"Payment Required"}}`},
			{name: "echoed deactivation", raw: `{"error":{"code":"invalid_value","message":"deactivated_workspace"},"input":{"code":"deactivated_workspace"}}`},
			{name: "failed terminal", event: "response.failed", wantBlocked: true},
			{name: "error after output", event: "error", output: true, wantBlocked: true},
			{name: "observed terminal", event: "response.failed", observe: true},
			{name: "attachment rejection", raw: rejected, route: "attachment", wantBlocked: true},
			{name: "tool correction rejection", raw: rejected, route: "correction", wantBlocked: true},
			{name: "tool regeneration rejection", raw: rejected, route: "regeneration", wantBlocked: true},
		} {
			t.Run(transport+"/"+tc.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tc.observe {
					ctx = withPelicanTestOptions(ctx, pelicanTestOptions{observeOnly: true})
				}
				status, wire := http.StatusPaymentRequired, tc.raw
				if tc.event != "" {
					status, wire = http.StatusOK, bpsFailureWire(t, tc.event, "deactivated_workspace", "", 0)
					if tc.output {
						wire = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"already delivered\"}\n\n" + wire
					}
				}
				var responseBody io.Reader = strings.NewReader(wire)
				if tc.canceled {
					responseBody = &excelBPSCancelReader{Reader: strings.NewReader(wire), cancel: cancel}
				}
				rejectedBody := &excelBPSRepairBody{Reader: responseBody}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: http.Header{}, Body: rejectedBody}}
				svc := openAIClientToolsTestService(upstream)
				repo, invalidator := setupExcelBPSAuth(svc)
				if tc.writeFailure {
					repo.writeErr = errors.New("database unavailable")
				}
				account := excelAccount()
				account.Credentials["refresh_token"] = "refresh-secret"
				ws := &excelBPSWSTestUpstream{httpUpstreamRecorder: upstream}
				if transport == "ws" {
					account.Extra[ExcelBPSTransportKey] = "ws"
					svc.httpUpstream = ws
				}
				body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":"test"}`, tc.output))
				wantRequests := 1
				switch tc.route {
				case "attachment":
					enableNativeAttachments(svc)
					body, _ = nativeGatewayBody(t)
				case "correction", "regeneration":
					wantRequests = 2
					summary, code := "Run", "text(42);"
					body = []byte(`{"model":"gpt-6-astra","input":"test","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]}`)
					if tc.route == "regeneration" {
						summary, code = "Call client tool", `{"name":"unknown","arguments":{}}`
						body = []byte(`{"model":"gpt-6-astra","input":"test","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`)
					}
					upstream.responses = []*http.Response{
						bpsCompletionResponse(http.StatusOK, excelBPSRepairWire(t, "access-state", summary, code)),
						upstream.resp,
					}
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
				if tc.keepalive {
					MarkOpenAICompactClientStream(c)
					stop := StartOpenAICompactSSEKeepalive(c, time.Hour)
					defer stop()
					value, ok := c.Get(openAICompactSSEKeepaliveKey)
					require.True(t, ok)
					require.True(t, value.(*openAICompactSSEKeepalive).beat())
				}
				_, err := svc.Forward(ctx, c, account, body)
				require.Error(t, err)
				var failover *UpstreamFailoverError
				wantFailover := tc.wantBlocked && tc.event == "" && tc.route == "" && !tc.canceled && !tc.keepalive
				if wantFailover {
					require.ErrorAs(t, err, &failover)
					require.Equal(t, OpenAIUpstreamAccessStateReason, failover.Reason)
					require.False(t, failover.RetryableOnSameAccount)
					require.Equal(t, http.StatusBadGateway, failover.ClientStatusCode)
					require.False(t, c.Writer.Written())
					require.False(t, IsResponseCommitted(c))
					require.NotContains(t, string(failover.ResponseBody), "PRIVATE_REQUEST")
				} else {
					require.NotErrorAs(t, err, &failover)
				}
				if tc.canceled {
					require.ErrorIs(t, err, context.Canceled)
				}
				if tc.keepalive {
					require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed\n"))
				}
				if tc.output {
					require.Contains(t, rec.Body.String(), "already delivered")
				}
				require.Equal(t, tc.wantBlocked, svc.isOpenAIAccountRuntimeBlocked(account))
				require.Equal(t, tc.wantBlocked, repo.errorCalls == 1)
				if tc.wantBlocked {
					require.Contains(t, repo.reason, "deactivated_workspace")
					require.NoError(t, repo.ctxErr)
					require.True(t, repo.deadline.After(time.Now()))
				}
				require.Zero(t, repo.tempCalls)
				require.Empty(t, invalidator.ids, "workspace deactivation must not invalidate the independent Codex grant")
				require.True(t, account.IsSchedulable(), "shared scheduling snapshots must stay immutable")
				require.True(t, account.IsExcelBPSEnabled())
				require.Len(t, upstream.requests, wantRequests)
				require.True(t, rejectedBody.closed.Load(), "every rejection must release its upstream body and connection")
				if tc.route == "attachment" {
					require.Contains(t, rec.Body.String(), "basispoints_upstream_access_unavailable")
					require.NotContains(t, rec.Body.String(), "scheduling was not changed")
				}
				if transport == "ws" && tc.route != "attachment" {
					require.Equal(t, wantRequests, ws.wsCalls)
					require.Zero(t, ws.httpCalls)
				}
				for _, secret := range []string{"PRIVATE_REQUEST", "PRIVATE_UPSTREAM_BODY", "test-token", "refresh-secret"} {
					require.NotContains(t, repo.reason, secret)
					require.NotContains(t, rec.Body.String(), secret)
				}
			})
		}
	}
}

func TestExcelBPSAccessStateIsolatesOnlyManagedConnection(t *testing.T) {
	for _, scenario := range []string{"current revision", "stale revision", "observation"} {
		t.Run(scenario, func(t *testing.T) {
			reauth, connections := connectionTypeService(AccountTypeOAuth)
			connections.managed = true
			connections.rows = []*OpenAIAuthConnection{
				{ID: "c910c45b-79a0-4367-a1dc-60e12210e199", AccountID: 42, Kind: "bps", Workspace: "disabled-workspace", Status: "active", RefreshState: "idle", Revision: 2, Capabilities: []string{"responses"}},
				{ID: "c910c45b-79a0-4367-a1dc-60e12210e200", AccountID: 42, Kind: "codex", Workspace: "healthy-workspace", Status: "active", RefreshState: "idle", Revision: 3, Capabilities: []string{"responses"}},
			}
			svc := openAIClientToolsTestService(nil)
			repo, invalidator := setupExcelBPSAuth(svc)
			svc.excelOAuthReauth = reauth
			account := excelAccount()
			account.ID, account.AuthConnectionID, account.AuthConnectionRevision = 42, connections.rows[0].ID, 2
			ctx := context.Background()
			if scenario == "stale revision" {
				account.AuthConnectionRevision = 1
			}
			if scenario == "observation" {
				ctx = withPelicanTestOptions(ctx, pelicanTestOptions{observeOnly: true})
			}
			failure := svc.handleExcelBPSAccessState(ctx, account, http.StatusPaymentRequired, nil, []byte(`{"error":{"code":"deactivated_workspace","message":"PRIVATE_WORKSPACE"}}`))
			require.NotNil(t, failure)
			require.Equal(t, OpenAIUpstreamAccessStateReason, failure.Reason)
			if scenario == "current revision" {
				require.Equal(t, "disabled", connections.rows[0].Status)
				require.EqualValues(t, 3, connections.rows[0].Revision)
				require.Equal(t, 1, connections.writes)
				require.Error(t, reauth.ValidateAuthConnectionSend(ctx, account))
			} else {
				require.Equal(t, "active", connections.rows[0].Status)
				require.EqualValues(t, 2, connections.rows[0].Revision)
				require.Zero(t, connections.writes)
			}
			healthy := *account
			healthy.AuthConnectionID, healthy.AuthConnectionRevision = connections.rows[1].ID, 3
			require.NoError(t, reauth.ValidateAuthConnectionSend(ctx, &healthy))
			require.Equal(t, "active", connections.rows[1].Status)
			require.Zero(t, repo.errorCalls, "other workspaces under the parent must not be disabled")
			require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
			require.Empty(t, invalidator.ids)
		})
	}
}

func TestExcelBPS401PersistsAfterCancellationAndBlocksOnWriteFailure(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		for _, writeFailure := range []bool{false, true} {
			t.Run(fmt.Sprintf("permanent=%t/writeFailure=%t", permanent, writeFailure), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				raw := `{}`
				if permanent {
					raw = `{"error":{"code":"token_revoked"}}`
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(&excelBPSCancelReader{Reader: strings.NewReader(raw), cancel: cancel})}}
				svc := openAIClientToolsTestService(upstream)
				repo, _ := setupExcelBPSAuth(svc)
				if writeFailure {
					repo.writeErr = errors.New("database unavailable")
				}
				account := excelAccount()
				account.Credentials["refresh_token"] = "refresh-secret"
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
				_, err := svc.Forward(ctx, c, account, []byte(`{"model":"gpt-5.6-sol","input":"test"}`))
				require.Error(t, err)
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				require.NoError(t, repo.ctxErr)
				require.Equal(t, 1, repo.tempCalls+repo.errorCalls)
				require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
			})
		}
	}
}

func TestExcelBPS401RespectsAPIKeyPolicies(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pool, custom bool
		codes        []any
		blocked      bool
	}{
		{name: "API key", blocked: true},
		{name: "pool", pool: true},
		{name: "custom excludes 401", custom: true, codes: []any{float64(403)}},
		{name: "custom includes 401", custom: true, codes: []any{float64(401)}, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := openAIClientToolsTestService(nil)
			repo, invalidator := setupExcelBPSAuth(svc)
			account := excelAccount()
			account.Type = AccountTypeAPIKey
			account.Credentials["pool_mode"] = tc.pool
			account.Credentials["custom_error_codes_enabled"] = tc.custom
			account.Credentials["custom_error_codes"] = tc.codes
			svc.handleExcelBPSUnauthorized(context.Background(), account, 401, http.Header{}, []byte(`{}`))
			require.Equal(t, tc.blocked, svc.isOpenAIAccountRuntimeBlocked(account))
			require.Equal(t, tc.blocked, repo.errorCalls == 1)
			require.Zero(t, repo.tempCalls)
			require.Empty(t, invalidator.ids)
			requireNoExcelBPSQuotaWrite(t, &repo.excelBPSQuotaRepo)
		})
	}
}

func TestExcelBPS401AppliesAuthenticationPolicy(t *testing.T) {
	for _, route := range []string{"responses", "attachment", "tool correction", "unknown tool regeneration"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name, raw                        string
				noRefresh, permanent, invalidate bool
			}{
				{name: "expired", raw: `{"error":{"message":"echo test-token refresh-secret PRIVATE_REQUEST file-private"}}`, invalidate: true},
				{name: "invalidated", raw: `{"error":{"code":"token_invalidated","message":"PRIVATE_REQUEST"}}`, permanent: true},
				{name: "revoked", raw: `{"error":{"code":"token_revoked"}}`, permanent: true},
				{name: "unauthorized detail", raw: `{"detail":"Unauthorized"}`, permanent: true},
				{name: "missing refresh token", raw: `{}`, noRefresh: true, permanent: true, invalidate: true},
				{name: "non JSON", raw: `PRIVATE_REQUEST test-token`, invalidate: true},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", route, stream, tc.name), func(t *testing.T) {
					rejected := &excelBPSRepairBody{Reader: strings.NewReader(tc.raw)}
					upstream := &httpUpstreamRecorder{}
					svc := openAIClientToolsTestService(upstream)
					repo, invalidator := setupExcelBPSAuth(svc)
					account := excelAccount()
					if !tc.noRefresh {
						account.Credentials["refresh_token"] = "refresh-secret"
					}
					account.Extra["openai_excel_bps_auto_disable_on_403"] = true
					body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,"input":"test"}`, stream))
					wantRequests := 1
					var first *excelBPSRepairBody
					switch route {
					case "attachment":
						enableNativeAttachments(svc)
						body, _ = nativeGatewayBody(t)
						var err error
						body, err = sjson.SetBytes(body, "stream", stream)
						require.NoError(t, err)
					case "tool correction", "unknown tool regeneration":
						wantRequests = 2
						summary, code := "Run", "text(42);"
						body = []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,"input":"test","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]}`, stream))
						if route == "unknown tool regeneration" {
							summary, code = "Call client tool", `{"name":"unknown","arguments":{}}`
							body = []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,"input":"test","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`, stream))
						}
						first = &excelBPSRepairBody{Reader: strings.NewReader(excelBPSRepairWire(t, "auth", summary, code))}
						upstream.responses = append(upstream.responses, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: first})
					}
					upstream.responses = append(upstream.responses, &http.Response{StatusCode: http.StatusUnauthorized, Header: excelBPSQuotaHeaders("100", "100"), Body: rejected})
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					started := time.Now()
					_, err := svc.Forward(context.Background(), c, account, body)
					require.Error(t, err)
					var failover *UpstreamFailoverError
					require.NotErrorAs(t, err, &failover, "authentication state updates must not replay BPS requests")
					require.Len(t, upstream.requests, wantRequests)
					require.True(t, rejected.closed.Load())
					if first != nil {
						require.True(t, first.closed.Load())
					}
					require.True(t, IsResponseCommitted(c))
					if wantRequests == 1 {
						require.Equal(t, http.StatusUnauthorized, rec.Code)
						require.Contains(t, rec.Body.String(), "authentication failed")
					}
					require.NotContains(t, rec.Body.String(), "scheduling was not changed")
					require.Equal(t, account.ID, repo.accountID)
					require.NoError(t, repo.ctxErr)
					require.False(t, repo.deadline.IsZero())
					require.LessOrEqual(t, repo.deadline.Sub(started), openAIAccountStateUpdateTimeout+time.Second)
					if tc.permanent {
						require.Equal(t, 1, repo.errorCalls)
						require.Zero(t, repo.tempCalls)
					} else {
						require.Equal(t, 1, repo.tempCalls)
						require.Zero(t, repo.errorCalls)
						require.WithinDuration(t, started.Add(7*time.Minute), repo.until, 5*time.Second)
					}
					if tc.invalidate {
						require.Equal(t, []int64{account.ID}, invalidator.ids)
					} else {
						require.Empty(t, invalidator.ids)
					}
					require.True(t, svc.isOpenAIAccountRuntimeBlocked(account), "stale scheduler snapshots must be excluded immediately")
					require.True(t, account.IsSchedulable(), "do not mutate the shared account snapshot")
					require.True(t, account.IsExcelBPSEnabled())
					requireNoExcelBPSQuotaWrite(t, &repo.excelBPSQuotaRepo)
					for _, secret := range []string{"test-token", "refresh-secret", "PRIVATE_REQUEST", "file-private"} {
						require.NotContains(t, repo.reason, secret)
						require.NotContains(t, rec.Body.String(), secret)
					}
				})
			}
		}
	}
}

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
)

type excelBPSExhaustionRepo struct {
	*excelBPSQuotaRepo
	current   *Account
	getErr    error
	casReject bool
	reads     chan excelBPSQuotaWrite
}

func (r *excelBPSExhaustionRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	deadline, _ := ctx.Deadline()
	r.reads <- excelBPSQuotaWrite{accountID: id, ctxErr: ctx.Err(), deadline: deadline}
	return r.current, r.getErr
}

func (r *excelBPSExhaustionRepo) SetRateLimitedIfUnchanged(ctx context.Context, id int64, updated time.Time, limited, reset *time.Time, until time.Time) (bool, error) {
	if r.casReject || r.limitErr != nil {
		return false, r.limitErr
	}
	if r.current == nil || !updated.Equal(r.current.UpdatedAt) || limited != r.current.RateLimitedAt || reset != r.current.RateLimitResetAt {
		return false, nil
	}
	return true, r.SetRateLimited(ctx, id, until)
}

func newExcelBPSExhaustionRepo(account *Account) (*excelBPSExhaustionRepo, time.Time) {
	current := *account
	current.Extra = shallowCopyMap(account.Extra)
	current.Credentials = shallowCopyMap(account.Credentials)
	current.UpdatedAt = time.Now().Add(-time.Minute)
	reset := time.Now().Add(6 * 24 * time.Hour).UTC().Truncate(time.Second)
	current.Extra["codex_7d_used_percent"] = 100.0
	current.Extra["codex_7d_reset_at"] = reset.Format(time.RFC3339)
	return &excelBPSExhaustionRepo{excelBPSQuotaRepo: &excelBPSQuotaRepo{writes: make(chan excelBPSQuotaWrite, 8)}, current: &current, reads: make(chan excelBPSQuotaWrite, 8)}, reset
}

func requireExcelBPSExhaustionWrite(t *testing.T, svc *OpenAIGatewayService, repo *excelBPSExhaustionRepo, account *Account, reset time.Time) {
	t.Helper()
	write := nextExcelBPSQuotaWrite(t, repo.excelBPSQuotaRepo)
	require.Equal(t, account.ID, write.accountID)
	require.Equal(t, reset, write.resetAt)
	require.Empty(t, write.updates, "do not persist BPS 429 headers as quota")
	require.NoError(t, write.ctxErr)
	require.True(t, write.deadline.After(time.Now()))
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, ok)
	require.Equal(t, reset, value)
	requireNoExcelBPSQuotaWrite(t, repo.excelBPSQuotaRepo)
}

func TestExcelBPSQuotaExhaustionUsesLatestTrustedSnapshot(t *testing.T) {
	for _, transport := range []string{"http", "ws"} {
		for _, event := range []string{"http429", "error", "response.failed"} {
			for _, stream := range []bool{false, true} {
				for _, identifier := range []string{"usage_limit_reached", "insufficient_quota", "type_only"} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/%s", transport, event, stream, identifier), func(t *testing.T) {
						account := excelAccount()
						if transport == "ws" {
							account.Extra[ExcelBPSTransportKey] = "ws"
						}
						code, errorType := identifier, ""
						if identifier == "type_only" {
							code, errorType = "", "usage_limit_reached"
						}
						status := http.StatusOK
						var wire string
						if event == "http429" {
							status = http.StatusTooManyRequests
							wire = fmt.Sprintf(`{"error":{"code":%q,"type":%q}}`, code, errorType)
						} else {
							wire = bpsFailureWire(t, event, code, errorType, 0)
						}
						upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: excelBPSQuotaHeaders("0", "0"), Body: io.NopCloser(strings.NewReader(wire))}}
						svc := openAIClientToolsTestService(upstream)
						ws := &excelBPSWSTestUpstream{httpUpstreamRecorder: upstream}
						if transport == "ws" {
							svc.httpUpstream = ws
						}
						repo, reset := newExcelBPSExhaustionRepo(account)
						svc.accountRepo = repo
						c, _ := gin.CreateTestContext(httptest.NewRecorder())
						c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
						result, err := svc.Forward(context.Background(), c, account, []byte(fmt.Sprintf(`{"model":"gpt-6-astra","input":"test","stream":%t}`, stream)))
						require.Error(t, err)
						if event == "http429" {
							requireExcelBPSRateLimitFailover(t, err, c)
						} else {
							var failover *UpstreamFailoverError
							require.NotErrorAs(t, err, &failover, "never replay an accepted generation")
							require.NotNil(t, result)
						}
						require.Len(t, upstream.requests, 1)
						if transport == "ws" {
							require.Equal(t, 1, ws.wsCalls)
							require.Zero(t, ws.httpCalls)
						}
						requireExcelBPSExhaustionWrite(t, svc, repo, account, reset)
						require.NotContains(t, account.Extra, "codex_7d_used_percent", "do not mutate the shared scheduling snapshot")
					})
				}
			}
		}
	}
}

func TestExcelBPSQuotaExhaustionDoesNotPromoteUntrustedEvidence(t *testing.T) {
	for _, scenario := range []string{"missing", "not_exhausted", "expired", "read_error", "identity_changed", "managed", "cas_changed", "persist_error"} {
		t.Run(scenario, func(t *testing.T) {
			account := excelAccount()
			repo, _ := newExcelBPSExhaustionRepo(account)
			switch scenario {
			case "missing":
				repo.current.Extra = map[string]any{}
			case "not_exhausted":
				repo.current.Extra["codex_7d_used_percent"] = 99.0
			case "expired":
				repo.current.Extra["codex_7d_reset_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
			case "read_error":
				repo.getErr = errors.New("synthetic read failure")
			case "identity_changed":
				repo.current.Credentials["access_token"] = "replacement-token"
			case "managed":
				account.Extra["openai_connections_managed"] = true
			case "cas_changed":
				repo.casReject = true
			case "persist_error":
				repo.limitErr = errors.New("synthetic write failure")
			}
			svc := &OpenAIGatewayService{accountRepo: repo}
			svc.handleExcelBPSRateLimit(context.Background(), account, "", []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":604800}}`))
			requireNoExcelBPSQuotaWrite(t, repo.excelBPSQuotaRepo)
			require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
			if scenario != "cas_changed" && scenario != "persist_error" {
				require.InDelta(t, 5, excelBPSCooldownRemaining(t, svc, account.ID).Seconds(), 1)
			}
		})
	}
}

func TestExcelBPSQuotaExhaustionTPMStaysShort(t *testing.T) {
	account := excelAccount()
	repo, _ := newExcelBPSExhaustionRepo(account)
	svc := &OpenAIGatewayService{accountRepo: repo}
	svc.handleExcelBPSRateLimit(context.Background(), account, "", []byte(`{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error"}}`))
	require.Empty(t, repo.reads, "TPM errors do not request quota state")
	requireNoExcelBPSQuotaWrite(t, repo.excelBPSQuotaRepo)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.InDelta(t, 5, excelBPSCooldownRemaining(t, svc, account.ID).Seconds(), 1)
}

func TestExcelBPSQuotaExhaustionPersistsAfterClientCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	account := excelAccount()
	repo, reset := newExcelBPSExhaustionRepo(account)
	reader := &excelBPSCancelReader{Reader: strings.NewReader(`{"error":{"code":"usage_limit_reached"}}`), cancel: cancel}
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(reader)}}
	svc := openAIClientToolsTestService(upstream)
	svc.accountRepo = repo
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	_, err := svc.Forward(ctx, c, account, []byte(`{"model":"gpt-6-astra","input":"test"}`))
	require.ErrorIs(t, err, context.Canceled)
	read := <-repo.reads
	require.NoError(t, read.ctxErr)
	require.True(t, read.deadline.After(time.Now()))
	requireExcelBPSExhaustionWrite(t, svc, repo, account, reset)
}

func TestExcelBPSQuotaExhaustionQualityObservationDoesNotChangeState(t *testing.T) {
	for _, status := range []int{200, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			account := excelAccount()
			repo, _ := newExcelBPSExhaustionRepo(account)
			wire := `{"error":{"code":"usage_limit_reached"}}`
			if status == 200 {
				wire = bpsFailureWire(t, "response.failed", "usage_limit_reached", "", 0)
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: excelBPSQuotaHeaders("100", "100"), Body: io.NopCloser(strings.NewReader(wire))}}
			svc := openAIClientToolsTestService(upstream)
			svc.accountRepo = repo
			ctx := withPelicanTestOptions(context.Background(), pelicanTestOptions{observeOnly: true})
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			_, err := svc.Forward(ctx, c, account, []byte(`{"model":"gpt-6-astra","input":"test"}`))
			require.Error(t, err)
			require.Empty(t, repo.reads)
			requireNoExcelBPSQuotaWrite(t, repo.excelBPSQuotaRepo)
			require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
			require.False(t, svc.isExcelBPSCoolingDown(account, "gpt-6-astra"))
		})
	}
}

func TestExcelBPSQuotaExhaustionImageAndRepairPaths(t *testing.T) {
	for _, route := range []string{"images", "tool_correction", "unknown_tool_regeneration", "stream_correction"} {
		t.Run(route, func(t *testing.T) {
			account := excelAccount()
			var responses []*http.Response
			if route != "images" {
				summary, code := "Run", "text(42);"
				if route == "unknown_tool_regeneration" {
					summary, code = "Call client tool", `{"name":"unknown","arguments":{}}`
				}
				responses = append(responses, bpsCompletionResponse(200, excelBPSRepairWire(t, "quota_repair", summary, code)))
			} else {
				account = excelBPSImagesAccount()
			}
			status, raw := 429, `{"error":{"type":"usage_limit_reached"}}`
			if route == "stream_correction" {
				status, raw = 200, bpsFailureWire(t, "response.failed", "", "usage_limit_reached", 0)
			}
			responses = append(responses, &http.Response{StatusCode: status, Header: excelBPSQuotaHeaders("0", "0"), Body: io.NopCloser(strings.NewReader(raw))})
			upstream := &httpUpstreamRecorder{responses: responses}
			svc := openAIClientToolsTestService(upstream)
			repo, reset := newExcelBPSExhaustionRepo(account)
			svc.accountRepo = repo
			if route == "images" {
				body := []byte(`{"model":"gpt-image-2","prompt":"draw"}`)
				c, _ := newOpenAIImagesTestContext(t, body)
				parsed, err := svc.ParseOpenAIImagesRequest(c, body)
				require.NoError(t, err)
				_, err = svc.ForwardImages(context.Background(), c, account, body, parsed, "")
				requireExcelBPSRateLimitFailover(t, err, c)
				require.Len(t, upstream.requests, 1)
			} else {
				body := []byte(`{"model":"gpt-5.6-sol","input":"test","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]}`)
				if route == "unknown_tool_regeneration" {
					body = []byte(`{"model":"gpt-5.6-sol","input":"test","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`)
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				_, err := svc.Forward(context.Background(), c, account, body)
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.NotErrorAs(t, err, &failover, "repair errors never replay accepted generation")
				require.Len(t, upstream.requests, 2)
			}
			requireExcelBPSExhaustionWrite(t, svc, repo, account, reset)
		})
	}
}

type excelBPSOrderedSnapshotRepo struct {
	*issue578Repo
	attempts chan map[string]any
}

func (r *excelBPSOrderedSnapshotRepo) UpdateCodexUsageSnapshot(ctx context.Context, id int64, updates map[string]any) (bool, error) {
	written, err := r.issue578Repo.UpdateCodexUsageSnapshot(ctx, id, updates)
	r.attempts <- updates
	return written, err
}

func TestExcelBPSQuotaHeaderKeepsOriginalObservationTime(t *testing.T) {
	account := issue578Account()
	repo := &excelBPSOrderedSnapshotRepo{issue578Repo: &issue578Repo{accounts: map[int64]*Account{account.ID: account}}, attempts: make(chan map[string]any, 2)}
	svc := &OpenAIGatewayService{accountRepo: repo, codexSnapshotThrottle: newAccountWriteThrottle(time.Millisecond)}
	t.Cleanup(svc.codexSnapshotThrottle.close)
	resp := bpsCompletionResponse(200, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
	resp.Header = excelBPSQuotaHeaders("0", "0")
	svc.observeExcelBPSQuotaResponse(context.Background(), account, resp)
	// Model a long-running body without depending on Windows clock tick
	// resolution or sleeping for a real stream duration.
	oldObservation := time.Now().Add(-time.Minute)
	resp.Body.(*excelBPSQuotaResponseBody).observedAt = oldObservation
	newObservation := time.Now()
	require.True(t, newObservation.After(oldObservation))
	written, err := repo.issue578Repo.UpdateCodexUsageSnapshot(context.Background(), account.ID, map[string]any{"codex_7d_used_percent": 100.0, "codex_usage_updated_at": newObservation.Format(time.RFC3339Nano)})
	require.NoError(t, err)
	require.True(t, written)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	select {
	case attempted := <-repo.attempts:
		require.Equal(t, oldObservation.UnixNano(), codexUpdatesTime(attempted).UnixNano(), "the terminal must not restamp early headers")
		versions := CodexUsageFieldVersions(attempted, codexUpdatesTime(attempted))
		require.Equal(t, fmt.Sprint(oldObservation.UnixNano()), versions["codex_7d_used_percent"])
	case <-time.After(3 * time.Second):
		t.Fatal("expected the delayed header write")
	}
	current, err := repo.GetByID(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, 100.0, current.Extra["codex_7d_used_percent"], "older 0%% headers must not overwrite the newer WHAM 100%% snapshot")
}

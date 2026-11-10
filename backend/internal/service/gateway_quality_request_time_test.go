//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestQualityRequestStartIncludesQueueAndNestedScope(t *testing.T) {
	now := time.Now()
	start := now.Add(-2 * time.Minute)
	ctx := context.WithValue(t.Context(), ctxkey.RequestStartTime, start)
	account := &Account{ID: 9962801}
	ctx, finish := beginAccountGatewayRequest(ctx, account)
	defer finish()
	nested, release := beginAccountGatewayRequest(context.WithoutCancel(ctx), account)
	defer release()
	require.True(t, accountrequest.StartedAt(nested).Equal(start))
	require.Zero(t, accountrequest.Default.CancelWindow(account.ID, now.Add(-time.Minute), now))
	require.NoError(t, nested.Err(), "queue wait must not make an old request new")
	require.Positive(t, accountrequest.Default.CancelWindow(account.ID, start.Add(-time.Second), start))
	require.True(t, accountrequest.QualityDegraded(nested))
}

func TestQualityInterruptedZeroUsageBypassesAllTariffs(t *testing.T) {
	for _, mode := range []BillingMode{BillingModeToken, BillingModePerRequest, BillingModeImage} {
		t.Run(string(mode), func(t *testing.T) {
			repo := &openAIRecordUsageLogRepoStub{inserted: true}
			users, subs := &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}
			bills := &openAIRecordUsageBillingRepoStub{}
			quota := &openAIRecordUsageAPIKeyQuotaStub{}
			svc := newOpenAIRecordUsageServiceForTest(repo, users, subs, nil)
			svc.usageBillingRepo = bills
			resolver := newOpenAIImageChannelPricingResolverForTest(t, 35, "gpt-6-astra", 9)
			resolver.channelService.cache.Load().(*channelCache).pricingByGroupModel[channelModelKey{groupID: 35, model: "gpt-6-astra"}].BillingMode = mode
			svc.resolver = resolver
			start := time.Now().Add(-15 * time.Second)
			result := &OpenAIForwardResult{Model: "gpt-6-astra", QualityInterrupted: true, RequestStartedAt: start, Stream: true, ImageEndpoint: mode == BillingModeImage, HeaderTimeoutRetries: 3, AudioUsage: &AudioUsage{Mode: "realtime"}}
			// Leave UsageUnavailable at its default false, matching partial BPS/WS results.
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), ctxkey.RequestStartTime, start))
			cancel()
			err := svc.RecordUsage(ctx, &OpenAIRecordUsageInput{Result: result, APIKey: &APIKey{ID: 1, GroupID: i64p(35)}, User: &User{ID: 2}, Account: &Account{ID: 3}, APIKeyService: quota})
			require.NoError(t, err)
			require.Equal(t, 1, repo.calls)
			require.True(t, repo.lastLog.QualityInterrupted)
			require.True(t, repo.lastLog.RequestStartedAt.Equal(start))
			require.Zero(t, repo.lastLog.TotalTokens())
			require.Zero(t, repo.lastLog.TotalCost)
			require.Zero(t, repo.lastLog.ActualCost)
			require.Zero(t, bills.calls)
			require.Zero(t, users.deductCalls)
			require.Zero(t, subs.incrementCalls)
			require.Zero(t, quota.quotaCalls)
			require.Zero(t, quota.rateLimitCalls)
			require.NoError(t, repo.lastCtxErr)
		})
	}
}

func TestQualityRequestUsagePreservesStartAndObservedTokens(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprint(interrupted), func(t *testing.T) {
			start := time.Now().Add(-time.Minute)
			ctx := context.WithValue(t.Context(), ctxkey.RequestStartTime, start)
			for _, generic := range []bool{false, true} {
				repo := &openAIRecordUsageLogRepoStub{inserted: true}
				users, subs := &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}
				key := &APIKey{ID: 1}
				user, account := &User{ID: 2}, &Account{ID: 3, Type: AccountTypeAPIKey}
				if generic {
					svc := newGatewayRecordUsageServiceForTest(repo, users, subs)
					require.NoError(t, svc.RecordUsage(ctx, &RecordUsageInput{APIKey: key, User: user, Account: account, Result: &ForwardResult{Model: "claude-sonnet-4-5", QualityInterrupted: interrupted, Usage: ClaudeUsage{InputTokens: 1000, OutputTokens: 100}}}))
				} else {
					svc := newOpenAIRecordUsageServiceForTest(repo, users, subs, nil)
					require.NoError(t, svc.RecordUsage(ctx, &OpenAIRecordUsageInput{APIKey: key, User: user, Account: account, Result: &OpenAIForwardResult{Model: "gpt-5.1", QualityInterrupted: interrupted, Usage: OpenAIUsage{InputTokens: 1000, OutputTokens: 100}}}))
				}
				require.NotNil(t, repo.lastLog)
				require.True(t, repo.lastLog.RequestStartedAt.Equal(start))
				require.Equal(t, interrupted, repo.lastLog.QualityInterrupted)
				require.Equal(t, 1000, repo.lastLog.InputTokens)
				require.Equal(t, 100, repo.lastLog.OutputTokens)
				require.Positive(t, repo.lastLog.TotalCost)
			}
		})
	}
}

func TestQualityHTTPAndBPSBeforeHeadersRecordsZeroUsage(t *testing.T) {
	for i, bps := range []bool{false, true} {
		t.Run(fmt.Sprint(bps), func(t *testing.T) {
			start := time.Now().Add(-10 * time.Second)
			ctx := context.WithValue(t.Context(), ctxkey.RequestStartTime, start)
			account := excelAccount()
			if !bps {
				account = &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk-test"}}
			}
			account.ID = int64(9962810 + i)
			svc := openAIClientToolsTestService(nil)
			svc.httpUpstream = &bpsTestUpstream{send: func(req *http.Request, _ string) (*http.Response, error) {
				_ = req.Body.Close()
				require.Positive(t, accountrequest.Default.CancelWindow(account.ID, start.Add(-time.Second), time.Now()))
				return nil, req.Context().Err()
			}}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			result, err := svc.Forward(ctx, c, account, []byte(`{"model":"gpt-6-astra","stream":true,"reasoning":{"effort":"high"},"input":"test"}`))
			require.ErrorIs(t, err, accountrequest.ErrQualityDegraded)
			require.NotNil(t, result)
			require.True(t, result.QualityInterrupted)
			require.True(t, result.UsageUnavailable)
			require.True(t, result.Stream)
			require.Equal(t, "high", *result.ReasoningEffort)
			require.True(t, result.RequestStartedAt.Equal(start))
			repo := &openAIRecordUsageLogRepoStub{inserted: true}
			svc.usageLogRepo = repo
			require.NoError(t, svc.RecordUsage(ctx, &OpenAIRecordUsageInput{Result: result, APIKey: &APIKey{ID: 1}, User: &User{ID: 2}, Account: account}))
			require.True(t, repo.lastLog.QualityInterrupted)
			require.True(t, repo.lastLog.RequestStartedAt.Equal(start))
			require.Zero(t, repo.lastLog.ActualCost)
		})
	}
}

func TestQualityWSTurnIDsRemainIndependentAcrossReconnect(t *testing.T) {
	ctx := context.WithValue(t.Context(), ctxkey.ClientRequestID, "same-client-request")
	repo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := &OpenAIGatewayService{usageLogRepo: repo}
	var previousID string
	for i := range 2 {
		start := time.Now()
		account := &Account{ID: int64(9962840 + i)}
		_, hooks, turns := newAccountWSTurns(ctx, account, &OpenAIWSIngressHooks{
			InitialFrameArrivedAt: start, InitialRequestModel: "gpt-6-astra",
			AfterTurn: func(_ int, result *OpenAIForwardResult, err error) {
				require.ErrorIs(t, err, accountrequest.ErrQualityDegraded)
				require.NotNil(t, result)
				require.True(t, strings.HasPrefix(result.RequestID, "quality-interrupt:"))
				require.Less(t, len(result.RequestID), 64)
				require.NotEqual(t, previousID, result.RequestID)
				input := &OpenAIRecordUsageInput{Result: result, APIKey: &APIKey{ID: 1}, User: &User{ID: 2}, Account: account, Subscription: &UserSubscription{ID: 9}, NativeCompactionV2: true}
				for range 2 {
					require.NoError(t, svc.RecordUsage(ctx, input))
					require.Equal(t, result.RequestID, repo.lastLog.RequestID)
					require.Equal(t, BillingTypeSubscription, repo.lastLog.BillingType)
					require.True(t, repo.lastLog.NativeCompactionV2)
				}
				previousID = result.RequestID
			},
		})
		require.Positive(t, accountrequest.Default.CancelWindow(account.ID, start.Add(-time.Second), time.Now()))
		hooks.AfterTurn(1, nil, nil)
		turns.close(hooks, nil)
	}
}

func TestQualityHTTPZeroUsageIDsIgnoreReusedClientID(t *testing.T) {
	ctx := context.WithValue(t.Context(), ctxkey.ClientRequestID, "same-http-client-id")
	repo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := &OpenAIGatewayService{usageLogRepo: repo}
	var previousID string
	for range 2 {
		result := &OpenAIForwardResult{Model: "gpt-6-astra", QualityInterrupted: true}
		input := &OpenAIRecordUsageInput{Result: result, APIKey: &APIKey{ID: 1}, User: &User{ID: 2}, Account: &Account{ID: 3}}
		require.NoError(t, svc.RecordUsage(ctx, input))
		require.NotEqual(t, previousID, result.RequestID)
		require.True(t, strings.HasPrefix(result.RequestID, "quality-interrupt:"))
		require.Equal(t, result.RequestID, repo.lastLog.RequestID)
		require.NoError(t, svc.RecordUsage(ctx, input))
		require.Equal(t, result.RequestID, repo.lastLog.RequestID)
		require.Zero(t, repo.lastLog.ActualCost)
		previousID = result.RequestID
	}
}

func TestQualityWSOldAndNewTurns(t *testing.T) {
	for i, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough, OpenAIWSIngressModeHTTPBridge} {
		t.Run(mode, func(t *testing.T) {
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.IngressModeDefault = mode
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 10
			upstream := newStagedPassthroughConn()
			defer func() { _ = upstream.Close() }()
			svc := newPassthroughLifecycleService(cfg, upstream)
			pool := newOpenAIWSConnPool(cfg)
			defer pool.Close()
			pool.setClientDialerForTest(&openAIWSSingleConnDialer{conn: upstream})
			svc.openaiWSPool = pool
			account := &Account{ID: int64(9962820 + i), Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-test"}, Extra: map[string]any{"openai_apikey_responses_websockets_v2_mode": mode}}
			created := `{"type":"response.created","response":{"id":"resp_old","model":"gpt-5.5","status":"in_progress"}}`
			completed := `{"type":"response.completed","response":{"id":"resp_old","model":"gpt-5.5","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":2}}}`
			allowOld := make(chan struct{})
			finishOld := sync.OnceFunc(func() { close(allowOld) })
			defer finishOld()
			if mode == OpenAIWSIngressModeHTTPBridge {
				calls := 0
				svc.httpUpstream = &bpsTestUpstream{send: func(req *http.Request, _ string) (*http.Response, error) {
					_ = req.Body.Close()
					calls++
					if calls > 1 {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					reader, writer := io.Pipe()
					go func() {
						defer func() { _ = writer.Close() }()
						_, _ = fmt.Fprintf(writer, "data: %s\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"old\"}\n\n", created)
						select {
						case <-allowOld:
						case <-req.Context().Done():
							return
						}
						_, _ = fmt.Fprintf(writer, "data: %s\n\n", completed)
					}()
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}, nil
				}}
			} else {
				upstream.Send(created)
			}
			type outcome struct {
				result *OpenAIForwardResult
				err    error
			}
			results := make(chan outcome, 4)
			secondStarted := make(chan time.Time, 1)
			secondParsed := make(chan struct{}, 1)
			done := make(chan error, 1)
			oldStart := time.Now().Add(-2 * time.Minute)
			lower := time.Now().Add(-time.Minute)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					done <- err
					return
				}
				defer func() { _ = conn.CloseNow() }()
				_, first, err := conn.Read(r.Context())
				if err != nil {
					done <- err
					return
				}
				// The HTTP upgrade can predate every logical turn.
				ctx := context.WithValue(r.Context(), ctxkey.RequestStartTime, oldStart.Add(-time.Hour))
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = r.WithContext(ctx)
				err = svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "sk-test", first, &OpenAIWSIngressHooks{
					InitialFrameArrivedAt: oldStart, InitialTurnStartedAt: oldStart, InitialRequestModel: "gpt-5.5",
					TurnStarted: func(turn int, at time.Time) {
						if turn == 2 {
							select {
							case secondStarted <- at:
							default:
							}
						}
					},
					BeforePayloadParse: func(turn int, _ []byte, _ string) error {
						if turn == 2 {
							secondParsed <- struct{}{}
						}
						return nil
					},
					AfterTurn: func(_ int, result *OpenAIForwardResult, err error) { results <- outcome{result, err} },
				})
				done <- err
			}))
			defer server.Close()
			client, _, err := coderws.Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer func() { _ = client.CloseNow() }()
			require.NoError(t, client.Write(t.Context(), coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.5","stream":true,"input":"old"}`)))
			readCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			_, frame, err := client.Read(readCtx)
			require.NoError(t, err)
			require.Contains(t, string(frame), "response.created")
			require.Zero(t, accountrequest.Default.CancelWindow(account.ID, lower, time.Now()), "old turn must keep running")
			if mode == OpenAIWSIngressModeHTTPBridge {
				finishOld()
			} else {
				upstream.Send(completed)
			}
			for !strings.Contains(string(frame), "response.completed") {
				_, frame, err = client.Read(readCtx)
				require.NoError(t, err)
			}
			select {
			case got := <-results:
				require.NoError(t, got.err)
				require.NotNil(t, got.result)
				require.True(t, got.result.RequestStartedAt.Equal(oldStart))
				require.Equal(t, "gpt-5.5", got.result.Model)
				require.False(t, got.result.QualityInterrupted)
			case <-readCtx.Done():
				t.Fatal("old turn was not settled")
			}
			newSent := time.Now()
			require.NoError(t, client.Write(t.Context(), coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-6-astra","stream":true,"input":"new"}`)))
			var newStart time.Time
			select {
			case newStart = <-secondStarted:
			case <-readCtx.Done():
				t.Fatal("new turn was not received")
			}
			require.False(t, newStart.Before(newSent))
			select {
			case <-secondParsed:
			case <-readCtx.Done():
				t.Fatal("current model was not recorded")
			}
			require.Positive(t, accountrequest.Default.CancelWindow(account.ID, lower, time.Now()))
			// The relay sends a close frame before returning/AfterTurn fallback.
			// Drain it concurrently so the websocket close handshake can finish.
			wireDone := make(chan struct{})
			go func() {
				defer close(wireDone)
				for {
					if _, _, err := client.Read(readCtx); err != nil {
						return
					}
				}
			}()

			select {
			case got := <-results:
				require.ErrorIs(t, got.err, accountrequest.ErrQualityDegraded)
				require.NotNil(t, got.result)
				require.True(t, got.result.QualityInterrupted)
				require.True(t, got.result.UsageUnavailable)
				require.True(t, got.result.RequestStartedAt.Equal(newStart))
				require.Equal(t, "gpt-6-astra", got.result.Model)
			case <-readCtx.Done():
				t.Fatal("new turn was not settled")
			}
			select {
			case err = <-done:
				require.ErrorIs(t, err, accountrequest.ErrQualityDegraded)
			case <-readCtx.Done():
				t.Fatal("relay did not stop")
			}
		})
	}
}

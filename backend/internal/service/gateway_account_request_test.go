package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type accountRequestInspectingUpstream struct {
	*httpUpstreamRecorder
	beforeSend func(*http.Request)
}

func (u *accountRequestInspectingUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	u.beforeSend(req)
	return u.httpUpstreamRecorder.Do(req, proxy, id, concurrency)
}

func (u *accountRequestInspectingUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.beforeSend(req)
	return u.httpUpstreamRecorder.DoWithTLS(req, proxy, id, concurrency, profile)
}

func TestAccountQualityInterruptSurvivesDetachedBuilderReleaseAndClientCancel(t *testing.T) {
	for _, withKeyOwner := range []bool{false, true} {
		t.Run(fmt.Sprint(withKeyOwner), func(t *testing.T) {
			client, cancelClient := context.WithCancel(context.Background())
			defer cancelClient()
			if withKeyOwner {
				owned, release := WithAPIKeyAdmissionOwner(client)
				defer release()
				client = owned
			}
			id := int64(9961901)
			if withKeyOwner {
				id++
			}
			business, releaseBusiness := beginAccountGatewayRequest(client, &Account{ID: id})
			defer releaseBusiness()
			upstream, builderRelease := detachUpstreamContext(business)
			builderRelease()
			cancelClient()
			require.NoError(t, upstream.Err(), "ordinary disconnect must retain billing drain")
			require.Positive(t, accountrequest.Default.Cancel(id, time.Now()))
			require.ErrorIs(t, context.Cause(upstream), accountrequest.ErrQualityDegraded)
			require.ErrorIs(t, accountRequestQualityError(business), accountrequest.ErrQualityDegraded,
				"quality must remain visible when the client canceled first")
		})
	}
}

type qualityWaitBody struct {
	ctx     context.Context
	initial io.Reader
}

func (b *qualityWaitBody) Read(p []byte) (int, error) {
	if b.initial != nil {
		n, err := b.initial.Read(p)
		if n > 0 {
			return n, nil
		}
		if err != io.EOF {
			return n, err
		}
		b.initial = nil
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b *qualityWaitBody) Close() error { return nil }

func TestExcelBPSQualityInterruptDoesNotDrainOrReplay(t *testing.T) {
	for _, stage := range []string{"headers", "stream"} {
		t.Run(stage, func(t *testing.T) {
			account := excelAccount()
			account.ID = 9961910
			if stage == "stream" {
				account.ID++
			}
			clientCtx := context.Background()
			writer := &cancelOnFirstWriteResponseWriter{}
			c, _ := gin.CreateTestContext(writer)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(clientCtx)
			writer.cancel = func() { accountrequest.Default.Cancel(account.ID, time.Now()) }
			calls := 0
			svc := openAIClientToolsTestService(nil)
			svc.httpUpstream = &bpsTestUpstream{send: func(req *http.Request, _ string) (*http.Response, error) {
				calls++
				_ = req.Body.Close()
				if stage == "headers" {
					accountrequest.Default.Cancel(account.ID, time.Now())
					return nil, req.Context().Err()
				}
				wire := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_quality_partial\",\"status\":\"in_progress\"}}\n\n"
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &qualityWaitBody{ctx: req.Context(), initial: strings.NewReader(wire)}}, nil
			}}
			result, err := svc.Forward(clientCtx, c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"test"}`))
			require.ErrorIs(t, err, accountrequest.ErrQualityDegraded)
			require.Equal(t, 1, calls)
			require.NoError(t, c.Request.Context().Err())
			if stage == "stream" {
				require.NotNil(t, result)
				require.False(t, result.ClientDisconnect)
			}
			if marked, ok := GetOpsStreamError(c); ok {
				require.NotEqual(t, OpsClientCanceledCode, marked.Code)
			}
			_, providerFailure := c.Get(OpsUpstreamErrorMessageKey)
			require.False(t, providerFailure)
		})
	}
}

func TestForwardOpenAIWSV2QualityInterruptDoesNotDrainOrFallback(t *testing.T) {
	for _, clientGoneFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(clientGoneFirst), func(t *testing.T) {
			clientCtx, cancelClient := context.WithCancel(context.Background())
			defer cancelClient()
			accountID := int64(9961920)
			if clientGoneFirst {
				accountID++
			}
			writer := &cancelOnFirstWriteResponseWriter{}
			c, _ := gin.CreateTestContext(writer)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(clientCtx)
			writer.cancel = func() {
				if clientGoneFirst {
					cancelClient()
					// Force the WS forwarder into its detached read before interruption.
					time.AfterFunc(30*time.Millisecond, func() { accountrequest.Default.Cancel(accountID, time.Now()) })
				} else {
					accountrequest.Default.Cancel(accountID, time.Now())
				}
			}
			captureConn := &openAIWSCancelSafeConn{openAIWSCaptureConn: &openAIWSCaptureConn{
				events: [][]byte{
					[]byte(`{"type":"response.created","response":{"id":"resp_quality_ws","model":"gpt-5.5"}}`),
					[]byte(`{"type":"response.output_text.delta","delta":"partial"}`),
					[]byte(`{"type":"response.completed","response":{"id":"resp_quality_ws","model":"gpt-5.5","usage":{"input_tokens":3,"output_tokens":5}}}`),
				},
				readDelays: []time.Duration{0, 0, 10 * time.Second},
			}}
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.APIKeyEnabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
			cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
			pool := newOpenAIWSConnPool(cfg)
			defer pool.Close()
			pool.setClientDialerForTest(&openAIWSClientConnCancelDialer{conn: captureConn})
			httpUpstream := &httpUpstreamRecorder{}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: httpUpstream, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
			account := &Account{ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-test"}, Extra: map[string]any{"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModeCtxPool}}
			started := time.Now()
			result, err := svc.Forward(clientCtx, c, account, []byte(`{"model":"gpt-5.5","stream":true,"input":[{"type":"input_text","text":"hello"}]}`))
			require.ErrorIs(t, err, accountrequest.ErrQualityDegraded)
			require.Less(t, time.Since(started), time.Second)
			require.NotNil(t, result)
			require.False(t, result.ClientDisconnect)
			require.Zero(t, result.Usage.OutputTokens, "must stop before terminal usage arrives")
			require.Nil(t, httpUpstream.lastReq, "quality interruption must not fall back to HTTP")
			captureConn.mu.Lock()
			require.True(t, captureConn.closed, "broken upstream must close before Forward returns")
			captureConn.mu.Unlock()
			require.Zero(t, accountrequest.Default.Cancel(accountID, time.Now()))
		})
	}
}

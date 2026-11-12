package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func bpsStreamErrorAccountTestService(account *Account, upstream *httpUpstreamRecorder) *AccountTestService {
	return &AccountTestService{
		accountRepo:          &stateProbeAccountRepo{account: account},
		openaiGatewayService: openAIClientToolsTestService(upstream),
	}
}

func assertBPSStreamErrorTestEvents(t *testing.T, wire, want string) {
	t.Helper()
	errors := 0
	for _, line := range strings.Split(wire, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event TestEvent
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
		require.NotEqual(t, "test_complete", event.Type, "a failed upstream must never complete the account test")
		require.False(t, event.Success)
		if event.Type == "error" {
			errors++
			require.Equal(t, want, event.Error)
		}
	}
	require.Equal(t, 1, errors, "the manual test must emit exactly one error event")
	require.NotContains(t, wire, "PRIVATE")
}

func TestAccountTestBPSStreamError(t *testing.T) {
	for _, tc := range []struct {
		name, wire, code string
	}{
		{"nested rate limit", bpsFailureWire(t, "error", "rate_limit_exceeded", "", 0), "rate_limit_exceeded"},
		{"flat rate limit", "event: error\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"PRIVATE_UPSTREAM_BODY\",\"param\":\"PRIVATE_PARAMETER\"}\n\n", "rate_limit_exceeded"},
		{"failed permission", bpsFailureWire(t, "response.failed", "permission_denied", "", 0), "permission_denied"},
		{"unknown identifiers", bpsFailureWire(t, "error", "PRIVATE_ERROR_CODE", "PRIVATE_ERROR_TYPE", 0), "basispoints_upstream_error"},
		{"upstream cancelled", bpsFailureWire(t, "response.cancelled", "", "", 0), "basispoints_upstream_cancelled"},
		{"upstream cancelled with rate limit", bpsFailureWire(t, "response.cancelled", "rate_limit_exceeded", "", 0), "rate_limit_exceeded"},
	} {
		for _, background := range []bool{false, true} {
			mode := "manual"
			if background {
				mode = "background"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				account := excelAccount()
				upstream := &httpUpstreamRecorder{resp: responsesAccountTestHTTP(tc.wire)}
				svc := bpsStreamErrorAccountTestService(account, upstream)
				want := "Excel BPS upstream failure: " + tc.code + "; request was not replayed"
				if background {
					result, err := svc.RunTestBackground(t.Context(), account.ID, "gpt-6-astra")
					require.NoError(t, err, "execution returns a failed result rather than an infrastructure error")
					require.NotNil(t, result)
					require.Equal(t, "failed", result.Status)
					require.Equal(t, want, result.ErrorMessage)
					require.Empty(t, result.ResponseText)
					require.NotContains(t, result.ErrorMessage, "PRIVATE")
				} else {
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
					err := svc.TestAccountConnection(c, account.ID, "gpt-6-astra", "Reply OK", AccountTestModeDefault)
					require.EqualError(t, err, want)
					assertBPSStreamErrorTestEvents(t, rec.Body.String(), want)
				}
				require.Len(t, upstream.requests, 1, "an HTTP 200 failure terminal must not replay or fall back")
				require.Equal(t, "bps.openai.com", upstream.lastReq.URL.Host)
				require.Equal(t, "/basispoints/api/responses", upstream.lastReq.URL.Path)
			})
		}
	}
}

// Cancel only once the real gateway starts reading an accepted HTTP 200 stream.
// This distinguishes local cancellation from an upstream response.cancelled event.
type bpsStreamErrorCanceledBody struct{ cancel context.CancelFunc }

func (b *bpsStreamErrorCanceledBody) Read([]byte) (int, error) {
	b.cancel()
	return 0, context.Canceled
}

func (*bpsStreamErrorCanceledBody) Close() error { return nil }

func TestAccountTestBPSStreamErrorLocalCancellation(t *testing.T) {
	for _, background := range []bool{false, true} {
		mode := "manual"
		if background {
			mode = "background"
		}
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			account := excelAccount()
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       &bpsStreamErrorCanceledBody{cancel: cancel},
			}}
			svc := bpsStreamErrorAccountTestService(account, upstream)
			if background {
				result, err := svc.RunTestBackground(ctx, account.ID, "gpt-6-astra")
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, "failed", result.Status)
				require.Equal(t, context.Canceled.Error(), result.ErrorMessage)
				require.Empty(t, result.ResponseText)
			} else {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(ctx)
				err := svc.TestAccountConnection(c, account.ID, "gpt-6-astra", "Reply OK", AccountTestModeDefault)
				require.EqualError(t, err, context.Canceled.Error())
				// A disconnected consumer need not receive another SSE frame.
				_, message := parseTestSSEOutput(rec.Body.String())
				if message != "" {
					require.Equal(t, context.Canceled.Error(), message)
				}
				require.NotContains(t, rec.Body.String(), `"type":"test_complete"`)
				require.NotContains(t, rec.Body.String(), "basispoints_upstream_cancelled")
				require.NotContains(t, rec.Body.String(), "Client disconnected:")
			}
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, "bps.openai.com", upstream.lastReq.URL.Host)
		})
	}
}

func TestAccountTestBPSStreamErrorRefusalPriority(t *testing.T) {
	// Both refusal evidence and a classified terminal are present. The refusal
	// must remain the result rather than the later permission error.
	wire := responsesTestWire(`{"type":"response.refusal.delta","delta":"PRIVATE_REFUSAL"}`) +
		bpsFailureWire(t, "response.failed", "permission_denied", "", 0)
	account := excelAccount()
	upstream := &httpUpstreamRecorder{resp: responsesAccountTestHTTP(wire)}
	svc := bpsStreamErrorAccountTestService(account, upstream)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
	err := svc.TestAccountConnection(c, account.ID, "gpt-6-astra", "Reply OK", AccountTestModeDefault)
	require.EqualError(t, err, errResponsesTestRefused.Error())
	assertBPSStreamErrorTestEvents(t, rec.Body.String(), errResponsesTestRefused.Error())
	require.NotContains(t, rec.Body.String(), "permission_denied")
	require.Len(t, upstream.requests, 1)
}

func TestAccountTestBPSStreamErrorNativeModel(t *testing.T) {
	// An account-wide BPS toggle can still send a model through native Codex.
	// Exercise the shared gateway helper with an actual non-BPS dispatch.
	account := excelAccount()
	account.Extra["openai_excel_bps_models"] = []string{"gpt-6-astra"}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("")),
	}}
	svc := bpsStreamErrorAccountTestService(account, upstream)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
	err := svc.testResponsesGatewayAccountConnection(c, account, "gpt-5.6-sol", "Reply OK")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "Excel BPS")
	assertBPSStreamErrorTestEvents(t, rec.Body.String(), err.Error())
	require.Len(t, upstream.requests, 1)
	require.Equal(t, chatgptCodexURL, upstream.lastReq.URL.String())
}

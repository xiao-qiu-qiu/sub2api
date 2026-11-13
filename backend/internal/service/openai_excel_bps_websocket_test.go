package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bpsws"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/Wei-Shaw/sub2api/internal/util/transportdiag"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type excelBPSWSTestUpstream struct {
	*httpUpstreamRecorder
	wsCalls, httpCalls int
	idle               time.Duration
	send               func(*http.Request, string) (*http.Response, error)
}

func (u *excelBPSWSTestUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	u.httpCalls++
	return u.httpUpstreamRecorder.Do(req, proxy, id, concurrency)
}

func (u *excelBPSWSTestUpstream) DoBPSWebSocket(req *http.Request, proxy string, id int64, concurrency int, idle time.Duration) (*http.Response, error) {
	u.wsCalls++
	u.idle = idle
	if u.send != nil {
		return u.send(req, proxy)
	}
	return u.httpUpstreamRecorder.Do(req, proxy, id, concurrency)
}

func TestExcelBPSWebSocketLocalPreparationIsNotSubmission(t *testing.T) {
	for _, connected := range []bool{false, true} {
		t.Run(fmt.Sprint(connected), func(t *testing.T) {
			account := excelAccount()
			account.Extra[ExcelBPSTransportKey] = "ws"
			account.Extra["openai_excel_bps_mihomo"] = true
			u := &excelBPSWSTestUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{}}
			u.send = func(req *http.Request, proxy string) (*http.Response, error) {
				// The real adapter must parse this JSON before dialing.
				body, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.Equal(t, `{"model":"test"}`, string(body))
				trace := httptrace.ContextClientTrace(req.Context())
				trace.GetConn("bps.openai.com:443")
				if u.wsCalls == 1 {
					if connected {
						trace.GotConn(httptrace.GotConnInfo{})
					}
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("local failure")}
				}
				require.Equal(t, "http://127.0.0.1:19002", proxy)
				return bpsCompletionResponse(200, "done"), nil
			}
			svc := &OpenAIGatewayService{httpUpstream: u}
			leases := []*bpsTestLease{{}, {}}
			acquired := 0
			acquire := func(context.Context, string, ...string) (string, excelBPSLease, error) {
				acquired++
				return fmt.Sprintf("http://127.0.0.1:%d", 19000+acquired), leases[acquired-1], nil
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			resp, lease, _, err := svc.doExcelBPSRequest(t.Context(), c, account, "ws-test", []byte(`{"model":"test"}`), "test-token", "test-account", acquire)
			if connected {
				require.Error(t, err)
				require.Equal(t, 1, u.wsCalls, "never replay once a connection could send")
			} else {
				require.NoError(t, err)
				require.Equal(t, 2, u.wsCalls, "an unsent dial failure may use another managed exit")
				require.NoError(t, resp.Body.Close())
				lease.Release()
			}
			require.Zero(t, u.httpCalls)
			require.Equal(t, 1, leases[0].releases)
		})
	}
}

func TestExcelBPSWebSocketTransportSelection(t *testing.T) {
	for _, tc := range []struct {
		name, target, transport string
		ws                      bool
	}{
		{"legacy default", basispoints.ResponsesURL, "", false},
		{"websocket", basispoints.ResponsesURL, "ws", true},
		{"attachments stay HTTP", basispoints.AttachmentsURL, "ws", false},
		{"image generation stays HTTP", basispoints.ImagesGenerationsURL, "ws", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := excelAccount()
			account.Extra[ExcelBPSTransportKey] = tc.transport
			u := &excelBPSWSTestUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{resp: bpsCompletionResponse(403, `{"error":{"code":"model_not_allowed"}}`)}}
			svc := &OpenAIGatewayService{httpUpstream: u}
			ctx := WithHTTPUpstreamProfile(context.Background(), HTTPUpstreamProfileExcelBPS)
			req, err := newExcelBPSRequestTo(ctx, tc.target, "text/event-stream", []byte(`{"model":"gpt-6-astra"}`), "test-token", "test-account")
			require.NoError(t, err)
			resp, err := svc.doExcelBPSUpstream(req, "http://proxy.example:8080", account, nil)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			require.Equal(t, 403, resp.StatusCode, "HTTP rejections retain existing handling")
			require.Equal(t, "http://proxy.example:8080", u.lastProxyURL)
			require.Len(t, u.requests, 1)
			if tc.ws {
				require.Equal(t, 1, u.wsCalls)
				require.Zero(t, u.httpCalls)
				require.Equal(t, 120*time.Second, u.idle)
				require.True(t, HTTPUpstreamRedirectsDisabled(u.lastReq.Context()))
				require.Equal(t, HTTPUpstreamProfileExcelBPSWebSocket, HTTPUpstreamProfileFromContext(u.lastReq.Context()))
			} else {
				require.Zero(t, u.wsCalls)
				require.Equal(t, 1, u.httpCalls)
			}
		})
	}
}

// UUID and resume-token encodings have fixed byte lengths. Include raw HTML
// characters, UTF-8 and a JSON escape so the boundary is the final WS encoding,
// not the HTTP body length or its rune count.
func excelBPSRequestAtWSThreshold(t *testing.T, offset int) []byte {
	t.Helper()
	const suffix = `<>&汉😀\u2028","stream":true}`
	const prefix = `{"model":"gpt-6-astra","input":"`
	req, err := newExcelBPSRequest(t.Context(), []byte(prefix+suffix), "test-token", "test-account")
	require.NoError(t, err)
	defer func() { _ = req.Body.Close() }()
	_, size, err := bpsws.PrepareRequest(req)
	require.NoError(t, err)
	return []byte(prefix + strings.Repeat("x", excelBPSLargeRequestHTTPThreshold-size+offset) + suffix)
}

func TestExcelBPSLargeRequestTransportSelection(t *testing.T) {
	for _, tc := range []struct {
		name, target, transport string
		setting                 *string
		offset                  int
		small, ws, fallback     bool
	}{
		{name: "small WS", target: basispoints.ResponsesURL, transport: "ws", small: true, ws: true},
		{name: "one byte below final threshold", target: basispoints.ResponsesURL, transport: "ws", offset: -1, ws: true},
		{name: "exact final threshold without settings service", target: basispoints.ResponsesURL, transport: "ws", fallback: true},
		{name: "above threshold", target: basispoints.ResponsesURL, transport: "ws", offset: 1, fallback: true},
		{name: "missing setting defaults enabled", target: basispoints.ResponsesURL, transport: "ws", setting: new(""), fallback: true},
		{name: "explicit enabled", target: basispoints.ResponsesURL, transport: "ws", setting: new("true"), fallback: true},
		{name: "explicit disabled keeps large WS", target: basispoints.ResponsesURL, transport: "ws", setting: new("false"), ws: true},
		{name: "HTTP account unchanged", target: basispoints.ResponsesURL, transport: "sse"},
		{name: "attachment unchanged", target: basispoints.AttachmentsURL, transport: "ws"},
		{name: "image endpoint unchanged", target: basispoints.ImagesGenerationsURL, transport: "ws"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetGatewayForwardingSettingsCacheForTest(t)
			body := []byte(`{"model":"gpt-6-astra","input":"hello"}`)
			if !tc.small {
				body = excelBPSRequestAtWSThreshold(t, tc.offset)
				assert.Less(t, len(body), excelBPSLargeRequestHTTPThreshold, "the raw body alone must not select the transport")
			}
			account := excelAccount()
			account.Extra[ExcelBPSTransportKey] = tc.transport
			u := &excelBPSWSTestUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{resp: bpsCompletionResponse(200, "done")}}
			svc := &OpenAIGatewayService{httpUpstream: u}
			if tc.setting != nil {
				values := map[string]string{}
				if *tc.setting != "" {
					values[SettingKeyExcelBPSLargeRequestHTTPFallbackEnabled] = *tc.setting
				}
				svc.settingService = NewSettingService(&gatewayTTLSettingRepo{data: values}, nil)
			}
			req, err := newExcelBPSRequestTo(t.Context(), tc.target, "text/event-stream", body, "test-token", "test-account")
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			resp, err := svc.doExcelBPSUpstream(req, "http://proxy.example:8080", account, c)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			want := ExcelBPSTransportSSE
			profile := HTTPUpstreamProfileExcelBPS
			if tc.ws {
				want, profile = ExcelBPSTransportWS, HTTPUpstreamProfileExcelBPSWebSocket
				assert.Equal(t, 1, u.wsCalls)
				assert.Zero(t, u.httpCalls)
			} else {
				assert.Equal(t, 1, u.httpCalls)
				assert.Zero(t, u.wsCalls)
			}
			assert.Equal(t, want, rec.Header().Get("X-Sub2API-BPS-Transport"))
			assert.Equal(t, want, c.GetString("excel_bps_transport"))
			assert.Equal(t, profile, HTTPUpstreamProfileFromContext(u.lastReq.Context()))
			assert.Equal(t, body, u.lastBody, "HTTP/SSE keeps the original prepared content")
			assert.Equal(t, "http://proxy.example:8080", u.lastProxyURL)
			assert.Equal(t, account.ExcelBPSTransport(), tc.transport)
			assert.Equal(t, "Bearer test-token", u.lastReq.Header.Get("Authorization"))
			assert.Equal(t, "test-account", u.lastReq.Header.Get("Chatgpt-Account-Id"))
			route := transportdiag.HTTPRoute(u.lastReq)
			assert.Equal(t, "excel_bps_"+want, route.Mode, "capture retains the selected BPS transport")
			assert.Equal(t, tc.fallback, route.FallbackReason == "large_request_http_fallback")
		})
	}
}

func TestExcelBPSLargeRequestForward(t *testing.T) {
	wire := "data: " + `{"type":"response.completed","response":{"id":"resp_large","status":"completed","model":"gpt-6-astra","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":10,"output_tokens":2}}}` + "\n\n"
	u := &excelBPSWSTestUpstream{httpUpstreamRecorder: &httpUpstreamRecorder{resp: bpsCompletionResponse(200, wire)}}
	svc := openAIClientToolsTestService(u.httpUpstreamRecorder)
	svc.httpUpstream = u
	account := excelAccount()
	account.Extra[ExcelBPSTransportKey] = ExcelBPSTransportWS
	body := excelBPSRequestAtWSThreshold(t, 1024)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	result, err := svc.Forward(t.Context(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1, u.httpCalls)
	assert.Zero(t, u.wsCalls)
	assert.Equal(t, ExcelBPSTransportSSE, rec.Header().Get("X-Sub2API-BPS-Transport"))
	assert.Contains(t, rec.Body.String(), "OK")
	assert.Equal(t, 10, result.Usage.InputTokens)
	assert.Equal(t, 2, result.Usage.OutputTokens)
	assert.Equal(t, "gpt-6-astra", gjson.GetBytes(u.lastBody, "model").String())
}

func TestExcelBPSWebSocketNeverFallsBackToHTTPAfterFailure(t *testing.T) {
	for _, tc := range []struct {
		name                string
		available, tooLarge bool
	}{
		{name: "unavailable"},
		{name: "transport error", available: true},
		{name: "remote 1009 terminal", available: true, tooLarge: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := excelAccount()
			account.Extra[ExcelBPSTransportKey] = "ws"
			recorder := &httpUpstreamRecorder{err: errors.New("transport failed")}
			if tc.tooLarge {
				recorder.err = nil
				recorder.resp = bpsCompletionResponse(http.StatusOK, bpsFailureWire(t, "response.failed", "basispoints_ws_message_too_big", "", 0))
			}
			svc := openAIClientToolsTestService(recorder)
			u := &excelBPSWSTestUpstream{httpUpstreamRecorder: recorder}
			if tc.available {
				svc.httpUpstream = u
			}
			body := []byte(`{"model":"gpt-6-astra","input":"hello","stream":true}`)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
			_, err := svc.Forward(context.Background(), c, account, body)
			require.Error(t, err)
			require.Zero(t, u.httpCalls)
			if tc.available {
				require.Equal(t, 1, u.wsCalls)
			} else {
				require.Empty(t, recorder.requests)
			}
			require.Equal(t, "ws", rec.Header().Get("X-Sub2API-BPS-Transport"))
			if tc.tooLarge {
				assert.Contains(t, rec.Body.String(), "basispoints_ws_message_too_big")
				assert.Len(t, recorder.requests, 1, "a remote 1009 must never cause another submission")
			}
		})
	}
}

func TestExcelBPSWebSocketForwardAndToolRepair(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, repair := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/repair=%t", stream, repair), func(t *testing.T) {
				wire := "data: " + `{"type":"response.completed","response":{"id":"resp_ws","status":"completed","model":"gpt-6-astra","output":[{"type":"message","id":"msg_ws","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":10,"output_tokens":2}}}` + "\n\n"
				recorder := &httpUpstreamRecorder{resp: bpsCompletionResponse(200, wire)}
				if repair {
					recorder.responses = []*http.Response{
						bpsCompletionResponse(200, excelBPSRepairWire(t, "0", "Run client tool", "text(42);")),
						bpsCompletionResponse(200, excelBPSRepairWire(t, "1", "codex2api.custom/functions.exec", "text(24);")),
					}
				}
				u := &excelBPSWSTestUpstream{httpUpstreamRecorder: recorder}
				svc := openAIClientToolsTestService(recorder)
				svc.httpUpstream = u
				account := excelAccount()
				account.Extra[ExcelBPSTransportKey] = "ws"
				body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"reasoning":{"effort":"max"},"input":"test","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]}`, stream))
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
				result, err := svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err)
				require.Zero(t, u.httpCalls)
				expectedCalls := 1
				if repair {
					expectedCalls = 2
					require.Contains(t, rec.Body.String(), `"input":"text(42);"`)
					require.NotContains(t, rec.Body.String(), "text(24);")
				} else {
					require.Contains(t, rec.Body.String(), "OK")
				}
				require.Equal(t, expectedCalls, u.wsCalls)
				require.Equal(t, expectedCalls*10, result.Usage.InputTokens)
				require.Equal(t, expectedCalls*2, result.Usage.OutputTokens)
				require.Equal(t, "ws", rec.Header().Get("X-Sub2API-BPS-Transport"))
				if !stream {
					require.True(t, gjson.Valid(rec.Body.String()))
				}
				for i, req := range u.requests {
					require.Equal(t, HTTPUpstreamProfileExcelBPSWebSocket, HTTPUpstreamProfileFromContext(req.Context()))
					require.Equal(t, "Bearer test-token", req.Header.Get("Authorization"))
					require.Equal(t, "xhigh", gjson.GetBytes(u.bodies[i], "reasoning_effort").String())
				}
			})
		}
	}
}

func TestExcelBPSWebSocketEncryptedRetryUsesSameTransport(t *testing.T) {
	recorder := &httpUpstreamRecorder{responses: []*http.Response{
		bpsCompletionResponse(400, excelBPSInvalidCiphertext),
		bpsCompletionResponse(200, "data: "+`{"type":"response.completed","response":{"id":"resp_retry","status":"completed","output":[]}}`+"\n\n"),
	}}
	u := &excelBPSWSTestUpstream{httpUpstreamRecorder: recorder}
	svc := openAIClientToolsTestService(recorder)
	svc.httpUpstream = u
	account := excelAccount()
	account.Extra[ExcelBPSTransportKey] = "ws"
	body := excelBPSEncryptedHistoryRequest(false)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.Equal(t, 2, u.wsCalls)
	require.Zero(t, u.httpCalls)
	require.Contains(t, string(u.bodies[0]), "private-ciphertext")
	require.NotContains(t, string(u.bodies[1]), "private-ciphertext")
}

func TestExcelBPSWebSocketCompactUsesSelectedTransport(t *testing.T) {
	wire := "data: " + `{"type":"response.completed","response":{"id":"resp_compact","status":"completed","output":[{"type":"compaction","encrypted_content":"compact-state"}],"usage":{"input_tokens":12,"output_tokens":3}}}` + "\n\n"
	recorder := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire))}}
	u := &excelBPSWSTestUpstream{httpUpstreamRecorder: recorder}
	svc := openAIClientToolsTestService(recorder)
	svc.httpUpstream = u
	account := excelAccount()
	account.Extra[ExcelBPSTransportKey] = "ws"
	body := []byte(`{"model":"gpt-6-astra","input":"compact this"}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/responses/compact", bytes.NewReader(body))
	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.Equal(t, 1, u.wsCalls)
	require.Zero(t, u.httpCalls)
	require.Contains(t, string(u.lastBody), "compaction_trigger")
	require.Contains(t, rec.Body.String(), "compact-state")
}

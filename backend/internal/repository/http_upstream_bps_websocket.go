package repository

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bpsws"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/requestcapture"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

var (
	_ service.ExcelBPSWebSocketUpstream = (*httpUpstreamService)(nil)
	_ service.ExcelBPSWebSocketUpstream = (*astraRoutingUpstream)(nil)
)

// BPS keeps its account/session proxy even when Codex routing is configured.
func (s *astraRoutingUpstream) DoBPSWebSocket(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, idleTimeout time.Duration) (*http.Response, error) {
	upstream, ok := s.delegate.(service.ExcelBPSWebSocketUpstream)
	if !ok {
		return nil, errors.New("upstream does not support BPS WebSocket")
	}
	return upstream.DoBPSWebSocket(req, proxyURL, accountID, accountConcurrency, idleTimeout)
}

func (s *httpUpstreamService) DoBPSWebSocket(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, idleTimeout time.Duration) (result *http.Response, resultErr error) {
	req, finishAccountRequest := beginAccountHTTPRequest(req, accountID)
	defer func() { result, resultErr = finishAccountRequest(result, resultErr) }()
	if req == nil || req.URL == nil {
		return nil, errors.New("request url is nil")
	}
	requestcapture.FromContext(req.Context()).BindAccount(accountID)
	ctx := requestcapture.WithAccount(req.Context(), accountID)
	ctx = service.WithHTTPUpstreamProfile(ctx, service.HTTPUpstreamProfileExcelBPSWebSocket)
	req = req.WithContext(service.WithHTTPUpstreamRedirectsDisabled(ctx))
	if err := s.validateRequestHost(req); err != nil {
		requestcapture.FromContext(req.Context()).SelectionFailed(accountID, 0, nil, nil, err)
		return nil, err
	}
	entry, err := s.acquireClientWithProfile(proxyURL, accountID, accountConcurrency, service.HTTPUpstreamProfileExcelBPSWebSocket)
	if err != nil {
		requestcapture.FromContext(req.Context()).SelectionFailed(accountID, 0, nil, nil, err)
		return nil, err
	}
	release := func() {
		atomic.AddInt64(&entry.inFlight, -1)
		atomic.StoreInt64(&entry.lastUsed, time.Now().UnixNano())
	}
	if slots := entry.bpsWebSocketSlots; slots != nil {
		select {
		case slots <- struct{}{}:
			finish := release
			release = func() {
				<-slots
				finish()
			}
		case <-req.Context().Done():
			release()
			requestcapture.FromContext(req.Context()).SelectionFailed(accountID, 0, nil, nil, req.Context().Err())
			return nil, req.Context().Err()
		}
	}
	client := s.httpClientForUpstreamRequest(entry.client, req)
	resp, err := openBPSWebSocket(client, req, idleTimeout)
	if err != nil {
		release()
		return nil, err
	}
	// Open has consumed the 101 ReadWriteCloser and returned the SSE adapter,
	// or an ordinary HTTP refusal. Both retain the pool until their body closes.
	resp.Body = wrapTrackedBody(resp.Body, release)
	return resp, nil
}

func openBPSWebSocket(client *http.Client, req *http.Request, idleTimeout time.Duration) (result *http.Response, resultErr error) {
	req, timingTrace := requesttiming.StartTransport(req)
	defer func() { timingTrace.Response(result, resultErr) }()
	_, observeCapture := requestcapture.FromContext(req.Context()).ObserveHTTPRequest(req, requestcapture.AccountFromContext(req.Context()))
	defer func() { observeCapture(result, resultErr) }()
	ctx, cancel := context.WithCancel(req.Context())
	// Do/doUpstreamRequest wrap 101 bodies as ReadCloser, destroying the Write
	// method needed for the WebSocket handshake. Open must own the raw client.
	resp, err := bpsws.Open(ctx, client, req.WithContext(ctx), idleTimeout)
	if err != nil {
		cancel()
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		cancel()
		return nil, errors.New("BPS WebSocket returned an empty response")
	}
	timingTrace.Headers(resp)
	requesttiming.Encoding(req.Context(), resp)
	decompressResponseBody(resp)
	resp.Body = requesttiming.WrapStage(req.Context(), resp.Body, "decoded_body_first")
	resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

package service

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bpsws"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/Wei-Shaw/sub2api/internal/util/transportdiag"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// A conservative policy threshold, not the upstream's exact message limit.
const excelBPSLargeRequestHTTPThreshold = 15_000_000

type excelBPSTransportSelectionKey struct{}

type excelBPSTransportSelection struct {
	transport    string
	payloadBytes int
	fallback     bool
}

// Select before any trace/body evidence is installed. The same decision drives
// dispatch, no-replay tracking and diagnostics for the whole network attempt.
func (s *OpenAIGatewayService) prepareExcelBPSUpstream(req *http.Request, account *Account) (*http.Request, error) {
	if _, ok := req.Context().Value(excelBPSTransportSelectionKey{}).(excelBPSTransportSelection); ok {
		return req, nil
	}
	selection := excelBPSTransportSelection{transport: ExcelBPSTransportSSE}
	if account.ExcelBPSTransport() == ExcelBPSTransportWS && req.Method == http.MethodPost && req.URL.String() == basispoints.ResponsesURL {
		selection.transport = ExcelBPSTransportWS
		if s.settingService == nil || s.settingService.IsExcelBPSLargeRequestHTTPFallbackEnabled(req.Context()) {
			prepared, size, err := bpsws.PrepareRequest(req)
			if err != nil {
				return nil, err
			}
			selection.payloadBytes = size
			if size >= excelBPSLargeRequestHTTPThreshold {
				selection.transport = ExcelBPSTransportSSE
				selection.fallback = true
			} else {
				req = prepared
			}
		}
	}
	ctx := context.WithValue(req.Context(), excelBPSTransportSelectionKey{}, selection)
	profile := HTTPUpstreamProfileExcelBPS
	if selection.transport == ExcelBPSTransportWS {
		profile = HTTPUpstreamProfileExcelBPSWebSocket
	}
	ctx = WithHTTPUpstreamProfile(ctx, profile)
	route := transportdiag.RouteFromContext(ctx)
	route.Mode = "excel_bps_" + selection.transport
	route.Reason = "account_transport"
	if selection.fallback {
		route.Reason = "large_request_http_fallback"
		route.FallbackReason = route.Reason
	}
	return req.WithContext(transportdiag.WithRoute(ctx, route)), nil
}

// Images and attachments retain their HTTP contracts. Only Responses changes
// transport; the same bridge, credentials, proxy lease and usage handling apply.
func (s *OpenAIGatewayService) doExcelBPSUpstream(req *http.Request, proxy string, account *Account, contexts ...*gin.Context) (*http.Response, error) {
	prepared, err := s.prepareExcelBPSUpstream(req, account)
	if err != nil {
		_ = req.Body.Close()
		return nil, err
	}
	req = prepared
	selection := req.Context().Value(excelBPSTransportSelectionKey{}).(excelBPSTransportSelection)
	var c *gin.Context
	if len(contexts) != 0 {
		c = contexts[0]
	}
	if c != nil {
		c.Header("X-Sub2API-BPS-Transport", selection.transport)
		c.Set("excel_bps_transport", selection.transport)
	}
	if selection.fallback {
		logger.FromContext(req.Context()).Info("excel_bps.large_request_http_fallback",
			zap.Int64("account_id", account.ID), zap.String("bps_transport", selection.transport),
			zap.Int("ws_payload_bytes", selection.payloadBytes), zap.Int("threshold_bytes", excelBPSLargeRequestHTTPThreshold))
	}
	if selection.transport != ExcelBPSTransportWS {
		return s.doPriorityObservedHTTP(req, proxy, account)
	}
	upstream, ok := s.httpUpstream.(ExcelBPSWebSocketUpstream)
	if !ok {
		return nil, errors.New("BPS WebSocket transport unavailable")
	}
	idle := 120 * time.Second
	if s.cfg != nil {
		idle = time.Duration(s.cfg.Gateway.ExcelBPSStreamDataIntervalTimeout) * time.Second
	}
	req = req.WithContext(WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileExcelBPSWebSocket)))
	return s.doPriorityObservedRequest(req, account, func(req *http.Request) (*http.Response, error) {
		return upstream.DoBPSWebSocket(req, proxy, account.ID, account.Concurrency, idle)
	})
}

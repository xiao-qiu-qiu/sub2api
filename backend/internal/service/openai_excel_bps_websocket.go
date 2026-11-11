package service

import (
	"errors"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
)

// Images and attachments retain their HTTP contracts. Only Responses changes
// transport; the same bridge, credentials, proxy lease and usage handling apply.
func (s *OpenAIGatewayService) doExcelBPSUpstream(req *http.Request, proxy string, account *Account) (*http.Response, error) {
	if account.ExcelBPSTransport() != ExcelBPSTransportWS || req.URL.String() != basispoints.ResponsesURL {
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

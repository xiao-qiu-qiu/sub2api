package repository

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

type ProxyProbeServiceSuite struct {
	suite.Suite
	ctx      context.Context
	proxySrv *httptest.Server
	prober   *proxyProbeService
}

func (s *ProxyProbeServiceSuite) SetupTest() {
	s.ctx = context.Background()
	s.prober = &proxyProbeService{
		allowPrivateHosts: true,
		// HTTP 仅用于本地代理桩；生产内置端点均使用 HTTPS。
		configuredProbeURLs: []configuredProbeTarget{
			{url: "http://ipwho.is/", parser: "ipwhois"},
			{url: "http://api64.ipify.org?format=json", parser: "ipify"},
		},
	}
}

func (s *ProxyProbeServiceSuite) TearDownTest() {
	if s.proxySrv != nil {
		s.proxySrv.Close()
		s.proxySrv = nil
	}
}

func (s *ProxyProbeServiceSuite) setupProxyServer(handler http.HandlerFunc) {
	s.proxySrv = newLocalTestServer(s.T(), handler)
}

func (s *ProxyProbeServiceSuite) TestProbeProxy_InvalidProxyURL() {
	_, _, err := s.prober.ProbeProxy(s.ctx, "://bad")
	require.Error(s.T(), err)
	require.ErrorContains(s.T(), err, "failed to create proxy client")
}

func (s *ProxyProbeServiceSuite) TestProbeProxy_UnsupportedProxyScheme() {
	_, _, err := s.prober.ProbeProxy(s.ctx, "ftp://127.0.0.1:1")
	require.Error(s.T(), err)
	require.ErrorContains(s.T(), err, "failed to create proxy client")
}

func (s *ProxyProbeServiceSuite) TestProbeProxy_Success_IPWhoisIPv6() {
	s.setupProxyServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.RequestURI, "ipwho.is") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"success":true,"ip":"2001:db8::1","city":"c","region":"r","country":"cc","country_code":"CC","timezone":{"id":"Asia/Tokyo"}}`)
			return
		}
		// 其他请求返回错误
		w.WriteHeader(http.StatusServiceUnavailable)
	}))

	info, latencyMs, err := s.prober.ProbeProxy(s.ctx, s.proxySrv.URL)
	require.NoError(s.T(), err, "ProbeProxy")
	require.GreaterOrEqual(s.T(), latencyMs, int64(0), "unexpected latency")
	require.Equal(s.T(), "2001:db8::1", info.IP)
	require.Equal(s.T(), "c", info.City)
	require.Equal(s.T(), "r", info.Region)
	require.Equal(s.T(), "cc", info.Country)
	require.Equal(s.T(), "CC", info.CountryCode)
	require.Equal(s.T(), "Asia/Tokyo", info.Timezone)
}

func (s *ProxyProbeServiceSuite) TestProbeProxy_Success_IPifyFallback() {
	s.setupProxyServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 归属服务失败仍允许 ipify 确认出口连通。
		if strings.Contains(r.RequestURI, "ipwho.is") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// ipify 成功
		if strings.Contains(r.RequestURI, "api64.ipify.org") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ip": "5.6.7.8"}`)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))

	info, latencyMs, err := s.prober.ProbeProxy(s.ctx, s.proxySrv.URL)
	require.NoError(s.T(), err, "ProbeProxy should fallback to ipify")
	require.GreaterOrEqual(s.T(), latencyMs, int64(0), "unexpected latency")
	require.Equal(s.T(), "5.6.7.8", info.IP)
}

func (s *ProxyProbeServiceSuite) TestProbeProxy_AllFailed() {
	s.setupProxyServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))

	_, _, err := s.prober.ProbeProxy(s.ctx, s.proxySrv.URL)
	require.Error(s.T(), err)
	require.ErrorContains(s.T(), err, "all probe URLs failed")
}

func (s *ProxyProbeServiceSuite) TestProbeProxy_InvalidJSON() {
	s.setupProxyServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.RequestURI, "ipwho.is") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "not-json")
			return
		}
		// ipify 也返回无效响应
		if strings.Contains(r.RequestURI, "api64.ipify.org") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "not-json")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))

	_, _, err := s.prober.ProbeProxy(s.ctx, s.proxySrv.URL)
	require.Error(s.T(), err)
	require.ErrorContains(s.T(), err, "all probe URLs failed")
}

func (s *ProxyProbeServiceSuite) TestProbeProxy_ProxyServerClosed() {
	s.setupProxyServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.proxySrv.Close()

	_, _, err := s.prober.ProbeProxy(s.ctx, s.proxySrv.URL)
	require.Error(s.T(), err, "expected error when proxy server is closed")
}

func (s *ProxyProbeServiceSuite) TestParseIPAPI_Success() {
	body := []byte(`{"status":"success","query":"1.2.3.4","city":"Beijing","regionName":"Beijing","country":"China","countryCode":"CN"}`)
	info, latencyMs, err := s.prober.parseIPAPI(body, 100)
	require.NoError(s.T(), err)
	require.Equal(s.T(), int64(100), latencyMs)
	require.Equal(s.T(), "1.2.3.4", info.IP)
	require.Equal(s.T(), "Beijing", info.City)
	require.Equal(s.T(), "Beijing", info.Region)
	require.Equal(s.T(), "China", info.Country)
	require.Equal(s.T(), "CN", info.CountryCode)
}

func (s *ProxyProbeServiceSuite) TestParseIPAPI_Failure() {
	body := []byte(`{"status":"fail","message":"rate limited"}`)
	_, _, err := s.prober.parseIPAPI(body, 100)
	require.Error(s.T(), err)
	require.ErrorContains(s.T(), err, "rate limited")
}

func (s *ProxyProbeServiceSuite) TestParseIPify_Success() {
	body := []byte(`{"ip": "2001:db8::1"}`)
	info, latencyMs, err := s.prober.parseIPify(body, 50)
	require.NoError(s.T(), err)
	require.Equal(s.T(), int64(50), latencyMs)
	require.Equal(s.T(), "2001:db8::1", info.IP)
}

func (s *ProxyProbeServiceSuite) TestParseIPify_NoIP() {
	body := []byte(`{"ip": ""}`)
	_, _, err := s.prober.parseIPify(body, 50)
	require.Error(s.T(), err)
	require.ErrorContains(s.T(), err, "no IP found")
}

func (s *ProxyProbeServiceSuite) TestParseChatGPTTrace_Success() {
	body := []byte("fl=abc\nh=chatgpt.com\nip=203.0.113.5\nts=1700000000\nloc=US\ntz=UTC\n")
	info, latencyMs, err := s.prober.parseChatGPTTrace(body, 320)
	require.NoError(s.T(), err)
	require.Equal(s.T(), int64(320), latencyMs)
	require.Equal(s.T(), "203.0.113.5", info.IP)
	require.Equal(s.T(), "US", info.CountryCode)
}

func (s *ProxyProbeServiceSuite) TestParseChatGPTTrace_NoIP() {
	body := []byte("fl=abc\nh=chatgpt.com\nloc=US\n")
	_, _, err := s.prober.parseChatGPTTrace(body, 100)
	require.Error(s.T(), err)
	require.ErrorContains(s.T(), err, "chatgpt-trace: no ip= found")
}

func TestProxyProbeServiceSuite(t *testing.T) {
	suite.Run(t, new(ProxyProbeServiceSuite))
}

func TestProxyProbeGeoEnrichment(t *testing.T) {
	const exitIP = "2001:db8::1"
	for _, tt := range []struct {
		name     string
		status   int
		body     string
		timeout  bool
		wantCity string
	}{
		{name: "IPv6 success", status: http.StatusOK, body: `{"success":true,"ip":"2001:db8:0:0:0:0:0:1","city":"Tokyo","region":"Tokyo","country":"Japan","country_code":"JP","timezone":{"id":"Asia/Tokyo"}}`, wantCity: "Tokyo"},
		{name: "rate limited", status: http.StatusTooManyRequests},
		{name: "application error", status: http.StatusOK, body: `{"success":false,"message":"Rate limit exceeded"}`},
		{name: "invalid JSON", status: http.StatusOK, body: `not-json`},
		{name: "different exit IP", status: http.StatusOK, body: `{"success":true,"ip":"2001:db8::2","city":"wrong"}`},
		{name: "bounded timeout", timeout: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				require.Equal(t, "https", req.URL.Scheme)
				require.Equal(t, "ipwho.is", req.URL.Host)
				require.Equal(t, "/"+exitIP, req.URL.Path)
				deadline, ok := req.Context().Deadline()
				require.True(t, ok, "geo lookup must have its own timeout")
				require.LessOrEqual(t, time.Until(deadline), defaultProxyGeoLookupTimeout)
				if tt.timeout {
					return nil, context.DeadlineExceeded
				}
				return &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader(tt.body)), Header: make(http.Header)}, nil
			})}
			// CFtrace 已确认出口和国家；归属仅补全空字段，任何失败保留原结果。
			info := &service.ProxyExitInfo{IP: exitIP, CountryCode: "JP"}
			(&proxyProbeService{}).enrichExitInfo(context.Background(), client, info)
			require.Equal(t, exitIP, info.IP)
			require.Equal(t, "JP", info.CountryCode)
			require.Equal(t, tt.wantCity, info.City)
			if tt.wantCity != "" {
				require.Equal(t, "Tokyo", info.Region)
				require.Equal(t, "Japan", info.Country)
				require.Equal(t, "Asia/Tokyo", info.Timezone)
			}
		})
	}
}

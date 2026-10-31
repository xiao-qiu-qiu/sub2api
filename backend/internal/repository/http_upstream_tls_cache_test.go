package repository

import (
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestHTTPUpstreamTLSCacheProfileContent(t *testing.T) {
	tests := []struct {
		name string
		edit func(*tlsfingerprint.Profile)
	}{
		{"cipher_order", func(p *tlsfingerprint.Profile) {
			p.CipherSuites[0], p.CipherSuites[1] = p.CipherSuites[1], p.CipherSuites[0]
		}},
		{"curves", func(p *tlsfingerprint.Profile) { p.Curves[0] = 23 }},
		{"point_formats", func(p *tlsfingerprint.Profile) { p.PointFormats[0] = 1 }},
		{"grease", func(p *tlsfingerprint.Profile) { p.EnableGREASE = true }},
		{"signature_algorithms", func(p *tlsfingerprint.Profile) { p.SignatureAlgorithms[0] = 0x0804 }},
		{"alpn", func(p *tlsfingerprint.Profile) { p.ALPNProtocols[0] = "h2" }},
		{"supported_versions", func(p *tlsfingerprint.Profile) { p.SupportedVersions[0] = 0x0303 }},
		{"key_share_groups", func(p *tlsfingerprint.Profile) { p.KeyShareGroups[0] = 23 }},
		{"psk_modes", func(p *tlsfingerprint.Profile) { p.PSKModes[0] = 0 }},
		{"extension_order", func(p *tlsfingerprint.Profile) { p.Extensions[0], p.Extensions[1] = p.Extensions[1], p.Extensions[0] }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewHTTPUpstream(&config.Config{Gateway: config.GatewayConfig{
				ConnectionPoolIsolation: config.ConnectionPoolIsolationProxy,
			}}).(*httpUpstreamService)
			original := newTLSCacheTestProfile()
			entry, err := svc.acquireClientWithTLS("http://proxy.example:8080", 41, 1, original, service.HTTPUpstreamProfileDefault)
			require.NoError(t, err)

			copy := newTLSCacheTestProfile()
			reused, err := svc.acquireClientWithTLS("http://proxy.example:8080", 41, 1, copy, service.HTTPUpstreamProfileDefault)
			require.NoError(t, err)
			require.Same(t, entry, reused, "equal profile contents must reuse the cached client")

			tt.edit(copy)
			require.Equal(t, original.Name, copy.Name)
			changed, err := svc.acquireClientWithTLS("http://proxy.example:8080", 41, 1, copy, service.HTTPUpstreamProfileDefault)
			require.NoError(t, err)
			require.NotSame(t, entry.client.Transport, changed.client.Transport, "same-name profile edits must rebuild the transport")
			require.Len(t, svc.clients, 2)
		})
	}
}

func TestHTTPUpstreamTLSCacheAccountIsolation(t *testing.T) {
	for _, isolation := range []string{
		config.ConnectionPoolIsolationProxy,
		config.ConnectionPoolIsolationAccount,
		config.ConnectionPoolIsolationAccountProxy,
	} {
		t.Run(isolation, func(t *testing.T) {
			svc := NewHTTPUpstream(&config.Config{Gateway: config.GatewayConfig{
				ConnectionPoolIsolation: isolation,
			}}).(*httpUpstreamService)
			first, err := svc.acquireClientWithTLS("http://proxy.example:8080", 41, 1, newTLSCacheTestProfile(), service.HTTPUpstreamProfileDefault)
			require.NoError(t, err)
			second, err := svc.acquireClientWithTLS("http://proxy.example:8080", 42, 1, newTLSCacheTestProfile(), service.HTTPUpstreamProfileDefault)
			require.NoError(t, err)
			require.NotSame(t, first.client.Transport, second.client.Transport, "TLS transports must remain account-isolated even for identical profiles")
			reused, err := svc.acquireClientWithTLS("http://proxy.example:8080", 41, 1, newTLSCacheTestProfile(), service.HTTPUpstreamProfileDefault)
			require.NoError(t, err)
			require.Same(t, first, reused)
			require.Len(t, svc.clients, 2)
		})
	}
}

func TestHTTPUpstreamTLSCacheProfileChangesRespectLRU(t *testing.T) {
	svc := NewHTTPUpstream(&config.Config{Gateway: config.GatewayConfig{
		ConnectionPoolIsolation: config.ConnectionPoolIsolationProxy,
		MaxUpstreamClients:      2,
	}}).(*httpUpstreamService)
	acquire := func(cipher uint16) (*upstreamClientEntry, error) {
		profile := &tlsfingerprint.Profile{Name: "random", CipherSuites: []uint16{cipher}}
		return svc.acquireClientWithTLS("http://proxy.example:8080", 41, 1, profile, service.HTTPUpstreamProfileDefault)
	}
	first, err := acquire(0x1301)
	require.NoError(t, err)
	second, err := acquire(0x1302)
	require.NoError(t, err)
	blocked, err := acquire(0x1303)
	require.ErrorIs(t, err, errUpstreamClientLimitReached)
	require.Nil(t, blocked)
	require.Len(t, svc.clients, 2)

	atomic.AddInt64(&first.inFlight, -1)
	third, err := acquire(0x1303)
	require.NoError(t, err)
	require.False(t, hasEntry(svc, first), "the idle profile variant should be evicted")
	require.True(t, hasEntry(svc, second), "an active profile variant must be retained")
	require.True(t, hasEntry(svc, third))
	require.Len(t, svc.clients, 2)

	atomic.AddInt64(&second.inFlight, -1)
	atomic.AddInt64(&third.inFlight, -1)
	for cipher := uint16(0x1304); cipher < 0x1308; cipher++ {
		entry, err := acquire(cipher)
		require.NoError(t, err)
		atomic.AddInt64(&entry.inFlight, -1)
		require.Len(t, svc.clients, 2, "successive profile edits must stay within the cache limit")
	}
}

func newTLSCacheTestProfile() *tlsfingerprint.Profile {
	return &tlsfingerprint.Profile{
		Name:                "same-name",
		CipherSuites:        []uint16{0x1301, 0x1302},
		Curves:              []uint16{29},
		PointFormats:        []uint16{0},
		SignatureAlgorithms: []uint16{0x0403},
		ALPNProtocols:       []string{"http/1.1"},
		SupportedVersions:   []uint16{0x0304},
		KeyShareGroups:      []uint16{29},
		PSKModes:            []uint16{1},
		Extensions:          []uint16{0, 10, 11, 13, 16, 43, 45, 51},
	}
}

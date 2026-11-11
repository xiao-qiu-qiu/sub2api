package bpsws

import (
	"context"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
)

// Keep opaque transport wrappers intact. For a concrete net/http transport,
// clone its configuration and bind raw sockets before CONNECT/TLS; net/http
// otherwise detaches connection setup from request cancellation. Wrapped
// transports retain their own dial lifecycle, with GotConn providing an extra
// cancellation hook without unwrapping or bypassing their policy/observers.
func scopedClient(ctx context.Context, original *http.Client) (*http.Client, context.Context, func()) {
	scope, cancel := context.WithCancel(ctx)
	client := *original
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Transport == nil {
		client.Transport = http.DefaultTransport
	}
	closeIdle := func() {}
	if base, ok := client.Transport.(*http.Transport); ok {
		transport := base.Clone()
		transport.DialContext = scopedDial(scope, base.DialContext, base.Dial)
		if base.DialTLSContext != nil || base.DialTLS != nil {
			transport.DialTLSContext = scopedDial(scope, base.DialTLSContext, base.DialTLS)
		}
		client.Transport = transport
		closeIdle = transport.CloseIdleConnections
	}
	var once sync.Once
	release := func() { once.Do(func() { cancel(); closeIdle() }) }
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if info.Conn != nil {
			context.AfterFunc(scope, func() { _ = info.Conn.Close() })
			if scope.Err() != nil {
				_ = info.Conn.Close()
			}
		}
	}}
	return &client, httptrace.WithClientTrace(scope, trace), release
}

func scopedDial(scope context.Context, dial func(context.Context, string, string) (net.Conn, error), legacy func(string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	if dial == nil {
		if legacy != nil {
			dial = func(_ context.Context, network, address string) (net.Conn, error) { return legacy(network, address) }
		} else {
			dial = (&net.Dialer{}).DialContext
		}
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		combined, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(scope, cancel)
		defer cancel()
		defer stop()
		if scope.Err() != nil {
			return nil, scope.Err()
		}
		conn, err := dial(combined, network, address)
		if err != nil {
			return nil, err
		}
		bound := &scopedConn{Conn: conn}
		bound.stop = context.AfterFunc(scope, func() { _ = conn.Close() })
		if scope.Err() != nil {
			_ = bound.Close()
			return nil, scope.Err()
		}
		return bound, nil
	}
}

type scopedConn struct {
	net.Conn
	stop func() bool
}

func (c *scopedConn) Close() error {
	c.stop()
	return c.Conn.Close()
}

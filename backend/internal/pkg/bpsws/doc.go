// Package bpsws adapts a prepared Basispoints Responses HTTP request to a
// request-scoped WebSocket and exposes its events as an SSE response body.
// It never reconnects, replays a request, or falls back to another transport.
//
// Implemented against the public wire contract documented by websocket.go and
// scoped_client.go in https://github.com/sunboyzoey/bps-local-public at commit
// 7134964. That source's LICENSE separately describes protocol-component terms;
// its baseline Unlicense is not a blanket license for those components.
package bpsws

//go:build unit

package service

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Gate the real upgraded socket, below coder/websocket's WriteFrame. Handshake
// writes pass through; tests arm it only after the first upstream generation.
type stickyMigrationWSSocketGate struct {
	net.Conn
	armed   atomic.Bool
	blocked atomic.Bool
	entered chan struct{}
	release chan error
	once    sync.Once
}

func (g *stickyMigrationWSSocketGate) Write(payload []byte) (int, error) {
	if g.armed.Load() && g.blocked.CompareAndSwap(false, true) {
		close(g.entered)
		if err := <-g.release; err != nil {
			return 0, err
		}
	}
	return g.Conn.Write(payload)
}

func (g *stickyMigrationWSSocketGate) unblock(err error) {
	g.once.Do(func() { g.release <- err })
}

type stickyMigrationWSUpgradeWriter struct {
	http.ResponseWriter
	gateReady chan<- *stickyMigrationWSSocketGate
}

func (w *stickyMigrationWSUpgradeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffered, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, err
	}
	gate := &stickyMigrationWSSocketGate{Conn: conn, entered: make(chan struct{}), release: make(chan error, 1)}
	w.gateReady <- gate
	return gate, bufio.NewReadWriter(buffered.Reader, bufio.NewWriter(gate)), nil
}

type stickyMigrationWSPassthrough struct {
	svc      *OpenAIGatewayService
	cache    *stickyMigrationTestCache
	request  OpenAIAccountScheduleRequest
	account  *Account
	ctx      context.Context
	upstream *stagedPassthroughConn
	client   *coderws.Conn
	gate     *stickyMigrationWSSocketGate
	settled  chan int
	finished chan struct{}
}

func newStickyMigrationWSPassthrough(t *testing.T, billConcurrently bool) *stickyMigrationWSPassthrough {
	t.Helper()
	base, _, cache, req := newStickyMigrationFixture(t)
	upstream := newStagedPassthroughConn()
	cfg := passthroughLifecycleConfig()
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 5
	cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 5
	svc := newPassthroughLifecycleService(cfg, upstream)
	svc.settingService, svc.cache = base.settingService, cache
	account := passthroughLifecycleAccount()
	account.ID, account.GroupIDs = 2, []int64{*req.GroupID}
	req.RequestedModel, req.HistoryModel = "gpt-5.1", "gpt-5.1"
	ctx, cancel := context.WithCancel(context.Background())
	ctx = stickyMigrationTestContext(t, svc, ctx, req)
	f := &stickyMigrationWSPassthrough{svc: svc, cache: cache, request: req, account: account, ctx: ctx, upstream: upstream, settled: make(chan int, 4), finished: make(chan struct{})}
	gateReady := make(chan *stickyMigrationWSSocketGate, 1)
	billingStop := make(chan struct{})
	var billing sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(f.finished)
		conn, err := coderws.Accept(&stickyMigrationWSUpgradeWriter{ResponseWriter: w, gateReady: gateReady}, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionDisabled})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, first, err := ReadOpenAIWSClientMessage(ctx, conn, 3*time.Second, coderws.StatusPolicyViolation, "missing first frame")
		if err != nil {
			return
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r.Clone(ctx)
		c.Set("api_key", &APIKey{GroupID: req.GroupID, Group: &Group{ID: *req.GroupID, Platform: PlatformOpenAI}})
		hooks := &OpenAIWSIngressHooks{AfterTurn: func(turn int, result *OpenAIForwardResult, forwardErr error) {
			if turn == 1 {
				// Mirror the handler's first-turn hook: an observed terminal must
				// not commit before the adapter delivers it to the client.
				svc.CompleteOpenAIStickyMigration(ctx, account, result, forwardErr)
				if billConcurrently && result != nil {
					billing.Add(1)
					go func() {
						defer billing.Done()
						requested := "priority"
						for {
							select {
							case <-billingStop:
								return
							default:
							}
							result.ServiceTier = &requested
							ApplyOpenAIServiceTierBillingResolution(account, result)
							runtime.Gosched()
						}
					}()
				}
			}
			f.settled <- turn
		}}
		_ = svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "sk-test", first, hooks)
	}))
	f.client = dialPassthroughLifecycleClient(t, server)
	select {
	case f.gate = <-gateReady:
	case <-time.After(3 * time.Second):
		t.Fatal("upgraded socket was not captured")
	}
	t.Cleanup(func() {
		f.gate.unblock(io.ErrClosedPipe)
		cancel()
		_ = f.client.CloseNow()
		_ = upstream.Close()
		select {
		case <-f.finished:
		case <-time.After(3 * time.Second):
			t.Error("passthrough adapter failed to join its relay goroutines")
		}
		close(billingStop)
		billing.Wait()
		server.Close()
	})
	require.Equal(t, "response.create", gjson.GetBytes(requirePassthroughUpstreamWrite(t, upstream, time.Second), "type").String())
	return f
}

func (f *stickyMigrationWSPassthrough) casCount() int {
	f.cache.mu.Lock()
	defer f.cache.mu.Unlock()
	return len(f.cache.calls)
}

func (f *stickyMigrationWSPassthrough) terminal(event, status, id string) {
	f.upstream.Send(`{"type":"` + event + `","response":{"id":"` + id + `","model":"gpt-5.1","status":"` + status + `","service_tier":"default","usage":{"input_tokens":7,"output_tokens":2}}}`)
}

func (f *stickyMigrationWSPassthrough) waitFinished(t *testing.T) {
	t.Helper()
	select {
	case <-f.finished:
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough adapter did not finish")
	}
}

func TestOpenAIStickyMigrationPassthroughWaitsForTerminalDelivery(t *testing.T) {
	f := newStickyMigrationWSPassthrough(t, true)
	f.gate.armed.Store(true)
	f.terminal("response.completed", "completed", "resp_delivery")
	select {
	case <-f.gate.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("terminal WriteFrame did not reach the physical client socket")
	}
	require.Equal(t, 1, <-f.settled, "the ingress settlement hook runs before the terminal write")
	require.Zero(t, f.casCount(), "neither the ingress hook nor the blocked write may migrate")
	requireStickyMigrationOwner(t, f.cache, f.request, 1)
	f.gate.unblock(nil)
	payload, err := readPassthroughLifecycleFrame(t, f.client, 3*time.Second)
	require.NoError(t, err)
	require.Equal(t, "response.completed", gjson.GetBytes(payload, "type").String())
	require.Eventually(t, func() bool { return f.casCount() == 1 }, time.Second, time.Millisecond)
	requireStickyMigrationOwner(t, f.cache, f.request, 2)
	_ = f.client.CloseNow()
	f.waitFinished(t)
	require.Equal(t, 1, f.casCount(), "closing the connection must not repeat completion")
}

func TestOpenAIStickyMigrationPassthroughTerminalWriteFailureKeepsOwner(t *testing.T) {
	f := newStickyMigrationWSPassthrough(t, false)
	f.gate.armed.Store(true)
	f.terminal("response.completed", "completed", "resp_write_failure")
	select {
	case <-f.gate.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("terminal WriteFrame was not attempted")
	}
	require.Zero(t, f.casCount())
	f.gate.unblock(errors.New("synthetic terminal socket write failure"))
	f.waitFinished(t)
	require.Zero(t, f.casCount(), "a failed physical terminal write cannot commit migration")
	requireStickyMigrationOwner(t, f.cache, f.request, 1)
}

func TestOpenAIStickyMigrationPassthroughUnfinishedTurnKeepsOwner(t *testing.T) {
	f := newStickyMigrationWSPassthrough(t, false)
	f.upstream.Send(`{"type":"response.output_text.delta","response_id":"resp_unfinished","delta":"partial"}`)
	payload, err := readPassthroughLifecycleFrame(t, f.client, 3*time.Second)
	require.NoError(t, err)
	require.Equal(t, "response.output_text.delta", gjson.GetBytes(payload, "type").String())
	f.upstream.Fail(io.EOF)
	// Consume and acknowledge the adapter's close frame; otherwise its normal
	// WebSocket close handshake waits for a client that this test is not reading.
	_, err = readPassthroughLifecycleFrame(t, f.client, 3*time.Second)
	require.Error(t, err)
	f.waitFinished(t)
	require.Zero(t, f.casCount(), "EOF after partial output is not terminal success")
	requireStickyMigrationOwner(t, f.cache, f.request, 1)
}

func TestOpenAIStickyMigrationPassthroughSecondTurnDoesNotCompleteFirstMigration(t *testing.T) {
	f := newStickyMigrationWSPassthrough(t, false)
	f.terminal("response.incomplete", "incomplete", "resp_first_incomplete")
	_, err := readPassthroughLifecycleFrame(t, f.client, 3*time.Second)
	require.NoError(t, err)
	require.Zero(t, f.casCount())
	writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	err = f.client.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"next"}`))
	cancel()
	require.NoError(t, err)
	require.Equal(t, "response.create", gjson.GetBytes(requirePassthroughUpstreamWrite(t, f.upstream, time.Second), "type").String())
	f.terminal("response.completed", "completed", "resp_second_completed")
	payload, err := readPassthroughLifecycleFrame(t, f.client, 3*time.Second)
	require.NoError(t, err)
	require.Equal(t, "resp_second_completed", gjson.GetBytes(payload, "response.id").String())
	_ = f.client.CloseNow()
	f.waitFinished(t)
	require.Zero(t, f.casCount(), "second-turn success must not submit the first turn's staged token")
	requireStickyMigrationOwner(t, f.cache, f.request, 1)
	require.False(t, stickyMigrationFromContext(f.ctx).completed.Load())
}

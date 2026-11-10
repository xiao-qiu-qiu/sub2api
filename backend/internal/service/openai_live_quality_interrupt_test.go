package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

type liveQualityConn struct {
	*liveTestFrameConn
	reading chan struct{}
	once    sync.Once
}

func (c *liveQualityConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	c.once.Do(func() { close(c.reading) })
	typ, data, err := c.liveTestFrameConn.ReadFrame(ctx)
	// Match coder/websocket: cancellation of an active read closes the socket.
	if ctx.Err() != nil {
		_ = c.Close()
	}
	return typ, data, err
}

type liveQualityDialer struct {
	conn        *liveQualityConn
	calls       atomic.Int32
	firstErr    error
	dialed      chan int32
	connections []*liveQualityConn
}

func (d *liveQualityDialer) Dial(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
	n := d.calls.Add(1)
	d.dialed <- n
	if n == 1 && d.firstErr != nil {
		return nil, 0, nil, d.firstErr
	}
	if int(n) <= len(d.connections) {
		return d.connections[n-1], http.StatusSwitchingProtocols, nil, nil
	}
	return d.conn, http.StatusSwitchingProtocols, nil, nil
}

func liveQualityFixture(t *testing.T, id int64) (*OpenAIGatewayService, *LiveCallRecord, *liveQualityDialer, *liveTestConcurrencyCache, *liveTestUsageRepo) {
	t.Helper()
	account := &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, GroupIDs: []int64{55}, Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"}}
	record := &LiveCallRecord{CallID: fmt.Sprintf("call_quality_%d", id), AccountID: id, APIKeyID: 22, UserID: 33, GroupID: 55, LeaseID: "quality-live-lease", Model: "gpt-live-test", Controller: LiveControllerPending, CreatedAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Hour)}
	record.CallHash = hashLiveCallID(record.CallID)
	cipher := newLiveAttestationCipher(&config.Config{JWT: config.JWTConfig{Secret: "live-quality-test-secret"}})
	var err error
	record.AttestationCiphertext, err = cipher.Encrypt(`{"v":1,"s":0,"t":"v1.quality-test"}`)
	require.NoError(t, err)
	store := &liveTestStore{}
	require.NoError(t, store.SaveLiveCall(t.Context(), record, time.Hour))
	dialer := &liveQualityDialer{conn: &liveQualityConn{liveTestFrameConn: newLiveTestFrameConn(), reading: make(chan struct{})}, dialed: make(chan int32, 8)}
	cache := &liveTestConcurrencyCache{}
	usage := &liveTestUsageRepo{}
	svc := &OpenAIGatewayService{cache: store, accountRepo: &liveTestAccountRepo{account: account}, concurrencyService: NewConcurrencyService(cache), usageLogRepo: usage, liveAttestationCipher: cipher, openaiWSPassthroughDialer: dialer}
	return svc, record, dialer, cache, usage
}

func requireLiveQualityClosed(t *testing.T, svc *OpenAIGatewayService, record *LiveCallRecord, dialer *liveQualityDialer, cache *liveTestConcurrencyCache, usage *liveTestUsageRepo) {
	t.Helper()
	select {
	case frame := <-dialer.conn.writes:
		require.JSONEq(t, `{"type":"session.close"}`, string(frame.payload))
	case <-time.After(time.Second):
		t.Fatal("quality interruption never sent upstream session.close")
	}
	select {
	case <-dialer.conn.closed:
	default:
		t.Fatal("upstream sideband was left open")
	}
	store, err := svc.liveStore()
	require.NoError(t, err)
	loaded, err := store.GetLiveCall(t.Context(), record.CallHash)
	require.NoError(t, err)
	require.Equal(t, LiveControllerClosed, loaded.Controller)
	claimed, err := store.ClaimLiveController(t.Context(), record.CallHash, LiveControllerProxy, "late-reconnect")
	require.NoError(t, err)
	require.False(t, claimed, "a closed call must not permit a new controller")
	cache.mu.Lock()
	releases := cache.releases
	cache.mu.Unlock()
	require.Equal(t, 1, releases)
	usage.mu.Lock()
	count := len(usage.logs)
	usage.mu.Unlock()
	require.Equal(t, 1, count)
	require.Zero(t, accountrequest.Default.Cancel(record.AccountID, time.Now()), "registration must finish with the call")
}

func TestLiveQualityInterruptProxyTerminatesSessionAndLease(t *testing.T) {
	svc, record, dialer, cache, usage := liveQualityFixture(t, 9961951)
	done := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		done <- svc.ProxyLiveSideband(r.Context(), record, conn)
	}))
	defer server.Close()
	client, _, err := coderws.Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()
	select {
	case <-dialer.conn.reading:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not start upstream relay")
	}
	require.Positive(t, accountrequest.Default.Cancel(record.AccountID, time.Now()))
	select {
	case err := <-done:
		require.ErrorIs(t, err, accountrequest.ErrQualityDegraded)
	case <-time.After(time.Second):
		t.Fatal("Live proxy waited for session expiry")
	}
	requireLiveQualityClosed(t, svc, record, dialer, cache, usage)
	require.EqualValues(t, 1, dialer.calls.Load(), "must not restart the observer")
}

func TestLiveQualityInterruptObserverTerminatesSessionAndLease(t *testing.T) {
	svc, record, dialer, cache, usage := liveQualityFixture(t, 9961952)
	done := make(chan struct{})
	go func() { defer close(done); svc.observeLiveCall(record) }()
	select {
	case <-dialer.conn.reading:
	case <-time.After(time.Second):
		t.Fatal("observer did not start")
	}
	require.Positive(t, accountrequest.Default.Cancel(record.AccountID, time.Now()))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("observer did not stop on quality interruption")
	}
	requireLiveQualityClosed(t, svc, record, dialer, cache, usage)
	require.EqualValues(t, 1, dialer.calls.Load())
}

func TestLiveQualityInterruptDuringRetryOnlyReconnectsToClose(t *testing.T) {
	svc, record, dialer, cache, usage := liveQualityFixture(t, 9961953)
	dialer.firstErr = errors.New("temporary sideband dial failure")
	done := make(chan struct{})
	go func() { defer close(done); svc.observeLiveCall(record) }()
	select {
	case <-dialer.dialed:
	case <-time.After(time.Second):
		t.Fatal("observer did not attempt its connection")
	}
	require.Positive(t, accountrequest.Default.Cancel(record.AccountID, time.Now()))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("quality interruption was stuck in observer backoff")
	}
	requireLiveQualityClosed(t, svc, record, dialer, cache, usage)
	require.EqualValues(t, 2, dialer.calls.Load(), "only the failed attempt and a close-only connection are permitted")
}

func TestLiveQualityMovedAccountCanResumeOlderCall(t *testing.T) {
	svc, record, dialer, cache, usage := liveQualityFixture(t, 9961954)
	repo, ok := svc.accountRepo.(*liveTestAccountRepo)
	require.True(t, ok)
	repo.account.GroupIDs = []int64{99}
	record.RequestStartedAt = time.Now().Add(-2 * time.Minute)
	ctx, finish := accountrequest.Default.Begin(liveRequestStartContext(t.Context(), record), record.AccountID)
	defer finish()
	require.True(t, accountrequest.StartedAt(ctx).Equal(record.RequestStartedAt))
	require.Zero(t, accountrequest.Default.CancelWindow(record.AccountID, time.Now().Add(-time.Minute), time.Now()))
	conn, err := svc.dialLiveSideband(ctx, record)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.EqualValues(t, 1, dialer.calls.Load(), "moving groups must not prevent old calls from reconnecting")
	require.Empty(t, usage.logs)
	require.Zero(t, cache.releases)
}

func TestLiveQualityObserverHandoffKeepsOriginalCancellation(t *testing.T) {
	svc, record, dialer, cache, usage := liveQualityFixture(t, 9961955)
	client, cancelClient := context.WithCancel(context.Background())
	ctx, finish := accountrequest.Default.Begin(client, record.AccountID)
	cancelClient()
	cutoff := time.Now()
	svc.startLiveObserver(ctx, record, finish)
	select {
	case <-dialer.conn.reading:
	case <-time.After(time.Second):
		t.Fatal("ordinary client disconnect prevented observer handoff")
	}
	require.Positive(t, accountrequest.Default.Cancel(record.AccountID, cutoff), "handoff must retain its registration time")
	require.Eventually(t, func() bool { usage.mu.Lock(); defer usage.mu.Unlock(); return len(usage.logs) == 1 }, time.Second, time.Millisecond)
	requireLiveQualityClosed(t, svc, record, dialer, cache, usage)
}

func TestLiveQualityObserverToProxyHandoffKeepsNewController(t *testing.T) {
	svc, record, dialer, cache, usage := liveQualityFixture(t, 9961956)
	oldConn := dialer.conn
	newConn := &liveQualityConn{liveTestFrameConn: newLiveTestFrameConn(), reading: make(chan struct{})}
	dialer.connections = []*liveQualityConn{oldConn, newConn}
	observerDone := make(chan struct{})
	go func() { defer close(observerDone); svc.observeLiveCall(record) }()
	select {
	case <-oldConn.reading:
	case <-time.After(time.Second):
		t.Fatal("old observer did not start")
	}
	proxyDone := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			proxyDone <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		proxyDone <- svc.ProxyLiveSideband(r.Context(), record, conn)
	}))
	defer server.Close()
	client, _, err := coderws.Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()
	select {
	case <-newConn.reading:
	case <-time.After(2 * time.Second):
		t.Fatal("new proxy did not acquire control")
	}
	select {
	case <-observerDone:
	case <-time.After(time.Second):
		t.Fatal("old observer did not relinquish control")
	}
	select {
	case frame := <-oldConn.writes:
		t.Fatalf("ordinary control handoff must not terminate media: %s", frame.payload)
	default:
	}
	require.Positive(t, accountrequest.Default.Cancel(record.AccountID, time.Now()))
	select {
	case err := <-proxyDone:
		require.ErrorIs(t, err, accountrequest.ErrQualityDegraded)
	case <-time.After(time.Second):
		t.Fatal("quality interrupt did not stop replacement controller")
	}
	dialer.conn = newConn // both workers have returned; inspect the replacement.
	requireLiveQualityClosed(t, svc, record, dialer, cache, usage)
	require.EqualValues(t, 2, dialer.calls.Load())
}

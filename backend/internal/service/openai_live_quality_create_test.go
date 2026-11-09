//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type liveQualityCreateUpstream struct{ liveLoopUpstreamStub }

func (s *liveQualityCreateUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	s.calls++
	accountrequest.Default.Cancel(accountID, time.Now())
	return nil, context.Cause(req.Context())
}
func (s *liveQualityCreateUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxy, accountID, concurrency)
}

func TestLiveQualityInterruptCreateDoesNotRetryAndReleasesJointLease(t *testing.T) {
	svc, cache, store := newLiveLoopFixture(t, &liveLoopUpstreamStub{}, true)
	upstream := &liveQualityCreateUpstream{}
	svc.httpUpstream = upstream
	created, err := svc.CreateLiveCall(t.Context(), liveLoopRequest(), liveLoopIdentity(), 1)
	require.Nil(t, created)
	require.ErrorIs(t, err, accountrequest.ErrQualityDegraded)
	require.Equal(t, 1, upstream.calls)
	require.Equal(t, 1, cache.transferCalls)
	require.Zero(t, cache.migrateCalls, "quality interruption must not escape into another SDP attempt")
	require.Equal(t, 1, cache.accountRelease)
	require.Equal(t, 1, cache.releaseLeaseCall)
	require.Zero(t, store.saves)
}

//go:build unit

package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAcquireUserSlotWithWait_RecoveryReleasesWaitCountAndKeySlot(t *testing.T) {
	recovering := false
	cache := &helperConcurrencyCacheStub{
		userSeq: []bool{false}, waitAllowed: true,
		waitIncrementHook: func() { recovering = true },
	}
	helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second)
	c, _ := newHelperTestContext(http.MethodPost, "/v1/responses")
	c.Request = c.Request.WithContext(service.WithOAuthRecoveryAdmissionCheck(c.Request.Context(), func(context.Context) bool { return recovering }))
	streamStarted := false
	release, err := helper.acquireUserSlotWithWaitTimeout(c, 202, 1, 77, 0, 30*time.Second, false, &streamStarted)
	require.Nil(t, release)
	require.ErrorIs(t, err, service.ErrOAuthRecoveryAdmissionInterrupted)
	require.NoError(t, c.Request.Context().Err())
	require.Equal(t, 1, cache.waitIncrementCalls)
	require.Equal(t, 1, cache.waitDecrementCalls)
	require.Equal(t, 1, cache.apiKeyReleaseCalls)
	require.False(t, streamStarted)
}

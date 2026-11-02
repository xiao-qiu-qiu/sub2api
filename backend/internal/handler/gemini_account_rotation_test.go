package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type geminiRotationAuxSettings struct{ mode string }

func (s geminiRotationAuxSettings) GetAccountRotationSettings(context.Context) (*service.AccountRotationSettings, error) {
	settings := service.DefaultAccountRotationSettings()
	settings.Enabled, settings.Mode = true, s.mode
	return settings, nil
}

type geminiRotationAuxCache struct {
	service.ConcurrencyCache
	phase       string
	max         int
	sticky      bool
	releaseCall atomic.Int64
}

func (c *geminiRotationAuxCache) CheckAccountRotation(context.Context, int64, service.AccountRotationPolicy) (*service.AccountRotationState, error) {
	return &service.AccountRotationState{Phase: c.phase}, nil
}

func (c *geminiRotationAuxCache) AcquireAccountSlotWithRotation(_ context.Context, _ int64, max int, _ string, policy service.AccountRotationPolicy, sticky bool, _ string) (bool, error) {
	c.max, c.sticky = max, sticky
	return c.phase != "resting" && (c.phase != "draining" || policy.Mode == service.AccountRotationModeSticky && sticky), nil
}

func (*geminiRotationAuxCache) RefreshAccountRotationSlot(context.Context, int64, string) error {
	return nil
}

func (c *geminiRotationAuxCache) ReleaseAccountSlot(context.Context, int64, string) error {
	c.releaseCall.Add(1)
	return nil
}

func TestGeminiRotationMetadataTracksWithoutGenerationLimitUntilOwnerRelease(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := &geminiRotationAuxCache{phase: "draining", max: -1}
	concurrency := service.NewConcurrencyService(cache)
	concurrency.SetAccountRotation(geminiRotationAuxSettings{mode: service.AccountRotationModeSticky}, nil)
	h := &GatewayHandler{concurrencyHelper: &ConcurrencyHelper{concurrencyService: concurrency}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx = service.WithAccountRotationSticky(ctx, 7)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil).WithContext(ctx)
	account := service.MarkAccountRotationSelection(ctx, &service.Account{ID: 7, Concurrency: 1})
	release, admitted := h.acquireGeminiRotationMetadataSlot(c, account)
	require.True(t, admitted)
	require.Zero(t, cache.max, "metadata must not inherit the generation limit")
	require.True(t, cache.sticky)
	require.True(t, service.HasAccountRotationTracking(c.Request.Context()))
	cancel()
	require.Zero(t, cache.releaseCall.Load())
	release()
	release()
	require.EqualValues(t, 1, cache.releaseCall.Load())
}

func TestGeminiRotationMetadataRestingReturns503AndOffRemainsNoop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := &geminiRotationAuxCache{phase: "resting", max: -1}
	concurrency := service.NewConcurrencyService(cache)
	concurrency.SetAccountRotation(geminiRotationAuxSettings{mode: service.AccountRotationModeAggressive}, nil)
	h := &GatewayHandler{concurrencyHelper: &ConcurrencyHelper{concurrencyService: concurrency}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	release, admitted := h.acquireGeminiRotationMetadataSlot(c, &service.Account{ID: 7})
	require.False(t, admitted)
	require.Nil(t, release)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Zero(t, cache.releaseCall.Load())

	h = &GatewayHandler{}
	c, _ = gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	release, admitted = h.acquireGeminiRotationMetadataSlot(c, &service.Account{ID: 7})
	require.True(t, admitted)
	release()
	require.False(t, service.HasAccountRotationTracking(c.Request.Context()))
}

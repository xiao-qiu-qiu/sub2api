//go:build unit

package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSchedulerCacheQualityAdmissionRoundTrip(t *testing.T) {
	ctx := context.Background()
	cache, _ := newSchedulerCacheUnitWithRedis(t)
	now := time.Now().UTC()
	state := map[string]any{
		"enabled":             true,
		"required_after":      now.Add(-time.Minute).Format(time.RFC3339Nano),
		"checked_started_at":  now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
		"checked_finished_at": now.Add(-30 * time.Second).Format(time.RFC3339Nano),
	}
	account := service.Account{
		ID: 42001, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{42000},
		Extra: map[string]any{service.QualityProbeAdmissionKey: state},
	}
	bucket := service.SchedulerBucket{GroupID: 42000, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{account}))

	assertRoundTrip := func(wantPending bool) {
		t.Helper()
		cached, err := cache.GetAccount(ctx, account.ID)
		require.NoError(t, err)
		require.NotNil(t, cached)
		require.Equal(t, state, cached.Extra[service.QualityProbeAdmissionKey], "full payload must preserve the complete admission state")
		require.Equal(t, wantPending, cached.QualityProbeAdmissionPending(now))

		raw, err := cache.rdb.Get(ctx, schedulerAccountMetaKey("42001")).Bytes()
		require.NoError(t, err)
		var metadata service.Account
		require.NoError(t, json.Unmarshal(raw, &metadata))
		require.Equal(t, state, metadata.Extra[service.QualityProbeAdmissionKey], "metadata whitelist must retain all admission timestamps")
		require.Equal(t, wantPending, metadata.QualityProbeAdmissionPending(now))

		snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
		require.NoError(t, err)
		require.True(t, hit)
		require.Len(t, snapshot, 1)
		require.Equal(t, state, snapshot[0].Extra[service.QualityProbeAdmissionKey])
		require.Equal(t, wantPending, snapshot[0].QualityProbeAdmissionPending(now), "candidate projection must enforce the same gate as the full account")
	}
	assertRoundTrip(true)
	state["checked_started_at"] = now.Add(-20 * time.Second).Format(time.RFC3339Nano)
	state["checked_finished_at"] = now.Add(-10 * time.Second).Format(time.RFC3339Nano)
	require.NoError(t, cache.SetAccount(ctx, &account))
	assertRoundTrip(false)
	state["required_after"] = now.Add(-5 * time.Second).Format(time.RFC3339Nano)
	require.NoError(t, cache.SetAccount(ctx, &account))
	assertRoundTrip(true)
	state["enabled"] = false
	require.NoError(t, cache.SetAccount(ctx, &account))
	assertRoundTrip(false)
}

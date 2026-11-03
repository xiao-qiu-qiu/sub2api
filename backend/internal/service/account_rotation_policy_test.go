package service

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccountRotationPolicyMapsHoldDefaultsAndCustomEarlyControls(t *testing.T) {
	svc, _ := newRotationHoldConcurrency(true)
	policy, err := svc.accountRotationPolicy(t.Context())
	require.NoError(t, err)
	require.True(t, policy.ScheduledHoldEnabled)
	require.Equal(t, 1200, policy.EarlyRestWindowSeconds)
	require.Equal(t, 2, policy.EarlyRestConcurrencyThreshold)
	require.Equal(t, 120, policy.EarlyRestSeconds)
	defaultRevision := policy.Revision
	for _, change := range []func(*AccountRotationSettings){
		func(s *AccountRotationSettings) { s.ScheduledHoldEnabled = false },
		func(s *AccountRotationSettings) { s.EarlyRestWindowMinutes = 1 },
		func(s *AccountRotationSettings) { s.EarlyRestConcurrencyThreshold = 3 },
		func(s *AccountRotationSettings) { s.EarlyRestSeconds = 7 },
	} {
		settings := *DefaultAccountRotationSettings()
		settings.Enabled = true
		change(&settings)
		svc.SetAccountRotation(openAIRotationSettingsStub{settings}, nil)
		policy, err := svc.accountRotationPolicy(t.Context())
		require.NoError(t, err)
		require.NotEqual(t, defaultRevision, policy.Revision, "fallback revision includes each new control")
	}
	settings := *DefaultAccountRotationSettings()
	settings.Enabled, settings.EarlyRestEnabled, settings.ScheduledHoldEnabled = true, true, false
	settings.EarlyRestWindowMinutes, settings.EarlyRestConcurrencyThreshold, settings.EarlyRestSeconds = 1, 3, 7
	settings.Revision = "server-owned"
	svc.SetAccountRotation(openAIRotationSettingsStub{settings}, nil)
	policy, err = svc.accountRotationPolicy(t.Context())
	require.NoError(t, err)
	require.False(t, policy.ScheduledHoldEnabled)
	require.True(t, policy.EarlyRestEnabled)
	require.Equal(t, 60, policy.EarlyRestWindowSeconds)
	require.Equal(t, 3, policy.EarlyRestConcurrencyThreshold)
	require.Equal(t, 7, policy.EarlyRestSeconds)
	require.Equal(t, "server-owned", policy.Revision)
	settings.Enabled = false
	svc.SetAccountRotation(openAIRotationSettingsStub{settings}, nil)
	policy, err = svc.accountRotationPolicy(t.Context())
	require.NoError(t, err)
	require.Zero(t, policy.WorkSeconds)
	require.False(t, policy.EarlyRestEnabled)
}

func TestAccountRotationStatesReturnHistoryWhenAutomaticDisabled(t *testing.T) {
	svc, cache := newRotationHoldConcurrency(false)
	cache.setState(1, AccountRotationState{Phase: "ready", LastManualRestAt: 123})
	cache.setState(2, AccountRotationState{Phase: "ready", LastAutomaticRestAt: 456, LastAutomaticRestReason: "early"})
	cache.setState(3, AccountRotationState{Phase: "ready"})
	cache.setState(4, AccountRotationState{Phase: "draining", Hold: true, Reason: "scheduled"})
	states, err := svc.GetAccountRotationStates(t.Context(), []int64{1, 2, 3, 4})
	require.NoError(t, err)
	require.Len(t, states, 3)
	require.EqualValues(t, 123, states[1].LastManualRestAt)
	require.EqualValues(t, 456, states[2].LastAutomaticRestAt)
	require.Equal(t, "early", states[2].LastAutomaticRestReason)
	require.NotContains(t, states, int64(3))
	require.True(t, states[4].Hold)
	for _, state := range states {
		require.False(t, state.AutomaticEnabled)
	}
	settings := *DefaultAccountRotationSettings()
	settings.Enabled = true
	svc.SetAccountRotation(openAIRotationSettingsStub{settings}, nil)
	states, err = svc.GetAccountRotationStates(t.Context(), []int64{1, 2, 3, 4})
	require.NoError(t, err)
	require.Len(t, states, 4)
	for _, state := range states {
		require.True(t, state.AutomaticEnabled)
	}
}

func TestAccountRotationManualRestSecondsValidationAndMinuteCompatibility(t *testing.T) {
	svc, _ := newRotationHoldConcurrency(false)
	for _, seconds := range []int{1, 7, 60, 7200} {
		state, err := svc.StartAccountManualRestSeconds(t.Context(), 7, seconds)
		require.NoError(t, err)
		require.Equal(t, seconds, state.RestSeconds)
	}
	for _, seconds := range []int{-1, 0, 7201, int(^uint(0) >> 1)} {
		_, err := svc.StartAccountManualRestSeconds(t.Context(), 7, seconds)
		require.Error(t, err)
	}
	_, err := svc.StartAccountManualRestSeconds(t.Context(), 0, 1)
	require.Error(t, err)
	for _, minutes := range []int{1, 120} {
		state, err := svc.StartAccountManualRest(t.Context(), 7, minutes)
		require.NoError(t, err)
		require.Equal(t, minutes*60, state.RestSeconds)
	}
	for _, minutes := range []int{-1, 0, 121, int(^uint(0) >> 1)} {
		_, err := svc.StartAccountManualRest(t.Context(), 7, minutes)
		require.Error(t, err)
	}
}

func TestAccountRotationScheduledHoldWaitStaysPinnedAcrossSettingsChange(t *testing.T) {
	for _, sticky := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "sticky"}[sticky], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, cache := newRotationHoldConcurrency(true)
				cache.setState(7, AccountRotationState{Phase: "draining", Hold: true, Reason: "scheduled"})
				ctx := svc.AccountRotationContext(t.Context())
				if sticky {
					ctx = WithAccountRotationSticky(ctx, 7)
				}
				done := make(chan rotationHoldAcquireOutcome, 1)
				go func() {
					result, err := svc.AcquireAccountSlot(ctx, 7, 2)
					done <- rotationHoldAcquireOutcome{result, err}
				}()
				synctest.Wait()
				cache.assertCounts(t, 7, 0, 1)
				settings := *DefaultAccountRotationSettings()
				settings.Enabled, settings.ScheduledHoldEnabled, settings.Mode = false, false, AccountRotationModeSticky
				svc.SetAccountRotation(openAIRotationSettingsStub{settings}, nil)
				time.Sleep(2 * time.Second)
				synctest.Wait()
				requireRotationHoldPending(t, done)
				cache.setState(7, AccountRotationState{Phase: "resting", Hold: true, Reason: "scheduled"})
				time.Sleep(time.Second)
				synctest.Wait()
				requireRotationHoldPending(t, done)
				cache.mu.Lock()
				cache.full[7] = true
				cache.mu.Unlock()
				cache.setState(7, AccountRotationState{Phase: "ready"})
				time.Sleep(time.Second)
				synctest.Wait()
				requireRotationHoldPending(t, done)
				cache.mu.Lock()
				cache.full[7] = false
				cache.mu.Unlock()
				time.Sleep(time.Second)
				outcome := <-done
				require.NoError(t, outcome.err)
				require.True(t, outcome.result.Acquired)
				outcome.result.ReleaseFunc()
				cache.assertCounts(t, 7, 0, 0)
				require.Greater(t, len(cache.calls), 2)
				for i, call := range cache.calls {
					require.EqualValues(t, 7, call.accountID)
					require.Equal(t, sticky, call.sticky)
					if i > 0 {
						require.Zero(t, call.policy.WorkSeconds)
						require.False(t, call.policy.ScheduledHoldEnabled)
					}
				}
			})
		})
	}
}

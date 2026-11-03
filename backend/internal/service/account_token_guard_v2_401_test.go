package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type tokenGuard401Prober struct {
	err         error
	freshCalls  int
	cachedCalls int
	onProbe     func()
}

func (p *tokenGuard401Prober) FetchOpenAIModelsList(context.Context, *Account) (*OpenAIModelsResponse, error) {
	p.cachedCalls++
	return nil, nil // A stale success must never mask the upstream 401.
}

func (p *tokenGuard401Prober) FetchOpenAIRecoveryModelsList(context.Context, *Account) (*OpenAIModelsResponse, error) {
	p.freshCalls++
	if p.onProbe != nil {
		p.onProbe()
	}
	return nil, p.err
}

func tokenGuard401Error(status int) error {
	return &codexModelsManifestUpstreamError{err: errors.New("synthetic rejection"), statusCode: status,
		body: []byte(`{"error":{"code":"token_invalidated"}}`)}
}

func TestTokenGuard401ConfirmationDelayAndRestart(t *testing.T) {
	for _, tc := range []struct{ delay, threshold int }{{30, 1}, {45, 10}, {1, 2}} {
		t.Run(fmt.Sprintf("delay=%d/threshold=%d", tc.delay, tc.threshold), func(t *testing.T) {
			reauth, reader, tasks, _, _, _ := newReauthTestService("acct-1")
			defer reauth.oauth.Stop()
			require.NoError(t, tasks.UpsertConfig(t.Context(), reauthEmailConfig(42, "user@example.test", "encrypted:https://mail.example.test/code")))
			repo := &accountTokenGuardV2TestRepo{record: &AccountTokenGuardV2Record{AccountID: 42, Enabled: true, AutoReloginEnabled: true}}
			repo.createTask = tasks.CreateTask
			prober := &tokenGuard401Prober{err: tokenGuard401Error(http.StatusUnauthorized)}
			rules := defaultAccountTokenGuardV2Rules()
			rules.UnauthorizedRetryIntervalSeconds, rules.FailStreakThreshold = tc.delay, tc.threshold
			raw, err := json.Marshal(rules)
			require.NoError(t, err)
			settings := &accountTokenGuardV2TestSettings{raw: string(raw)}
			now := time.Now().UTC()
			svc := NewAccountTokenGuardV2Service(repo, settings, reader, prober, reauth)
			defer svc.cancel()
			svc.now = func() time.Time { return now }
			// Time the delay from the response, not the beginning of a slow request.
			prober.onProbe = func() { now = now.Add(3 * time.Second) }
			svc.runProbe(t.Context(), *repo.record)
			prober.onProbe = nil
			retryAt := now.Add(time.Duration(tc.delay) * time.Second)
			require.Equal(t, retryAt, repo.record.NextProbeAt)
			require.Equal(t, &retryAt, repo.record.UnauthorizedRetryAt)
			require.Zero(t, tasks.createCalls, "even threshold=1 must wait for confirmation")
			require.Zero(t, prober.cachedCalls)
			require.Equal(t, 1, prober.freshCalls)

			// A replacement service must use the persisted deadline, and an
			// early manual probe must not trigger re-login or push it backwards.
			svc = NewAccountTokenGuardV2Service(repo, settings, reader, prober, reauth)
			defer svc.cancel()
			svc.now = func() time.Time { return now }
			now = retryAt.Add(-time.Millisecond)
			svc.runProbe(t.Context(), *repo.record)
			require.Zero(t, tasks.createCalls)
			require.Equal(t, retryAt, repo.record.NextProbeAt)
			now = retryAt
			svc.runProbe(t.Context(), *repo.record)
			require.Equal(t, 1, tasks.createCalls, "confirmed 401 ignores the general failure threshold")
			require.Nil(t, repo.record.UnauthorizedRetryAt)
			require.Empty(t, repo.record.UnauthorizedCredentialsHash)
			require.Equal(t, OpenAIOAuthReauthStatusQueued, tasks.task.Status)
		})
	}
}

func TestTokenGuard401ConfirmationRequiresConsecutiveResponses(t *testing.T) {
	for name, middle := range map[string]error{
		"recovered": nil,
		"network":   errors.New("connection refused"),
		"429":       tokenGuard401Error(http.StatusTooManyRequests),
		"503":       tokenGuard401Error(http.StatusServiceUnavailable),
		"403":       &codexModelsManifestUpstreamError{err: errors.New("forbidden"), statusCode: 403},
	} {
		t.Run(name, func(t *testing.T) {
			reauth, reader, tasks, _, _, _ := newReauthTestService("acct-1")
			defer reauth.oauth.Stop()
			require.NoError(t, tasks.UpsertConfig(t.Context(), reauthEmailConfig(42, "user@example.test", "encrypted:https://mail.example.test/code")))
			repo := &accountTokenGuardV2TestRepo{record: &AccountTokenGuardV2Record{AccountID: 42, Enabled: true, AutoReloginEnabled: true}, createTask: tasks.CreateTask}
			prober := &tokenGuard401Prober{err: tokenGuard401Error(401)}
			svc := NewAccountTokenGuardV2Service(repo, nil, reader, prober, reauth)
			defer svc.cancel()
			now := time.Now()
			svc.now = func() time.Time { return now }
			svc.runProbe(t.Context(), *repo.record)
			now = repo.record.NextProbeAt
			prober.err = middle
			svc.runProbe(t.Context(), *repo.record)
			require.Nil(t, repo.record.UnauthorizedRetryAt)
			require.Zero(t, tasks.createCalls)
			prober.err = tokenGuard401Error(401)
			svc.runProbe(t.Context(), *repo.record)
			require.Equal(t, now.Add(30*time.Second), repo.record.NextProbeAt)
			require.Zero(t, tasks.createCalls, "interrupted 401 sequence starts a new confirmation")
		})
	}
}

func TestTokenGuard401ConfirmationCredentialChanges(t *testing.T) {
	for _, duringProbe := range []bool{false, true} {
		t.Run(fmt.Sprintf("duringProbe=%t", duringProbe), func(t *testing.T) {
			reauth, reader, tasks, _, _, _ := newReauthTestService("acct-1")
			defer reauth.oauth.Stop()
			require.NoError(t, tasks.UpsertConfig(t.Context(), reauthEmailConfig(42, "user@example.test", "encrypted:https://mail.example.test/code")))
			repo := &accountTokenGuardV2TestRepo{record: &AccountTokenGuardV2Record{AccountID: 42, Enabled: true, AutoReloginEnabled: true}, createTask: tasks.CreateTask}
			prober := &tokenGuard401Prober{err: tokenGuard401Error(401)}
			svc := NewAccountTokenGuardV2Service(repo, nil, reader, prober, reauth)
			defer svc.cancel()
			now := time.Now()
			svc.now = func() time.Time { return now }
			svc.runProbe(t.Context(), *repo.record)
			now = repo.record.NextProbeAt
			change := func() { reader.account.Credentials["access_token"] = "synthetic-new-token" }
			if duringProbe {
				prober.onProbe = change
			} else {
				change()
			}
			svc.runProbe(t.Context(), *repo.record)
			require.Zero(t, tasks.createCalls, "old-token observations cannot authorize re-login of a new token")
			if !duringProbe {
				require.Equal(t, now.Add(30*time.Second), repo.record.NextProbeAt)
			}
		})
	}
}

func TestTokenGuard401ConfirmationRetainsAutomaticGates(t *testing.T) {
	for _, gate := range []string{"inspection_disabled", "relogin_disabled", "cooldown", "exhausted", "active_task"} {
		t.Run(gate, func(t *testing.T) {
			reauth, reader, tasks, _, _, _ := newReauthTestService("acct-1")
			defer reauth.oauth.Stop()
			require.NoError(t, tasks.UpsertConfig(t.Context(), reauthEmailConfig(42, "user@example.test", "encrypted:https://mail.example.test/code")))
			now := time.Now()
			retryAt := now.Add(-time.Second)
			hash, err := hashReauthCredentials(reader.account.Credentials)
			require.NoError(t, err)
			repo := &accountTokenGuardV2TestRepo{record: &AccountTokenGuardV2Record{AccountID: 42, Enabled: true, AutoReloginEnabled: true,
				UnauthorizedRetryAt: &retryAt, UnauthorizedCredentialsHash: hash}, createTask: tasks.CreateTask}
			switch gate {
			case "inspection_disabled":
				repo.record.Enabled = false
			case "relogin_disabled":
				repo.record.AutoReloginEnabled = false
			case "cooldown":
				until := now.Add(time.Hour)
				repo.record.CooldownUntil = &until
			case "exhausted":
				repo.record.AutoReloginExhausted = true
			case "active_task":
				repo.outcomes = []AccountReloginOutcome{{ID: 1, Status: OpenAIOAuthReauthStatusRunning}}
			}
			svc := NewAccountTokenGuardV2Service(repo, nil, reader, &tokenGuard401Prober{err: tokenGuard401Error(401)}, reauth)
			defer svc.cancel()
			svc.runProbe(t.Context(), *repo.record)
			require.Zero(t, tasks.createCalls)
		})
	}
}

func TestTokenGuard401IntervalSettingsCompatibility(t *testing.T) {
	settings := &accountTokenGuardV2TestSettings{raw: `{"probe_interval_seconds":1800,"retry_interval_seconds":300,"relogin_cooldown_seconds":1800,"fail_streak_threshold":5}`}
	svc := NewAccountTokenGuardV2Service(&accountTokenGuardV2TestRepo{}, settings, nil, nil, nil)
	defer svc.cancel()
	rules, err := svc.GetRules(t.Context())
	require.NoError(t, err)
	require.Equal(t, 30, rules.UnauthorizedRetryIntervalSeconds)
	for _, seconds := range []int{1, 45, 86400} {
		rules.UnauthorizedRetryIntervalSeconds = seconds
		_, err := svc.SaveRules(t.Context(), rules)
		require.NoError(t, err)
		got, err := svc.GetRules(t.Context())
		require.NoError(t, err)
		require.Equal(t, seconds, got.UnauthorizedRetryIntervalSeconds)
	}
	for _, seconds := range []int{-1, 86401} {
		rules.UnauthorizedRetryIntervalSeconds = seconds
		_, err := svc.SaveRules(t.Context(), rules)
		require.Error(t, err)
	}
}

type tokenGuard401ScheduledRepo struct{ accountTokenGuardV2TestRepo }

func (r *tokenGuard401ScheduledRepo) ClaimDue(context.Context, string, time.Duration, int) ([]AccountTokenGuardV2Record, error) {
	if r.record == nil || !r.record.Enabled || time.Now().Before(r.record.NextProbeAt) {
		return nil, nil
	}
	return []AccountTokenGuardV2Record{*r.record}, nil
}

func TestTokenGuard401SchedulerRechecksAfterThirtySeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reauth, reader, tasks, _, _, _ := newReauthTestService("acct-1")
		defer reauth.oauth.Stop()
		require.NoError(t, tasks.UpsertConfig(t.Context(), reauthEmailConfig(42, "user@example.test", "encrypted:https://mail.example.test/code")))
		repo := &tokenGuard401ScheduledRepo{accountTokenGuardV2TestRepo{record: &AccountTokenGuardV2Record{
			AccountID: 42, Enabled: true, AutoReloginEnabled: true}, createTask: tasks.CreateTask}}
		prober := &tokenGuard401Prober{err: tokenGuard401Error(401)}
		svc := NewAccountTokenGuardV2Service(repo, nil, reader, prober, reauth)
		svc.Start()
		defer svc.Stop()
		synctest.Wait()
		require.Equal(t, 1, prober.freshCalls)
		require.Zero(t, tasks.createCalls)
		time.Sleep(29 * time.Second)
		synctest.Wait()
		require.Equal(t, 1, prober.freshCalls, "ordinary ticks must not recheck the account early")
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, 2, prober.freshCalls)
		require.Equal(t, 1, tasks.createCalls, "the scan loop must not defer confirmation to the old one-minute tick")
	})
}

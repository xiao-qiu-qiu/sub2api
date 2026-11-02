package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type immediateReauthTestRepo struct {
	*accountTokenGuardV2TestRepo
	request func(context.Context, int64, map[string]any, string, AccountTokenGuardV2Rules, time.Time) (*OpenAIOAuthReauthTaskRecord, error)
	get     func(context.Context, int64) (*OpenAIOAuthReauthTaskRecord, error)
	verify  func(context.Context, int64, map[string]any, AccountTokenGuardV2ProbeCompletion, AccountTokenGuardV2Rules) error
}

func (r *immediateReauthTestRepo) RequestImmediateReauth(ctx context.Context, id int64, snapshot map[string]any, hash string, rules AccountTokenGuardV2Rules, at time.Time) (*OpenAIOAuthReauthTaskRecord, error) {
	return r.request(ctx, id, snapshot, hash, rules, at)
}

func (r *immediateReauthTestRepo) GetImmediateReauthTask(ctx context.Context, id int64) (*OpenAIOAuthReauthTaskRecord, error) {
	if r.get == nil {
		return nil, nil
	}
	return r.get(ctx, id)
}

func (r *immediateReauthTestRepo) CompleteImmediateReauthVerification(ctx context.Context, id int64, snapshot map[string]any, result AccountTokenGuardV2ProbeCompletion, rules AccountTokenGuardV2Rules) error {
	return r.verify(ctx, id, snapshot, result, rules)
}

func immediateReauthFixture(t *testing.T) (*AccountTokenGuardV2Service, *immediateReauthTestRepo, *reauthTestAccountReader, *reauthTestRepo) {
	t.Helper()
	reauth, reader, tasks, _, _, _ := newReauthTestService("acct-1")
	reader.account.Status, reader.account.Schedulable = StatusActive, true
	require.NoError(t, tasks.UpsertConfig(context.Background(), reauthEmailConfig(42, "user@example.test", "encrypted:https://mail.example.test/code")))
	repo := &immediateReauthTestRepo{accountTokenGuardV2TestRepo: &accountTokenGuardV2TestRepo{record: &AccountTokenGuardV2Record{
		AccountID: 42, Enabled: false, AutoReloginEnabled: true, NextProbeAt: time.Now().Add(time.Hour),
	}}}
	svc := NewAccountTokenGuardV2Service(repo, nil, reader, accountTokenGuardV2TestProber{}, reauth)
	return svc, repo, reader, tasks
}

func TestImmediateReauthEventUsesAtomicRepositoryWithoutInspectionThreshold(t *testing.T) {
	svc, repo, reader, tasks := immediateReauthFixture(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	repo.record.FailStreak = 0
	calls := 0
	repo.request = func(_ context.Context, id int64, snapshot map[string]any, hash string, rules AccountTokenGuardV2Rules, at time.Time) (*OpenAIOAuthReauthTaskRecord, error) {
		calls++
		require.EqualValues(t, 42, id)
		require.Equal(t, now, at)
		require.Equal(t, reader.account.Credentials, snapshot)
		want, err := hashReauthCredentials(snapshot)
		require.NoError(t, err)
		require.Equal(t, want, hash)
		require.Equal(t, AccountTokenGuardV2FailThreshold, rules.FailStreakThreshold)
		return &OpenAIOAuthReauthTaskRecord{ID: 91, AccountID: id, OAuthProfile: "codex", Status: OpenAIOAuthReauthStatusQueued}, nil
	}
	task, err := svc.RequestImmediateReauth(context.Background(), reader.account)
	require.NoError(t, err)
	require.EqualValues(t, 91, task.ID)
	require.Equal(t, 1, calls)
	require.Zero(t, tasks.createCalls, "manual CreateTask must not bypass the durable budget")
	require.False(t, repo.claimed, "event recovery must not wait for an inspection lease")
}

func TestImmediateReauthUnavailableDoesNotQueue(t *testing.T) {
	for _, name := range []string{"unmonitored", "auto disabled", "no config", "incomplete config", "manual stop", "disabled", "api key", "shadow", "encryption disabled", "unsupported repository"} {
		t.Run(name, func(t *testing.T) {
			svc, repo, reader, tasks := immediateReauthFixture(t)
			repo.request = func(context.Context, int64, map[string]any, string, AccountTokenGuardV2Rules, time.Time) (*OpenAIOAuthReauthTaskRecord, error) {
				t.Fatal("unavailable account reached the event queue")
				return nil, nil
			}
			switch name {
			case "unmonitored":
				repo.record = nil
			case "auto disabled":
				repo.record.AutoReloginEnabled = false
			case "no config":
				tasks.config = nil
			case "incomplete config":
				tasks.config.OTPURLCiphertext = ""
			case "manual stop":
				reader.account.Schedulable = false
			case "disabled":
				reader.account.Status = StatusDisabled
			case "api key":
				reader.account.Type = AccountTypeAPIKey
			case "shadow":
				parent := int64(90)
				reader.account.ParentAccountID = &parent
			case "encryption disabled":
				svc.reauth.encryptionKeyConfigured = false
			case "unsupported repository":
				svc.repo = repo.accountTokenGuardV2TestRepo
			}
			task, err := svc.RequestImmediateReauth(context.Background(), reader.account)
			require.ErrorIs(t, err, ErrOAuthRecoveryUnavailable)
			require.Nil(t, task)
			require.Zero(t, tasks.createCalls)
		})
	}
	var absent *AccountTokenGuardV2Service
	_, err := absent.RequestImmediateReauth(context.Background(), nil)
	require.ErrorIs(t, err, ErrOAuthRecoveryUnavailable)
}

func TestImmediateReauthLate401AndRepositoryGates(t *testing.T) {
	svc, repo, reader, tasks := immediateReauthFixture(t)
	old, err := reader.GetAccount(context.Background(), 42)
	require.NoError(t, err)
	reader.account.Credentials["access_token"] = "refreshed-access"
	task, err := svc.RequestImmediateReauth(context.Background(), old)
	require.NoError(t, err)
	require.Nil(t, task)
	require.Zero(t, tasks.createCalls)
	for _, queueErr := range []error{ErrOAuthRecoveryUnavailable, errors.New("database offline")} {
		repo.request = func(context.Context, int64, map[string]any, string, AccountTokenGuardV2Rules, time.Time) (*OpenAIOAuthReauthTaskRecord, error) {
			return nil, queueErr
		}
		_, err := svc.RequestImmediateReauth(context.Background(), reader.account)
		require.ErrorIs(t, err, queueErr)
	}
}

type immediateReauthFreshProber struct {
	t        *testing.T
	freshErr error
	calls    int
}

func (p *immediateReauthFreshProber) FetchOpenAIModelsList(context.Context, *Account) (*OpenAIModelsResponse, error) {
	p.t.Fatal("cached discovery must not verify recovery")
	return nil, nil
}

func (p *immediateReauthFreshProber) FetchOpenAIRecoveryModelsList(_ context.Context, account *Account) (*OpenAIModelsResponse, error) {
	p.calls++
	require.Equal(p.t, "recovered-access", account.GetCredential("access_token"))
	return nil, p.freshErr
}

func TestImmediateReauthVerificationUsesFreshProbeWithoutCreatingTasks(t *testing.T) {
	for _, name := range []string{"accepted", "auth", "transient", "cas changed"} {
		t.Run(name, func(t *testing.T) {
			svc, repo, reader, tasks := immediateReauthFixture(t)
			reader.account.Credentials["access_token"] = "recovered-access"
			prober := &immediateReauthFreshProber{t: t}
			svc.prober = prober
			wantState := AccountTokenGuardV2ProbeOK
			if name == "auth" {
				wantState = AccountTokenGuardV2ProbeAuth
				prober.freshErr = &codexModelsManifestUpstreamError{statusCode: http.StatusUnauthorized, err: errors.New("synthetic private body")}
			} else if name == "transient" {
				wantState = AccountTokenGuardV2ProbeTransient
				prober.freshErr = &codexModelsManifestUpstreamError{statusCode: http.StatusBadGateway, err: errors.New("synthetic private body")}
			}
			verified := false
			repo.verify = func(_ context.Context, id int64, snapshot map[string]any, result AccountTokenGuardV2ProbeCompletion, _ AccountTokenGuardV2Rules) error {
				verified = true
				require.EqualValues(t, 42, id)
				require.Equal(t, "recovered-access", snapshot["access_token"])
				require.Equal(t, wantState, result.ProbeState)
				require.NotContains(t, result.ProbeDetail, "synthetic private body")
				if name == "cas changed" {
					return infraerrors.Conflict("TOKEN_GUARD_V2_RECOVERY_CREDENTIALS_CHANGED", "Credentials changed")
				}
				return nil
			}
			err := svc.VerifyImmediateReauth(context.Background(), 42)
			if name == "accepted" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.True(t, verified)
			require.Equal(t, 1, prober.calls)
			require.Zero(t, tasks.createCalls)
			require.False(t, repo.claimed)
		})
	}
}

func TestImmediateReauthTaskLookupPreservesIdentityAndSanitizes(t *testing.T) {
	svc, repo, _, tasks := immediateReauthFixture(t)
	record := &OpenAIOAuthReauthTaskRecord{ID: 88, AccountID: 42, OAuthProfile: "codex", Status: OpenAIOAuthReauthStatusRunning, Error: "synthetic private provider output"}
	repo.get = func(_ context.Context, id int64) (*OpenAIOAuthReauthTaskRecord, error) {
		require.EqualValues(t, 42, id)
		return record, nil
	}
	task, err := svc.GetImmediateReauthTask(context.Background(), 42)
	require.NoError(t, err)
	require.EqualValues(t, 88, task.ID)
	require.NotContains(t, task.Error, "synthetic private")
	tasks.task = record
	task, err = svc.GetImmediateReauthTaskByID(context.Background(), 88)
	require.NoError(t, err)
	require.EqualValues(t, 88, task.ID)
	require.NotContains(t, task.Error, "synthetic private")
}

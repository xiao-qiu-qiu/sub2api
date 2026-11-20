package service

import (
	"context"
	"strings"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func queueTwoFALoginForTest(t *testing.T, ctx context.Context, svc *OpenAIOAuthReauthService, proxyID *int64) <-chan twoFALoginWorkerResult {
	t.Helper()
	result := make(chan twoFALoginWorkerResult, 1)
	go func() {
		credentials, err := svc.LoginTwoFA(ctx, AccountTokenGuardReloginAccount{Email: " User@Example.com ", Password: "fixture-password", MFASecret: " FIXTURETOTP "}, proxyID)
		result <- twoFALoginWorkerResult{credentials: credentials, err: err}
	}()
	require.Eventually(t, func() bool {
		svc.twoFALoginQueue.mu.Lock()
		defer svc.twoFALoginQueue.mu.Unlock()
		return len(svc.twoFALoginQueue.tasks) == 1
	}, 2*time.Second, time.Millisecond)
	return result
}

func TestTwoFALoginLocalWorkerClaimsAheadOfExistingAccounts(t *testing.T) {
	ctx := t.Context()
	svc, reader, repo, updater, _, _ := newReauthTestService("acct-1")
	svc.workerToken = strings.Repeat("a", 32)
	svc.MarkWorkerSeen()
	savePasswordReauthConfig(t, svc)
	regularTask, err := svc.CreateTask(ctx, 42)
	require.NoError(t, err)
	proxyID := int64(9)
	reader.proxy = &Proxy{ID: proxyID, Protocol: "socks5h", Host: "203.0.113.9", Port: 1080, Status: StatusActive}
	result := queueTwoFALoginForTest(t, ctx, svc, &proxyID)
	remote, err := svc.ClaimTaskWithEngines(ctx, "studio", []string{OpenAIOAuthReauthEngineSessionStudio})
	require.NoError(t, err)
	require.Nil(t, remote)
	claim, err := svc.ClaimTaskWithEngines(ctx, "worker-a", []string{OpenAIOAuthReauthEngineLocal, OpenAIOAuthReauthEngineSessionStudio})
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.True(t, isTwoFALoginWorkerTaskID(claim.TaskID))
	require.Equal(t, claim.TaskID, claim.AccountID)
	require.Equal(t, "codex", claim.OAuthProfile)
	require.Equal(t, OpenAIOAuthReauthModePasswordTOTP, claim.CredentialMode)
	require.Equal(t, OpenAIOAuthReauthEngineLocal, claim.Engine)
	require.Equal(t, "user@example.com", claim.LoginEmail)
	require.Equal(t, "fixture-password", claim.Password)
	require.Equal(t, "FIXTURETOTP", claim.TOTPSecret)
	require.Equal(t, "socks5h://203.0.113.9:1080", claim.ProxyURL)
	require.Empty(t, claim.ReloginEndpoint)
	require.Empty(t, claim.ReloginHeaders)
	credentials := directReauthCredentials("acct-new", "user-new", "USER@example.com")
	credentials["password"] = "never-echo"
	credentials["chatgpt_user_id"] = "claimed-worker-user"
	_, err = svc.SubmitCredentials(ctx, claim.TaskID, "another-worker", credentials, nil)
	require.Equal(t, "OPENAI_TWO_FA_TASK_NOT_ACTIVE", infraerrors.Reason(err))
	require.NoError(t, svc.UpdateStage(ctx, claim.TaskID, "worker-a", OpenAIOAuthReauthStageApplyingCredentials))
	completed, err := svc.SubmitCredentials(ctx, claim.TaskID, "worker-a", credentials, nil)
	require.NoError(t, err)
	require.Equal(t, OpenAIOAuthReauthStatusSucceeded, completed.Status)
	login := <-result
	require.NoError(t, login.err)
	require.Equal(t, "user@example.com", login.credentials["email"])
	require.Equal(t, openai.ClientID, login.credentials["client_id"])
	require.Equal(t, "acct-new", login.credentials["chatgpt_account_id"])
	require.Equal(t, "user-new", login.credentials["chatgpt_user_id"])
	require.Equal(t, "new-refresh", login.credentials["refresh_token"])
	require.NotContains(t, login.credentials, "password")
	require.Equal(t, OpenAIOAuthReauthStatusQueued, repo.task.Status)
	require.False(t, updater.applied)
	require.Equal(t, 1, repo.createCalls)
	require.Equal(t, "OPENAI_TWO_FA_TASK_NOT_ACTIVE", infraerrors.Reason(svc.FailTask(ctx, claim.TaskID, "worker-a", "password_rejected")))
	claim, err = svc.ClaimTask(ctx, "worker-a")
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.Equal(t, regularTask.ID, claim.TaskID)
}

func TestTwoFALoginLocalWorkerCancelRejectsLateCallbacks(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	svc := &OpenAIOAuthReauthService{oauth: &OpenAIOAuthService{}, workerToken: strings.Repeat("a", 32)}
	svc.MarkWorkerSeen()
	result := queueTwoFALoginForTest(t, ctx, svc, nil)
	claim, err := svc.ClaimTask(ctx, "worker-a")
	require.NoError(t, err)
	require.NotNil(t, claim)
	cancel()
	login := <-result
	require.ErrorIs(t, login.err, context.Canceled)
	require.Empty(t, login.credentials)
	for _, instance := range []*OpenAIOAuthReauthService{svc, {}} {
		_, err := instance.SubmitCredentials(t.Context(), claim.TaskID, "worker-a", nil, nil)
		require.Equal(t, "OPENAI_TWO_FA_TASK_NOT_ACTIVE", infraerrors.Reason(err))
		_, err = instance.SubmitCallback(t.Context(), claim.TaskID, "worker-a", "callback")
		require.Equal(t, "OPENAI_TWO_FA_TASK_NOT_ACTIVE", infraerrors.Reason(err))
		require.Equal(t, "OPENAI_TWO_FA_TASK_NOT_ACTIVE", infraerrors.Reason(instance.UpdateStage(t.Context(), claim.TaskID, "worker-a", OpenAIOAuthReauthStageStarting)))
		require.Equal(t, "OPENAI_TWO_FA_TASK_NOT_ACTIVE", infraerrors.Reason(instance.FailTask(t.Context(), claim.TaskID, "worker-a", "totp_rejected")))
	}
}

func TestTwoFALoginLocalWorkerRejectsInvalidResults(t *testing.T) {
	for _, test := range []struct {
		name        string
		mutate      func(map[string]any, map[string]any)
		errorReason string
	}{
		{"missing_refresh", func(creds, _ map[string]any) { delete(creds, "refresh_token") }, "OPENAI_TWO_FA_INVALID_CREDENTIALS"},
		{"foreign_client", func(_, extra map[string]any) { extra["client_id"] = openai.ExcelClientID }, "OPENAI_TWO_FA_INVALID_CREDENTIALS"},
		{"foreign_audience", func(creds, _ map[string]any) {
			creds["id_token"] = reauthTestJWT(map[string]any{"email": "user@example.com", "aud": []string{openai.ExcelClientID}, "exp": time.Now().Add(time.Hour).Unix()})
		}, "OPENAI_TWO_FA_INVALID_CREDENTIALS"},
		{"expired", func(creds, _ map[string]any) {
			creds["id_token"] = reauthTestJWT(map[string]any{"email": "user@example.com", "exp": time.Now().Add(-time.Second).Unix()})
		}, "OPENAI_TWO_FA_INVALID_CREDENTIALS"},
		{"wrong_email", func(creds, _ map[string]any) {
			creds["id_token"] = reauthTestJWT(map[string]any{"email": "someone@example.com", "exp": time.Now().Add(time.Hour).Unix()})
			creds["email"] = "user@example.com"
		}, "OPENAI_REAUTH_EMAIL_MISMATCH"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			svc := &OpenAIOAuthReauthService{oauth: &OpenAIOAuthService{}, workerToken: strings.Repeat("a", 32)}
			svc.MarkWorkerSeen()
			result := queueTwoFALoginForTest(t, ctx, svc, nil)
			claim, err := svc.ClaimTask(ctx, "worker-a")
			require.NoError(t, err)
			require.NotNil(t, claim)
			credentials := directReauthCredentials("acct-new", "user-new", "user@example.com")
			extra := map[string]any{}
			test.mutate(credentials, extra)
			_, err = svc.SubmitCredentials(ctx, claim.TaskID, "worker-a", credentials, extra)
			require.Equal(t, test.errorReason, infraerrors.Reason(err))
			login := <-result
			require.Equal(t, test.errorReason, infraerrors.Reason(login.err))
			require.Nil(t, login.credentials)
		})
	}
}

func TestTwoFALoginLocalWorkerRequiresOnlineWorkerAndUsableProxy(t *testing.T) {
	ctx := t.Context()
	svc, reader, _, _, _, _ := newReauthTestService("acct-1")
	svc.workerToken = strings.Repeat("a", 32)
	require.Equal(t, "OPENAI_REAUTH_WORKER_UNAVAILABLE", infraerrors.Reason(svc.CheckTwoFALoginWorker(ctx, nil)))
	svc.MarkWorkerSeen()
	proxyID := int64(9)
	reader.proxy = &Proxy{ID: proxyID, Protocol: "socks5h", Host: "203.0.113.9", Port: 1080, Status: StatusDisabled}
	require.Equal(t, "OPENAI_TWO_FA_PROXY_UNAVAILABLE", infraerrors.Reason(svc.CheckTwoFALoginWorker(ctx, &proxyID)))
	require.NoError(t, svc.CheckTwoFALoginWorker(ctx, nil))
}

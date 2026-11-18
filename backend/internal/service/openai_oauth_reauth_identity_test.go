package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthReauthUsesExplicitWorkspaceOverSessionID(t *testing.T) {
	svc, _, _, updater, _, _ := newReauthTestService("acct-1")
	savePasswordReauthConfig(t, svc)
	ctx := t.Context()
	_, err := svc.CreateTask(ctx, 42)
	require.NoError(t, err)
	claim, err := svc.ClaimTask(ctx, "worker-a")
	require.NoError(t, err)
	credentials := map[string]any{
		"access_token": reauthTestJWT(map[string]any{
			"https://api.openai.com/auth":    map[string]string{"chatgpt_account_id": "acct-1", "chatgpt_user_id": "user-1"},
			"https://api.openai.com/profile": map[string]string{"email": "user@example.com"},
		}),
		"id_token": reauthTestJWT(map[string]any{
			"sid": "current-login-session", "sub": "subject-1", "exp": time.Now().Add(time.Hour).Unix(),
		}),
		"refresh_token": "fixture-refresh",
	}
	task, err := svc.SubmitCredentials(ctx, claim.TaskID, "worker-a", credentials, nil)
	require.NoError(t, err)
	require.Equal(t, OpenAIOAuthReauthStatusSucceeded, task.Status)
	require.Equal(t, "acct-1", updater.credentials["chatgpt_account_id"])
	require.Equal(t, "user-1", updater.credentials["chatgpt_user_id"])
}

func TestOpenAIOAuthReauthRejectsInconsistentTokenWorkspace(t *testing.T) {
	svc, _, repo, updater, _, blocker := newReauthTestService("acct-1")
	savePasswordReauthConfig(t, svc)
	ctx := t.Context()
	_, err := svc.CreateTask(ctx, 42)
	require.NoError(t, err)
	claim, err := svc.ClaimTask(ctx, "worker-a")
	require.NoError(t, err)
	credentials := map[string]any{
		"access_token": reauthTestJWT(map[string]any{
			"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "other-workspace", "chatgpt_user_id": "user-1"},
		}),
		"id_token": reauthTestJWT(map[string]any{
			"email":                       "user@example.com",
			"exp":                         time.Now().Add(time.Hour).Unix(),
			"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "acct-1", "chatgpt_user_id": "user-1"},
		}),
		"refresh_token": "fixture-refresh",
	}
	_, err = svc.SubmitCredentials(ctx, claim.TaskID, "worker-a", credentials, nil)
	require.Error(t, err)
	require.False(t, updater.applied)
	require.Zero(t, blocker.clearedAccountID)
	require.Equal(t, OpenAIOAuthReauthStatusFailed, repo.task.Status)
	require.NotContains(t, repo.task.Error, "other-workspace")
}

package admin

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestTwoFALoginHTTPUsesLocalWorkerBeforeAccountCreation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	token := strings.Repeat("w", 32)
	t.Setenv(openAIOAuthReauthWorkerTokenEnv, token)
	// No account repository or legacy guard settings: first login must work
	// before an account exists and cannot call the external relogin service.
	reauth := service.NewOpenAIOAuthReauthService(nil, nil, nil, &service.OpenAIOAuthService{}, &credentialHandlerEncryptor{ready: true}, true, nil, nil)
	reauth.MarkWorkerSeen()
	guard := service.NewAccountTokenGuardService(nil, nil, nil, nil, nil)
	admin := NewAccountTokenGuardHandler(guard, reauth)
	worker := NewOpenAIOAuthReauthHandler(reauth)
	router := gin.New()
	router.POST("/login", admin.StartTwoFALogin)
	router.GET("/login/:id", admin.TwoFALogin)
	router.DELETE("/login/:id", admin.DeleteTwoFALogin)
	router.POST("/claim", worker.Claim)
	router.POST("/:task_id/credentials", worker.Credentials)
	router.POST("/:task_id/fail", worker.Fail)

	request := func(method, path, body, workerToken string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if workerToken != "" {
			req.Header.Set("X-OpenAI-Reauth-Worker-Token", workerToken)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	start := request("POST", "/login", `{"email":"user@example.com","password":"fixture-password","mfa_secret":"fixture-totp","credential_target":"operations"}`, "")
	require.Equal(t, http.StatusAccepted, start.Code, start.Body.String())
	require.Equal(t, "no-store", start.Header().Get("Cache-Control"))
	var started struct{ Data service.OpenAITwoFALoginJob }
	require.NoError(t, json.Unmarshal(start.Body.Bytes(), &started))
	id := started.Data.ID
	require.NotEmpty(t, id)
	t.Cleanup(func() { guard.DeleteTwoFALogin(id) })
	for _, secret := range []string{"fixture-password", "fixture-totp"} {
		require.NotContains(t, start.Body.String(), secret)
	}
	unauthorized := request("POST", "/claim", `{"worker_id":"worker-a"}`, "wrong")
	require.Equal(t, http.StatusUnauthorized, unauthorized.Code)

	var claim struct {
		Data *service.OpenAIOAuthReauthClaim
	}
	require.Eventually(t, func() bool {
		w := request("POST", "/claim", `{"worker_id":"worker-a","engines":["local_worker","session_studio"]}`, token)
		return w.Code == http.StatusOK && json.Unmarshal(w.Body.Bytes(), &claim) == nil && claim.Data != nil
	}, 3*time.Second, 5*time.Millisecond)
	require.Equal(t, "local_worker", claim.Data.Engine)
	require.Equal(t, "codex", claim.Data.OAuthProfile)
	require.Equal(t, "fixture-password", claim.Data.Password)
	require.Equal(t, "fixture-totp", claim.Data.TOTPSecret)
	require.Empty(t, claim.Data.ReloginEndpoint)
	require.Empty(t, claim.Data.ReloginHeaders)
	status := request("GET", "/login/"+id, "", "")
	require.NotContains(t, status.Body.String(), "fixture-password")
	require.NotContains(t, status.Body.String(), "fixture-totp")

	claims, err := json.Marshal(map[string]any{
		"email": "user@example.com", "exp": time.Now().Add(time.Hour).Unix(), "aud": []string{openai.ClientID},
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "user"},
	})
	require.NoError(t, err)
	jwt := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"
	credentials := map[string]any{"access_token": "access-fixture", "refresh_token": "refresh-fixture", "id_token": jwt, "client_id": openai.ClientID}
	postResult := func(workerID string) *httptest.ResponseRecorder {
		payload, err := json.Marshal(map[string]any{"worker_id": workerID, "credentials": credentials})
		require.NoError(t, err)
		return request("POST", fmt.Sprintf("/%d/credentials", claim.Data.TaskID), string(payload), token)
	}
	require.Equal(t, http.StatusConflict, postResult("other-worker").Code)
	completed := postResult("worker-a")
	require.Equal(t, http.StatusOK, completed.Code, completed.Body.String())
	require.Contains(t, completed.Body.String(), `"status":"succeeded"`)
	require.Eventually(t, func() bool {
		status = request("GET", "/login/"+id, "", "")
		var result struct{ Data service.OpenAITwoFALoginJob }
		return json.Unmarshal(status.Body.Bytes(), &result) == nil && result.Data.Status == "succeeded"
	}, time.Second, 5*time.Millisecond)
	require.Contains(t, status.Body.String(), "access-fixture")
	require.Contains(t, status.Body.String(), "user@example.com")
	require.NotContains(t, status.Body.String(), "fixture-password")
	require.NotContains(t, status.Body.String(), "fixture-totp")
	deleted := request("DELETE", "/login/"+id, "", "")
	require.Equal(t, http.StatusOK, deleted.Code)
	require.Equal(t, http.StatusNotFound, request("GET", "/login/"+id, "", "").Code)
	// Reserved IDs remain transient even after removal; no late callback may
	// fall through to persistent account task lookup.
	require.Equal(t, http.StatusConflict, postResult("worker-a").Code)
}

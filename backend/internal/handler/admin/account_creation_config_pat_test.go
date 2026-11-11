//go:build unit

package admin

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type patCreationTransport func(*http.Request) (*http.Response, error)

func (f patCreationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAccountCreationConfigPATFieldsForwarded(t *testing.T) {
	// Replace only the cached PAT-validation transport; no provider request is sent.
	client, err := httpclient.GetClient(httpclient.Options{Timeout: 20 * time.Second, ResponseHeaderTimeout: 15 * time.Second})
	require.NoError(t, err)
	original := client.Transport
	t.Cleanup(func() { client.Transport = original })
	validationCalls := 0
	client.Transport = patCreationTransport(func(r *http.Request) (*http.Response, error) {
		validationCalls++
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "Bearer at-preset-fixture", r.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"email":"fixture@example.com","chatgpt_user_id":"fixture-user","chatgpt_account_id":"fixture-account","chatgpt_plan_type":"plus","chatgpt_account_is_fedramp":false}`)), Request: r}, nil
	})
	oauth := service.NewOpenAIOAuthService(nil, nil)
	t.Cleanup(oauth.Stop)
	for _, preserve := range []any{nil, false, true} {
		stub := newStubAdminService()
		h := NewOpenAIOAuthHandler(oauth, stub, nil, nil)
		r := gin.New()
		r.POST("/pat", h.CreateAccountFromCodexPAT)
		body := map[string]any{"access_token": "at-preset-fixture", "name": "preset PAT", "concurrency": 7, "priority": -17, "load_factor": nil, "rate_multiplier": 0, "group_ids": []int64{}, "proxy_id": nil, "auto_pause_on_expired": false,
			"extra": map[string]any{"cost_multiplier": 0, "openai_excel_bps": false}, "credential_extras": map[string]any{"model_mapping": map[string]any{"manual-model": "manual-target"}}}
		if preserve != nil {
			body["preserve_creation_config"] = preserve
			body["group_rate_multiplier"] = 0
		}
		w := accountPriorityRequest(t, r, "/pat", body)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Len(t, stub.createdAccounts, 1)
		in := stub.createdAccounts[0]
		require.Equal(t, preserve == true, in.PreserveCreationConfig)
		if preserve == nil {
			require.Nil(t, in.GroupRateMultiplier)
		} else {
			require.NotNil(t, in.GroupRateMultiplier)
			require.Zero(t, *in.GroupRateMultiplier)
		}
		require.Equal(t, service.PlatformOpenAI, in.Platform)
		require.Equal(t, service.AccountTypeOAuth, in.Type)
		require.Equal(t, -17, in.Priority)
		require.Equal(t, 7, in.Concurrency)
		require.Nil(t, in.LoadFactor)
		require.Empty(t, in.GroupIDs)
		require.NotNil(t, in.RateMultiplier)
		require.Zero(t, *in.RateMultiplier)
		require.EqualValues(t, 0, in.Extra["cost_multiplier"])
		require.NotNil(t, in.AutoPauseOnExpired)
		require.False(t, *in.AutoPauseOnExpired)
		require.False(t, in.InitialOpenAITwoFA)
		require.Equal(t, service.OpenAIAuthModePersonalAccessToken, in.Credentials["auth_mode"])
		require.Equal(t, map[string]any{"manual-model": "manual-target"}, in.Credentials["model_mapping"])
	}
	require.Equal(t, 3, validationCalls)
}

func TestAccountCreationConfigPATRejectsInvalidGroupRateAndBPSBeforeValidation(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		for _, patch := range []map[string]any{
			{"group_rate_multiplier": -0.1},
			{"group_rate_multiplier": "0"},
			{"extra": map[string]any{"openai_excel_bps": true}},
		} {
			stub := newStubAdminService()
			// A nil OAuth service proves these requests fail before PAT validation.
			h := NewOpenAIOAuthHandler(nil, stub, nil, nil)
			r := gin.New()
			r.POST("/pat", h.CreateAccountFromCodexPAT)
			body := map[string]any{"access_token": "at-preset-fixture", "preserve_creation_config": preserve}
			for key, value := range patch {
				body[key] = value
			}
			w := accountPriorityRequest(t, r, "/pat", body)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.Empty(t, stub.createdAccounts)
		}
	}
}

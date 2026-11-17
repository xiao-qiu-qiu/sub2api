package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountOpsPushPlusEncryptsRetainsAndRotatesToken(t *testing.T) {
	ctx := context.Background()
	settings := &accountOpsSettingsStub{}
	svc := NewAccountOpsService(settings, &accountOpsRepoStub{}, nil)
	svc.SetNotificationDependencies(nil, opsTestEncryptor{}, true, "UTC")
	cfg := defaultAccountOpsConfig()
	cfg.Enabled = true
	cfg.Webhooks = []AccountOpsWebhook{{ID: "push", Provider: "pushplus", Enabled: true, Token: "  synthetic-pushplus-token  "}}
	require.NoError(t, svc.SaveConfig(ctx, cfg), "PushPlus alone is a valid notification destination")
	require.NotContains(t, settings.raw, "synthetic-pushplus-token")
	var stored storedOpsConfig
	require.NoError(t, json.Unmarshal([]byte(settings.raw), &stored))
	require.NotEmpty(t, stored.StoredWebhooks[0].TokenCipher)
	require.Empty(t, stored.StoredWebhooks[0].URLCipher)
	require.Empty(t, stored.StoredWebhooks[0].SecretCipher)
	out, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.True(t, out.Webhooks[0].TokenConfigured)
	require.False(t, out.Webhooks[0].URLConfigured)
	require.False(t, out.Webhooks[0].SecretConfigured)
	public, err := json.Marshal(out)
	require.NoError(t, err)
	require.Contains(t, string(public), `"token_configured":true`)
	require.NotContains(t, string(public), `"token":`)
	require.NotContains(t, string(public), "synthetic-pushplus-token")
	require.NotContains(t, string(public), "cipher")
	before := settings.raw
	require.NoError(t, svc.SaveConfig(ctx, out))
	require.JSONEq(t, before, settings.raw, "a redacted settings round trip retains the token and revision")
	loaded, err := svc.loadConfig(ctx)
	require.NoError(t, err)
	identity, err := svc.robotIdentity(loaded.Webhooks[0])
	require.NoError(t, err)
	require.NotContains(t, identity, "synthetic")

	// Display names remain editable when the credential key is unavailable.
	svc.SetNotificationDependencies(nil, nil, false, "UTC")
	name := "值班微信"
	out, err = svc.SaveWebhook(ctx, "push", AccountOpsWebhook{Enabled: true, Name: &name})
	require.NoError(t, err)
	require.True(t, out.Webhooks[0].TokenConfigured)
	var renamed storedOpsConfig
	require.NoError(t, json.Unmarshal([]byte(settings.raw), &renamed))
	require.Equal(t, stored.StoredWebhooks[0].TokenCipher, renamed.StoredWebhooks[0].TokenCipher)
	require.Equal(t, stored.StoredWebhooks[0].Revision, renamed.StoredWebhooks[0].Revision)

	svc.SetNotificationDependencies(nil, opsTestEncryptor{}, true, "UTC")
	_, err = svc.SaveWebhook(ctx, "push", AccountOpsWebhook{Provider: "pushplus", Enabled: true, Token: "replacement-pushplus-token"})
	require.NoError(t, err)
	loaded, err = svc.loadConfig(ctx)
	require.NoError(t, err)
	require.NotEqual(t, stored.StoredWebhooks[0].Revision, loaded.Webhooks[0].revision)
	newIdentity, err := svc.robotIdentity(loaded.Webhooks[0])
	require.NoError(t, err)
	require.NotEqual(t, identity, newIdentity, "each PushPlus token has its own delivery rate identity")
	copyDestination := loaded.Webhooks[0]
	copyDestination.ID = "other-channel"
	copyIdentity, err := svc.robotIdentity(copyDestination)
	require.NoError(t, err)
	require.Equal(t, newIdentity, copyIdentity, "two channels using one token share its delivery rate identity")

	_, err = svc.SaveWebhook(ctx, "push", AccountOpsWebhook{Provider: "custom", Enabled: true, URL: "https://hooks.example.test/send"})
	require.NoError(t, err)
	loaded, err = svc.loadConfig(ctx)
	require.NoError(t, err)
	require.Empty(t, loaded.Webhooks[0].tokenCipher, "changing provider removes the PushPlus token")
	_, err = svc.SaveWebhook(ctx, "push", AccountOpsWebhook{Provider: "pushplus", Enabled: true})
	require.ErrorIs(t, err, ErrAccountOpsConfigValidation, "switching back must provide a new token")
}

func TestAccountOpsPushPlusRejectsInvalidCredentialsBeforeSaving(t *testing.T) {
	template := `{"text":"{{message}}"}`
	for _, tc := range []struct {
		name  string
		input AccountOpsWebhook
	}{
		{"missing", AccountOpsWebhook{Provider: "pushplus"}},
		{"claimed_configured", AccountOpsWebhook{Provider: "pushplus", TokenConfigured: true}},
		{"overlong", AccountOpsWebhook{Provider: "pushplus", Token: strings.Repeat("x", 513)}},
		{"control", AccountOpsWebhook{Provider: "pushplus", Token: "synthetic\ntoken"}},
		{"whitespace", AccountOpsWebhook{Provider: "pushplus", Token: "synthetic token"}},
		{"url", AccountOpsWebhook{Provider: "pushplus", Token: "synthetic-token", URL: accountOpsPushPlusURL}},
		{"secret", AccountOpsWebhook{Provider: "pushplus", Token: "synthetic-token", Secret: "synthetic-secret"}},
		{"template", AccountOpsWebhook{Provider: "pushplus", Token: "synthetic-token", MessageTemplate: &template}},
		{"wrong_provider", AccountOpsWebhook{Provider: "custom", Token: "synthetic-token", URL: "https://hooks.example.test/send"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := &accountOpsSettingsStub{}
			svc := NewAccountOpsService(settings, &accountOpsRepoStub{}, nil)
			svc.SetNotificationDependencies(nil, opsTestEncryptor{}, true, "UTC")
			_, err := svc.SaveWebhook(context.Background(), "push", tc.input)
			require.ErrorIs(t, err, ErrAccountOpsConfigValidation)
			require.NotContains(t, err.Error(), "synthetic")
			require.Empty(t, settings.raw)
		})
	}
	t.Run("encryption_required", func(t *testing.T) {
		settings := &accountOpsSettingsStub{}
		svc := NewAccountOpsService(settings, &accountOpsRepoStub{}, nil)
		_, err := svc.SaveWebhook(context.Background(), "push", AccountOpsWebhook{Provider: "pushplus", Token: "synthetic-token"})
		require.ErrorContains(t, err, "fixed encryption key")
		require.Empty(t, settings.raw)
	})
	t.Run("plaintext_settings_rejected", func(t *testing.T) {
		svc := NewAccountOpsService(&accountOpsSettingsStub{raw: `{"cooldown_minutes":60,"webhooks":[{"id":"push","provider":"pushplus","token":"synthetic-token"}]}`}, &accountOpsRepoStub{}, nil)
		_, err := svc.loadConfig(context.Background())
		require.Error(t, err)
		require.NotContains(t, err.Error(), "synthetic")
	})
}

func TestAccountOpsPushPlusFixedEndpointValidation(t *testing.T) {
	provider, err := detectAccountOpsProvider(accountOpsPushPlusURL)
	require.NoError(t, err)
	require.Equal(t, "pushplus", provider)
	require.Error(t, validateAccountOpsCustomURL(accountOpsPushPlusURL), "official destinations cannot bypass their provider validation")
	for _, raw := range []string{
		"http://www.pushplus.plus/send", "https://www.pushplus.plus/send?token=synthetic-token",
		"https://www.pushplus.plus:443/send", "https://www.pushplus.plus./send",
		"https://www.pushplus.plus.evil.test/send", "https://www.pushplus.plus/send/",
	} {
		require.Error(t, validateAccountOpsURL("pushplus", raw))
	}
}

func TestAccountOpsPushPlusPayloadAndBusinessResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		ok     bool
	}{
		{"success", 200, `{"code":200,"msg":"请求成功","data":"synthetic-id"}`, true},
		{"business_error", 200, `{"code":600,"msg":"synthetic-token rejected"}`, false},
		{"other_provider_success", 200, `{"errcode":0,"code":0}`, false},
		{"missing_code", 200, `{}`, false},
		{"null_code", 200, `{"code":null}`, false},
		{"string_code", 200, `{"code":"200"}`, false},
		{"malformed", 200, `synthetic-token`, false},
		{"oversized", 200, strings.Repeat("x", 16385), false},
		{"redirect", 302, `{"code":200}`, false},
		{"http_error", 500, `{"code":200}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewAccountOpsService(&accountOpsSettingsStub{}, &accountOpsRepoStub{}, nil)
			svc.SetNotificationDependencies(nil, opsTestEncryptor{}, true, "UTC")
			cipher, err := svc.encryptor.Encrypt("synthetic-token")
			require.NoError(t, err)
			calls := 0
			svc.robotClient = &http.Client{Transport: opsRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, accountOpsPushPlusURL, req.URL.String())
				require.Equal(t, http.MethodPost, req.Method)
				require.Equal(t, "application/json", req.Header.Get("Content-Type"))
				payload, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.JSONEq(t, `{"token":"synthetic-token","title":"账号降智","content":"账户：测试账号\n模型：gpt-example","template":"txt"}`, string(payload))
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": []string{"https://second.example.test/send"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			err = svc.sendRobot(context.Background(), AccountOpsWebhook{Provider: "pushplus", tokenCipher: cipher}, accountOpsMessage{title: "邮件标题", robotTitle: "账号降智", plain: "账户：测试账号\n模型：gpt-example"})
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "synthetic-token")
			}
			require.Equal(t, 1, calls, "a redirect must not forward the token")
		})
	}
}

func TestAccountOpsPushPlusTestAndDeliveryRetry(t *testing.T) {
	settings := &accountOpsSettingsStub{}
	repo := &accountOpsRepoStub{}
	svc := NewAccountOpsService(settings, repo, nil)
	svc.SetNotificationDependencies(nil, opsTestEncryptor{}, true, "UTC")
	cfg := defaultAccountOpsConfig()
	cfg.Enabled = true
	cfg.Webhooks = []AccountOpsWebhook{{ID: "push", Provider: "pushplus", Enabled: true, Token: "synthetic-token"}}
	require.NoError(t, svc.SaveConfig(context.Background(), cfg))
	calls, code := 0, 200
	svc.robotClient = &http.Client{Transport: opsRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"code":%d}`, code)))}, nil
	})}
	require.NoError(t, svc.TestWebhook(context.Background(), "push"))
	require.Equal(t, 1, calls)
	code = 600
	event := &AccountOpsEvent{AccountID: 7, Kind: "balance_low", Attempts: 1}
	svc.deliverNotification(context.Background(), event)
	require.Equal(t, "failed", repo.state)
	code = 200
	event.Attempts = 2
	svc.deliverNotification(context.Background(), event)
	require.Equal(t, "sent", repo.state)
	require.Equal(t, 3, calls)
	svc.deliverNotification(context.Background(), event)
	require.Equal(t, 3, calls, "successful deliveries are not repeated")

	svc.robotClient = &http.Client{Transport: opsRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("transport failed for synthetic-token")
	})}
	err := svc.TestWebhook(context.Background(), "push")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "synthetic-token")
}

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type twoFALoginSettings struct {
	SettingRepository
	mu     sync.Mutex
	raw    string
	setErr error
	writes int
}

func (s *twoFALoginSettings) GetValue(context.Context, string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.raw, nil
}

func (s *twoFALoginSettings) Set(_ context.Context, _, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setErr != nil {
		return s.setErr
	}
	s.raw = value
	s.writes++
	return nil
}

func newTwoFATestService(t *testing.T, server *httptest.Server) *AccountTokenGuardService {
	t.Helper()
	cfg := defaultAccountTokenGuardConfig()
	cfg.Enabled, cfg.AutoRelogin = false, false
	cfg.ReloginEndpoint = server.URL
	cfg.ReloginHeaders["X-Test-Client"] = "{{uuid}}"
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	svc := NewAccountTokenGuardService(&twoFALoginSettings{raw: string(raw)}, nil, nil, nil, nil)
	svc.httpClient = server.Client()
	t.Cleanup(svc.clearTwoFALogins)
	return svc
}

// The HTTP fixture exercises the existing asynchronous job lifecycle. Production
// first-login requests supply OpenAIOAuthReauthService instead of this test worker.
type twoFATestWorker struct {
	check func(context.Context, *int64) error
	login func(context.Context, AccountTokenGuardReloginAccount, *int64) (map[string]any, error)
}

func (w twoFATestWorker) CheckTwoFALoginWorker(ctx context.Context, proxyID *int64) error {
	if w.check != nil {
		return w.check(ctx, proxyID)
	}
	return nil
}

func (w twoFATestWorker) LoginTwoFA(ctx context.Context, entry AccountTokenGuardReloginAccount, proxyID *int64) (map[string]any, error) {
	return w.login(ctx, entry, proxyID)
}

func startTwoFATestLogin(svc *AccountTokenGuardService, ctx context.Context, entry AccountTokenGuardReloginAccount, legacy ...bool) (*OpenAITwoFALoginJob, error) {
	worker := twoFATestWorker{login: func(ctx context.Context, entry AccountTokenGuardReloginAccount, _ *int64) (map[string]any, error) {
		cfg, err := svc.GetConfig(ctx)
		if err != nil {
			return nil, err
		}
		return svc.relogin(ctx, cfg, entry)
	}}
	save := len(legacy) == 0 || legacy[0]
	return svc.StartTwoFALogin(ctx, entry, worker, nil, save)
}

func TestTwoFALoginUsesLocalWorkerWithoutGuardConfig(t *testing.T) {
	// No settings repository: a current import must never load the old external
	// endpoint or write the password into legacy settings.
	svc := NewAccountTokenGuardService(nil, nil, nil, nil, nil)
	t.Cleanup(svc.clearTwoFALogins)
	proxyID := int64(23)
	requested := make(chan AccountTokenGuardReloginAccount, 1)
	worker := twoFATestWorker{
		check: func(_ context.Context, id *int64) error {
			require.Equal(t, &proxyID, id)
			return nil
		},
		login: func(_ context.Context, entry AccountTokenGuardReloginAccount, id *int64) (map[string]any, error) {
			if id == nil || *id != proxyID {
				return nil, errors.New("wrong proxy")
			}
			requested <- entry
			return map[string]any{"access_token": "access", "password": "discard-me"}, nil
		},
	}
	job, err := svc.StartTwoFALogin(context.Background(), AccountTokenGuardReloginAccount{Email: " USER@example.com ", Password: "password", MFASecret: " secret "}, worker, &proxyID, false)
	require.NoError(t, err)
	result := waitTwoFALogin(t, svc, job.ID)
	require.Equal(t, "succeeded", result.Status)
	require.Equal(t, AccountTokenGuardReloginAccount{Email: "user@example.com", Password: "password", MFASecret: "secret"}, <-requested)
	require.Equal(t, map[string]any{"email": "user@example.com", "access_token": "access"}, result.Credential)
}

func TestTwoFALoginWorkerPreflightDoesNotCreateJob(t *testing.T) {
	svc := NewAccountTokenGuardService(nil, nil, nil, nil, nil)
	worker := twoFATestWorker{check: func(context.Context, *int64) error { return errors.New("worker offline") }}
	_, err := svc.StartTwoFALogin(context.Background(), AccountTokenGuardReloginAccount{Email: "user@example.com", Password: "password", MFASecret: "secret"}, worker, nil, false)
	require.ErrorContains(t, err, "worker offline")
	require.Empty(t, svc.logins)
}

func waitTwoFALogin(t *testing.T, svc *AccountTokenGuardService, id string) *OpenAITwoFALoginJob {
	t.Helper()
	var job *OpenAITwoFALoginJob
	require.Eventually(t, func() bool {
		var ok bool
		job, ok = svc.TwoFALogin(id)
		return ok && job.Status != "running"
	}, 3*time.Second, 5*time.Millisecond)
	return job
}

func TestTwoFALoginForOperationsDoesNotSaveLegacyCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "{\"credential\":{\"access_token\":\"access\",\"refresh_token\":\"refresh\",\"id_token\":\"id\"}}")
	}))
	defer server.Close()
	svc := newTwoFATestService(t, server)
	settings, ok := svc.settings.(*twoFALoginSettings)
	require.True(t, ok)
	before := settings.raw
	entry := AccountTokenGuardReloginAccount{Email: "operations@example.com", Password: "test-password", MFASecret: "test-secret"}
	job, err := startTwoFATestLogin(svc, context.Background(), entry, false)
	require.NoError(t, err)
	result := waitTwoFALogin(t, svc, job.ID)
	require.Equal(t, "succeeded", result.Status)
	settings.mu.Lock()
	require.Zero(t, settings.writes)
	require.Equal(t, before, settings.raw)
	settings.mu.Unlock()
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(raw), entry.Password)
	require.NotContains(t, string(raw), entry.MFASecret)
}

func TestTwoFALoginInitialLoginWithoutExistingAccount(t *testing.T) {
	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error("invalid login payload")
		}
		payload["test_client"] = r.Header.Get("X-Test-Client")
		payload["relogin_header"] = r.Header.Get("X-Session-Studio-Relogin")
		requests <- payload
		_, _ = io.WriteString(w, `{"type":"result","payload":{"credential":{"access_token":"access","refresh_token":"refresh","id_token":"id","account_id":"workspace","password":"echo-password","mfa_secret":"echo-secret","model_mapping":{"bad":"bad"}}}}`)
	}))
	defer server.Close()
	svc := newTwoFATestService(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	job, err := startTwoFATestLogin(svc, ctx, AccountTokenGuardReloginAccount{Email: " USER@example.com ", Password: "p,a!ss", MFASecret: "TEST-SECRET"})
	require.NoError(t, err)
	cancel() // The HTTP request may finish immediately after acceptance.
	result := waitTwoFALogin(t, svc, job.ID)
	require.Equal(t, "succeeded", result.Status)
	cfg, err := svc.GetConfig(context.Background())
	require.NoError(t, err)
	require.False(t, cfg.Enabled)
	require.False(t, cfg.AutoRelogin)
	require.Equal(t, []AccountTokenGuardReloginAccount{{Email: "user@example.com", Password: "p,a!ss", MFASecret: "TEST-SECRET"}}, cfg.ReloginAccounts)
	require.Equal(t, "user@example.com", result.Credential["email"])
	require.Equal(t, "workspace", result.Credential["account_id"])
	require.NotContains(t, result.Credential, "password")
	require.NotContains(t, result.Credential, "mfa_secret")
	require.NotContains(t, result.Credential, "model_mapping")
	payload := <-requests
	require.Equal(t, "password_2fa", payload["auth_mode"])
	require.Equal(t, "start", payload["action"])
	require.Equal(t, "p,a!ss", payload["password"])
	require.Equal(t, "1", payload["relogin_header"])
	require.NotEmpty(t, payload["test_client"])
	require.NotEqual(t, "{{uuid}}", payload["test_client"])
	result.Credential["access_token"] = "mutated"
	original, ok := svc.TwoFALogin(job.ID)
	require.True(t, ok)
	require.Equal(t, "access", original.Credential["access_token"])
	svc.DeleteTwoFALogin(job.ID)
	_, ok = svc.TwoFALogin(job.ID)
	require.False(t, ok)
}

func TestTwoFALoginFailureDoesNotExposeProviderData(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"password-canary-secret","message":"secret"}}`,
		`{"credential":{"access_token":"access-only-secret"}}`,
	} {
		t.Run("redacted", func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			defer server.Close()
			svc := newTwoFATestService(t, server)
			job, err := startTwoFATestLogin(svc, context.Background(), AccountTokenGuardReloginAccount{Email: "a@example.com", Password: "secret", MFASecret: "secret"})
			require.NoError(t, err)
			result := waitTwoFALogin(t, svc, job.ID)
			require.Equal(t, "failed", result.Status)
			raw, err := json.Marshal(result)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "secret")
			require.Nil(t, result.Credential)
			cfg, err := svc.GetConfig(context.Background())
			require.NoError(t, err)
			require.Empty(t, cfg.ReloginAccounts)
			settings, ok := svc.settings.(*twoFALoginSettings)
			require.True(t, ok)
			require.Zero(t, settings.writes)
		})
	}
}

func TestTwoFALoginReportsOnlySafeWorkerDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name, code, stage, reason string
		err                       error
	}{
		{"startup protocol error", "unknown", "starting", "protocol_error", errors.New("local_protocol_failed stage=starting reason=protocol_error elapsed_ms=1472 exit=1")},
		{"password rejected", "password_rejected", "web_login", "password_rejected", errors.New("local_protocol_failed stage=web_login reason=password_rejected elapsed_ms=1472 exit=1")},
		{"malformed diagnostic", "unknown", "unknown", "", errors.New("local_protocol_failed stage=starting reason=protocol_error elapsed_ms=1472 exit=1 private-provider-text")},
		{"timeout", "timeout", "unknown", "", context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc := NewAccountTokenGuardService(nil, nil, nil, nil, nil)
			t.Cleanup(svc.clearTwoFALogins)
			worker := twoFATestWorker{login: func(context.Context, AccountTokenGuardReloginAccount, *int64) (map[string]any, error) {
				return nil, test.err
			}}
			input := AccountTokenGuardReloginAccount{Email: "fixture@example.com", Password: "fixture-password", MFASecret: "fixture-totp"}
			job, err := svc.StartTwoFALogin(t.Context(), input, worker, nil, false)
			require.NoError(t, err)
			require.Nil(t, job.Failure)
			require.Nil(t, job.Diagnostic)
			result := waitTwoFALogin(t, svc, job.ID)
			require.Equal(t, "failed", result.Status)
			require.Nil(t, result.Credential)
			require.NotNil(t, result.Failure)
			require.Equal(t, test.code, result.Failure.Code)
			require.Equal(t, test.stage, result.Failure.Stage)
			if test.reason == "" {
				require.Nil(t, result.Diagnostic)
			} else {
				require.NotNil(t, result.Diagnostic)
				require.Equal(t, test.reason, result.Diagnostic.Reason)
				require.Equal(t, test.stage, result.Diagnostic.Stage)
			}
			raw, err := json.Marshal(result)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "private-provider-text")
			require.NotContains(t, string(raw), "fixture-")
		})
	}
}

func TestTwoFALoginCancellation(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	svc := newTwoFATestService(t, server)
	job, err := startTwoFATestLogin(svc, context.Background(), AccountTokenGuardReloginAccount{Email: "a@example.com", Password: "secret", MFASecret: "secret"})
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("login did not start")
	}
	svc.DeleteTwoFALogin(job.ID)
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream login was not canceled")
	}
	_, ok := svc.TwoFALogin(job.ID)
	require.False(t, ok)
	cfg, err := svc.GetConfig(context.Background())
	require.NoError(t, err)
	require.Empty(t, cfg.ReloginAccounts)
}

func TestTwoFALoginRejectsInvalidInputBeforeNetwork(t *testing.T) {
	svc := &AccountTokenGuardService{}
	for _, entry := range []AccountTokenGuardReloginAccount{
		{Email: "not-email", Password: "p", MFASecret: "s"},
		{Email: "User <a@example.com>", Password: "p", MFASecret: "s"},
		{Email: "a@example.com", Password: "p"},
		{Email: "a@example.com", Password: strings.Repeat("p", 4097), MFASecret: "s"},
	} {
		_, err := startTwoFATestLogin(svc, context.Background(), entry)
		require.Error(t, err)
	}
}

func TestTwoFALoginMergesLatestGuardConfig(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, `{"type":"result","payload":{"credential":{"access_token":"test-access","refresh_token":"test-refresh","id_token":"test-id"}}}`)
	}))
	defer server.Close()
	svc := newTwoFATestService(t, server)
	// Release even if an assertion fails, so the server can close.
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	job, err := startTwoFATestLogin(svc, context.Background(), AccountTokenGuardReloginAccount{Email: "USER@example.com", Password: "new-password", MFASecret: "new-mfa"})
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("login did not start")
	}
	cfg, err := svc.GetConfig(context.Background())
	require.NoError(t, err)
	cfg.GroupIDs = []int64{7}
	cfg.IntervalSeconds = 123
	cfg.ReloginAccounts = []AccountTokenGuardReloginAccount{
		{Email: "user@example.com", Password: "old-password", MFASecret: "old-mfa"},
		{Email: "other@example.com", Password: "other-password", MFASecret: "other-mfa"},
	}
	cfg, err = svc.SaveConfig(context.Background(), cfg)
	require.NoError(t, err)
	unblock()
	require.Equal(t, "succeeded", waitTwoFALogin(t, svc, job.ID).Status)
	saved, err := svc.GetConfig(context.Background())
	require.NoError(t, err)
	cfg.ReloginAccounts[1] = AccountTokenGuardReloginAccount{Email: "user@example.com", Password: "new-password", MFASecret: "new-mfa"}
	require.Equal(t, cfg, saved)
	// A fresh service must see the credentials, not just the original cache.
	restarted := NewAccountTokenGuardService(svc.settings, nil, nil, nil, nil)
	persisted, err := restarted.GetConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, saved, persisted)
}

func TestTwoFALoginConcurrentGuardEnrollment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"type":"result","payload":{"credential":{"access_token":"test-access","refresh_token":"test-refresh","id_token":"test-id"}}}`)
	}))
	defer server.Close()
	svc := newTwoFATestService(t, server)
	var jobs []*OpenAITwoFALoginJob
	for i := 0; i < 10; i++ {
		job, err := startTwoFATestLogin(svc, context.Background(), AccountTokenGuardReloginAccount{Email: fmt.Sprintf("user%d@example.com", i), Password: "test-password", MFASecret: "test-mfa"})
		require.NoError(t, err)
		jobs = append(jobs, job)
	}
	for _, job := range jobs {
		require.Equal(t, "succeeded", waitTwoFALogin(t, svc, job.ID).Status)
	}
	cfg, err := svc.GetConfig(context.Background())
	require.NoError(t, err)
	require.Len(t, cfg.ReloginAccounts, 10)
}

func TestTwoFALoginGuardSaveFailureDoesNotReportSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"type":"result","payload":{"credential":{"access_token":"test-access","refresh_token":"test-refresh","id_token":"test-id"}}}`)
	}))
	defer server.Close()
	svc := newTwoFATestService(t, server)
	settings, ok := svc.settings.(*twoFALoginSettings)
	require.True(t, ok)
	settings.setErr = errors.New("storage-password-canary")
	job, err := startTwoFATestLogin(svc, context.Background(), AccountTokenGuardReloginAccount{Email: "user@example.com", Password: "test-password", MFASecret: "test-mfa"})
	require.NoError(t, err)
	result := waitTwoFALogin(t, svc, job.ID)
	require.Equal(t, "failed", result.Status)
	require.Nil(t, result.Credential)
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "canary")
	cfg, err := svc.GetConfig(context.Background())
	require.NoError(t, err)
	require.Empty(t, cfg.ReloginAccounts)
	require.Empty(t, svc.currentConfig().ReloginAccounts)
	// The same input can be retried after persistence recovers.
	settings.mu.Lock()
	settings.setErr = nil
	settings.mu.Unlock()
	retry, err := startTwoFATestLogin(svc, context.Background(), AccountTokenGuardReloginAccount{Email: "user@example.com", Password: "test-password", MFASecret: "test-mfa"})
	require.NoError(t, err)
	require.Equal(t, "succeeded", waitTwoFALogin(t, svc, retry.ID).Status)
	cfg, err = svc.GetConfig(context.Background())
	require.NoError(t, err)
	require.Len(t, cfg.ReloginAccounts, 1)
}

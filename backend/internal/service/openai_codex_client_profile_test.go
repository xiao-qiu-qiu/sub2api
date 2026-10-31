package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// These tests change process-wide identity state; do not run them in parallel.
func withCodexClientProfileIdentityState(t *testing.T) {
	t.Helper()
	codexCanonicalUAMu.RLock()
	previousResolver := codexCanonicalUAResolver
	codexCanonicalUAMu.RUnlock()
	previousEnforcement := codexIdentityEnforcement.Load()
	t.Cleanup(func() {
		SetCodexCanonicalUserAgentResolver(previousResolver)
		SetCodexIdentityEnforcementEnabled(previousEnforcement)
	})
	SetCodexCanonicalUserAgentResolver(func() string { return buildCodexCLIUserAgent("0.200.1") })
	SetCodexIdentityEnforcementEnabled(true)
}

func TestCodexClientProfilePlatformsAndIdentity(t *testing.T) {
	withCodexClientProfileIdentityState(t)
	for _, platform := range []struct{ name, fingerprint string }{
		{"windows", "Windows 10.0.26200; x86_64"},
		{"macos", "Mac OS 26.7.1; arm64"},
		{"linux", "Ubuntu 26.4.0; x86_64"},
	} {
		for _, client := range []string{"cli", "desktop"} {
			t.Run(client+"_"+platform.name, func(t *testing.T) {
				input := CodexClientProfile{Client: client, Platform: platform.name, VersionMode: "latest"}
				originator, trailer := "codex_cli_rs", ""
				if client == "desktop" {
					input.DesktopVersion = "26.1016.62342"
					originator, trailer = "Codex Desktop", " (Codex Desktop; 26.1016.62342)"
				}
				profile, err := ParseCodexClientProfile(input)
				require.NoError(t, err)
				identity := profile.identity()
				require.Equal(t, originator+"/0.200.1 ("+platform.fingerprint+") unknown"+trailer, identity.userAgent)
				require.Equal(t, originator, identity.originator)
				require.Equal(t, "0.200.1", identity.version)
			})
		}
	}
	profile, err := ParseCodexClientProfile(map[string]any{
		"client": "cli", "platform": "linux", "cliEntry": "exec", "versionMode": "fixed",
		"codexVersion": "0.144.1-alpha.1", "osType": "Debian", "osVersion": "12",
		"arch": "aarch64", "terminal": "xterm-256color",
	})
	require.NoError(t, err)
	require.Equal(t, codexOutboundIdentity{
		userAgent:  "codex_exec/0.144.1-alpha.1 (Debian 12; aarch64) xterm-256color (codex_exec; 0.144.1-alpha.1)",
		originator: "codex_exec", version: "0.144.1-alpha.1",
	}, profile.identity())
}

func TestCodexClientProfileVersionSources(t *testing.T) {
	withCodexClientProfileIdentityState(t)
	latest, err := ParseCodexClientProfile(json.RawMessage(`{"client":"desktop","platform":"windows","versionMode":"latest","desktopVersion":"26.1016.62342"}`))
	require.NoError(t, err)
	fixed, err := ParseCodexClientProfile(CodexClientProfile{
		Client: "desktop", Platform: "macos", VersionMode: "fixed", CodexVersion: "0.160.0", DesktopVersion: "26.1016.51333",
	})
	require.NoError(t, err)
	for _, version := range []string{"0.200.1", "0.201.0"} {
		SetCodexCanonicalUserAgentResolver(func() string { return buildCodexCLIUserAgent(version) })
		identity := latest.identity()
		require.Equal(t, version, identity.version)
		require.Contains(t, identity.userAgent, "Codex Desktop/"+version+" ")
		require.True(t, strings.HasSuffix(identity.userAgent, "(Codex Desktop; 26.1016.62342)"))
		require.Equal(t, "0.160.0", fixed.identity().version)
		require.Contains(t, fixed.identity().userAgent, "(Codex Desktop; 26.1016.51333)")
	}
}

func TestCodexClientProfileParsingRejectsAmbiguousAndUnsafeValues(t *testing.T) {
	valid := `{"client":"cli","platform":"linux","versionMode":"latest","versionLag":0}`
	_, err := ParseCodexClientProfile(json.RawMessage(valid))
	require.NoError(t, err)
	for _, raw := range []any{
		nil, json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(``), valid,
		json.RawMessage(valid + ` {}`), json.RawMessage(valid + ` garbage`),
		json.RawMessage(`{"client":"cli","platform":"linux","versionMode":"latest","originator":"custom"}`),
		json.RawMessage(`{"client":"cli","platform":"android","versionMode":"latest"}`),
		json.RawMessage(`{"client":"tui","platform":"linux","versionMode":"latest"}`),
		json.RawMessage(`{"client":"cli","platform":"linux","versionMode":"latest","cliEntry":"tui"}`),
		json.RawMessage(`{"client":"desktop","platform":"linux","versionMode":"latest","cliEntry":"exec","desktopVersion":"26.1.1"}`),
		json.RawMessage(`{"client":"cli","platform":"linux"}`),
		json.RawMessage(`{"client":"cli","platform":"linux","versionMode":"latest","versionLag":1}`),
		json.RawMessage(`{"client":"cli","platform":"linux","versionMode":"fixed","versionLag":-1,"codexVersion":"0.160.0"}`),
		json.RawMessage(`{"client":"cli","platform":"linux","versionMode":"latest","codexVersion":"0.160.0"}`),
		json.RawMessage(`{"client":"cli","platform":"linux","versionMode":"fixed"}`),
		json.RawMessage(`{"client":"cli","platform":"linux","versionMode":"fixed","codexVersion":"0.143.9"}`),
		json.RawMessage(`{"client":"cli","platform":"linux","versionMode":"fixed","codexVersion":"0.144.0-alpha.1"}`),
		json.RawMessage(`{"client":"cli","platform":"linux","versionMode":"fixed","codexVersion":"0.160.0\r\n"}`),
		json.RawMessage(`{"client":"cli","platform":"linux","versionMode":"latest","desktopVersion":"26.1.1"}`),
		json.RawMessage(`{"client":"desktop","platform":"linux","versionMode":"latest"}`),
		json.RawMessage(`{"client":"desktop","platform":"linux","versionMode":"fixed","codexVersion":"0.160.0"}`),
		json.RawMessage(`{"client":"desktop","platform":"linux","versionMode":"latest","desktopVersion":"26.x"}`),
		json.RawMessage(`{"client":"desktop","platform":"linux","versionMode":"latest","desktopVersion":"26.1.1-alpha.1"}`),
	} {
		profile, err := ParseCodexClientProfile(raw)
		require.Error(t, err, "%v", raw)
		require.Nil(t, profile)
	}
	for _, field := range []string{"osType", "osVersion", "arch", "terminal"} {
		for _, unsafe := range []string{"bad\r\nheader", "bad\x00", "bad\x7f", "中文", "(client; 1)", "a\\b", " leading", "trailing ", strings.Repeat("x", 129)} {
			_, err := ParseCodexClientProfile(map[string]any{
				"client": "cli", "platform": "linux", "versionMode": "latest", field: unsafe,
			})
			require.Error(t, err, "%s=%q", field, unsafe)
		}
	}
}

func TestCodexClientProfileExtraValidation(t *testing.T) {
	profile := map[string]any{"client": "cli", "platform": "linux", "versionMode": "latest"}
	extra := map[string]any{codexClientProfileExtraKey: profile}
	parentID := int64(1)
	for _, account := range []*Account{
		{Platform: PlatformOpenAI, Type: AccountTypeOAuth},
		{Platform: PlatformOpenAI, Type: AccountTypeSetupToken},
	} {
		require.NoError(t, validateCodexClientProfileExtra(account, extra))
	}
	for _, account := range []*Account{
		nil,
		{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		{Platform: PlatformAnthropic, Type: AccountTypeOAuth},
		{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parentID},
	} {
		err := validateCodexClientProfileExtra(account, extra)
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
		require.Equal(t, "CODEX_CLIENT_PROFILE_INVALID", infraerrors.Reason(err))
		require.NoError(t, validateCodexClientProfileExtra(account, nil))
		require.NoError(t, validateCodexClientProfileExtra(account, map[string]any{codexClientProfileExtraKey: nil}))
	}
	err := validateCodexClientProfileExtra(&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, map[string]any{
		codexClientProfileExtraKey: map[string]any{"client": "desktop", "platform": "macos", "versionMode": "latest"},
	})
	require.Equal(t, "CODEX_CLIENT_PROFILE_INVALID", infraerrors.Reason(err))
}

func TestCodexClientProfileEnforcementUsesCredentialSourceAndForce(t *testing.T) {
	withCodexClientProfileIdentityState(t)
	parentID := int64(1)
	parent := &Account{ID: parentID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		codexClientProfileExtraKey: CodexClientProfile{
			Client: "desktop", Platform: "windows", VersionMode: "fixed", CodexVersion: "0.160.0", DesktopVersion: "26.1016.62342",
		},
	}}
	shadow := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parentID, Extra: map[string]any{
		codexClientProfileExtraKey: CodexClientProfile{Client: "cli", Platform: "linux", VersionMode: "latest"},
	}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(codexAccountIdentitySourceContextKey, parent)
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	h := http.Header{"Originator": {"incoming"}, "User-Agent": {"curl/8"}, "user-agent": {"stale/1"}, "version": {"1.0"}}
	svc.enforceCodexClientIdentity(c, h, shadow)
	require.Equal(t, "Codex Desktop", h.Get("originator"))
	require.Equal(t, "0.160.0", h.Get("version"))
	require.Equal(t, "Codex Desktop/0.160.0 (Windows 10.0.26200; x86_64) unknown (Codex Desktop; 26.1016.62342)", h.Get("user-agent"))
	require.NotContains(t, h, "user-agent")
	require.NotContains(t, h, "version")

	svc.cfg.Gateway.ForceCodexCLI = true
	svc.enforceCodexClientIdentity(c, h, shadow)
	require.Equal(t, resolveCodexOutboundIdentity(""), codexOutboundIdentity{
		userAgent: h.Get("user-agent"), originator: h.Get("originator"), version: h.Get("version"),
	})

	// The next attempt stages a new source, so the previous parent's profile must disappear.
	svc.cfg.Gateway.ForceCodexCLI = false
	next := &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	c.Set(codexAccountIdentitySourceContextKey, next)
	svc.enforceCodexClientIdentity(c, h, next)
	require.Equal(t, "codex_cli_rs", h.Get("originator"))
	require.Equal(t, "0.200.1", h.Get("version"))
}

func TestCodexClientProfilePreservesLegacyFallbackAndDisabledEnforcement(t *testing.T) {
	withCodexClientProfileIdentityState(t)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
		"user_agent": "codex_vscode/0.150.0 (Windows 10.0.26200; x86_64) vscode",
	}, Extra: map[string]any{}}
	legacy := resolveCodexOutboundIdentity(account.GetOpenAIUserAgent())
	for _, raw := range []any{nil, map[string]any{"client": "desktop", "platform": "macos", "versionMode": "latest"}} {
		account.Extra[codexClientProfileExtraKey] = raw
		require.Equal(t, legacy, codexClientIdentityFor(account, account))
	}
	require.Equal(t, resolveCodexOutboundIdentity(""), codexClientIdentityFor(nil, nil))
	account.Extra[codexClientProfileExtraKey] = CodexClientProfile{
		Client: "desktop", Platform: "macos", VersionMode: "latest", DesktopVersion: "26.1016.62342",
	}
	account.Type = AccountTypeAPIKey
	require.Equal(t, legacy, codexClientIdentityFor(account, account), "API keys ignore stored profiles")
	account.Type = AccountTypeOAuth
	svc := &OpenAIGatewayService{}
	withoutOriginator := http.Header{"User-Agent": {"curl/8"}}
	svc.enforceCodexClientIdentity(nil, withoutOriginator, account)
	require.Equal(t, http.Header{"User-Agent": {"curl/8"}}, withoutOriginator)
	svc.enforceCodexClientIdentity(nil, nil, account)

	SetCodexIdentityEnforcementEnabled(false)
	for _, force := range []bool{false, true} {
		svc.cfg = &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: force}}
		h := http.Header{}
		h.Set("User-Agent", "codex_exec/0.180.0 (Debian 12; x86_64) screen (codex_exec; 0.180.0)")
		h.Set("Originator", "mismatched")
		h.Set("Version", "0.180.0")
		expected := h.Clone()
		enforceCodexIdentityHeadersWithUA(expected, svc.codexIdentityOverrideUA(account))
		svc.enforceCodexClientIdentity(nil, h, account)
		require.Equal(t, expected, h, "disabled enforcement follows the existing pairing path")
		require.Equal(t, "codex_exec", h.Get("Originator"))
	}
}

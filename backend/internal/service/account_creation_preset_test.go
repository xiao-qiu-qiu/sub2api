package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/stretchr/testify/require"
)

type creationPresetSettings struct {
	SettingRepository
	values map[string]string
	err    error
}

func (r *creationPresetSettings) GetValue(_ context.Context, key string) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	v, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return v, nil
}
func (r *creationPresetSettings) Set(_ context.Context, key, value string) error {
	if r.err != nil {
		return r.err
	}
	r.values[key] = value
	return nil
}

type creationPresetGroups struct {
	GroupRepository
	groups map[int64]*Group
}

func (r creationPresetGroups) GetByID(_ context.Context, id int64) (*Group, error) {
	if g := r.groups[id]; g != nil {
		return g, nil
	}
	return nil, ErrGroupNotFound
}

type creationPresetProxies struct {
	ProxyRepository
	proxy *Proxy
	err   error
}

func (r creationPresetProxies) GetByID(context.Context, int64) (*Proxy, error) { return r.proxy, r.err }

func creationPresetFixture() AccountCreationPreset {
	return AccountCreationPreset{Version: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Concurrency: 7, Priority: -20, RateMultiplier: 0, GroupRateMultiplier: 0,
		GroupIDs: []int64{}, Extra: map[string]any{AccountCostMultiplierExtraKey: float64(0)},
		ModelMapping: map[string]string{"source": "target"}, AutoPauseOnExpired: true}
}
func creationPresetPayload(t *testing.T, edit func(map[string]any)) []byte {
	t.Helper()
	raw, err := json.Marshal(creationPresetFixture())
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	edit(fields)
	raw, err = json.Marshal(fields)
	require.NoError(t, err)
	return raw
}

func TestAccountCreationPresetSaveReadAndIndependentKey(t *testing.T) {
	r := &creationPresetSettings{values: map[string]string{SettingKeyOAuthAutoConfig: "leave this unchanged"}}
	s := NewAccountOpsService(r, nil, nil)
	p, err := s.GetAccountCreationPreset(t.Context())
	require.NoError(t, err)
	require.Nil(t, p)
	in := creationPresetFixture()
	saved, err := s.SaveAccountCreationPreset(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, in, *saved)
	require.Equal(t, "leave this unchanged", r.values[SettingKeyOAuthAutoConfig])
	require.Contains(t, r.values, SettingKeyAccountCreationPreset)
	// A new service instance sees persisted values; caller edits cannot mutate storage.
	in.Extra[AccountCostMultiplierExtraKey] = 5
	loaded, err := NewAccountOpsService(r, nil, nil).GetAccountCreationPreset(t.Context())
	require.NoError(t, err)
	require.Equal(t, saved, loaded)
	require.Nil(t, loaded.LoadFactor)
	require.Zero(t, loaded.Priority+20)
	require.Zero(t, loaded.RateMultiplier)
	require.Zero(t, loaded.GroupRateMultiplier)
	for _, raw := range []string{"", "{}", "{", "null"} {
		r.values[SettingKeyAccountCreationPreset] = raw
		p, err := s.GetAccountCreationPreset(t.Context())
		if raw == "" {
			require.NoError(t, err)
			require.Nil(t, p)
		} else {
			require.Error(t, err)
			require.Nil(t, p)
		}
	}
	r.err = errors.New("offline")
	_, err = s.GetAccountCreationPreset(t.Context())
	require.Error(t, err)
}

func TestAccountCreationPresetRequiredFieldsAndLimits(t *testing.T) {
	for _, field := range []string{"version", "platform", "type", "concurrency", "priority", "load_factor", "rate_multiplier", "group_rate_multiplier", "group_ids", "proxy_id", "extra", "model_mapping"} {
		t.Run("missing_"+field, func(t *testing.T) {
			_, err := DecodeAccountCreationPreset(creationPresetPayload(t, func(m map[string]any) { delete(m, field) }))
			require.Error(t, err)
		})
	}
	for name, edit := range map[string]func(map[string]any){
		"secret_top_level": func(m map[string]any) { m["credentials"] = map[string]any{"access_token": "secret"} },
		"unknown":          func(m map[string]any) { m["future_setting"] = true },
		"version":          func(m map[string]any) { m["version"] = 2 },
		"platform":         func(m map[string]any) { m["platform"] = PlatformAnthropic },
		"type":             func(m map[string]any) { m["type"] = AccountTypeAPIKey },
		"null_concurrency": func(m map[string]any) { m["concurrency"] = nil },
		"concurrency":      func(m map[string]any) { m["concurrency"] = 10001 },
		"fraction":         func(m map[string]any) { m["concurrency"] = 1.5 },
		"priority":         func(m map[string]any) { m["priority"] = -10000 },
		"load_factor":      func(m map[string]any) { m["load_factor"] = 0 },
		"negative_rate":    func(m map[string]any) { m["rate_multiplier"] = -0.1 },
		"group_rate":       func(m map[string]any) { m["group_rate_multiplier"] = 1000001 },
		"groups":           func(m map[string]any) { m["group_ids"] = []int{2, 2} },
		"proxy":            func(m map[string]any) { m["proxy_id"] = 0 },
		"model":            func(m map[string]any) { m["model_mapping"] = map[string]string{"invalid model": "target"} },
		"huge": func(m map[string]any) {
			m["extra"] = map[string]any{"cost_multiplier": strings.Repeat("x", MaxAccountCreationPresetBytes)}
		},
		"null_pause": func(m map[string]any) { m["auto_pause_on_expired"] = nil },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeAccountCreationPreset(creationPresetPayload(t, edit))
			require.Error(t, err)
		})
	}
	p, err := DecodeAccountCreationPreset(creationPresetPayload(t, func(m map[string]any) {
		delete(m, "auto_pause_on_expired")
		m["priority"] = -9999
		m["concurrency"] = 0
	}))
	require.NoError(t, err)
	require.True(t, p.AutoPauseOnExpired)
	_, err = DecodeAccountCreationPreset(append(creationPresetPayload(t, func(map[string]any) {}), []byte("{}")...))
	require.Error(t, err)
}

func TestAccountCreationPresetRejectsSecretsRuntimeAndNestedUnknowns(t *testing.T) {
	for _, key := range []string{"access_token", "refresh_token", "id_token", "password", "mfa_secret", "cookie", "email", "account_id", "codex_fingerprint_seed",
		"auto_config_initial_revision", AutoConfigConcurrencyExtraKey, OpenAITwoFAAdmissionExtraKey,
		"openai_excel_bps_403_disabled_at", "openai_excel_bps_authorization_pending", "openai_compact_supported", "upstream_billing_probe", "unknown"} {
		t.Run(key, func(t *testing.T) {
			_, err := DecodeAccountCreationPreset(creationPresetPayload(t, func(m map[string]any) { m["extra"] = map[string]any{key: "secret"} }))
			require.Error(t, err)
		})
	}
	for name, extra := range map[string]map[string]any{
		"nested_secret":       {"codex_wire_options": map[string]any{"cookie": "secret"}},
		"location_secret":     {"codex_wire_user_location": map[string]any{"email": "secret"}},
		"profile_secret":      {"codex_client_profile": map[string]any{"access_token": "secret"}},
		"boolean_type":        {"openai_excel_bps": "true"},
		"transport_enum":      {"openai_excel_bps_transport": "http"},
		"ws_enum":             {"openai_oauth_responses_websockets_v2_mode": "unknown"},
		"compact_enum":        {"openai_compact_mode": "enabled"},
		"timezone":            {"openai_request_timezone": "not/a/timezone"},
		"model_length":        {"openai_excel_bps_models": []string{strings.Repeat("m", 201)}},
		"rpm_fraction":        {"base_rpm": 3.1},
		"recovery":            {ExcelBPS403RecoveryIntervalMinutesKey: 0},
		"recovery_dependency": {ExcelBPSAutoRecoverOn403Key: true},
		"bad_header":          {"upstream_request_id_header": "foo\r\nbar"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeAccountCreationPreset(creationPresetPayload(t, func(m map[string]any) { m["extra"] = extra }))
			require.Error(t, err)
		})
	}
}

func TestAccountCreationPresetAcceptsBPSAndCodexConfiguration(t *testing.T) {
	extra := DefaultExcelBPSDefaults().extra()
	extra["openai_excel_bps_transport"] = "ws"
	extra["openai_excel_bps_config_mode"] = "defaults"
	extra["openai_excel_bps_proxy_source"] = "ip_pool"
	extra["openai_request_timezone"] = "Asia/Singapore"
	extra["base_rpm"] = 15
	extra["rpm_strategy"] = "sticky_exempt"
	extra["rpm_sticky_buffer"] = 1
	extra["cost_multiplier"] = 0
	extra["openai_oauth_responses_websockets_v2_mode"] = "ctx_pool"
	extra["openai_oauth_responses_websockets_v2_enabled"] = true
	extra["openai_oauth_ws_sse_acceleration"] = true
	extra["openai_passthrough"] = true
	extra["openai_long_context_billing_enabled"] = true
	extra["openai_compact_mode"] = "force_off"
	extra["codex_fingerprint_mode"] = "full"
	extra["codex_identity_scheme"] = "v2"
	extra["codex_wire_options"] = map[string]any{"body_order": true, "cookie_replay": false}
	extra["codex_client_profile"] = map[string]any{"client": "cli", "platform": "windows", "versionMode": "latest"}
	extra["codex_wire_timezone"] = "Asia/Shanghai"
	extra["codex_wire_user_location"] = map[string]any{"country": "CN", "city": "Beijing"}
	_, err := DecodeAccountCreationPreset(creationPresetPayload(t, func(m map[string]any) { m["extra"] = extra }))
	require.NoError(t, err)
}

func TestAccountCreationPresetValidatesGroupsAndProxy(t *testing.T) {
	r := &creationPresetSettings{values: map[string]string{}}
	s := NewAccountOpsService(r, nil, nil)
	s.autoGroups = creationPresetGroups{groups: map[int64]*Group{1: {ID: 1, Platform: PlatformOpenAI, Status: StatusActive}, 2: {ID: 2, Platform: PlatformComposite, Status: StatusActive}, 3: {ID: 3, Platform: PlatformAnthropic, Status: StatusActive}, 4: {ID: 4, Platform: PlatformOpenAI, Status: "inactive"}}}
	proxyID := int64(8)
	s.creationPresetProxies = creationPresetProxies{proxy: &Proxy{ID: proxyID, Status: StatusActive}}
	p := creationPresetFixture()
	p.GroupIDs = []int64{1, 2}
	p.ProxyID = &proxyID
	_, err := s.SaveAccountCreationPreset(t.Context(), p)
	require.NoError(t, err)
	good := r.values[SettingKeyAccountCreationPreset]
	for _, id := range []int64{3, 4, 999} {
		p.GroupIDs = []int64{id}
		_, err := s.SaveAccountCreationPreset(t.Context(), p)
		require.Error(t, err)
		require.Equal(t, good, r.values[SettingKeyAccountCreationPreset])
	}
	p.GroupIDs = []int64{1}
	p.Extra[ExcelBPS403TargetGroupIDKey] = 3
	_, err = s.SaveAccountCreationPreset(t.Context(), p)
	require.Error(t, err)
	delete(p.Extra, ExcelBPS403TargetGroupIDKey)
	past := time.Now().Add(-time.Minute)
	for _, proxy := range []*Proxy{nil, {ID: 8, Status: "inactive"}, {ID: 8, Status: StatusActive, ExpiresAt: &past}} {
		s.creationPresetProxies = creationPresetProxies{proxy: proxy}
		_, err = s.SaveAccountCreationPreset(t.Context(), p)
		require.Error(t, err)
	}
}

func TestAccountCreationPresetObserverAndScopedKeyIsolation(t *testing.T) {
	r := &creationPresetSettings{values: map[string]string{}}
	s := NewAccountOpsService(r, nil, nil)
	for _, ctx := range []context.Context{WithObserverScope(t.Context(), []int64{1}), adminaccess.WithPrincipal(t.Context(), &adminaccess.Principal{})} {
		_, err := s.GetAccountCreationPreset(ctx)
		require.Error(t, err)
		_, err = s.SaveAccountCreationPreset(ctx, creationPresetFixture())
		require.Error(t, err)
		require.Empty(t, r.values)
	}
}

func TestAccountCreationPresetDoesNotGrantBPSAuthorizationOrModels(t *testing.T) {
	p, err := DecodeAccountCreationPreset(creationPresetPayload(t, func(m map[string]any) {
		m["extra"] = map[string]any{"openai_excel_bps": true, "openai_excel_bps_transport": "ws", "openai_excel_bps_models": []string{"gpt-6-astra"}}
	}))
	require.NoError(t, err)
	// Model routing remains limited to the submitted allowlist.
	routing := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: p.Extra}
	require.True(t, routing.IsExcelBPSEnabledForModel("gpt-6-astra"))
	require.False(t, routing.IsExcelBPSEnabledForModel("gpt-5.6-sol"))
	// The real create path still marks a normal ChatGPT credential as pending.
	s := &adminServiceImpl{accountRepo: &autoConfigAccountRepo{}, excelOAuthReauth: &OpenAIOAuthReauthService{}}
	a, err := s.CreateAccount(t.Context(), &CreateAccountInput{PreserveCreationConfig: true, Name: "pending", Platform: p.Platform, Type: p.Type, Concurrency: p.Concurrency, Priority: p.Priority, Extra: p.Extra, Credentials: map[string]any{"access_token": "fixture", "plan_type": "plus"}, GroupIDs: p.GroupIDs})
	require.NoError(t, err)
	require.Equal(t, true, a.Extra[ExcelBPSAuthorizationPendingKey])
	require.False(t, a.IsExcelBPSEnabled())
	require.False(t, a.IsExcelBPSEnabledForModel("gpt-6-astra"))
	require.False(t, a.IsExcelBPSEnabledForModel("gpt-5.6-sol"))
}

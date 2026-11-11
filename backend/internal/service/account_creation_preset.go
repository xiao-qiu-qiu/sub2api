package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const SettingKeyAccountCreationPreset = "account_creation_preset_v1"
const MaxAccountCreationPresetBytes = 64 << 10

// AccountCreationPreset contains reusable configuration only, never account identity or state.
type AccountCreationPreset struct {
	Version             int               `json:"version"`
	Platform            string            `json:"platform"`
	Type                string            `json:"type"`
	Concurrency         int               `json:"concurrency"`
	Priority            int               `json:"priority"`
	LoadFactor          *int              `json:"load_factor"`
	RateMultiplier      float64           `json:"rate_multiplier"`
	GroupRateMultiplier float64           `json:"group_rate_multiplier"`
	GroupIDs            []int64           `json:"group_ids"`
	ProxyID             *int64            `json:"proxy_id"`
	Extra               map[string]any    `json:"extra"`
	ModelMapping        map[string]string `json:"model_mapping"`
	AutoPauseOnExpired  bool              `json:"auto_pause_on_expired"`
}

func creationPresetInvalid(message string) error {
	return infraerrors.BadRequest("ACCOUNT_CREATION_PRESET_INVALID", message)
}

// DecodeAccountCreationPreset rejects unknown top-level fields and incomplete replacements.
func DecodeAccountCreationPreset(data []byte) (AccountCreationPreset, error) {
	p := AccountCreationPreset{AutoPauseOnExpired: true}
	if len(data) > MaxAccountCreationPresetBytes {
		return p, creationPresetInvalid("creation preset exceeds 64 KiB")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return p, creationPresetInvalid("creation preset must be an object")
	}
	required := []string{"version", "platform", "type", "concurrency", "priority", "load_factor", "rate_multiplier", "group_rate_multiplier", "group_ids", "proxy_id", "extra", "model_mapping"}
	for _, key := range required {
		value, present := fields[key]
		if !present || (bytes.Equal(bytes.TrimSpace(value), []byte("null")) && key != "load_factor" && key != "proxy_id") {
			return p, creationPresetInvalid("missing or null field: " + key)
		}
	}
	for key, value := range fields {
		if !slices.Contains(required, key) && key != "auto_pause_on_expired" {
			return p, creationPresetInvalid("unknown creation preset field")
		}
		if key == "auto_pause_on_expired" && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return p, creationPresetInvalid("auto_pause_on_expired must be a boolean")
		}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, creationPresetInvalid("invalid creation preset field type")
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return p, creationPresetInvalid("unexpected trailing data")
	}
	return p, validateAccountCreationPreset(p)
}

func validateAccountCreationPreset(p AccountCreationPreset) error {
	bad := creationPresetInvalid
	if p.Version != 1 || p.Platform != PlatformOpenAI || p.Type != AccountTypeOAuth {
		return bad("only version 1 OpenAI OAuth presets are supported")
	}
	if p.Concurrency < 0 || p.Concurrency > 10000 || (p.LoadFactor != nil && (*p.LoadFactor < 1 || *p.LoadFactor > 10000)) {
		return bad("concurrency must be 0-10000; load_factor must be null or 1-10000")
	}
	if err := ValidateAccountPriority(p.Priority); err != nil {
		return err
	}
	for _, value := range []float64{p.RateMultiplier, p.GroupRateMultiplier} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1000000 {
			return bad("multipliers must be finite numbers between 0 and 1000000")
		}
	}
	if p.GroupIDs == nil || len(p.GroupIDs) > 100 {
		return bad("group_ids must be an array of at most 100 groups")
	}
	seen := map[int64]bool{}
	for _, id := range p.GroupIDs {
		if id <= 0 || seen[id] {
			return bad("group IDs must be positive and unique")
		}
		seen[id] = true
	}
	if p.ProxyID != nil && *p.ProxyID <= 0 {
		return bad("proxy_id must be null or positive")
	}
	if p.ModelMapping == nil || len(p.ModelMapping) > 200 {
		return bad("model_mapping must be an object with at most 200 entries")
	}
	for source, target := range p.ModelMapping {
		if !creationPresetModelName(source) || !creationPresetModelName(target) {
			return bad("invalid model mapping name")
		}
	}
	if p.Extra == nil {
		return bad("extra must be an object")
	}
	return validateCreationPresetExtra(p.Extra)
}

func creationPresetModelName(value string) bool {
	return value != "" && len(value) <= 200 && strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

// An explicit whitelist prevents credentials, per-account identifiers, runtime markers
// and future unreviewed extra keys from entering this globally reusable setting.
func validateCreationPresetExtra(extra map[string]any) error {
	bad := creationPresetInvalid
	for key, raw := range extra {
		switch key {
		case "openai_excel_bps", "openai_excel_bps_mihomo", "openai_excel_bps_cache_creation_as_input",
			"openai_excel_bps_omit_unsupported_tools", "openai_excel_bps_ignore_encrypted_content",
			"openai_excel_bps_auto_disable_on_403", "openai_excel_bps_auto_recover_on_403", "openai_excel_bps_auto_move_on_403",
			"openai_oauth_responses_websockets_v2_enabled", "openai_oauth_ws_sse_acceleration",
			"openai_passthrough", "openai_oauth_passthrough", "openai_responses_flatten_namespaces",
			"openai_long_context_billing_enabled", "codex_cli_only", "codex_cli_only_allow_app_server":
			if _, ok := raw.(bool); !ok {
				return bad(key + " must be a boolean")
			}
		case "openai_excel_bps_config_mode":
			if !creationPresetEnum(raw, "initial", "defaults") {
				return bad("invalid BPS configuration mode")
			}
		case "openai_excel_bps_transport":
			if !creationPresetEnum(raw, "sse", "ws") {
				return bad("invalid BPS transport")
			}
		case "openai_excel_bps_proxy_source":
			if !creationPresetEnum(raw, "mihomo", "ip_pool") {
				return bad("invalid BPS proxy source")
			}
		case "openai_excel_bps_models":
			models, ok := raw.([]any)
			if !ok || len(models) > 100 {
				return bad("BPS models must be an array with at most 100 entries")
			}
			seen := map[string]bool{}
			for _, value := range models {
				model, ok := value.(string)
				if !ok || !creationPresetModelName(model) || seen[model] {
					return bad("BPS models must be valid unique names")
				}
				seen[model] = true
			}
		case ExcelBPS403RecoveryIntervalMinutesKey:
			if _, ok := excelBPS403RecoveryMinutes(raw); !ok {
				return bad("BPS recovery interval must be 1-10080 minutes")
			}
		case ExcelBPS403TargetGroupIDKey:
			if _, ok := excelBPS403GroupID(raw); !ok {
				return bad("BPS target group must be a nonnegative integer")
			}
		case "base_rpm", "rpm_sticky_buffer":
			n, ok := raw.(float64)
			if !ok || math.Trunc(n) != n || n < 0 || n > 1000000 {
				return bad(key + " must be an integer between 0 and 1000000")
			}
		case "rpm_strategy":
			if !creationPresetEnum(raw, "tiered", "sticky_exempt") {
				return bad("invalid RPM strategy")
			}
		case "cost_multiplier":
			if _, ok := accountCostMultiplierNumber(raw); !ok {
				return bad("invalid cost multiplier")
			}
		case "openai_oauth_responses_websockets_v2_mode":
			if !creationPresetEnum(raw, "off", "ctx_pool", "passthrough", "http_bridge") {
				return bad("invalid OAuth WebSocket mode")
			}
		case "codex_fingerprint_mode":
			if !creationPresetEnum(raw, "off", "device", "session", "full") {
				return bad("invalid Codex fingerprint mode")
			}
		case "openai_compact_mode":
			if !creationPresetEnum(raw, "auto", "force_on", "force_off") {
				return bad("invalid compact mode")
			}
		case "upstream_request_id_header":
			if value, ok := raw.(string); !ok || len(value) > 128 {
				return bad("invalid upstream request ID header")
			}
		case openAIRequestTimezoneExtraKey:
			if value, ok := raw.(string); !ok || len(value) > 64 {
				return bad("invalid OpenAI request timezone")
			}
		case "codex_identity_scheme", "codex_client_profile", codexWireOptionsExtraKey, "codex_wire_timezone", "codex_wire_user_location":
			// Reuse the strict nested profile, option and location validators below.
		default:
			return bad("extra contains an unsupported configuration field")
		}
	}
	if extra[ExcelBPSAutoRecoverOn403Key] == true && extra["openai_excel_bps_auto_disable_on_403"] != true {
		return bad("BPS recovery requires automatic disabling on 403")
	}
	if err := validateExcelBPS403GroupExtra(extra); err != nil {
		return err
	}
	if err := ValidateAccountCostMultiplierExtra(extra); err != nil {
		return err
	}
	if err := ValidateOpenAIRequestTimezoneExtra(PlatformOpenAI, extra); err != nil {
		return err
	}
	if err := ValidateUpstreamRequestIDHeaderExtra(extra); err != nil {
		return err
	}
	return validateCodexFingerprintSettings(&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, extra)
}

func creationPresetEnum(raw any, allowed ...string) bool {
	value, ok := raw.(string)
	return ok && slices.Contains(allowed, value)
}

func authorizeCreationPreset(ctx context.Context) error {
	if _, observer := ObserverGroupIDs(ctx); observer {
		return ErrObserverScope
	}
	if adminaccess.FromContext(ctx) != nil {
		return infraerrors.Forbidden("ACCOUNT_CREATION_PRESET_FORBIDDEN", "global creation presets require an administrator")
	}
	return nil
}

func (s *AccountOpsService) GetAccountCreationPreset(ctx context.Context) (*AccountCreationPreset, error) {
	if err := authorizeCreationPreset(ctx); err != nil {
		return nil, err
	}
	if s == nil || s.settings == nil {
		return nil, errors.New("creation preset storage unavailable")
	}
	raw, err := s.settings.GetValue(ctx, SettingKeyAccountCreationPreset)
	if errors.Is(err, ErrSettingNotFound) || (err == nil && raw == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p, err := DecodeAccountCreationPreset([]byte(raw))
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *AccountOpsService) SaveAccountCreationPreset(ctx context.Context, p AccountCreationPreset) (*AccountCreationPreset, error) {
	if err := authorizeCreationPreset(ctx); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, creationPresetInvalid("invalid creation preset")
	}
	p, err = DecodeAccountCreationPreset(raw)
	if err != nil {
		return nil, err
	}
	if s == nil || s.settings == nil {
		return nil, errors.New("creation preset storage unavailable")
	}
	ids := slices.Clone(p.GroupIDs)
	if id, ok := excelBPS403GroupID(p.Extra[ExcelBPS403TargetGroupIDKey]); ok && id > 0 && !slices.Contains(ids, id) {
		ids = append(ids, id)
	}
	for _, id := range ids {
		if s.autoGroups == nil {
			return nil, errors.New("creation preset group repository unavailable")
		}
		g, err := s.autoGroups.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if g == nil || g.Status != StatusActive || (g.Platform != PlatformOpenAI && g.Platform != PlatformComposite) {
			return nil, creationPresetInvalid("select active OpenAI or composite groups")
		}
	}
	if p.ProxyID != nil {
		if s.creationPresetProxies == nil {
			return nil, errors.New("creation preset proxy repository unavailable")
		}
		proxy, err := s.creationPresetProxies.GetByID(ctx, *p.ProxyID)
		if err != nil {
			return nil, err
		}
		if proxy == nil || !proxy.IsActive() || proxy.IsExpired(time.Now()) {
			return nil, creationPresetInvalid("select an active, unexpired proxy")
		}
	}
	if err := s.settings.Set(ctx, SettingKeyAccountCreationPreset, string(raw)); err != nil {
		return nil, err
	}
	return &p, nil
}

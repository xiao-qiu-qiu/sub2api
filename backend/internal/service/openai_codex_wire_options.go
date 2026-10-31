package service

import (
	"fmt"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
)

const codexWireOptionsExtraKey = "codex_wire_options"

var codexWireOptionNames = map[string]struct{}{
	"body_order": {}, "request_compression": {}, "environment_timezone": {},
	"web_search_location": {}, "settings_user": {}, "guardian_credits": {}, "cookie_replay": {},
}

// Transport decoration is explicitly enabled per credential. It never follows
// identity-scheme selection implicitly, and cannot replace a harvested identity.
func codexWireOptionEnabled(c *gin.Context, account *Account, option string) bool {
	if account == nil || !account.IsOpenAIOAuthLike() {
		return false
	}
	if c != nil {
		if skip, _ := c.Get("codex_wire_skip"); skip == true {
			return false
		}
	}
	source := codexAccountIdentitySource(c, account)
	if source == nil || !source.IsOpenAIOAuthLike() {
		return false
	}
	options, _ := source.Extra[codexWireOptionsExtraKey].(map[string]any)
	enabled, _ := options[option].(bool)
	return enabled
}

func hasCodexFingerprintSettings(extra map[string]any) bool {
	for _, key := range []string{"codex_identity_scheme", "codex_client_profile", codexWireOptionsExtraKey, "codex_wire_timezone", "codex_wire_user_location"} {
		if _, ok := extra[key]; ok {
			return true
		}
	}
	return false
}

// Validate the supplied keys only; a partial update must not overwrite the
// credential's remaining configuration or reject unrelated historical values.
func validateCodexFingerprintSettings(account *Account, extra map[string]any) error {
	if !hasCodexFingerprintSettings(extra) {
		return nil
	}
	bad := func(message string) error { return infraerrors.BadRequest("CODEX_FINGERPRINT_INVALID", message) }
	if account == nil || !account.IsOpenAIOAuthLike() {
		return bad("Codex fingerprint settings require an OpenAI OAuth or setup-token account")
	}
	if account.IsCredentialShadow() {
		return bad("Configure Codex fingerprint settings on the credential account")
	}
	if raw, exists := extra["codex_identity_scheme"]; exists && raw != nil {
		value, ok := raw.(string)
		if !ok || (value != "v1" && value != "v2") {
			return bad("codex_identity_scheme must be v1 or v2")
		}
	}
	if raw, exists := extra[codexWireOptionsExtraKey]; exists && raw != nil {
		options, ok := raw.(map[string]any)
		if !ok {
			return bad("codex_wire_options must be an object")
		}
		for name, value := range options {
			if _, known := codexWireOptionNames[name]; !known {
				return bad(fmt.Sprintf("Unknown Codex wire option: %s", name))
			}
			if _, ok := value.(bool); !ok {
				return bad(fmt.Sprintf("Codex wire option %s must be a boolean", name))
			}
		}
	}
	if raw, exists := extra["codex_wire_timezone"]; exists && raw != nil {
		name, ok := raw.(string)
		if !ok {
			return bad("codex_wire_timezone must be an IANA timezone")
		}
		if name != "" {
			if _, err := time.LoadLocation(name); err != nil || len(name) > 64 || strings.ContainsAny(name, "\r\n\x00") {
				return bad("codex_wire_timezone must be an IANA timezone")
			}
		}
	}
	if raw, exists := extra["codex_wire_user_location"]; exists && raw != nil {
		location, ok := raw.(map[string]any)
		if !ok {
			return bad("codex_wire_user_location must be an object")
		}
		for name, rawValue := range location {
			if name != "city" && name != "region" && name != "country" && name != "timezone" {
				return bad("Unknown Codex location field: " + name)
			}
			value, ok := rawValue.(string)
			if !ok || len(value) > 128 || strings.ContainsAny(value, "\r\n\x00") {
				return bad("Invalid Codex location field: " + name)
			}
			if name == "timezone" && value != "" {
				if _, err := time.LoadLocation(value); err != nil {
					return bad("Invalid Codex location timezone")
				}
			}
		}
	}
	return validateCodexClientProfileExtra(account, extra)
}

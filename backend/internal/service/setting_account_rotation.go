package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

const (
	SettingKeyAccountRotationSettings = "account_rotation_settings"
	AccountRotationModeAggressive     = "aggressive"
	AccountRotationModeSticky         = "sticky"
	accountRotationSettingsCacheTTL   = 30 * time.Second
)

// AccountRotationSettings configures experimental rotation for all normally
// scheduled accounts, with staggered windows planned separately per platform.
// A work window starts when account concurrency first becomes positive. During
// work, 300 continuous seconds without requests resets that window (fixed).
// After work ends, ScheduledHoldEnabled keeps new requests waiting on the same
// account through draining and rest. With that option disabled, aggressive
// rejects all new requests; sticky admits only existing sticky sessions while
// draining. Rest begins at zero concurrency and lasts a random duration in
// [RestMinMinutes, RestMaxMinutes]. Manual and early rest always hold requests.
type AccountRotationSettings struct {
	Enabled                bool `json:"enabled"`
	ScheduledHoldEnabled   bool `json:"scheduled_hold_enabled"`
	EarlyRestEnabled       bool `json:"early_rest_enabled"`
	EarlyRestWindowMinutes int  `json:"early_rest_window_minutes"`
	// Early rest requires concurrency strictly below this threshold.
	EarlyRestConcurrencyThreshold int    `json:"early_rest_concurrency_threshold"`
	EarlyRestSeconds              int    `json:"early_rest_seconds"`
	Mode                          string `json:"mode"`
	WorkMinutes                   int    `json:"work_minutes"`
	RestMinMinutes                int    `json:"rest_min_minutes"`
	RestMaxMinutes                int    `json:"rest_max_minutes"`
	StaggerPercent                int    `json:"stagger_percent"` // Elastic work-window adjustment, percent.
	RecoveryMinIntervalMS         int    `json:"recovery_min_interval_ms"`
	RecoveryMaxIntervalMS         int    `json:"recovery_max_interval_ms"`
	// Revision is server-owned and changes only when policy content changes.
	// Unsaved defaults and legacy records have no revision; schedulers may use
	// a hash of policy content to identify those configurations.
	Revision string `json:"revision,omitempty"`
}

func DefaultAccountRotationSettings() *AccountRotationSettings {
	return &AccountRotationSettings{
		ScheduledHoldEnabled:          true,
		EarlyRestWindowMinutes:        20,
		EarlyRestConcurrencyThreshold: 2,
		EarlyRestSeconds:              120,
		Mode:                          AccountRotationModeAggressive,
		WorkMinutes:                   300,
		RestMinMinutes:                10,
		RestMaxMinutes:                30,
		StaggerPercent:                10,
		RecoveryMinIntervalMS:         500,
		RecoveryMaxIntervalMS:         2000,
	}
}

func (settings *AccountRotationSettings) Validate() error {
	if settings == nil {
		return fmt.Errorf("account_rotation_settings must be an object")
	}
	if settings.EarlyRestWindowMinutes < 1 || settings.EarlyRestWindowMinutes > 1440 {
		return fmt.Errorf("account_rotation_settings.early_rest_window_minutes must be between 1 and 1440")
	}
	if settings.EarlyRestConcurrencyThreshold < 1 || settings.EarlyRestConcurrencyThreshold > 10000 {
		return fmt.Errorf("account_rotation_settings.early_rest_concurrency_threshold must be between 1 and 10000")
	}
	if settings.EarlyRestSeconds < 1 || settings.EarlyRestSeconds > 7200 {
		return fmt.Errorf("account_rotation_settings.early_rest_seconds must be between 1 and 7200")
	}
	if settings.Mode != AccountRotationModeAggressive && settings.Mode != AccountRotationModeSticky {
		return fmt.Errorf("account_rotation_settings.mode must be aggressive or sticky")
	}
	if settings.WorkMinutes < 1 || settings.WorkMinutes > 1440 {
		return fmt.Errorf("account_rotation_settings.work_minutes must be between 1 and 1440")
	}
	if settings.RestMinMinutes < 1 || settings.RestMinMinutes > 120 {
		return fmt.Errorf("account_rotation_settings.rest_min_minutes must be between 1 and 120")
	}
	if settings.RestMaxMinutes < settings.RestMinMinutes || settings.RestMaxMinutes > 120 {
		return fmt.Errorf("account_rotation_settings.rest_max_minutes must be between rest_min_minutes and 120")
	}
	if settings.StaggerPercent < 0 || settings.StaggerPercent > 30 {
		return fmt.Errorf("account_rotation_settings.stagger_percent must be between 0 and 30")
	}
	if settings.RecoveryMinIntervalMS < 1 || settings.RecoveryMinIntervalMS > 60000 {
		return fmt.Errorf("account_rotation_settings.recovery_min_interval_ms must be between 1 and 60000")
	}
	if settings.RecoveryMaxIntervalMS < settings.RecoveryMinIntervalMS || settings.RecoveryMaxIntervalMS > 60000 {
		return fmt.Errorf("account_rotation_settings.recovery_max_interval_ms must be between recovery_min_interval_ms and 60000")
	}
	return nil
}

func parseAccountRotationSettings(raw string) *AccountRotationSettings {
	settings := DefaultAccountRotationSettings()
	if strings.TrimSpace(raw) == "" {
		return settings
	}
	// Absent fields inherit defaults, but explicit null must not silently enable
	// scheduled holds or turn malformed early-rest values into valid defaults.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return DefaultAccountRotationSettings()
	}
	for _, field := range []string{"scheduled_hold_enabled", "early_rest_window_minutes", "early_rest_concurrency_threshold", "early_rest_seconds"} {
		if bytes.Equal(bytes.TrimSpace(fields[field]), []byte("null")) {
			return DefaultAccountRotationSettings()
		}
	}
	if err := json.Unmarshal([]byte(raw), settings); err != nil || settings.Validate() != nil {
		return DefaultAccountRotationSettings()
	}
	return settings
}

type cachedAccountRotationSettings struct {
	settings  AccountRotationSettings
	expiresAt time.Time
}

// GetAccountRotationSettings returns a copy backed by a 30-second local cache.
// Missing settings use defaults; storage errors are returned without caching.
func (s *SettingService) GetAccountRotationSettings(ctx context.Context) (*AccountRotationSettings, error) {
	if s == nil || s.settingRepo == nil {
		return DefaultAccountRotationSettings(), nil
	}
	for {
		if cached, ok := s.accountRotationSettingsCache.Load().(*cachedAccountRotationSettings); ok && cached != nil && time.Now().Before(cached.expiresAt) {
			result := cached.settings
			return &result, nil
		}
		s.accountRotationSettingsMu.Lock()
		generation := s.accountRotationSettingsGen
		s.accountRotationSettingsMu.Unlock()
		value, err := s.settingRepo.GetValue(ctx, SettingKeyAccountRotationSettings)
		if err != nil && !errors.Is(err, ErrSettingNotFound) {
			return nil, fmt.Errorf("get account rotation settings: %w", err)
		}
		if errors.Is(err, ErrSettingNotFound) {
			value = ""
		}
		settings := parseAccountRotationSettings(value)
		s.accountRotationSettingsMu.Lock()
		if generation != s.accountRotationSettingsGen {
			s.accountRotationSettingsMu.Unlock()
			continue
		}
		s.accountRotationSettingsCache.Store(&cachedAccountRotationSettings{
			settings: *settings, expiresAt: time.Now().Add(accountRotationSettingsCacheTTL),
		})
		s.accountRotationSettingsMu.Unlock()
		return settings, nil
	}
}

// InvalidateAccountRotationSettingsCache prevents in-flight reads from restoring
// an older snapshot after an admin save. Other instances refresh within 30s.
func (s *SettingService) InvalidateAccountRotationSettingsCache() {
	if s == nil {
		return
	}
	s.accountRotationSettingsMu.Lock()
	defer s.accountRotationSettingsMu.Unlock()
	s.accountRotationSettingsGen++
	s.accountRotationSettingsCache.Store((*cachedAccountRotationSettings)(nil))
}

func (s *SettingService) marshalAccountRotationSettingsUpdate(ctx context.Context, settings *AccountRotationSettings) (string, error) {
	if err := settings.Validate(); err != nil {
		return "", infraerrors.BadRequest("INVALID_ACCOUNT_ROTATION_SETTINGS", err.Error())
	}
	if settings.Enabled && s.cfg != nil && s.cfg.Runtime.RegionalConcurrencyFile != "" {
		return "", infraerrors.BadRequest("ACCOUNT_ROTATION_REGIONAL_CONCURRENCY_UNSUPPORTED", "account rotation cannot be enabled while runtime.regional_concurrency_file is configured: regional concurrency mode is not supported")
	}
	// Read storage rather than the 30-second runtime cache, so a repeated save
	// preserves the revision written by another instance as well.
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyAccountRotationSettings)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return "", fmt.Errorf("get account rotation settings before update: %w", err)
	}
	if errors.Is(err, ErrSettingNotFound) {
		raw = ""
	}
	previous := *parseAccountRotationSettings(raw)
	previousRevision := previous.Revision
	previous.Revision = ""
	next := *settings
	next.Revision = "" // Client revisions never influence policy identity.
	if next == previous {
		next.Revision = previousRevision
	} else {
		next.Revision = uuid.NewString()
	}
	data, err := json.Marshal(&next)
	if err != nil {
		return "", fmt.Errorf("marshal account rotation settings: %w", err)
	}
	return string(data), nil
}

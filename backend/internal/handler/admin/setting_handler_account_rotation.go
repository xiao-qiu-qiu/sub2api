package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// A supplied rotation object replaces the whole configuration. Pointers reject
// omitted/null required fields while preserving valid false and zero values.
// Scheduled holds, early rest and recovery intervals are optional for legacy
// clients; explicit null is rejected just like the required policy fields.
type updateAccountRotationSettingsRequest struct {
	Enabled                       *bool           `json:"enabled"`
	ScheduledHoldEnabled          json.RawMessage `json:"scheduled_hold_enabled"`
	EarlyRestEnabled              json.RawMessage `json:"early_rest_enabled"`
	EarlyRestWindowMinutes        json.RawMessage `json:"early_rest_window_minutes"`
	EarlyRestConcurrencyThreshold json.RawMessage `json:"early_rest_concurrency_threshold"`
	EarlyRestSeconds              json.RawMessage `json:"early_rest_seconds"`
	Mode                          *string         `json:"mode"`
	WorkMinutes                   *int            `json:"work_minutes"`
	RestMinMinutes                *int            `json:"rest_min_minutes"`
	RestMaxMinutes                *int            `json:"rest_max_minutes"`
	StaggerPercent                *int            `json:"stagger_percent"`
	RecoveryMinIntervalMS         json.RawMessage `json:"recovery_min_interval_ms"`
	RecoveryMaxIntervalMS         json.RawMessage `json:"recovery_max_interval_ms"`
	Revision                      string          `json:"revision"` // Accepted for GET/PUT round trips; ignored.
}

func decodeAccountRotationSettings(raw json.RawMessage) (*service.AccountRotationSettings, error) {
	var req updateAccountRotationSettingsRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return nil, fmt.Errorf("invalid account_rotation_settings: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("account_rotation_settings must contain a single JSON object")
	}
	if req.Enabled == nil || req.Mode == nil || req.WorkMinutes == nil || req.RestMinMinutes == nil || req.RestMaxMinutes == nil || req.StaggerPercent == nil {
		return nil, fmt.Errorf("account_rotation_settings requires enabled, mode, work_minutes, rest_min_minutes, rest_max_minutes and stagger_percent; fields cannot be null")
	}
	settings := service.DefaultAccountRotationSettings()
	settings.Enabled, settings.Mode, settings.WorkMinutes = *req.Enabled, *req.Mode, *req.WorkMinutes
	settings.RestMinMinutes, settings.RestMaxMinutes = *req.RestMinMinutes, *req.RestMaxMinutes
	settings.StaggerPercent = *req.StaggerPercent
	for _, field := range []struct {
		name   string
		raw    json.RawMessage
		target *bool
	}{
		{"scheduled_hold_enabled", req.ScheduledHoldEnabled, &settings.ScheduledHoldEnabled},
		{"early_rest_enabled", req.EarlyRestEnabled, &settings.EarlyRestEnabled},
	} {
		if len(field.raw) == 0 {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(field.raw), []byte("null")) {
			return nil, fmt.Errorf("account_rotation_settings.%s cannot be null", field.name)
		}
		if err := json.Unmarshal(field.raw, field.target); err != nil {
			return nil, fmt.Errorf("invalid account_rotation_settings.%s: %w", field.name, err)
		}
	}
	for _, field := range []struct {
		name   string
		raw    json.RawMessage
		target *int
	}{
		{"early_rest_window_minutes", req.EarlyRestWindowMinutes, &settings.EarlyRestWindowMinutes},
		{"early_rest_concurrency_threshold", req.EarlyRestConcurrencyThreshold, &settings.EarlyRestConcurrencyThreshold},
		{"early_rest_seconds", req.EarlyRestSeconds, &settings.EarlyRestSeconds},
		{"recovery_min_interval_ms", req.RecoveryMinIntervalMS, &settings.RecoveryMinIntervalMS},
		{"recovery_max_interval_ms", req.RecoveryMaxIntervalMS, &settings.RecoveryMaxIntervalMS},
	} {
		if len(field.raw) == 0 {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(field.raw), []byte("null")) {
			return nil, fmt.Errorf("account_rotation_settings.%s cannot be null", field.name)
		}
		if err := json.Unmarshal(field.raw, field.target); err != nil {
			return nil, fmt.Errorf("invalid account_rotation_settings.%s: %w", field.name, err)
		}
	}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	return settings, nil
}

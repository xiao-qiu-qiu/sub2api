package admin

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// A supplied rotation object replaces the whole configuration. Pointers reject
// omitted/null required fields while preserving valid false and zero values.
// EarlyRestEnabled is optional for clients predating early rest; explicit null
// is rejected just like the required policy fields.
type updateAccountRotationSettingsRequest struct {
	Enabled          *bool           `json:"enabled"`
	EarlyRestEnabled json.RawMessage `json:"early_rest_enabled"`
	Mode             *string         `json:"mode"`
	WorkMinutes      *int            `json:"work_minutes"`
	RestMinMinutes   *int            `json:"rest_min_minutes"`
	RestMaxMinutes   *int            `json:"rest_max_minutes"`
	StaggerPercent   *int            `json:"stagger_percent"`
	Revision         string          `json:"revision"` // Accepted for GET/PUT round trips; ignored.
}

func decodeAccountRotationSettings(raw json.RawMessage) (*service.AccountRotationSettings, error) {
	var req updateAccountRotationSettingsRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return nil, fmt.Errorf("invalid account_rotation_settings: %w", err)
	}
	if req.Enabled == nil || req.Mode == nil || req.WorkMinutes == nil || req.RestMinMinutes == nil || req.RestMaxMinutes == nil || req.StaggerPercent == nil {
		return nil, fmt.Errorf("account_rotation_settings requires enabled, mode, work_minutes, rest_min_minutes, rest_max_minutes and stagger_percent; fields cannot be null")
	}
	settings := &service.AccountRotationSettings{
		Enabled: *req.Enabled, Mode: *req.Mode, WorkMinutes: *req.WorkMinutes,
		RestMinMinutes: *req.RestMinMinutes, RestMaxMinutes: *req.RestMaxMinutes,
		StaggerPercent: *req.StaggerPercent,
	}
	if len(req.EarlyRestEnabled) != 0 {
		if bytes.Equal(bytes.TrimSpace(req.EarlyRestEnabled), []byte("null")) {
			return nil, fmt.Errorf("account_rotation_settings.early_rest_enabled cannot be null")
		}
		if err := json.Unmarshal(req.EarlyRestEnabled, &settings.EarlyRestEnabled); err != nil {
			return nil, fmt.Errorf("invalid account_rotation_settings.early_rest_enabled: %w", err)
		}
	}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	return settings, nil
}

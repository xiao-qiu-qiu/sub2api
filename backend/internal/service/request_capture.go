package service

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/requestcapture"
)

const (
	SettingKeyRequestCaptureEnabled                        = "request_capture_enabled"
	SettingKeyRequestCaptureQuotaMiB                       = "request_capture_quota_mib"
	SettingKeyRequestCaptureRetentionDays                  = "request_capture_retention_days"
	SettingKeyRequestCaptureIncludeDistillation            = "request_capture_include_distillation"
	SettingKeyRequestCaptureDistillationMinOutputTokens    = "request_capture_distillation_min_output_tokens"
	SettingKeyRequestCaptureDistillationMaxCacheReadTokens = "request_capture_distillation_max_cache_read_tokens"
	SettingKeyRequestCaptureDistillationNonStreamOnly      = "request_capture_distillation_non_stream_only"

	defaultRequestCaptureDistillationMinOutputTokens int64 = 3000
)

func parseRequestCaptureDistillationTokens(raw string, fallback int64) int64 {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 || value > requestcapture.MaxDistillationTokens {
		return fallback
	}
	return value
}

func (s *SystemSettings) requestCaptureConfig() requestcapture.Config {
	c := requestcapture.Config{
		Enabled:                        s.RequestCaptureEnabled,
		QuotaMiB:                       s.RequestCaptureQuotaMiB,
		RetentionDays:                  s.RequestCaptureRetentionDays,
		IncludeDistillation:            s.RequestCaptureIncludeDistillation,
		DistillationMinOutputTokens:    s.RequestCaptureDistillationMinOutputTokens,
		DistillationMaxCacheReadTokens: s.RequestCaptureDistillationMaxCacheReadTokens,
		DistillationNonStreamOnly:      s.RequestCaptureDistillationNonStreamOnly,
	}
	if c.QuotaMiB == 0 {
		c.QuotaMiB = 1024
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = 7
	}
	return c
}
func ProvideRequestCaptureManager(db *sql.DB, settings *SettingService) (*requestcapture.Manager, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config, err := settings.GetAllSettings(ctx)
	if err != nil {
		return nil, err
	}
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "./data"
	}
	manager, err := requestcapture.New(&requestcapture.SQLStore{DB: db}, filepath.Join(dir, "request-captures"), config.requestCaptureConfig())
	if err != nil {
		return nil, err
	}
	settings.requestCapture = manager
	return manager, nil
}

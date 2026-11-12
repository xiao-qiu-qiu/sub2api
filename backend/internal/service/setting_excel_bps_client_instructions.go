package service

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
)

const (
	ExcelBPSClientInstructionsMaxLength = 16000
	excelBPSClientInstructionsCacheTTL  = 60 * time.Second
	excelBPSClientInstructionsErrorTTL  = 5 * time.Second
	excelBPSClientInstructionsDBTimeout = 5 * time.Second
)

type cachedExcelBPSClientInstructions struct {
	value     string
	expiresAt int64
}

// NormalizeExcelBPSClientInstructions validates and returns the stored override.
// An empty override selects the built-in default; internal whitespace is preserved.
func NormalizeExcelBPSClientInstructions(value string) (string, error) {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > ExcelBPSClientInstructionsMaxLength {
		return "", infraerrors.BadRequest("INVALID_EXCEL_BPS_CLIENT_INSTRUCTIONS", "Excel BPS client instructions must not exceed 16000 Unicode characters")
	}
	if value == strings.TrimSpace(basispoints.DefaultClientInstructions) {
		return "", nil
	}
	return value, nil
}

func effectiveExcelBPSClientInstructions(value string) string {
	value, err := NormalizeExcelBPSClientInstructions(value)
	if err != nil || value == "" {
		return basispoints.DefaultClientInstructions
	}
	return value
}

// GetExcelBPSClientInstructions returns the effective administrator-managed prompt.
// Fresh reads are atomic; saves publish immediately and other nodes refresh by TTL.
func (s *SettingService) GetExcelBPSClientInstructions(ctx context.Context) string {
	if s == nil || s.settingRepo == nil {
		return basispoints.DefaultClientInstructions
	}
	if cached, ok := s.excelBPSClientInstructionsCache.Load().(*cachedExcelBPSClientInstructions); ok && time.Now().UnixNano() < cached.expiresAt {
		return cached.value
	}
	result, _, _ := s.excelBPSClientInstructionsSF.Do(SettingKeyExcelBPSClientInstructions, func() (any, error) {
		s.excelBPSClientInstructionsMu.Lock()
		generation := s.excelBPSClientInstructionsGen
		if cached, ok := s.excelBPSClientInstructionsCache.Load().(*cachedExcelBPSClientInstructions); ok && time.Now().UnixNano() < cached.expiresAt {
			s.excelBPSClientInstructionsMu.Unlock()
			return cached.value, nil
		}
		s.excelBPSClientInstructionsMu.Unlock()
		if ctx == nil {
			ctx = context.Background()
		}
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), excelBPSClientInstructionsDBTimeout)
		defer cancel()
		value, err := s.settingRepo.GetValue(dbCtx, SettingKeyExcelBPSClientInstructions)
		s.excelBPSClientInstructionsMu.Lock()
		defer s.excelBPSClientInstructionsMu.Unlock()
		if generation != s.excelBPSClientInstructionsGen {
			if cached, ok := s.excelBPSClientInstructionsCache.Load().(*cachedExcelBPSClientInstructions); ok {
				return cached.value, nil
			}
			return basispoints.DefaultClientInstructions, nil
		}
		ttl := excelBPSClientInstructionsCacheTTL
		if err != nil && !errors.Is(err, ErrSettingNotFound) {
			value = basispoints.DefaultClientInstructions
			if cached, ok := s.excelBPSClientInstructionsCache.Load().(*cachedExcelBPSClientInstructions); ok {
				value = cached.value
			}
			ttl = excelBPSClientInstructionsErrorTTL
		}
		value = effectiveExcelBPSClientInstructions(value)
		s.storeExcelBPSClientInstructionsCache(value, ttl)
		return value, nil
	})
	value, ok := result.(string)
	if !ok {
		return basispoints.DefaultClientInstructions
	}
	return value
}

// persistSystemSettingsUpdates keeps the prompt cache ordered with successful writes.
// Reads never hold this lock during DB I/O, so old snapshots cannot delay a save.
func (s *SettingService) persistSystemSettingsUpdates(ctx context.Context, updates map[string]string) error {
	value, changed := updates[SettingKeyExcelBPSClientInstructions]
	if !changed {
		return s.settingRepo.SetMultiple(ctx, updates)
	}
	s.excelBPSClientInstructionsMu.Lock()
	defer s.excelBPSClientInstructionsMu.Unlock()
	if err := s.settingRepo.SetMultiple(ctx, updates); err != nil {
		return err
	}
	s.excelBPSClientInstructionsGen++
	s.storeExcelBPSClientInstructionsCache(effectiveExcelBPSClientInstructions(value), excelBPSClientInstructionsCacheTTL)
	return nil
}

func (s *SettingService) storeExcelBPSClientInstructionsCache(value string, ttl time.Duration) {
	s.excelBPSClientInstructionsCache.Store(&cachedExcelBPSClientInstructions{
		value:     value,
		expiresAt: time.Now().Add(ttl).UnixNano(),
	})
}

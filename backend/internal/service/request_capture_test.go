//go:build unit

package service

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/requestcapture"
	"github.com/stretchr/testify/require"
)

func TestRequestCaptureDistillationSettingsRefreshManager(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	ctx := t.Context()
	repo := newMockSettingRepo()
	settingsService := NewSettingService(repo, &config.Config{})
	resetGatewayForwardingSettingsCacheForTest(t)

	database, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	mock.ExpectQuery(`SELECT data FROM request_capture_tasks WHERE instance_id=`).
		WithArgs(sqlmock.AnyArg(), 100, 0).
		WillReturnRows(sqlmock.NewRows([]string{"data"}))
	manager, err := ProvideRequestCaptureManager(database, settingsService)
	require.NoError(t, err)
	t.Cleanup(manager.Close)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, requestcapture.Config{
		QuotaMiB: 1024, RetentionDays: 7, DistillationMinOutputTokens: 3000, DistillationNonStreamOnly: true,
	}, manager.Config())

	require.NoError(t, settingsService.UpdateSettings(ctx, &SystemSettings{
		RequestCaptureEnabled:                        true,
		RequestCaptureIncludeDistillation:            true,
		RequestCaptureDistillationMinOutputTokens:    4500,
		RequestCaptureDistillationMaxCacheReadTokens: 25,
	}))
	want := requestcapture.Config{
		Enabled: true, QuotaMiB: 1024, RetentionDays: 7, IncludeDistillation: true,
		DistillationMinOutputTokens: 4500, DistillationMaxCacheReadTokens: 25,
	}
	require.Equal(t, want, manager.Config())

	require.NoError(t, settingsService.UpdateSettingsWithAuthSourceDefaultsOmitting(ctx, &SystemSettings{
		RequestCaptureDistillationMinOutputTokens: 0,
		RequestCaptureDistillationNonStreamOnly:   true,
	}, &AuthSourceDefaultSettings{}, OmittedSettingKeys{
		SettingKeyRequestCaptureEnabled:                        {},
		SettingKeyRequestCaptureQuotaMiB:                       {},
		SettingKeyRequestCaptureRetentionDays:                  {},
		SettingKeyRequestCaptureIncludeDistillation:            {},
		SettingKeyRequestCaptureDistillationMaxCacheReadTokens: {},
	}))
	want.DistillationMinOutputTokens = 0
	want.DistillationNonStreamOnly = true
	require.Equal(t, want, manager.Config())
}

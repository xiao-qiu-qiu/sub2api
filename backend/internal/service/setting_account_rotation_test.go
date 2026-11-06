package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type accountRotationSettingRepo struct {
	SettingRepository
	values   map[string]string
	reads    int
	err      error
	writeErr error
	readFn   func() (string, error)
}

func (r *accountRotationSettingRepo) GetValue(_ context.Context, _ string) (string, error) {
	r.reads++
	if r.readFn != nil {
		return r.readFn()
	}
	if r.err != nil {
		return "", r.err
	}
	value, ok := r.values[SettingKeyAccountRotationSettings]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (r *accountRotationSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return r.values, nil
}

func (r *accountRotationSettingRepo) GetMultiple(_ context.Context, _ []string) (map[string]string, error) {
	return r.values, nil
}

func (r *accountRotationSettingRepo) SetMultiple(_ context.Context, values map[string]string) error {
	if r.writeErr != nil {
		return r.writeErr
	}
	if r.values == nil {
		r.values = make(map[string]string)
	}
	for key, value := range values {
		r.values[key] = value
	}
	return nil
}

func TestAccountRotationSettingsDefaultsAndInvalidStorage(t *testing.T) {
	expected := &AccountRotationSettings{
		Enabled: false, Mode: "aggressive", WorkMinutes: 300,
		ScheduledHoldEnabled: true, EarlyRestWindowMinutes: 20,
		EarlyRestConcurrencyThreshold: 2, EarlyRestSeconds: 120,
		RestMinMinutes: 10, RestMaxMinutes: 30, StaggerPercent: 10,
		RecoveryMinIntervalMS: 500, RecoveryMaxIntervalMS: 2000,
	}
	for _, raw := range []string{"", "null", "{", `{"enabled":true,"mode":"invalid"}`, `{"enabled":true,"work_minutes":0}`, `{"enabled":true,"rest_min_minutes":31}`} {
		t.Run(raw, func(t *testing.T) {
			repo := &accountRotationSettingRepo{values: map[string]string{SettingKeyAccountRotationSettings: raw}}
			svc := NewSettingService(repo, &config.Config{})
			actual, err := svc.GetAccountRotationSettings(t.Context())
			require.NoError(t, err)
			require.Equal(t, expected, actual)
			all, err := svc.GetAllSettings(t.Context())
			require.NoError(t, err)
			require.Equal(t, actual, all.AccountRotationSettings)
		})
	}
	var svc *SettingService
	actual, err := svc.GetAccountRotationSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, expected, actual)
}

func TestAccountRotationSettingsCacheExpiryAndCopy(t *testing.T) {
	repo := &accountRotationSettingRepo{}
	svc := NewSettingService(repo, nil)
	actual, err := svc.GetAccountRotationSettings(t.Context())
	require.NoError(t, err)
	actual.Enabled = true
	actual.WorkMinutes = 1
	again, err := svc.GetAccountRotationSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, DefaultAccountRotationSettings(), again)
	require.Equal(t, 1, repo.reads)
	cached, ok := svc.accountRotationSettingsCache.Load().(*cachedAccountRotationSettings)
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(30*time.Second), cached.expiresAt, time.Second)
	svc.accountRotationSettingsCache.Store(&cachedAccountRotationSettings{settings: cached.settings, expiresAt: time.Now().Add(-time.Second)})
	_, err = svc.GetAccountRotationSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, 2, repo.reads)
}

func TestAccountRotationSettingsWriteInvalidatesCacheAndOmissionPreserves(t *testing.T) {
	repo := &accountRotationSettingRepo{}
	svc := NewSettingService(repo, &config.Config{})
	_, err := svc.GetAccountRotationSettings(t.Context())
	require.NoError(t, err)
	settings := DefaultAccountRotationSettings()
	settings.Enabled, settings.Mode, settings.WorkMinutes = true, AccountRotationModeSticky, 240
	require.NoError(t, svc.UpdateSettings(t.Context(), &SystemSettings{AccountRotationSettings: settings}))
	actual, err := svc.GetAccountRotationSettings(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, actual.Revision)
	settings.Revision = actual.Revision
	require.Equal(t, settings, actual)
	require.Equal(t, 3, repo.reads)
	stored := repo.values[SettingKeyAccountRotationSettings]
	var persisted AccountRotationSettings
	require.NoError(t, json.Unmarshal([]byte(stored), &persisted))
	require.Equal(t, *settings, persisted)
	require.NoError(t, svc.UpdateSettings(t.Context(), &SystemSettings{}))
	require.Equal(t, stored, repo.values[SettingKeyAccountRotationSettings])
	settings.WorkMinutes = 0
	require.Error(t, svc.UpdateSettings(t.Context(), &SystemSettings{AccountRotationSettings: settings}))
	require.Equal(t, stored, repo.values[SettingKeyAccountRotationSettings])
	settings.WorkMinutes = 120
	repo.writeErr = errors.New("write unavailable")
	require.ErrorContains(t, svc.UpdateSettings(t.Context(), &SystemSettings{AccountRotationSettings: settings}), "write unavailable")
	require.Equal(t, stored, repo.values[SettingKeyAccountRotationSettings])
	actual, err = svc.GetAccountRotationSettings(t.Context())
	require.NoError(t, err, "failed writes must preserve the last cached settings")
	require.Equal(t, 240, actual.WorkMinutes)
}

func TestAccountRotationSettingsStorageErrorsAreNotCached(t *testing.T) {
	repo := &accountRotationSettingRepo{err: errors.New("storage unavailable")}
	svc := NewSettingService(repo, nil)
	settings, err := svc.GetAccountRotationSettings(t.Context())
	require.Nil(t, settings)
	require.ErrorContains(t, err, "storage unavailable")
	repo.err = nil
	settings, err = svc.GetAccountRotationSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, DefaultAccountRotationSettings(), settings)
	require.Equal(t, 2, repo.reads)
}

func TestAccountRotationSettingsRevisionTracksContentOnly(t *testing.T) {
	repo := &accountRotationSettingRepo{}
	svc := NewSettingService(repo, &config.Config{})
	initial, err := svc.GetAccountRotationSettings(t.Context())
	require.NoError(t, err)
	require.Empty(t, initial.Revision)
	require.Empty(t, repo.values, "reading defaults must not persist a revision")
	settings := DefaultAccountRotationSettings()
	settings.Enabled, settings.Revision = true, "client-controlled"
	save := func() *AccountRotationSettings {
		t.Helper()
		require.NoError(t, svc.UpdateSettings(t.Context(), &SystemSettings{AccountRotationSettings: settings}))
		actual, err := svc.GetAccountRotationSettings(t.Context())
		require.NoError(t, err)
		return actual
	}
	previous := save()
	require.NotEmpty(t, previous.Revision)
	require.NotEqual(t, "client-controlled", previous.Revision)
	settings.Revision = "another-client-value"
	require.Equal(t, previous.Revision, save().Revision, "identical content must preserve the stored revision")
	// A second instance has an old runtime cache but must compare fresh storage.
	other := NewSettingService(repo, &config.Config{})
	other.accountRotationSettingsCache.Store(&cachedAccountRotationSettings{settings: *initial, expiresAt: time.Now().Add(time.Minute)})
	require.NoError(t, other.UpdateSettings(t.Context(), &SystemSettings{AccountRotationSettings: settings}))
	actual, err := other.GetAccountRotationSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, previous.Revision, actual.Revision)
	for _, change := range []func(){
		func() { settings.Enabled = false },
		func() { settings.Enabled = true },
		func() { settings.Mode = AccountRotationModeSticky },
		func() { settings.WorkMinutes++ },
		func() { settings.RestMinMinutes++ },
		func() { settings.RestMaxMinutes++ },
		func() { settings.StaggerPercent++ },
		func() { settings.EarlyRestEnabled = true },
		func() { settings.EarlyRestEnabled = false },
		func() { settings.ScheduledHoldEnabled = false },
		func() { settings.ScheduledHoldEnabled = true },
		func() { settings.EarlyRestWindowMinutes++ },
		func() { settings.EarlyRestConcurrencyThreshold++ },
		func() { settings.EarlyRestSeconds++ },
		func() { settings.RecoveryMinIntervalMS++ },
		func() { settings.RecoveryMaxIntervalMS++ },
	} {
		change()
		next := save()
		require.NotEqual(t, previous.Revision, next.Revision)
		previous = next
	}
}

func TestAccountRotationSettingsRecoveryValidation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		min, max     int
		invalidField string
	}{
		{"defaults", 500, 2000, ""},
		{"minimum", 1, 1, ""},
		{"full range", 1, 60000, ""},
		{"maximum", 60000, 60000, ""},
		{"zero pair", 0, 0, "recovery_min_interval_ms"},
		{"zero min", 0, 2000, "recovery_min_interval_ms"},
		{"negative min", -1, 2000, "recovery_min_interval_ms"},
		{"excessive min", 60001, 60001, "recovery_min_interval_ms"},
		{"zero max", 1, 0, "recovery_max_interval_ms"},
		{"negative max", 1, -1, "recovery_max_interval_ms"},
		{"inverted", 2000, 500, "recovery_max_interval_ms"},
		{"excessive max", 500, 60001, "recovery_max_interval_ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := DefaultAccountRotationSettings()
			settings.RecoveryMinIntervalMS, settings.RecoveryMaxIntervalMS = tc.min, tc.max
			for _, enabled := range []bool{false, true} {
				settings.Enabled = enabled
				if tc.invalidField == "" {
					require.NoError(t, settings.Validate())
				} else {
					require.ErrorContains(t, settings.Validate(), tc.invalidField)
				}
			}
		})
	}
}

func TestAccountRotationSettingsLegacyRecoveryDefaultsPreservePolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fields   map[string]any
		min, max int
	}{
		{"both missing", nil, 500, 2000},
		{"max missing", map[string]any{"recovery_min_interval_ms": 125}, 125, 2000},
		{"min missing", map[string]any{"recovery_max_interval_ms": 60000}, 500, 60000},
		{"custom intervals", map[string]any{"recovery_min_interval_ms": 123, "recovery_max_interval_ms": 456}, 123, 456},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacy := map[string]any{
				"enabled": true, "early_rest_enabled": true, "mode": "sticky", "work_minutes": 75,
				"rest_min_minutes": 2, "rest_max_minutes": 9, "stagger_percent": 23, "revision": "legacy-policy",
			}
			for field, value := range tc.fields {
				legacy[field] = value
			}
			raw, err := json.Marshal(legacy)
			require.NoError(t, err)
			repo := &accountRotationSettingRepo{values: map[string]string{SettingKeyAccountRotationSettings: string(raw)}}
			svc := NewSettingService(repo, &config.Config{})
			settings, err := svc.GetAccountRotationSettings(t.Context())
			require.NoError(t, err)
			require.Equal(t, &AccountRotationSettings{
				Enabled: true, EarlyRestEnabled: true, Mode: "sticky", WorkMinutes: 75,
				ScheduledHoldEnabled: true, EarlyRestWindowMinutes: 20,
				EarlyRestConcurrencyThreshold: 2, EarlyRestSeconds: 120,
				RestMinMinutes: 2, RestMaxMinutes: 9, StaggerPercent: 23, Revision: "legacy-policy",
				RecoveryMinIntervalMS: tc.min, RecoveryMaxIntervalMS: tc.max,
			}, settings)
			require.Equal(t, string(raw), repo.values[SettingKeyAccountRotationSettings], "reads must not rewrite legacy storage")
			require.NoError(t, svc.UpdateSettings(t.Context(), &SystemSettings{AccountRotationSettings: settings}))
			reloaded, err := NewSettingService(repo, &config.Config{}).GetAccountRotationSettings(t.Context())
			require.NoError(t, err)
			require.Equal(t, settings, reloaded, "saving defaulted fields must preserve the policy and revision")
		})
	}
}

func TestAccountRotationSettingsLegacyEarlyRestDefaultsAndPersists(t *testing.T) {
	repo := &accountRotationSettingRepo{values: map[string]string{
		SettingKeyAccountRotationSettings: `{"enabled":true,"mode":"aggressive","work_minutes":300,"rest_min_minutes":10,"rest_max_minutes":30,"stagger_percent":10,"revision":"legacy-policy"}`,
	}}
	svc := NewSettingService(repo, &config.Config{})
	settings, err := svc.GetAccountRotationSettings(t.Context())
	require.NoError(t, err)
	require.False(t, settings.EarlyRestEnabled)
	require.Equal(t, "legacy-policy", settings.Revision)
	settings.EarlyRestEnabled = true
	require.NoError(t, svc.UpdateSettings(t.Context(), &SystemSettings{AccountRotationSettings: settings}))
	persisted, err := NewSettingService(repo, &config.Config{}).GetAccountRotationSettings(t.Context())
	require.NoError(t, err)
	require.True(t, persisted.EarlyRestEnabled)
	require.NotEqual(t, settings.Revision, persisted.Revision)
	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(repo.values[SettingKeyAccountRotationSettings]), &raw))
	require.Equal(t, true, raw["early_rest_enabled"])
}

func TestAccountRotationSettingsEarlyRestValidation(t *testing.T) {
	for _, field := range []struct {
		name    string
		maximum int
		set     func(*AccountRotationSettings, int)
	}{
		{"early_rest_window_minutes", 1440, func(s *AccountRotationSettings, v int) { s.EarlyRestWindowMinutes = v }},
		{"early_rest_concurrency_threshold", 10000, func(s *AccountRotationSettings, v int) { s.EarlyRestConcurrencyThreshold = v }},
		{"early_rest_seconds", 7200, func(s *AccountRotationSettings, v int) { s.EarlyRestSeconds = v }},
	} {
		for _, value := range []int{-1, 0, 1, field.maximum, field.maximum + 1} {
			t.Run(fmt.Sprintf("%s/%d", field.name, value), func(t *testing.T) {
				for _, enabled := range []bool{false, true} {
					settings := DefaultAccountRotationSettings()
					settings.Enabled, settings.EarlyRestEnabled, settings.ScheduledHoldEnabled = enabled, enabled, enabled
					field.set(settings, value)
					if value >= 1 && value <= field.maximum {
						require.NoError(t, settings.Validate())
					} else {
						require.ErrorContains(t, settings.Validate(), field.name)
					}
				}
			})
		}
	}
}

func TestAccountRotationSettingsScheduledHoldDefaultsAndExplicitFalse(t *testing.T) {
	for _, tc := range []struct {
		name string
		hold *bool
	}{
		{"legacy missing", nil},
		{"explicit false", new(false)},
		{"explicit true", new(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacy := map[string]any{"enabled": true, "early_rest_enabled": true, "mode": "sticky", "revision": "legacy-policy"}
			if tc.hold != nil {
				legacy["scheduled_hold_enabled"] = *tc.hold
			}
			raw, err := json.Marshal(legacy)
			require.NoError(t, err)
			repo := &accountRotationSettingRepo{values: map[string]string{SettingKeyAccountRotationSettings: string(raw)}}
			svc := NewSettingService(repo, &config.Config{})
			actual, err := svc.GetAccountRotationSettings(t.Context())
			require.NoError(t, err)
			expected := DefaultAccountRotationSettings()
			expected.Enabled, expected.EarlyRestEnabled, expected.Mode, expected.Revision = true, true, "sticky", "legacy-policy"
			if tc.hold != nil {
				expected.ScheduledHoldEnabled = *tc.hold
			}
			require.Equal(t, expected, actual)
			require.Equal(t, string(raw), repo.values[SettingKeyAccountRotationSettings])
			require.NoError(t, svc.UpdateSettings(t.Context(), &SystemSettings{AccountRotationSettings: actual}))
			reloaded, err := NewSettingService(repo, &config.Config{}).GetAccountRotationSettings(t.Context())
			require.NoError(t, err)
			require.Equal(t, expected, reloaded, "saving defaults must preserve legacy revision and explicit false")
		})
	}
}

func TestAccountRotationSettingsInvalidNewStoredFieldsFallBackToDefaults(t *testing.T) {
	for field, values := range map[string][]any{
		"scheduled_hold_enabled":           {nil, "false", 0, []any{}, map[string]any{}},
		"early_rest_window_minutes":        {nil, 0, -1, 1441, "20", true, json.Number("1.0"), json.Number("1e1"), []any{}, map[string]any{}},
		"early_rest_concurrency_threshold": {nil, 0, -1, 10001, "2", true, json.Number("1.0"), []any{}, map[string]any{}},
		"early_rest_seconds":               {nil, 0, -1, 7201, "120", true, json.Number("1.5"), []any{}, map[string]any{}},
	} {
		for i, value := range values {
			t.Run(fmt.Sprintf("%s/%d", field, i), func(t *testing.T) {
				raw, err := json.Marshal(map[string]any{"enabled": true, "mode": "sticky", "revision": "invalid-policy", field: value})
				require.NoError(t, err)
				require.Equal(t, DefaultAccountRotationSettings(), parseAccountRotationSettings(string(raw)))
			})
		}
	}
}

func TestAccountRotationSettingsLegacyRevisionRemainsEmptyUntilContentChanges(t *testing.T) {
	settings := DefaultAccountRotationSettings()
	settings.Enabled = true
	legacy, err := json.Marshal(settings)
	require.NoError(t, err)
	repo := &accountRotationSettingRepo{values: map[string]string{SettingKeyAccountRotationSettings: string(legacy)}}
	svc := NewSettingService(repo, &config.Config{})
	actual, err := svc.GetAccountRotationSettings(t.Context())
	require.NoError(t, err)
	require.Empty(t, actual.Revision)
	settings.Revision = "forged"
	require.NoError(t, svc.UpdateSettings(t.Context(), &SystemSettings{AccountRotationSettings: settings}))
	actual, err = svc.GetAccountRotationSettings(t.Context())
	require.NoError(t, err)
	require.Empty(t, actual.Revision)
	settings.WorkMinutes++
	require.NoError(t, svc.UpdateSettings(t.Context(), &SystemSettings{AccountRotationSettings: settings}))
	actual, err = svc.GetAccountRotationSettings(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, actual.Revision)
}

func TestAccountRotationSettingsInvalidationSupersedesInflightRead(t *testing.T) {
	started, resume := make(chan struct{}), make(chan struct{})
	repo := &accountRotationSettingRepo{}
	repo.readFn = func() (string, error) {
		if repo.reads == 1 {
			close(started)
			<-resume
			return `{"enabled":false}`, nil
		}
		return `{"enabled":true,"mode":"sticky"}`, nil
	}
	svc := NewSettingService(repo, nil)
	type result struct {
		settings *AccountRotationSettings
		err      error
	}
	done := make(chan result, 1)
	go func() {
		settings, err := svc.GetAccountRotationSettings(t.Context())
		done <- result{settings, err}
	}()
	<-started
	svc.InvalidateAccountRotationSettingsCache()
	close(resume)
	actual := <-done
	require.NoError(t, actual.err)
	require.True(t, actual.settings.Enabled)
	require.Equal(t, AccountRotationModeSticky, actual.settings.Mode)
	require.Equal(t, 2, repo.reads)
}

package service

import (
	"context"
	"encoding/json"
	"errors"
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
		RestMinMinutes: 10, RestMaxMinutes: 30, StaggerPercent: 10,
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
	cached := svc.accountRotationSettingsCache.Load().(*cachedAccountRotationSettings)
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
	} {
		change()
		next := save()
		require.NotEqual(t, previous.Revision, next.Revision)
		previous = next
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

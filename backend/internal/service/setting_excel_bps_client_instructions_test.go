package service

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/stretchr/testify/require"
)

type excelBPSClientInstructionsRepo struct {
	*excelBPSImageSettingsRepo
	reads    atomic.Int32
	writes   atomic.Int32
	read     func(context.Context) (string, error)
	writeErr error
}

func (r *excelBPSClientInstructionsRepo) GetValue(ctx context.Context, key string) (string, error) {
	if key == SettingKeyExcelBPSClientInstructions {
		r.reads.Add(1)
		if r.read != nil {
			return r.read(ctx)
		}
	}
	return r.excelBPSImageSettingsRepo.GetValue(ctx, key)
}

func (r *excelBPSClientInstructionsRepo) SetMultiple(ctx context.Context, values map[string]string) error {
	r.writes.Add(1)
	if r.writeErr != nil {
		return r.writeErr
	}
	return r.excelBPSImageSettingsRepo.SetMultiple(ctx, values)
}

func newExcelBPSClientInstructionsService(values map[string]string) (*SettingService, *excelBPSClientInstructionsRepo) {
	repo := &excelBPSClientInstructionsRepo{excelBPSImageSettingsRepo: &excelBPSImageSettingsRepo{values: values}}
	return NewSettingService(repo, &config.Config{}), repo
}

func TestExcelBPSClientInstructionsDefaults(t *testing.T) {
	var nilService *SettingService
	require.Equal(t, basispoints.DefaultClientInstructions, nilService.GetExcelBPSClientInstructions(context.Background()))
	require.Equal(t, basispoints.DefaultClientInstructions, (&SettingService{}).GetExcelBPSClientInstructions(context.Background()))
	for _, stored := range []string{"", " \n\t　", basispoints.DefaultClientInstructions, strings.Repeat("界", ExcelBPSClientInstructionsMaxLength+1)} {
		svc, _ := newExcelBPSClientInstructionsService(map[string]string{SettingKeyExcelBPSClientInstructions: stored})
		require.Equal(t, basispoints.DefaultClientInstructions, svc.GetExcelBPSClientInstructions(context.Background()))
		settings, err := svc.GetAllSettings(context.Background())
		require.NoError(t, err)
		require.Equal(t, basispoints.DefaultClientInstructions, settings.ExcelBPSClientInstructions)
		require.Equal(t, basispoints.DefaultClientInstructions, settings.ExcelBPSDefaultClientInstructions)
	}
	for _, readErr := range []error{ErrSettingNotFound, errors.New("database unavailable")} {
		svc, repo := newExcelBPSClientInstructionsService(nil)
		repo.read = func(context.Context) (string, error) { return "", readErr }
		require.Equal(t, basispoints.DefaultClientInstructions, svc.GetExcelBPSClientInstructions(context.Background()))
	}
}

func TestExcelBPSClientInstructionsSaveResetAndOmission(t *testing.T) {
	ctx := context.Background()
	svc, repo := newExcelBPSClientInstructionsService(nil)
	require.Equal(t, basispoints.DefaultClientInstructions, svc.GetExcelBPSClientInstructions(ctx))
	custom := "第一段\r\n\r\n  second paragraph\n\tkeep indentation"
	require.NoError(t, svc.UpdateSettings(ctx, &SystemSettings{ExcelBPSClientInstructions: " \t" + custom + "\n　"}))
	require.Equal(t, custom, repo.values[SettingKeyExcelBPSClientInstructions])
	require.Equal(t, custom, svc.GetExcelBPSClientInstructions(ctx))
	settings, err := svc.GetAllSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, custom, settings.ExcelBPSClientInstructions)
	require.NoError(t, svc.UpdateSettingsOmitting(ctx, &SystemSettings{ExcelBPSClientInstructions: "ignored"}, OmittedSettingKeys{SettingKeyExcelBPSClientInstructions: {}}))
	require.Equal(t, custom, repo.values[SettingKeyExcelBPSClientInstructions])
	require.Equal(t, custom, svc.GetExcelBPSClientInstructions(ctx))
	for _, reset := range []string{"", " \n\t　", "\n" + basispoints.DefaultClientInstructions + "\t"} {
		require.NoError(t, svc.UpdateSettings(ctx, &SystemSettings{ExcelBPSClientInstructions: custom}))
		require.NoError(t, svc.UpdateSettings(ctx, &SystemSettings{ExcelBPSClientInstructions: reset}))
		require.Empty(t, repo.values[SettingKeyExcelBPSClientInstructions])
		require.Equal(t, basispoints.DefaultClientInstructions, svc.GetExcelBPSClientInstructions(ctx))
	}
	require.EqualValues(t, 1, repo.reads.Load(), "fresh runtime reads and successful saves must not reread the prompt")
}

func TestExcelBPSClientInstructionsUnicodeValidationIsAtomic(t *testing.T) {
	ctx := context.Background()
	svc, repo := newExcelBPSClientInstructionsService(nil)
	maximum := strings.Repeat("界🙂", ExcelBPSClientInstructionsMaxLength/2)
	require.NoError(t, svc.UpdateSettings(ctx, &SystemSettings{ExcelBPSClientInstructions: " \n" + maximum + "\t"}))
	writes := repo.writes.Load()
	for _, oversized := range []string{maximum + "字", strings.Repeat("a", ExcelBPSClientInstructionsMaxLength+1)} {
		err := svc.UpdateSettings(ctx, &SystemSettings{SiteName: "must not save", ExcelBPSClientInstructions: oversized})
		require.ErrorContains(t, err, "16000")
		require.NotContains(t, err.Error(), oversized)
		require.Equal(t, writes, repo.writes.Load())
		require.Equal(t, maximum, repo.values[SettingKeyExcelBPSClientInstructions])
		require.NotEqual(t, "must not save", repo.values[SettingKeySiteName])
		require.Equal(t, maximum, svc.GetExcelBPSClientInstructions(ctx))
	}
}

func TestExcelBPSClientInstructionsRuntimeCacheRefreshAndFailure(t *testing.T) {
	svc, repo := newExcelBPSClientInstructionsService(map[string]string{SettingKeyExcelBPSClientInstructions: "old prompt"})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	repo.read = func(ctx context.Context) (string, error) {
		require.NoError(t, ctx.Err())
		_, bounded := ctx.Deadline()
		require.True(t, bounded)
		return "old prompt", nil
	}
	for i := 0; i < 20; i++ {
		require.Equal(t, "old prompt", svc.GetExcelBPSClientInstructions(canceled))
	}
	require.EqualValues(t, 1, repo.reads.Load())
	svc.storeExcelBPSClientInstructionsCache("old prompt", -time.Second)
	repo.read = func(context.Context) (string, error) { return "remote update", nil }
	require.Equal(t, "remote update", svc.GetExcelBPSClientInstructions(context.Background()))
	svc.storeExcelBPSClientInstructionsCache("remote update", -time.Second)
	repo.read = func(context.Context) (string, error) { return "", errors.New("read failed") }
	require.Equal(t, "remote update", svc.GetExcelBPSClientInstructions(context.Background()))
	require.EqualValues(t, 3, repo.reads.Load())
	require.Equal(t, "remote update", svc.GetExcelBPSClientInstructions(context.Background()))
	require.EqualValues(t, 3, repo.reads.Load(), "errors should use a short retry cache")
	repo.writeErr = errors.New("write failed")
	require.Error(t, svc.UpdateSettings(context.Background(), &SystemSettings{ExcelBPSClientInstructions: "unsaved"}))
	require.Equal(t, "remote update", svc.GetExcelBPSClientInstructions(context.Background()))
	require.Equal(t, "old prompt", repo.values[SettingKeyExcelBPSClientInstructions])
}

func TestExcelBPSClientInstructionsSaveSupersedesInflightRead(t *testing.T) {
	svc, repo := newExcelBPSClientInstructionsService(map[string]string{SettingKeyExcelBPSClientInstructions: "stale"})
	started, release := make(chan struct{}), make(chan struct{})
	repo.read = func(context.Context) (string, error) {
		close(started)
		<-release
		return "stale", nil
	}
	readDone := make(chan string, 1)
	go func() { readDone <- svc.GetExcelBPSClientInstructions(context.Background()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime read did not start")
	}
	saveDone := make(chan error, 1)
	go func() {
		saveDone <- svc.UpdateSettings(context.Background(), &SystemSettings{ExcelBPSClientInstructions: "fresh"})
	}()
	select {
	case err := <-saveDone:
		close(release)
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("an in-flight read blocked the save")
	}
	select {
	case value := <-readDone:
		require.Equal(t, "fresh", value)
	case <-time.After(5 * time.Second):
		t.Fatal("runtime read did not complete")
	}
	require.Equal(t, "fresh", svc.GetExcelBPSClientInstructions(context.Background()))
}

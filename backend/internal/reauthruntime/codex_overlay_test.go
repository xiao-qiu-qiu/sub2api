package reauthruntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCodexOverlayBootstrapsBaseWorker(t *testing.T) {
	python := "python3"
	if runtime.GOOS == "windows" {
		python = "python"
	}
	python, err := exec.LookPath(python)
	if err != nil {
		t.Skip("requires Python")
	}
	base := t.TempDir()
	baseWorker := filepath.Join(base, "worker.py")
	workerFixture := `from pathlib import Path
import sys
import openai_codex_oauth_adapter
import openai_excel_oauth_adapter

assert __name__ == "__main__"
assert Path(sys.argv[0]).resolve() == Path(__file__).resolve()
assert sys.argv[1:] == ["--check"]
assert Path(openai_codex_oauth_adapter.__file__).parent.name.startswith("codeq-codex-")
assert Path(openai_excel_oauth_adapter.__file__).parent == Path(__file__).parent
assert Path(openai_codex_oauth_adapter.__file__).with_name("openai_codex_password_flow.mjs").is_file()
print("overlay worker ready")
`
	require.NoError(t, os.WriteFile(baseWorker, []byte(workerFixture), 0600))
	baseAdapter := filepath.Join(base, "openai_codex_oauth_adapter.py")
	baseAdapterContents := "raise AssertionError('base Codex adapter should be shadowed')\n"
	require.NoError(t, os.WriteFile(baseAdapter, []byte(baseAdapterContents), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(base, "openai_excel_oauth_adapter.py"), []byte("def replace_exact(*args): pass\n"), 0600))

	worker, err := codexOverlayWorker(base, codexOverlayVersion)
	require.NoError(t, err)
	require.Equal(t, base, filepath.Dir(filepath.Dir(worker)))
	cmd := exec.CommandContext(t.Context(), python, "-B", worker, "--check")
	cmd.Dir = base
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.Contains(t, string(output), "overlay worker ready")
	workerContents, err := os.ReadFile(baseWorker)
	require.NoError(t, err)
	require.Equal(t, workerFixture, string(workerContents))
	adapterContents, err := os.ReadFile(baseAdapter)
	require.NoError(t, err)
	require.Equal(t, baseAdapterContents, string(adapterContents))
}

func TestCodexOverlayUsesPinnedVersionAndVerifiesCachedContents(t *testing.T) {
	base := t.TempDir()
	worker, err := codexOverlayWorker(base, "2.19.2")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(base, "worker.py"), worker)
	entries, err := os.ReadDir(base)
	require.NoError(t, err)
	require.Empty(t, entries)

	worker, err = codexOverlayWorker(base, codexOverlayVersion)
	require.NoError(t, err)
	reused, err := codexOverlayWorker(base, codexOverlayVersion)
	require.NoError(t, err)
	require.Equal(t, worker, reused)

	adapter := filepath.Join(filepath.Dir(worker), "openai_codex_oauth_adapter.py")
	contents, err := os.ReadFile(adapter)
	require.NoError(t, err)
	require.NotEmpty(t, contents)
	contents[0] ^= 1
	require.NoError(t, os.WriteFile(adapter, contents, 0600))
	_, err = codexOverlayWorker(base, codexOverlayVersion)
	require.ErrorContains(t, err, "invalid Codex overlay contents")
}

func TestManagedRuntimeStartsWorkerFromCodexOverlay(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux worker process")
	}
	root := t.TempDir()
	base := filepath.Join(root, codexOverlayVersion+"-linux-"+runtime.GOARCH)
	require.NoError(t, os.Mkdir(base, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(base, "ready"), []byte("cached"), 0600))
	python := `#!/bin/sh
printf '%s\n' "$1" "$TOSUB2_ROOT" "$NODE_EXECUTABLE" > worker-launch
exec /bin/sleep 60
`
	require.NoError(t, os.WriteFile(filepath.Join(base, "python"), []byte(python), 0700))
	manager := New(root, codexOverlayVersion, "http://127.0.0.1:4040", "synthetic-worker-token")
	manager.Ensure()
	t.Cleanup(manager.Stop)
	worker, err := codexOverlayWorker(base, codexOverlayVersion)
	require.NoError(t, err)
	expected := worker + "\n" + filepath.Join(base, "tosub2") + "\n" + filepath.Join(base, "node") + "\n"
	require.Eventually(t, func() bool {
		actual, err := os.ReadFile(filepath.Join(base, "worker-launch"))
		return err == nil && string(actual) == expected && manager.Status().State == "running"
	}, 5*time.Second, 10*time.Millisecond)
}

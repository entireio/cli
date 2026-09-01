//go:build e2e

package tests

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/entireio/cli/e2e/agents"
	"github.com/entireio/cli/e2e/entire"
	"github.com/entireio/cli/e2e/testutil"
)

func TestMain(m *testing.M) {
	runDir := os.Getenv("E2E_ARTIFACT_DIR")
	if runDir == "" {
		_, file, _, _ := runtime.Caller(0)
		testutil.ArtifactRoot = filepath.Join(filepath.Dir(file), "..", "artifacts")
		runDir = testutil.ArtifactRunDir()
	}
	_ = os.MkdirAll(runDir, 0o755)
	testutil.SetRunDir(runDir)

	// Route every spawned entire binary (and the git hooks that invoke it) at
	// the file-backed token store so e2e never touches the developer's real
	// OS keychain. internal/entireclient/tokenstore honors these env vars
	// unconditionally, and they are inherited by child processes.
	// In-process keyring.MockInit() cannot help here: the binary is a subprocess.
	os.Setenv("ENTIRE_TOKEN_STORE", "file")
	os.Setenv("ENTIRE_TOKEN_STORE_PATH", filepath.Join(runDir, "e2e-tokenstore.json"))

	// Same for the CLI's config and cache directories: contexts.json,
	// version_check.json, and the discovery caches must never resolve to the
	// developer's real ~/.config/entire or ~/.cache/entire from a spawned
	// binary (testing.Testing() is false there, so the internal/testdirs
	// fallback cannot protect it).
	os.Setenv("ENTIRE_CONFIG_DIR", filepath.Join(runDir, "entire-config"))
	os.Setenv("XDG_CACHE_HOME", filepath.Join(runDir, "entire-cache"))

	// Select the checkpoint storage backend for the whole suite. E2E_CHECKPOINT_STORE
	// (e.g. "git-refs") maps to the ENTIRE_CHECKPOINTS_PRIMARY override the spawned
	// binary honors, so every condensation/read/push in the run exercises that
	// backend. Unset pins git-branch: the harness's backend-aware assertions
	// (testutil.checkpointStoreMode) default to the v1 branch while first-run
	// enable now defaults new setups to git-refs — without the pin the binary
	// writes refs and every branch-shaped assertion times out. The env also
	// suppresses that first-run settings write.
	store := os.Getenv("E2E_CHECKPOINT_STORE")
	if store == "" {
		store = "git-branch"
	}
	os.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", store)

	// Resolve the entire binary (set by mise run build via E2E_ENTIRE_BIN).
	entireBin := entire.BinPath()
	if err := ensureHookEntireBinary(entireBin); err != nil {
		fmt.Fprintf(os.Stderr, "preflight: prepare hook entire binary: %v\n", err)
		os.Exit(1)
	}

	// Prepend the binary's directory to PATH so that git hooks and agent
	// hooks (which call bare "entire") resolve to the same binary the test
	// harness uses, not a system-installed one.
	os.Setenv("PATH", filepath.Dir(entireBin)+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Preflight: verify required dependencies before running any tests.
	// tmux is only required on Unix (interactive session tests are skipped on Windows).
	var missing []string
	requiredBins := []string{"git"}
	if runtime.GOOS != "windows" {
		requiredBins = append(requiredBins, "tmux")
	}
	for _, bin := range requiredBins {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	for _, a := range agents.All() {
		if _, err := exec.LookPath(a.Binary()); err != nil {
			missing = append(missing, fmt.Sprintf("%s (%s)", a.Binary(), a.Name()))
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "preflight: missing required binaries: %v\n", missing)
		os.Exit(1)
	}

	// Preflight: at least one agent must be registered. Registration happens in
	// each agent package's init, and is conditional on E2E_AGENT naming that
	// agent AND its binary being on PATH at process start. When nothing
	// registers, the missing-binaries loop above has nothing to iterate,
	// ForEachAgent calls t.Skip in every test, and the run exits 0 — the suite
	// reports success having exercised nothing. Measured on this suite: with
	// E2E_AGENT=vogon and vogon absent from PATH, 54 of 56 tests skip and
	// `go test -tags=e2e ./e2e/tests` passes in 1.5s.
	//
	// The e2e canary is the deterministic gate that guards checkpoint capture
	// on every PR, so a green run that ran nothing is worse than a red one.
	// Fail the same way a missing binary does.
	if len(agents.All()) == 0 {
		fmt.Fprintf(os.Stderr,
			"preflight: no agents registered (E2E_AGENT=%q); every test would skip and the run would report success\n",
			os.Getenv("E2E_AGENT"))
		os.Exit(1)
	}

	version := "unknown"
	if out, err := exec.Command(entireBin, "version").Output(); err == nil {
		version = string(out)
	}

	// Write preflight info to artifact dir only — gotestsum swallows both
	// stdout and stderr, so the test-e2e script cats this file at the end.
	preflight := fmt.Sprintf("entire binary:  %s\nentire version: %s\n",
		entireBin, version)
	_ = os.WriteFile(filepath.Join(runDir, "entire-version.txt"), []byte(preflight), 0o644)

	// Don't look at user's Git config, ignore everything except the project-local
	// Git settings. This avoids oddball configs in ~/.gitconfig messing with our
	// E2E tests; the isolation is inherited by every spawned binary and git hook.
	gitenv.IsolateMain()

	os.Exit(m.Run())
}

func ensureHookEntireBinary(entireBin string) error {
	dir := filepath.Dir(entireBin)
	hookName := "entire"
	if runtime.GOOS == "windows" {
		hookName = "entire.exe"
	}
	hookBin := filepath.Join(dir, hookName)
	if filepath.Clean(entireBin) == filepath.Clean(hookBin) {
		return nil
	}

	_ = os.Remove(hookBin)

	if runtime.GOOS == "windows" {
		data, err := os.ReadFile(entireBin)
		if err != nil {
			return err
		}
		return os.WriteFile(hookBin, data, 0o755)
	}

	return os.Symlink(entireBin, hookBin)
}

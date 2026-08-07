//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// The shell hook runs `entire shellhook check` from inside the user's prompt,
// so the properties worth verifying against the real binary are the ones the
// in-process tests cannot see: the process exits 0, the warning lands on
// stderr, and stdout stays completely clean.

// shellhookEnv returns an environment with the per-user config and cache dirs
// pointed at throwaway directories. A spawned binary is NOT covered by the
// in-process testdirs safety net, so this plumbing is mandatory — without it
// the child would read and write the developer's real ~/.config/entire.
func shellhookEnv(t *testing.T, mode string) []string {
	t.Helper()

	configDir := t.TempDir()
	cacheDir := t.TempDir()
	if mode != "" {
		prefs := map[string]any{"version": 1, "mode": mode}
		data, err := json.Marshal(prefs)
		if err != nil {
			t.Fatalf("marshal preferences: %v", err)
		}
		if err := os.WriteFile(filepath.Join(configDir, "shellhook.json"), data, 0o600); err != nil {
			t.Fatalf("write preferences: %v", err)
		}
	}

	env := append(os.Environ(),
		"ENTIRE_CONFIG_DIR="+configDir,
		"XDG_CACHE_HOME="+cacheDir,
	)
	return env
}

// runShellhookCheck spawns the real binary and returns its streams separately.
func runShellhookCheck(t *testing.T, repoDir string, env []string) (stdout, stderr string) {
	t.Helper()

	cmd := execx.NonInteractive(t.Context(), getTestBinary(), "shellhook", "check", "--root", repoDir)
	cmd.Dir = repoDir
	cmd.Env = env

	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("shellhook check exited non-zero (%v)\nstdout: %s\nstderr: %s", err, out.String(), errOut.String())
	}
	return out.String(), errOut.String()
}

func shellhookRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "hello")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	return dir
}

func TestShellhookCheck_WarnsOnStderrAndExitsZero(t *testing.T) {
	t.Parallel()

	repoDir := shellhookRepo(t)
	env := shellhookEnv(t, "warn")

	stdout, stderr := runShellhookCheck(t, repoDir, env)
	if stdout != "" {
		t.Errorf("stdout = %q, want empty — the hook must never write to stdout", stdout)
	}
	if !strings.Contains(stderr, "checkpointing is not enabled") {
		t.Errorf("stderr = %q, want the warning", stderr)
	}

	// Throttled on the second visit, still exit 0.
	stdout, stderr = runShellhookCheck(t, repoDir, env)
	if stdout != "" || stderr != "" {
		t.Errorf("second check produced output: stdout=%q stderr=%q, want silence", stdout, stderr)
	}
}

func TestShellhookCheck_OffModeIsSilentAndExitsZero(t *testing.T) {
	t.Parallel()

	repoDir := shellhookRepo(t)

	// No preferences file at all: the un-installed default.
	stdout, stderr := runShellhookCheck(t, repoDir, shellhookEnv(t, ""))
	if stdout != "" || stderr != "" {
		t.Errorf("stdout=%q stderr=%q, want silence with no preferences", stdout, stderr)
	}

	stdout, stderr = runShellhookCheck(t, repoDir, shellhookEnv(t, "off"))
	if stdout != "" || stderr != "" {
		t.Errorf("stdout=%q stderr=%q, want silence for mode=off", stdout, stderr)
	}
}

func TestShellhookCheck_AutoModeWithoutTTYWarnsInsteadOfEnabling(t *testing.T) {
	t.Parallel()

	repoDir := shellhookRepo(t)

	// execx.NonInteractive detaches the child from any controlling terminal,
	// so auto mode must degrade to a warning rather than run `entire enable`.
	_, stderr := runShellhookCheck(t, repoDir, shellhookEnv(t, "auto"))
	if !strings.Contains(stderr, "checkpointing is not enabled") {
		t.Errorf("stderr = %q, want the warning fallback", stderr)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".entire", "settings.json")); err == nil {
		t.Error("auto mode enabled the repo without a terminal")
	}
}

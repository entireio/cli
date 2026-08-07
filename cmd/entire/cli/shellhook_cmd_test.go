package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/shellhook"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// isolateShellhook points the per-user config and cache dirs at throwaway
// directories. t.Setenv forbids t.Parallel, so every test using it is serial.
func isolateShellhook(t *testing.T) {
	t.Helper()
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
}

// shellhookTestRepo returns a fresh temp git repo. It never touches the real
// repo: nothing here chdirs, and `check` is driven with an explicit --root.
func shellhookTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "hi")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	// EvalSymlinks so comparisons match what git reports (macOS /var
	// -> /private/var).
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return dir
	}
	return resolved
}

func setShellhookMode(t *testing.T, mode shellhook.Mode) {
	t.Helper()
	if err := shellhook.SavePreferences(&shellhook.Preferences{
		Version: shellhook.PreferencesVersion,
		Mode:    mode,
	}); err != nil {
		t.Fatalf("SavePreferences() error = %v", err)
	}
}

// runCheck drives the hot path and returns what it wrote to each stream.
func runCheck(t *testing.T, root string) (stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	runShellhookCheck(context.Background(), shellhookIO{
		stdin:  strings.NewReader(""),
		stdout: &out,
		stderr: &errOut,
	}, root)
	return out.String(), errOut.String()
}

func TestShellhookCheck_ModeOffIsSilent(t *testing.T) {
	isolateShellhook(t)
	repo := shellhookTestRepo(t)

	// No preferences file at all — the un-installed default.
	if _, stderr := runCheck(t, repo); stderr != "" {
		t.Errorf("stderr = %q, want empty when the hook is off", stderr)
	}

	setShellhookMode(t, shellhook.ModeOff)
	if _, stderr := runCheck(t, repo); stderr != "" {
		t.Errorf("stderr = %q, want empty for mode=off", stderr)
	}
}

func TestShellhookCheck_WarnsOnceThenThrottles(t *testing.T) {
	isolateShellhook(t)
	setShellhookMode(t, shellhook.ModeWarn)
	repo := shellhookTestRepo(t)

	stdout, stderr := runCheck(t, repo)
	if stdout != "" {
		t.Errorf("stdout = %q, want empty (the hook must never pollute stdout)", stdout)
	}
	if !strings.Contains(stderr, "checkpointing is not enabled") {
		t.Errorf("stderr = %q, want a warning", stderr)
	}
	if !strings.Contains(stderr, "entire shellhook dismiss") {
		t.Errorf("stderr = %q, want a dismissal hint", stderr)
	}

	if _, stderr := runCheck(t, repo); stderr != "" {
		t.Errorf("second check stderr = %q, want empty (throttled)", stderr)
	}
}

func TestShellhookCheck_WarnsAgainAfterThrottleExpires(t *testing.T) {
	isolateShellhook(t)
	setShellhookMode(t, shellhook.ModeWarn)
	repo := shellhookTestRepo(t)

	if _, stderr := runCheck(t, repo); stderr == "" {
		t.Fatal("first check produced no warning")
	}

	// Rewind the recorded warning past the throttle window.
	state, err := shellhook.LoadState()
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if len(state.Repos) != 1 {
		t.Fatalf("state has %d repos, want 1", len(state.Repos))
	}
	for key := range state.Repos {
		state.MarkWarned(key, time.Now().Add(-25*time.Hour))
	}
	if err := shellhook.SaveState(state); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}

	if _, stderr := runCheck(t, repo); stderr == "" {
		t.Error("check after the throttle window produced no warning")
	}
}

func TestShellhookCheck_EnabledRepoIsSilent(t *testing.T) {
	isolateShellhook(t)
	setShellhookMode(t, shellhook.ModeWarn)
	repo := shellhookTestRepo(t)

	for _, name := range []string{"settings.json", "settings.local.json"} {
		t.Run(name, func(t *testing.T) {
			settingsPath := filepath.Join(repo, ".entire", name)
			if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.WriteFile(settingsPath, []byte(`{"enabled":true}`), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			defer func() { _ = os.Remove(settingsPath) }()

			if _, stderr := runCheck(t, repo); stderr != "" {
				t.Errorf("stderr = %q, want empty for a repo with %s", stderr, name)
			}
		})
	}
}

func TestShellhookCheck_DismissedRepoIsSilent(t *testing.T) {
	isolateShellhook(t)
	setShellhookMode(t, shellhook.ModeWarn)
	repo := shellhookTestRepo(t)

	key, err := shellhookRepoKey(context.Background(), repo)
	if err != nil {
		t.Fatalf("shellhookRepoKey() error = %v", err)
	}
	state := &shellhook.State{}
	state.MarkDismissed(key, time.Now().Add(-365*24*time.Hour))
	if err := shellhook.SaveState(state); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}

	if _, stderr := runCheck(t, repo); stderr != "" {
		t.Errorf("stderr = %q, want empty for a dismissed repo", stderr)
	}
}

func TestShellhookCheck_NonRepoRootIsSilent(t *testing.T) {
	isolateShellhook(t)
	setShellhookMode(t, shellhook.ModeWarn)

	for name, root := range map[string]string{
		"empty root":     "",
		"not a git repo": t.TempDir(),
		"missing path":   filepath.Join(t.TempDir(), "nope"),
	} {
		if _, stderr := runCheck(t, root); stderr != "" {
			t.Errorf("%s: stderr = %q, want empty", name, stderr)
		}
	}
}

func TestShellhookCheck_NamesDetectedAgent(t *testing.T) {
	isolateShellhook(t)
	setShellhookMode(t, shellhook.ModeWarn)
	repo := shellhookTestRepo(t)

	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	_, stderr := runCheck(t, repo)
	if !strings.Contains(stderr, "Claude Code") {
		t.Errorf("stderr = %q, want the detected agent named", stderr)
	}
}

func TestShellhookCheck_NeverWritesToTheRepo(t *testing.T) {
	isolateShellhook(t)
	setShellhookMode(t, shellhook.ModeWarn)
	repo := shellhookTestRepo(t)

	before := snapshotTree(t, repo)
	runCheck(t, repo)
	if after := snapshotTree(t, repo); !slices.Equal(before, after) {
		t.Errorf("check modified the repository:\nbefore = %v\nafter  = %v", before, after)
	}
}

// TestShellhookCheck_AutoModeWithoutTTYDegradesToWarn covers the safety
// property that matters most: in a non-interactive shell, auto mode must warn
// rather than run `entire enable` unattended.
func TestShellhookCheck_AutoModeWithoutTTYDegradesToWarn(t *testing.T) {
	isolateShellhook(t)
	setShellhookMode(t, shellhook.ModeAuto)
	repo := shellhookTestRepo(t)

	// interactive.CanPromptInteractively() is false under `go test`.
	stdout, stderr := runCheck(t, repo)
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "checkpointing is not enabled") {
		t.Errorf("stderr = %q, want the warning fallback", stderr)
	}
	if _, err := os.Stat(filepath.Join(repo, ".entire", "settings.json")); err == nil {
		t.Error("auto mode enabled the repo without a terminal")
	}
}

func TestShellhookCheckCmd_ExitsZero(t *testing.T) {
	isolateShellhook(t)
	setShellhookMode(t, shellhook.ModeWarn)
	repo := shellhookTestRepo(t)

	cmd := newShellhookCheckCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--root", repo})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("check returned error %v, want nil (it runs inside the user's prompt)", err)
	}
	if !strings.Contains(errOut.String(), "checkpointing is not enabled") {
		t.Errorf("stderr = %q, want a warning", errOut.String())
	}
}

func TestShellhookWarning_OmitsDetectedClauseWhenEmpty(t *testing.T) {
	t.Parallel()

	if got := shellhookWarning(nil); strings.Contains(got, "detected") {
		t.Errorf("warning = %q, want no detected clause", got)
	}
	got := shellhookWarning([]string{"Claude Code", "Codex"})
	if !strings.Contains(got, "(detected: Claude Code, Codex)") {
		t.Errorf("warning = %q, want both agents listed", got)
	}
}

func TestShellhookAutoEnableAgents_UnionsDetectedAndDefaults(t *testing.T) {
	t.Parallel()

	got := shellhookAutoEnableAgents([]string{testAgentName, recapTestAgentCodex}, []string{"Claude Code"})
	if len(got) != 2 {
		t.Fatalf("agents = %v, want 2 entries", got)
	}
	if got[0] != testAgentName {
		t.Errorf("agents[0] = %q, want the detected agent first", got[0])
	}
	if got[1] != recapTestAgentCodex {
		t.Errorf("agents[1] = %q, want the configured default", got[1])
	}
}

// ---------------------------------------------------------------- install

func TestShellhookInstall_IsIdempotent(t *testing.T) {
	isolateShellhook(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")

	rcFile := filepath.Join(home, ".zshrc")
	writeFileString(t, rcFile, "export EDITOR=vim\n")

	for range 2 {
		if out, err := runShellhookCmd(t, "install", "--yes"); err != nil {
			t.Fatalf("install error = %v (output: %s)", err, out)
		}
	}

	content := readFileString(t, rcFile)
	if n := strings.Count(content, shellhookComment); n != 1 {
		t.Errorf("marker appears %d times, want exactly 1:\n%s", n, content)
	}
	if !strings.Contains(content, "entire shellhook init zsh") {
		t.Errorf("rc file missing the zsh init line:\n%s", content)
	}
	if !strings.HasPrefix(content, "export EDITOR=vim\n") {
		t.Errorf("install rewrote pre-existing content:\n%s", content)
	}

	prefs, err := shellhook.LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences() error = %v", err)
	}
	if prefs.Mode != shellhook.ModeWarn {
		t.Errorf("Mode = %q, want %q", prefs.Mode, shellhook.ModeWarn)
	}
}

func TestShellhookInstall_AutoModeStoresAgents(t *testing.T) {
	isolateShellhook(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/bash")

	if out, err := runShellhookCmd(t, "install", "--yes", "--mode", "auto", "--agent", testAgentName); err != nil {
		t.Fatalf("install error = %v (output: %s)", err, out)
	}

	prefs, err := shellhook.LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences() error = %v", err)
	}
	if prefs.Mode != shellhook.ModeAuto {
		t.Errorf("Mode = %q, want %q", prefs.Mode, shellhook.ModeAuto)
	}
	if len(prefs.DefaultAgents) != 1 || prefs.DefaultAgents[0] != testAgentName {
		t.Errorf("DefaultAgents = %v, want [claude-code]", prefs.DefaultAgents)
	}
}

func TestShellhookInstall_RejectsBadMode(t *testing.T) {
	isolateShellhook(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/zsh")

	if _, err := runShellhookCmd(t, "install", "--yes", "--mode", "off"); err == nil {
		t.Error("install --mode off error = nil, want a validation error")
	}
	if _, err := runShellhookCmd(t, "install", "--yes", "--mode", "shout"); err == nil {
		t.Error("install --mode shout error = nil, want a validation error")
	}
}

func TestShellhookInstall_UnsupportedShellIsFriendly(t *testing.T) {
	isolateShellhook(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/usr/bin/nu")

	out, err := runShellhookCmd(t, "install", "--yes")
	if err == nil {
		t.Fatal("install error = nil, want an unsupported-shell error")
	}
	if !strings.Contains(out, supportedShellNames) {
		t.Errorf("output = %q, want the supported shells listed", out)
	}
}

func TestShellhookUninstall_RemovesBlockAndDisables(t *testing.T) {
	isolateShellhook(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")

	rcFile := filepath.Join(home, ".zshrc")
	original := "export EDITOR=vim\n\n# unrelated\nalias g=git\n"
	writeFileString(t, rcFile, original)

	if out, err := runShellhookCmd(t, "install", "--yes"); err != nil {
		t.Fatalf("install error = %v (output: %s)", err, out)
	}
	if out, err := runShellhookCmd(t, "uninstall"); err != nil {
		t.Fatalf("uninstall error = %v (output: %s)", err, out)
	}

	if got := readFileString(t, rcFile); got != original {
		t.Errorf("rc file after uninstall = %q, want %q", got, original)
	}
	prefs, err := shellhook.LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences() error = %v", err)
	}
	if prefs.Mode != shellhook.ModeOff {
		t.Errorf("Mode = %q, want %q", prefs.Mode, shellhook.ModeOff)
	}
}

func TestShellhookStatus_JSON(t *testing.T) {
	isolateShellhook(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")

	out, err := runShellhookCmd(t, "status", "--json")
	if err != nil {
		t.Fatalf("status error = %v", err)
	}
	var before shellhookStatusJSON
	if err := json.Unmarshal([]byte(out), &before); err != nil {
		t.Fatalf("status --json is not valid JSON (%v): %s", err, out)
	}
	if before.Installed {
		t.Error("Installed = true before install")
	}
	if before.Mode != string(shellhook.ModeOff) {
		t.Errorf("Mode = %q, want %q", before.Mode, shellhook.ModeOff)
	}

	if out, err := runShellhookCmd(t, "install", "--yes"); err != nil {
		t.Fatalf("install error = %v (output: %s)", err, out)
	}

	out, err = runShellhookCmd(t, "status", "--json")
	if err != nil {
		t.Fatalf("status error = %v", err)
	}
	var after shellhookStatusJSON
	if err := json.Unmarshal([]byte(out), &after); err != nil {
		t.Fatalf("status --json is not valid JSON (%v): %s", err, out)
	}
	if !after.Installed {
		t.Error("Installed = false after install")
	}
	if after.Mode != string(shellhook.ModeWarn) {
		t.Errorf("Mode = %q, want %q", after.Mode, shellhook.ModeWarn)
	}
	if after.RCFile != filepath.Join(home, ".zshrc") {
		t.Errorf("RCFile = %q, want %q", after.RCFile, filepath.Join(home, ".zshrc"))
	}
}

// runShellhookCmd executes a shellhook subcommand with captured output.
func runShellhookCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newShellhookCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

// snapshotTree lists every path under root except the git directory, whose
// internals (index mtimes, refs) change for reasons unrelated to this command.
func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel == ".git" {
			return filepath.SkipDir
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk(%s) error = %v", root, err)
	}
	return paths
}

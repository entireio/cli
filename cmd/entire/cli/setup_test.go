package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/agent"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/agent/vogon"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Note: Tests for hook manipulation functions (addHookToMatcher, hookCommandExists, etc.)
// have been moved to the agent/claudecode package where these functions now reside.
// See cmd/entire/cli/agent/claudecode/hooks_test.go for those tests.

// setupTestDir creates a temp directory, changes to it, and returns it.
// It also registers cleanup to restore the original directory.
func setupTestDir(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	hideExternalAgentsFromPath(t)
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	return tmpDir
}

// setupTestRepo creates a temp directory with a git repo initialized.
func setupTestRepo(t *testing.T) {
	t.Helper()
	tmpDir := setupTestDir(t)
	testutil.InitRepo(t, tmpDir)
}

// writeSettings writes settings content to the settings file.
func writeSettings(t *testing.T, content string) {
	t.Helper()
	settingsDir := filepath.Dir(EntireSettingsFile)
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatalf("Failed to create settings dir: %v", err)
	}
	if err := os.WriteFile(EntireSettingsFile, []byte(content), 0o644); err != nil {
		t.Fatalf("Failed to write settings file: %v", err)
	}
}

func hideExternalAgentsFromPath(t *testing.T) {
	t.Helper()

	pathDir := t.TempDir()
	for _, name := range []string{"git", "sh"} {
		if err := preserveToolOnPath(name, pathDir); err != nil {
			t.Fatalf("preserve %s on PATH: %v", name, err)
		}
	}

	t.Setenv("PATH", pathDir)
}

func TestSetupTestDir_HidesExternalAgentsButKeepsGitAvailable(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}

	sharedDir := t.TempDir()
	if err := copyExecutable(gitPath, filepath.Join(sharedDir, "git")); err != nil {
		t.Fatalf("copy git executable: %v", err)
	}
	writeExternalAgentBinary(t, sharedDir, "ext-shared-dir")
	t.Setenv("PATH", sharedDir)

	setupTestDir(t)

	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("expected git to remain available after test PATH isolation: %v", err)
	}
	if _, err := exec.LookPath("entire-agent-ext-shared-dir"); err == nil {
		t.Fatal("expected external agent to be hidden from PATH")
	}
}

func preserveToolOnPath(name, dstDir string) error {
	src, err := exec.LookPath(name)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil
		}
		return err
	}

	return copyExecutable(src, filepath.Join(dstDir, filepath.Base(src)))
}

func copyExecutable(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := os.Symlink(src, dst); err == nil {
		return nil
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	return os.WriteFile(dst, data, info.Mode())
}

func writeExternalAgentBinary(t *testing.T, dir, name string) {
	t.Helper()
	writeExternalAgentBinaryEx(t, dir, name, false)
}

// writeExternalAgentBinaryEx writes a mock external-agent binary whose
// are-hooks-installed subcommand reports hooksInstalled, so callers can
// simulate both installed and available (uninstalled) external plugins.
func writeExternalAgentBinaryEx(t *testing.T, dir, name string, hooksInstalled bool) {
	t.Helper()
	// Whatever this mock is written for, discovering it registers it into the
	// process-global registry, which outlives the TempDir holding the binary.
	// Restore the registry so later tests walking agent.List() do not exec a
	// path that no longer exists.
	t.Cleanup(agent.SnapshotRegistryForTesting())

	installed := strconv.FormatBool(hooksInstalled)

	// When ENTIRE_TEST_EXEC_LOG is set, record every invocation's subcommand so a
	// test can assert which subcommands the CLI actually invoked. Unset in other
	// tests, so they are undisturbed.
	script := `#!/bin/sh
[ -n "$ENTIRE_TEST_EXEC_LOG" ] && echo "$1" >> "$ENTIRE_TEST_EXEC_LOG"
case "$1" in
  info)
    echo '{"protocol_version":1,"name":"` + name + `","type":"` + name + ` Agent","description":"External test agent","is_preview":false,"protected_dirs":[],"hook_names":["stop"],"capabilities":{"hooks":true}}'
    ;;
  detect)
    if [ "$ENTIRE_TEST_EXTERNAL_PRESENT" = "1" ]; then
      echo '{"present": true}'
    else
      echo '{"present": false}'
    fi
    ;;
  install-hooks)
    echo '{"hooks_installed": 1}'
    ;;
  uninstall-hooks)
    # ENTIRE_TEST_FAIL_UNINSTALL_HOOKS simulates a plugin that cannot remove its
    # own hooks, so a test can drive the CLI's leftover-hooks recovery path.
    if [ -n "$ENTIRE_TEST_FAIL_UNINSTALL_HOOKS" ]; then
      echo "mock uninstall-hooks failure" >&2
      exit 1
    fi
    exit 0
    ;;
  are-hooks-installed)
    # ENTIRE_TEST_PROBE drives the two ways a plugin can fail to answer, so a test
    # can tell "no hooks" apart from "could not say".
    case "$ENTIRE_TEST_PROBE" in
      fail)    echo "mock probe failure" >&2; exit 1 ;;
      garbage) echo 'not json' ;;
      *)       echo '{"installed": ` + installed + `}' ;;
    esac
    ;;
  *)
    echo '{}'
    ;;
esac
`

	if err := os.WriteFile(filepath.Join(dir, "entire-agent-"+name), []byte(script), 0o755); err != nil {
		t.Fatalf("Failed to write external agent binary: %v", err)
	}
}

func writeExternalSummaryAgentBinary(t *testing.T, dir, name string) {
	t.Helper()
	t.Cleanup(agent.SnapshotRegistryForTesting())

	script := `#!/bin/sh
case "$1" in
  info)
    echo '{"protocol_version":1,"name":"` + name + `","type":"` + name + ` Agent","description":"External summary test agent","is_preview":false,"protected_dirs":[],"hook_names":[],"capabilities":{"hooks":false,"transcript_analyzer":false,"transcript_preparer":false,"token_calculator":false,"compact_transcript":false,"text_generator":true,"hook_response_writer":false,"subagent_aware_extractor":false}}'
    ;;
  detect)
    echo '{"present": true}'
    ;;
  generate-text)
    if [ -n "$ENTIRE_TEST_EXTERNAL_MODEL_RECORD" ]; then
      printf '%s\n%s\n' "$2" "$3" > "$ENTIRE_TEST_EXTERNAL_MODEL_RECORD"
    fi
    echo '{"text":"{\"intent\":\"Intent\",\"outcome\":\"Outcome\",\"learnings\":{\"repo\":[],\"code\":[],\"workflow\":[]},\"friction\":[],\"open_items\":[]}"}'
    ;;
  *)
    echo '{}'
    ;;
esac
`

	if err := os.WriteFile(filepath.Join(dir, "entire-agent-"+name), []byte(script), 0o755); err != nil {
		t.Fatalf("Failed to write external summary agent binary: %v", err)
	}
}

func TestRunEnable(t *testing.T) {
	setupTestDir(t)
	writeSettings(t, testSettingsDisabled)

	var stdout bytes.Buffer
	if err := runEnable(context.Background(), &stdout, false); err != nil {
		t.Fatalf("runEnable() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "enabled") {
		t.Errorf("Expected output to contain 'enabled', got: %s", stdout.String())
	}

	enabled, err := IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled(context.Background()) error = %v", err)
	}
	if !enabled {
		t.Error("Entire should be enabled after running enable command")
	}
}

func TestRunEnable_AlreadyEnabled(t *testing.T) {
	setupTestDir(t)
	writeSettings(t, testSettingsEnabled)

	var stdout bytes.Buffer
	if err := runEnable(context.Background(), &stdout, false); err != nil {
		t.Fatalf("runEnable() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "enabled") {
		t.Errorf("Expected output to mention enabled state, got: %s", stdout.String())
	}
}

// TestRunEnableOnConfiguredRepo_RecoversLegacySplitState covers recovering
// the split state a pre-fix binary left on disk — committed
// settings.json enabled:false, settings.local.json enabled:true. The local
// override wins in the merged view, so IsEnabled reports true; a bare early
// return on the merged view would leave the committed project file disabled
// forever, even with an explicit --project. runEnableOnConfiguredRepo must
// detect that the target scope is itself disabled and flip it.
func TestRunEnableOnConfiguredRepo_RecoversLegacySplitState(t *testing.T) {
	setupTestRepo(t)
	// Legacy split state.
	writeSettings(t, testSettingsDisabled)
	writeLocalSettings(t, `{"enabled": true}`)

	// Sanity: the merged view already reports enabled (local override wins).
	enabled, err := IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
	}
	if !enabled {
		t.Fatal("precondition: merged view should report enabled (local override wins)")
	}

	cmd := newEnableCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := runEnableOnConfiguredRepo(context.Background(), cmd, EnableOptions{UseProjectSettings: true}); err != nil {
		t.Fatalf("runEnableOnConfiguredRepo(--project) error = %v", err)
	}

	// The committed project file must now be enabled — this split state could
	// not recover before this fix.
	projectS, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to load project settings: %v", err)
	}
	if !projectS.Enabled {
		t.Error("committed settings.json should be enabled:true after enable --project recovered the split state")
	}
}

// TestRunEnableOnConfiguredRepo_BareEnable_RecoversLegacySplitState verifies the
// same recovery happens for a bare `entire enable` (no --project), which
// resolves to the committed settings.json via settingsTargetFile.
func TestRunEnableOnConfiguredRepo_BareEnable_RecoversLegacySplitState(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsDisabled)
	writeLocalSettings(t, `{"enabled": true}`)

	cmd := newEnableCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := runEnableOnConfiguredRepo(context.Background(), cmd, EnableOptions{}); err != nil {
		t.Fatalf("runEnableOnConfiguredRepo() error = %v", err)
	}

	projectS, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to load project settings: %v", err)
	}
	if !projectS.Enabled {
		t.Error("committed settings.json should be enabled:true after a bare enable recovered the split state")
	}
}

// TestRunEnableOnConfiguredRepo_AlreadyEnabled_NoSplit verifies the early
// return still fires (nothing to flip, "already enabled") when the merged view
// AND the resolved target scope agree that Entire is enabled.
func TestRunEnableOnConfiguredRepo_AlreadyEnabled_NoSplit(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	cmd := newEnableCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := runEnableOnConfiguredRepo(context.Background(), cmd, EnableOptions{}); err != nil {
		t.Fatalf("runEnableOnConfiguredRepo() error = %v", err)
	}
	if !strings.Contains(buf.String(), "already enabled") {
		t.Errorf("expected 'already enabled' output when nothing to recover, got: %s", buf.String())
	}
}

// TestRunEnable_ProjectFlag_ClearsLocalDisable verifies that `entire enable
// --project` clears a real local disable override. The precondition is seeded
// directly (settings.local.json enabled:false with a local-only field) rather
// than through runDisable, so the "local override wins and must be cleared"
// scenario is genuinely exercised — the local-sync in setEnabledFlag's project
// branch is what makes the re-enable stick.
func TestRunEnable_ProjectFlag_ClearsLocalDisable(t *testing.T) {
	setupTestDir(t)
	writeSettings(t, testSettingsEnabled)
	// A real local disable override with a local-only field to prove the sync
	// touches only the enabled key.
	writeLocalSettings(t, `{"enabled": false, "absolute_git_hook_path": true}`)

	// Precondition: the local override wins, so the merged view is disabled.
	enabled, err := IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
	}
	if enabled {
		t.Fatal("precondition: local override should make the merged view disabled")
	}

	var buf bytes.Buffer
	if err := runEnable(context.Background(), &buf, true); err != nil {
		t.Fatalf("runEnable(project=true) error = %v", err)
	}

	// Must actually be enabled — the local override must have been cleared.
	enabled, err = IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
	}
	if !enabled {
		t.Error("Expected enabled after runEnable --project, but IsEnabled() returned false (local override not cleared)")
	}

	// The local file's enabled key was synced to true, and its local-only
	// field survived.
	localContent, err := os.ReadFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("failed to read local settings: %v", err)
	}
	if !strings.Contains(string(localContent), `"enabled":true`) && !strings.Contains(string(localContent), `"enabled": true`) {
		t.Errorf("local override should be synced to enabled:true, got: %s", localContent)
	}
	if !strings.Contains(string(localContent), "absolute_git_hook_path") {
		t.Errorf("local-only field absolute_git_hook_path should be retained, got: %s", localContent)
	}
}

// TestRunEnable_ProjectScope_ClearsExplicitLocalDisable seeds both files
// disabled (committed settings.json enabled:false AND settings.local.json
// enabled:false with absolute_git_hook_path) and asserts that a project-scope enable flips
// both and retains the local-only field. This is the mutation-sensitive test
// for setEnabledFlag's project-branch local sync: skipping the sync leaves the
// local override at enabled:false, which would win and keep IsEnabled false.
func TestRunEnable_ProjectScope_ClearsExplicitLocalDisable(t *testing.T) {
	setupTestDir(t)
	writeSettings(t, testSettingsDisabled)
	writeLocalSettings(t, `{"enabled": false, "absolute_git_hook_path": true}`)

	var buf bytes.Buffer
	if err := runEnable(context.Background(), &buf, true); err != nil {
		t.Fatalf("runEnable(project=true) error = %v", err)
	}

	enabled, err := IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
	}
	if !enabled {
		t.Error("Expected enabled after runEnable --project (local override must be synced to enabled:true)")
	}

	projectContent, err := os.ReadFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to read project settings: %v", err)
	}
	if !strings.Contains(string(projectContent), `"enabled":true`) && !strings.Contains(string(projectContent), `"enabled": true`) {
		t.Errorf("committed project settings should be enabled:true, got: %s", projectContent)
	}

	localContent, err := os.ReadFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("failed to read local settings: %v", err)
	}
	if !strings.Contains(string(localContent), `"enabled":true`) && !strings.Contains(string(localContent), `"enabled": true`) {
		t.Errorf("local override should be synced to enabled:true, got: %s", localContent)
	}
	if !strings.Contains(string(localContent), "absolute_git_hook_path") {
		t.Errorf("local-only field absolute_git_hook_path should be retained, got: %s", localContent)
	}
}

// TestRunEnable_DefaultFlag_ClearsLocalDisable verifies that `entire enable`
// (default/local scope) clears an explicitly-seeded local disable override.
func TestRunEnable_DefaultFlag_ClearsLocalDisable(t *testing.T) {
	setupTestDir(t)
	writeSettings(t, testSettingsEnabled)
	writeLocalSettings(t, `{"enabled": false, "absolute_git_hook_path": true}`)

	// Precondition: local override wins → disabled.
	enabled, err := IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
	}
	if enabled {
		t.Fatal("precondition: local override should make the merged view disabled")
	}

	var buf bytes.Buffer
	if err := runEnable(context.Background(), &buf, false); err != nil {
		t.Fatalf("runEnable(project=false) error = %v", err)
	}

	enabled, err = IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
	}
	if !enabled {
		t.Error("Expected enabled after runEnable, but IsEnabled() returned false")
	}

	localContent, err := os.ReadFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("failed to read local settings: %v", err)
	}
	if !strings.Contains(string(localContent), "absolute_git_hook_path") {
		t.Errorf("local-only field absolute_git_hook_path should be retained, got: %s", localContent)
	}
}

// TestSetupAgentHooksNonInteractive_ClearsLocalDisable verifies that a
// project-scope `enable --agent` clears a real local disable override. The
// precondition is seeded directly (settings.local.json enabled:false) rather
// than via runDisable, and the assertion checks the local override was actually
// synced — otherwise "ClearsLocalDisable" would assert nothing.
func TestSetupAgentHooksNonInteractive_ClearsLocalDisable(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	writeLocalSettings(t, `{"enabled": false, "absolute_git_hook_path": true}`)
	writeClaudeHooksFixture(t)

	// Precondition: local override wins → disabled.
	enabled, err := IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
	}
	if enabled {
		t.Fatal("precondition: local override should make the merged view disabled")
	}

	ag, err := agent.Get(types.AgentName("claude-code"))
	if err != nil {
		t.Fatalf("agent.Get(claude-code) error = %v", err)
	}

	var buf bytes.Buffer
	// UseProjectSettings so the enable resolves to the committed file and its
	// project branch syncs the local override.
	if err := setupAgentHooksNonInteractive(context.Background(), &buf, ag, EnableOptions{UseProjectSettings: true}); err != nil {
		t.Fatalf("setupAgentHooksNonInteractive() error = %v", err)
	}

	enabled, err = IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
	}
	if !enabled {
		t.Fatal("expected enabled after setupAgentHooksNonInteractive (local override must be cleared)")
	}

	localContent, err := os.ReadFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("failed to read local settings: %v", err)
	}
	if !strings.Contains(string(localContent), `"enabled":true`) && !strings.Contains(string(localContent), `"enabled": true`) {
		t.Errorf("local override should be synced to enabled:true, got: %s", localContent)
	}
	if !strings.Contains(string(localContent), "absolute_git_hook_path") {
		t.Errorf("local-only field absolute_git_hook_path should be retained, got: %s", localContent)
	}
}

// TestSetupAgentHooksNonInteractive_DoesNotLeakLocalOverridesIntoProject:
// `entire enable --agent <name>` on an already-configured repo used to load the
// merged settings view (LoadEntireSettings) and write it back wholesale to the
// project file via saveEnabledState, flattening settings.local.json-only
// overrides (e.g. log_level) into the shared, committed settings.json — the
// same leak fixed for the bare enable/disable path, just via a different
// entry point (setupAgentHooksNonInteractive).
func TestSetupAgentHooksNonInteractive_DoesNotLeakLocalOverridesIntoProject(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	writeLocalSettings(t, `{"log_level": "debug"}`)
	writeClaudeHooksFixture(t)

	ag, err := agent.Get(types.AgentName("claude-code"))
	if err != nil {
		t.Fatalf("agent.Get(claude-code) error = %v", err)
	}

	var buf bytes.Buffer
	if err := setupAgentHooksNonInteractive(context.Background(), &buf, ag, EnableOptions{}); err != nil {
		t.Fatalf("setupAgentHooksNonInteractive() error = %v", err)
	}

	projectS, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to load project settings: %v", err)
	}
	if projectS.LogLevel != "" {
		t.Errorf("local-only log_level leaked into project settings: %q", projectS.LogLevel)
	}
	if !projectS.Enabled {
		t.Error("expected project settings to remain enabled")
	}

	localS, err := settings.LoadFromFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("failed to load local settings: %v", err)
	}
	if localS.LogLevel != "debug" {
		t.Errorf("expected local log_level to be preserved, got %q", localS.LogLevel)
	}
}

// TestSetupAgentHooksNonInteractive_UsesMergedAbsoluteHookPathForHookInstall
// covers the merged view for hook generation:
// with absolute_git_hook_path set only in settings.local.json while the enable
// resolves (via --project) to settings.json, the generated hook must embed the
// absolute binary path from the merged view — not fall back to the bare
// "entire" prefix the target-scoped struct alone would yield. Guards against a
// mutation reverting hookAbsoluteGitHookPath to the scoped struct.
func TestSetupAgentHooksNonInteractive_UsesMergedAbsoluteHookPathForHookInstall(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	// absolute_git_hook_path only in the local override.
	writeLocalSettings(t, `{"enabled": true, "absolute_git_hook_path": true}`)
	writeClaudeHooksFixture(t)

	ag, err := agent.Get(types.AgentName("claude-code"))
	if err != nil {
		t.Fatalf("agent.Get(claude-code) error = %v", err)
	}

	var buf bytes.Buffer
	opts := EnableOptions{UseProjectSettings: true}
	if err := setupAgentHooksNonInteractive(context.Background(), &buf, ag, opts); err != nil {
		t.Fatalf("setupAgentHooksNonInteractive() error = %v", err)
	}

	// The hook must embed the resolved absolute executable path (what
	// absolute_git_hook_path produces), proving the merged override was honored.
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}

	hooksDir, err := strategy.GetHooksDir(context.Background())
	if err != nil {
		t.Fatalf("GetHooksDir() error = %v", err)
	}
	hookContent, err := os.ReadFile(filepath.Join(hooksDir, "post-commit"))
	if err != nil {
		t.Fatalf("failed to read post-commit hook: %v", err)
	}
	if !strings.Contains(string(hookContent), resolved) {
		t.Errorf("expected hook to embed absolute binary path %q from the merged view, got: %s", resolved, hookContent)
	}

	// The write path must still stay scoped: absolute_git_hook_path must not
	// leak into the committed project settings.json.
	projectS, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to load project settings: %v", err)
	}
	if projectS.AbsoluteGitHookPath {
		t.Error("local-only absolute_git_hook_path override leaked into project settings")
	}
}

// TestSetupAgentHooksNonInteractive_LocalTarget_DoesNotLeakProjectFieldsIntoLocal
// covers the mirror-image direction: writing to settings.local.json (--local)
// must not flatten project-only fields into the local file either.
func TestSetupAgentHooksNonInteractive_LocalTarget_DoesNotLeakProjectFieldsIntoLocal(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, `{"enabled": true, "log_level": "warn"}`)
	writeClaudeHooksFixture(t)

	ag, err := agent.Get(types.AgentName("claude-code"))
	if err != nil {
		t.Fatalf("agent.Get(claude-code) error = %v", err)
	}

	var buf bytes.Buffer
	opts := EnableOptions{UseLocalSettings: true}
	if err := setupAgentHooksNonInteractive(context.Background(), &buf, ag, opts); err != nil {
		t.Fatalf("setupAgentHooksNonInteractive() error = %v", err)
	}

	localS, err := settings.LoadFromFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("failed to load local settings: %v", err)
	}
	if localS.LogLevel != "" {
		t.Errorf("project-only log_level leaked into local settings: %q", localS.LogLevel)
	}
	if !localS.Enabled {
		t.Error("expected local settings to be enabled")
	}

	projectS, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to load project settings: %v", err)
	}
	if projectS.LogLevel != "warn" {
		t.Errorf("expected project log_level to be preserved, got %q", projectS.LogLevel)
	}
}

// TestSetupAgentHooksNonInteractive_RefusesToClobberUnparseableSettings covers
// the finding that `entire enable --agent` silently wiped a corrupt or
// newer-versioned target settings file to defaults. settings.LoadFromFile
// errors on invalid JSON AND on any unknown key (DisallowUnknownFields); the
// old catch replaced the struct with defaults and wrote it back, so a
// settings.json with strategy_options/log_level/one-unknown-key became exactly
// {"enabled": true}. Now it refuses and leaves the file untouched.
func TestSetupAgentHooksNonInteractive_RefusesToClobberUnparseableSettings(t *testing.T) {
	setupTestRepo(t)
	// A settings.json a newer CLI could write: valid JSON, real content, plus a
	// key this build doesn't recognize (rejected by DisallowUnknownFields).
	original := `{"enabled": false, "log_level": "debug", "totally_unknown_future_key": 42}`
	writeSettings(t, original)
	writeClaudeHooksFixture(t)

	ag, err := agent.Get(types.AgentName("claude-code"))
	if err != nil {
		t.Fatalf("agent.Get(claude-code) error = %v", err)
	}

	var buf bytes.Buffer
	if err := setupAgentHooksNonInteractive(context.Background(), &buf, ag, EnableOptions{}); err == nil {
		t.Fatal("expected setupAgentHooksNonInteractive to refuse on an unparseable settings file, got nil error")
	}

	// The file must be left as-is, not wiped to {"enabled": true}.
	got, err := os.ReadFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to read project settings: %v", err)
	}
	if !strings.Contains(string(got), "totally_unknown_future_key") {
		t.Errorf("unknown key must survive (file must not be clobbered), got: %s", got)
	}
	if !strings.Contains(string(got), "log_level") {
		t.Errorf("log_level must survive (file must not be clobbered), got: %s", got)
	}
	if strings.Contains(string(got), `"enabled": true`) || strings.Contains(string(got), `"enabled":true`) {
		t.Errorf("enabled must not have been flipped/rewritten, got: %s", got)
	}
}

func TestSetupAgentHooksNonInteractive_InstallsCodexWhenDiscoveryIsUnresolved(t *testing.T) {
	ag, linkedRoot := setupCodexAgentWithUnresolvedDiscovery(t)

	var output bytes.Buffer
	if err := setupAgentHooksNonInteractive(t.Context(), &output, ag, EnableOptions{}); err != nil {
		t.Fatalf("enable Codex with unresolved discovery: %v", err)
	}
	if _, err := os.Stat(filepath.Join(linkedRoot, ".codex", "hooks.json")); err != nil {
		t.Fatalf("current-worktree hooks were not installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(linkedRoot, ".entire")); err != nil {
		t.Fatalf("enable did not write project state: %v", err)
	}
}

func TestRunEnableInteractive_InstallsCodexWhenDiscoveryIsUnresolved(t *testing.T) {
	ag, linkedRoot := setupCodexAgentWithUnresolvedDiscovery(t)

	var output bytes.Buffer
	if err := runEnableInteractive(t.Context(), &output, []agent.Agent{ag}, EnableOptions{Yes: true, Telemetry: true}); err != nil {
		t.Fatalf("interactive enable Codex with unresolved discovery: %v", err)
	}
	if _, err := os.Stat(filepath.Join(linkedRoot, ".codex", "hooks.json")); err != nil {
		t.Fatalf("current-worktree hooks were not installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(linkedRoot, ".entire")); err != nil {
		t.Fatalf("enable did not write project state: %v", err)
	}
}

func setupCodexAgentWithUnresolvedDiscovery(t *testing.T) (agent.Agent, string) {
	t.Helper()
	tmp := setupTestDir(t)
	primaryRoot := filepath.Join(tmp, "primary")
	linkedRoot := filepath.Join(tmp, "linked")
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = testutil.GitIsolatedEnv()
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	testutil.InitRepo(t, primaryRoot)
	testutil.WriteFile(t, primaryRoot, "README.md", "initial\n")
	testutil.GitAdd(t, primaryRoot, "README.md")
	testutil.GitCommit(t, primaryRoot, "initial")
	runGit(primaryRoot, "worktree", "add", "-b", "feature", linkedRoot)
	t.Chdir(linkedRoot)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	// Codex discovery refuses a project layer that is also CODEX_HOME, while
	// current-worktree installation remains independently valid.
	if err := os.MkdirAll(filepath.Join(primaryRoot, ".codex"), 0o750); err != nil {
		t.Fatalf("create colliding CODEX_HOME: %v", err)
	}
	t.Setenv("CODEX_HOME", filepath.Join(primaryRoot, ".codex"))

	ag, err := agent.Get(agent.AgentNameCodex)
	if err != nil {
		t.Fatalf("get Codex agent: %v", err)
	}
	return ag, linkedRoot
}

func TestRunDisable(t *testing.T) {
	setupTestDir(t)
	writeSettings(t, testSettingsEnabled)

	var stdout bytes.Buffer
	if err := runDisable(context.Background(), &stdout, false); err != nil {
		t.Fatalf("runDisable() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "disabled") {
		t.Errorf("Expected output to contain 'disabled', got: %s", stdout.String())
	}

	enabled, err := IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled(context.Background()) error = %v", err)
	}
	if enabled {
		t.Error("Entire should be disabled after running disable command")
	}
}

func TestRunDisable_AlreadyDisabled(t *testing.T) {
	setupTestDir(t)
	writeSettings(t, testSettingsDisabled)

	var stdout bytes.Buffer
	if err := runDisable(context.Background(), &stdout, false); err != nil {
		t.Fatalf("runDisable() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "disabled") {
		t.Errorf("Expected output to mention disabled state, got: %s", stdout.String())
	}
}

func TestCheckDisabledGuard(t *testing.T) {
	setupTestDir(t)

	// No settings file - should not be disabled (defaults to enabled)
	var stdout bytes.Buffer
	if checkDisabledGuard(context.Background(), &stdout) {
		t.Error("checkDisabledGuard() should return false when no settings file exists")
	}
	if stdout.String() != "" {
		t.Errorf("checkDisabledGuard() should not print anything when enabled, got: %s", stdout.String())
	}

	// Settings with enabled: true
	writeSettings(t, testSettingsEnabled)
	stdout.Reset()
	if checkDisabledGuard(context.Background(), &stdout) {
		t.Error("checkDisabledGuard() should return false when enabled")
	}

	// Settings with enabled: false
	writeSettings(t, testSettingsDisabled)
	stdout.Reset()
	if !checkDisabledGuard(context.Background(), &stdout) {
		t.Error("checkDisabledGuard() should return true when disabled")
	}
	output := stdout.String()
	if !strings.Contains(output, "Entire is disabled") {
		t.Errorf("Expected disabled message, got: %s", output)
	}
	if !strings.Contains(output, "entire enable") {
		t.Errorf("Expected message to mention 'entire enable', got: %s", output)
	}
}

// writeLocalSettings writes settings content to the local settings file.
func writeLocalSettings(t *testing.T, content string) {
	t.Helper()
	settingsDir := filepath.Dir(EntireSettingsLocalFile)
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatalf("Failed to create settings dir: %v", err)
	}
	if err := os.WriteFile(EntireSettingsLocalFile, []byte(content), 0o644); err != nil {
		t.Fatalf("Failed to write local settings file: %v", err)
	}
}

func TestRunDisable_WithLocalSettings(t *testing.T) {
	setupTestDir(t)
	// Create both settings files with enabled: true
	writeSettings(t, testSettingsEnabled)
	writeLocalSettings(t, `{"enabled": true}`)

	var stdout bytes.Buffer
	if err := runDisable(context.Background(), &stdout, false); err != nil {
		t.Fatalf("runDisable() error = %v", err)
	}

	// Should be disabled because runDisable updates local settings when it exists
	enabled, err := IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled(context.Background()) error = %v", err)
	}
	if enabled {
		t.Error("Entire should be disabled after running disable command (local settings should be updated)")
	}

	// Verify local settings file was updated
	localContent, err := os.ReadFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("Failed to read local settings: %v", err)
	}
	if !strings.Contains(string(localContent), `"enabled":false`) && !strings.Contains(string(localContent), `"enabled": false`) {
		t.Errorf("Local settings should have enabled:false, got: %s", localContent)
	}
}

func TestRunDisable_WithProjectFlag(t *testing.T) {
	setupTestDir(t)
	// Create both settings files with enabled: true
	writeSettings(t, testSettingsEnabled)
	writeLocalSettings(t, `{"enabled": true}`)

	var stdout bytes.Buffer
	// Use --project flag (useProjectSettings = true)
	if err := runDisable(context.Background(), &stdout, true); err != nil {
		t.Fatalf("runDisable() error = %v", err)
	}

	// Verify project settings file was updated (not local)
	projectContent, err := os.ReadFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("Failed to read project settings: %v", err)
	}
	if !strings.Contains(string(projectContent), `"enabled":false`) && !strings.Contains(string(projectContent), `"enabled": false`) {
		t.Errorf("Project settings should have enabled:false, got: %s", projectContent)
	}

	// Local settings should also be updated to stay in sync
	localContent, err := os.ReadFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("Failed to read local settings: %v", err)
	}
	if !strings.Contains(string(localContent), `"enabled":false`) && !strings.Contains(string(localContent), `"enabled": false`) {
		t.Errorf("Local settings should also have enabled:false to stay in sync, got: %s", localContent)
	}
}

// TestRunDisable_BareCommand_WritesLocalOverrideWhenProjectOnly verifies that a
// bare `entire disable`, on a repo that only has a committed settings.json (no
// settings.local.json yet), writes the enabled:false override into
// settings.local.json and leaves the committed settings.json untouched. Bare
// disable is a personal, non-destructive silence: because local overrides
// project in the merged view, it makes IsEnabled false without editing shared
// team config. Restores origin/main behavior; regression test for the bare
// disable scope-resolution finding.
func TestRunDisable_BareCommand_WritesLocalOverrideWhenProjectOnly(t *testing.T) {
	setupTestDir(t)
	// Only create project settings (no local settings)
	writeSettings(t, testSettingsEnabled)

	var stdout bytes.Buffer
	if err := runDisable(context.Background(), &stdout, false); err != nil {
		t.Fatalf("runDisable() error = %v", err)
	}

	// Should be disabled (local override wins in the merged view).
	enabled, err := IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled(context.Background()) error = %v", err)
	}
	if enabled {
		t.Error("Entire should be disabled after running disable command")
	}

	// The local override should be created with enabled:false.
	localContent, err := os.ReadFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("settings.local.json should have been created: %v", err)
	}
	if !strings.Contains(string(localContent), `"enabled":false`) && !strings.Contains(string(localContent), `"enabled": false`) {
		t.Errorf("local settings should have enabled:false, got: %s", localContent)
	}

	// The committed project file must be left untouched (still enabled).
	projectContent, err := os.ReadFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("Failed to read project settings: %v", err)
	}
	if !strings.Contains(string(projectContent), `"enabled":true`) && !strings.Contains(string(projectContent), `"enabled": true`) {
		t.Errorf("committed project settings should stay enabled:true after a bare disable, got: %s", projectContent)
	}
}

// TestRunDisable_CreatesSettingsDirWhenMissing verifies that a bare `entire
// disable` succeeds in a repo that has never created a .entire/ directory,
// creating settings.local.json (with its parent dir) rather than hard-failing.
// End-to-end regression test for the saveRaw MkdirAll fix.
func TestRunDisable_CreatesSettingsDirWhenMissing(t *testing.T) {
	setupTestDir(t)
	// No .entire/ directory or settings files at all.

	var stdout bytes.Buffer
	if err := runDisable(context.Background(), &stdout, false); err != nil {
		t.Fatalf("runDisable() in a repo with no .entire/ dir should succeed, got: %v", err)
	}

	enabled, err := IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled(context.Background()) error = %v", err)
	}
	if enabled {
		t.Error("Entire should be disabled after running disable command")
	}

	localContent, err := os.ReadFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("settings.local.json should have been created: %v", err)
	}
	if !strings.Contains(string(localContent), `"enabled":false`) && !strings.Contains(string(localContent), `"enabled": false`) {
		t.Errorf("local settings should have enabled:false, got: %s", localContent)
	}
}

// TestRunDisable_BareCommand_WritesLocalWhenBothExist verifies that a bare
// `entire disable`, when both settings.json and settings.local.json exist,
// writes enabled:false into the local override only and leaves the committed
// settings.json untouched (no field leakage between scopes). Regression test
// for the bare disable scope-resolution finding.
func TestRunDisable_BareCommand_WritesLocalWhenBothExist(t *testing.T) {
	setupTestDir(t)
	writeSettings(t, `{"enabled": true, "log_level": "warn"}`)
	writeLocalSettings(t, `{"enabled": true, "absolute_git_hook_path": true}`)

	var stdout bytes.Buffer
	if err := runDisable(context.Background(), &stdout, false); err != nil {
		t.Fatalf("runDisable() error = %v", err)
	}

	enabled, err := IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
	}
	if enabled {
		t.Error("Entire should be disabled after running disable command")
	}

	// The committed project file must be untouched: still enabled, keeps its
	// own fields, and never gains the local-only override.
	projectContent, err := os.ReadFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to read project settings: %v", err)
	}
	if !strings.Contains(string(projectContent), `"enabled":true`) && !strings.Contains(string(projectContent), `"enabled": true`) {
		t.Errorf("committed project settings should stay enabled:true after a bare disable, got: %s", projectContent)
	}
	if !strings.Contains(string(projectContent), "log_level") {
		t.Errorf("project settings should retain its own log_level field, got: %s", projectContent)
	}
	if strings.Contains(string(projectContent), "absolute_git_hook_path") {
		t.Errorf("project settings must not gain local-only override absolute_git_hook_path, got: %s", projectContent)
	}

	// The local override carries the disable and keeps its own fields.
	localContent, err := os.ReadFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("failed to read local settings: %v", err)
	}
	if !strings.Contains(string(localContent), `"enabled":false`) && !strings.Contains(string(localContent), `"enabled": false`) {
		t.Errorf("local settings should have enabled:false, got: %s", localContent)
	}
	if !strings.Contains(string(localContent), "absolute_git_hook_path") {
		t.Errorf("local settings should retain its own absolute_git_hook_path field, got: %s", localContent)
	}
}

// TestRunDisable_ProjectFlag_WritesCommittedFile verifies that `entire disable
// --project` flips the committed settings.json and syncs the local override so
// a stale local file can't leave the repo enabled.
func TestRunDisable_ProjectFlag_WritesCommittedFile(t *testing.T) {
	setupTestDir(t)
	writeSettings(t, `{"enabled": true, "log_level": "warn"}`)
	writeLocalSettings(t, `{"enabled": true, "absolute_git_hook_path": true}`)

	var stdout bytes.Buffer
	if err := runDisable(context.Background(), &stdout, true); err != nil {
		t.Fatalf("runDisable(project=true) error = %v", err)
	}

	projectContent, err := os.ReadFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to read project settings: %v", err)
	}
	if !strings.Contains(string(projectContent), `"enabled":false`) && !strings.Contains(string(projectContent), `"enabled": false`) {
		t.Errorf("project settings should have enabled:false, got: %s", projectContent)
	}
	if strings.Contains(string(projectContent), "absolute_git_hook_path") {
		t.Errorf("project settings must not leak local-only override absolute_git_hook_path, got: %s", projectContent)
	}

	localContent, err := os.ReadFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("failed to read local settings: %v", err)
	}
	if !strings.Contains(string(localContent), `"enabled":false`) && !strings.Contains(string(localContent), `"enabled": false`) {
		t.Errorf("local settings should be synced to enabled:false, got: %s", localContent)
	}
}

// TestRunEnable_ProjectFlag_DoesNotLeakLocalOverrides verifies that
// `entire enable --project` with a local-only override present (e.g.
// absolute_git_hook_path, set via settings.local.json) does not write that override into
// the shared, committed project settings.json — only the enabled flag should
// change there (runEnable must not round-trip the merged settings view
// through the project file).
func TestRunEnable_ProjectFlag_DoesNotLeakLocalOverrides(t *testing.T) {
	setupTestDir(t)
	writeSettings(t, testSettingsDisabled)
	writeLocalSettings(t, `{"enabled": true, "absolute_git_hook_path": true}`)

	var buf bytes.Buffer
	if err := runEnable(context.Background(), &buf, true); err != nil {
		t.Fatalf("runEnable(project=true) error = %v", err)
	}

	// The merged view is correctly enabled.
	enabled, err := IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
	}
	if !enabled {
		t.Error("expected enabled after runEnable --project")
	}

	// The project file must be flipped to enabled, and must NOT gain the
	// local-only override.
	projectContent, err := os.ReadFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to read project settings: %v", err)
	}
	if !strings.Contains(string(projectContent), `"enabled":true`) && !strings.Contains(string(projectContent), `"enabled": true`) {
		t.Errorf("project settings should have enabled:true, got: %s", projectContent)
	}
	if strings.Contains(string(projectContent), "absolute_git_hook_path") {
		t.Errorf("project settings must not leak local-only override absolute_git_hook_path, got: %s", projectContent)
	}

	// The local file's own override must be preserved untouched.
	localContent, err := os.ReadFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("failed to read local settings: %v", err)
	}
	if !strings.Contains(string(localContent), "absolute_git_hook_path") {
		t.Errorf("local settings should still contain absolute_git_hook_path override, got: %s", localContent)
	}
}

// TestRunEnable_LocalScope_PreservesLocalOnlyFields verifies that `entire
// enable` (default, no --project) with an existing local-only override only
// flips the enabled flag in settings.local.json and leaves the rest of that
// file's own content (like absolute_git_hook_path) intact.
func TestRunEnable_LocalScope_PreservesLocalOnlyFields(t *testing.T) {
	setupTestDir(t)
	writeSettings(t, testSettingsEnabled)
	writeLocalSettings(t, `{"enabled": false, "absolute_git_hook_path": true}`)

	var buf bytes.Buffer
	if err := runEnable(context.Background(), &buf, false); err != nil {
		t.Fatalf("runEnable(project=false) error = %v", err)
	}

	enabled, err := IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
	}
	if !enabled {
		t.Error("expected enabled after runEnable")
	}

	localContent, err := os.ReadFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("failed to read local settings: %v", err)
	}
	if !strings.Contains(string(localContent), `"enabled":true`) && !strings.Contains(string(localContent), `"enabled": true`) {
		t.Errorf("local settings should have enabled:true, got: %s", localContent)
	}
	if !strings.Contains(string(localContent), "absolute_git_hook_path") {
		t.Errorf("local settings should still contain absolute_git_hook_path override, got: %s", localContent)
	}

	// Project settings must be untouched by the local-scope write.
	projectContent, err := os.ReadFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to read project settings: %v", err)
	}
	if strings.Contains(string(projectContent), "absolute_git_hook_path") {
		t.Errorf("project settings must not gain local-only override absolute_git_hook_path, got: %s", projectContent)
	}
}

func TestDetermineSettingsTarget_ExplicitLocalFlag(t *testing.T) {
	tmpDir := t.TempDir()

	// Create settings.json
	settingsPath := filepath.Join(tmpDir, paths.SettingsFileName)
	if err := os.WriteFile(settingsPath, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("Failed to create settings file: %v", err)
	}

	// With --local flag, should always use local
	useLocal, showNotification := determineSettingsTarget(tmpDir, true, false)
	if !useLocal {
		t.Error("determineSettingsTarget() should return useLocal=true with --local flag")
	}
	if showNotification {
		t.Error("determineSettingsTarget() should not show notification with explicit --local flag")
	}
}

func TestDetermineSettingsTarget_ExplicitProjectFlag(t *testing.T) {
	tmpDir := t.TempDir()

	// Create settings.json
	settingsPath := filepath.Join(tmpDir, paths.SettingsFileName)
	if err := os.WriteFile(settingsPath, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("Failed to create settings file: %v", err)
	}

	// With --project flag, should always use project
	useLocal, showNotification := determineSettingsTarget(tmpDir, false, true)
	if useLocal {
		t.Error("determineSettingsTarget() should return useLocal=false with --project flag")
	}
	if showNotification {
		t.Error("determineSettingsTarget() should not show notification with explicit --project flag")
	}
}

func TestDetermineSettingsTarget_SettingsExists_NoFlags(t *testing.T) {
	tmpDir := t.TempDir()

	// Create settings.json
	settingsPath := filepath.Join(tmpDir, paths.SettingsFileName)
	if err := os.WriteFile(settingsPath, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("Failed to create settings file: %v", err)
	}

	// Without flags, should auto-redirect to local with notification
	useLocal, showNotification := determineSettingsTarget(tmpDir, false, false)
	if !useLocal {
		t.Error("determineSettingsTarget() should return useLocal=true when settings.json exists")
	}
	if !showNotification {
		t.Error("determineSettingsTarget() should show notification when auto-redirecting to local")
	}
}

func TestDetermineSettingsTarget_SettingsNotExists_NoFlags(t *testing.T) {
	tmpDir := t.TempDir()

	// No settings.json exists

	// Should use project settings (create new)
	useLocal, showNotification := determineSettingsTarget(tmpDir, false, false)
	if useLocal {
		t.Error("determineSettingsTarget() should return useLocal=false when settings.json doesn't exist")
	}
	if showNotification {
		t.Error("determineSettingsTarget() should not show notification when creating new settings")
	}
}

// Tests for runUninstall and helper functions

func TestRunUninstall_Force_NothingInstalled(t *testing.T) {
	setupTestRepo(t)

	var stdout, stderr bytes.Buffer
	err := runUninstall(context.Background(), &stdout, &stderr, true)
	if err != nil {
		t.Fatalf("runUninstall() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "not installed") {
		t.Errorf("Expected output to indicate nothing installed, got: %s", output)
	}
}

func TestRunUninstall_Force_RemovesEntireDirectory(t *testing.T) {
	setupTestRepo(t)

	// Create .entire directory with settings
	writeSettings(t, testSettingsEnabled)

	// Verify directory exists
	entireDir := paths.EntireDir
	if _, err := os.Stat(entireDir); os.IsNotExist(err) {
		t.Fatal(".entire directory should exist before uninstall")
	}

	var stdout, stderr bytes.Buffer
	err := runUninstall(context.Background(), &stdout, &stderr, true)
	if err != nil {
		t.Fatalf("runUninstall() error = %v", err)
	}

	// Verify directory is removed
	if _, err := os.Stat(entireDir); !os.IsNotExist(err) {
		t.Error(".entire directory should be removed after uninstall")
	}

	output := stdout.String()
	if !strings.Contains(output, "has been removed from this repository") {
		t.Errorf("Expected success message, got: %s", output)
	}
}

func TestRunUninstall_Force_RemovesGitHooks(t *testing.T) {
	setupTestRepo(t)

	// Create .entire directory (required for git hooks)
	writeSettings(t, testSettingsEnabled)

	// Install git hooks
	if _, err := strategy.InstallGitHook(context.Background(), true, false); err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}

	// Verify hooks are installed
	if !strategy.IsGitHookInstalled(context.Background()) {
		t.Fatal("git hooks should be installed before uninstall")
	}

	var stdout, stderr bytes.Buffer
	err := runUninstall(context.Background(), &stdout, &stderr, true)
	if err != nil {
		t.Fatalf("runUninstall() error = %v", err)
	}

	// Verify hooks are removed
	if strategy.IsGitHookInstalled(context.Background()) {
		t.Error("git hooks should be removed after uninstall")
	}

	output := stdout.String()
	if !strings.Contains(output, "Removed git hooks") {
		t.Errorf("Expected output to mention removed git hooks, got: %s", output)
	}
}

// installExternalAgentPluginForUninstall prepares a repo whose $PATH carries a
// mock external agent plugin reporting its hooks as installed, with
// external_agents enabled — the state `entire agent add <plugin>` leaves behind,
// and the only state in which uninstall has an external agent to deal with.
//
// setupTestRepo scrubs $PATH down to git and sh, so a real entire-agent-* binary
// on the developer's machine cannot leak in.
func installExternalAgentPluginForUninstall(t *testing.T, agentName string, hooksInstalled bool) {
	t.Helper()

	// The mock is a #!/bin/sh script.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	setupTestRepo(t)
	writeSettings(t, `{"enabled":true,"external_agents":true}`)

	externalDir := t.TempDir()
	writeExternalAgentBinaryEx(t, externalDir, agentName, hooksInstalled)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestRunUninstall_RemovesExternalAgentHooks pins that `entire disable
// --uninstall` actually uninstalls an external agent's hooks. The uninstall
// path reaches the plugin only through the registry, so without discovery the
// plugin is invisible: its UninstallHooks is never called and its hooks survive
// an uninstall that reports success.
func TestRunUninstall_RemovesExternalAgentHooks(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	const agentName = "ext-uninstall-test"
	installExternalAgentPluginForUninstall(t, agentName, true)

	execLog := filepath.Join(t.TempDir(), "exec.log")
	t.Setenv("ENTIRE_TEST_EXEC_LOG", execLog)

	var stdout, stderr bytes.Buffer
	if err := runUninstall(context.Background(), &stdout, &stderr, true); err != nil {
		t.Fatalf("runUninstall() error = %v\nstderr: %s", err, stderr.String())
	}

	data, err := os.ReadFile(execLog)
	if err != nil {
		t.Fatalf("reading exec log: %v (uninstall never executed the plugin)", err)
	}
	if !strings.Contains(string(data), "uninstall-hooks") {
		t.Errorf("uninstall must invoke the external plugin's uninstall-hooks, exec log:\n%s\nstdout:\n%s", data, stdout.String())
	}
	// Every are-hooks-installed is a subprocess. One uninstall needs one answer:
	// the confirmation summary's detection is reused for the removal decision.
	if got := strings.Count(string(data), "are-hooks-installed"); got != 1 {
		t.Errorf("plugin queried for installed hooks %d times, want 1, exec log:\n%s", got, data)
	}
}

// TestRunUninstall_SummaryNamesExternalAgent pins the other half: the
// confirmation summary must name the external agent whose hooks are about to be
// removed, or the user approves an uninstall whose scope they cannot see.
func TestRunUninstall_SummaryNamesExternalAgent(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	const agentName = "ext-uninstall-summary-test"
	installExternalAgentPluginForUninstall(t, agentName, true)

	// force=false prints the summary, then stops at the no-terminal guard: a
	// `go test` run never counts as interactive. Asserting that specific stop
	// keeps the test from passing on an early return that never reached the
	// summary at all.
	var stdout, stderr bytes.Buffer
	err := runUninstall(context.Background(), &stdout, &stderr, false)
	if err == nil {
		t.Fatalf("expected the no-terminal confirmation guard to stop the uninstall, stdout:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Re-run with --force") {
		t.Errorf("expected the guard to point at --force, stderr:\n%s", stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "agent hooks") {
		t.Fatalf("expected the summary to list agent hooks, got:\n%s", out)
	}
	if !strings.Contains(out, agentName) {
		t.Errorf("summary must name the external agent %q whose hooks will be removed, got:\n%s", agentName, out)
	}
}

// TestRunUninstall_SkipsUnenabledExternalAgent pins that uninstall leaves a
// plugin alone when it reports no hooks installed. Discovery registers every
// entire-agent-* binary on $PATH, not just the one `entire agent add` chose, so
// calling the mutating uninstall-hooks on all of them runs a write command
// against plugins the user never enabled.
func TestRunUninstall_SkipsUnenabledExternalAgent(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	const agentName = "ext-uninstall-unenabled-test"
	installExternalAgentPluginForUninstall(t, agentName, false)

	execLog := filepath.Join(t.TempDir(), "exec.log")
	t.Setenv("ENTIRE_TEST_EXEC_LOG", execLog)

	var stdout, stderr bytes.Buffer
	if err := runUninstall(context.Background(), &stdout, &stderr, true); err != nil {
		t.Fatalf("runUninstall() error = %v\nstderr: %s", err, stderr.String())
	}

	// A missing log is a pass: it means the plugin was never executed at all.
	data, err := os.ReadFile(execLog)
	if err != nil {
		return
	}
	if strings.Contains(string(data), "uninstall-hooks") {
		t.Errorf("uninstall must not invoke uninstall-hooks on a plugin reporting no hooks installed, exec log:\n%s", data)
	}
}

// TestRunUninstall_ReportsUnremovedExternalAgentHooks pins the failure path
// for a plugin whose uninstall-hooks fails: the run exits non-zero with a
// did-not-complete verdict, and the hooks left behind are named with both
// remedies — re-running the uninstall (discovery is ungated, so a re-run
// reaches the plugin again) and the run-by-hand plugin command, since the
// plugin binary itself may be what is broken.
func TestRunUninstall_ReportsUnremovedExternalAgentHooks(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	const agentName = "ext-uninstall-failure-test"
	installExternalAgentPluginForUninstall(t, agentName, true)
	t.Setenv("ENTIRE_TEST_FAIL_UNINSTALL_HOOKS", "1")

	var stdout, stderr bytes.Buffer
	err := runUninstall(context.Background(), &stdout, &stderr, true)
	if err == nil {
		t.Fatalf("a plugin failure must fail the uninstall, stdout:\n%s", stdout.String())
	}
	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Errorf("expected a SilentError (warnings are already printed), got %T: %v", err, err)
	}

	assertLeftoverPluginReported(t, stderr.String(), agentName, "hooks are still installed")
	if !strings.Contains(stderr.String(), "entire disable --uninstall") {
		t.Errorf("stderr must also point at re-running the uninstall, got:\n%s", stderr.String())
	}

	out := stdout.String()
	if strings.Contains(out, "has been removed from this repository") {
		t.Errorf("uninstall must not report success when a plugin's hooks were not removed, stdout:\n%s", out)
	}
	if !strings.Contains(out, "did not complete") {
		t.Errorf("the closing line must say the uninstall did not complete, stdout:\n%s", out)
	}
}

// TestRunUninstall_EntireDirRemovalFailureFailsTheCommand pins the exit-code
// policy on Entire's own removal steps: a failure in one of them — here
// .entire/ — fails the command. The directory is made unremovable by denying
// writes on a subdirectory, so os.RemoveAll cannot unlink its child.
func TestRunUninstall_EntireDirRemovalFailureFailsTheCommand(t *testing.T) {
	// Cannot use t.Parallel: mutates cwd.
	if runtime.GOOS == windowsGOOS {
		t.Skip("permission-based removal failure is not portable to Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks")
	}

	setupTestRepo(t)
	writeSettings(t, `{"enabled":true}`)
	locked := filepath.Join(paths.EntireDir, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	// Restore before t.TempDir's own cleanup, which fails the test if it cannot
	// delete the tree.
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0o755); err != nil {
			t.Logf("restoring permissions: %v", err)
		}
	})

	var stdout, stderr bytes.Buffer
	err := runUninstall(context.Background(), &stdout, &stderr, true)
	if err == nil {
		t.Fatalf("a failed .entire removal must fail the uninstall, stdout:\n%s", stdout.String())
	}
	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Errorf("expected a SilentError (the message is already printed), got %T: %v", err, err)
	}

	if !strings.Contains(stderr.String(), "failed to remove .entire directory") {
		t.Errorf("the failure must be warned about on stderr, got:\n%s", stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "has been removed from this repository") {
		t.Errorf("uninstall must not report success when its own step failed, stdout:\n%s", out)
	}
	if !strings.Contains(out, "did not complete") {
		t.Errorf("the closing line must say the uninstall did not complete, stdout:\n%s", out)
	}
}

// TestRunUninstall_HalfUninstallRecoversOnRerun pins that a half-completed
// uninstall converges to a clean one. Three steps fail on the first run —
// agent hooks (unreadable config), git hooks and .entire/ (write-denied
// directories) — each printing its own warning while the others still do
// their work. Once the causes are fixed, a re-run removes exactly what
// survived and reports success.
func TestRunUninstall_HalfUninstallRecoversOnRerun(t *testing.T) {
	// Cannot use t.Parallel: mutates cwd and file permissions.
	if runtime.GOOS == windowsGOOS {
		t.Skip("permission-based failures are not portable to Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks")
	}

	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	// Real Claude Code hooks, made unreadable so the sweep cannot check them.
	claudeSettings := filepath.Join(".claude", "settings.json")
	if err := os.MkdirAll(".claude", 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	hookJSON := `{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"entire hooks claude-code stop"}]}]}}`
	if err := os.WriteFile(claudeSettings, []byte(hookJSON), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Chmod(claudeSettings, 0o000); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	// Real git hooks, in a directory that denies their removal.
	if _, err := strategy.InstallGitHook(context.Background(), true, false); err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}
	hooksDir := filepath.Join(".git", "hooks")
	if err := os.Chmod(hooksDir, 0o555); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	// .entire/ with a write-denied subdirectory so RemoveAll cannot clear it.
	locked := filepath.Join(paths.EntireDir, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	// Safety net for early t.Fatal: t.TempDir's cleanup fails the test if it
	// cannot delete the tree.
	t.Cleanup(func() {
		for path, mode := range map[string]os.FileMode{claudeSettings: 0o600, hooksDir: 0o755, locked: 0o755} {
			// A path the re-run already removed is the expected end state.
			if err := os.Chmod(path, mode); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Logf("restoring permissions on %s: %v", path, err)
			}
		}
	})

	var stdout, stderr bytes.Buffer
	err := runUninstall(context.Background(), &stdout, &stderr, true)
	if err == nil {
		t.Fatalf("first run must fail while three steps cannot finish, stdout:\n%s", stdout.String())
	}
	errOut := stderr.String()
	for _, want := range []string{
		"could not check whether Claude Code hooks are installed",
		"failed to remove git hooks",
		"failed to remove .entire directory",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("first run must warn %q, stderr:\n%s", want, errOut)
		}
	}
	if !strings.Contains(stdout.String(), "did not complete") {
		t.Errorf("first run must close with did-not-complete, stdout:\n%s", stdout.String())
	}

	// Fix all three causes; the re-run should finish the job.
	for path, mode := range map[string]os.FileMode{claudeSettings: 0o600, hooksDir: 0o755, locked: 0o755} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("Chmod(%s) error = %v", path, err)
		}
	}

	stdout.Reset()
	stderr.Reset()
	if err := runUninstall(context.Background(), &stdout, &stderr, true); err != nil {
		t.Fatalf("re-run error = %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "has been removed from this repository") {
		t.Errorf("re-run must complete the uninstall, stdout:\n%s", stdout.String())
	}

	// Nothing of Entire's survives.
	data, err := os.ReadFile(claudeSettings)
	if err != nil {
		t.Fatalf("reading claude settings: %v", err)
	}
	if strings.Contains(string(data), "entire hooks") {
		t.Errorf("claude settings must no longer carry Entire hooks, got:\n%s", data)
	}
	if strategy.IsGitHookInstalled(context.Background()) {
		t.Error("git hooks must be removed by the re-run")
	}
	if checkEntireDirExists(context.Background()) {
		t.Error(".entire/ must be removed by the re-run")
	}
}

// TestRunUninstall_RerunAfterPluginFailureFinishesTheJob pins that the
// uninstall is re-runnable after a plugin failure. The first run deletes
// .entire/ — and with it the external_agents setting that normally gates
// discovery — so only uninstall's ungated discovery lets the re-run still see
// the plugin, retry its uninstall-hooks, and finish the job.
func TestRunUninstall_RerunAfterPluginFailureFinishesTheJob(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	const agentName = "ext-uninstall-rerun-test"
	installExternalAgentPluginForUninstall(t, agentName, true)
	t.Setenv("ENTIRE_TEST_FAIL_UNINSTALL_HOOKS", "1")

	var stdout, stderr bytes.Buffer
	if err := runUninstall(context.Background(), &stdout, &stderr, true); err == nil {
		t.Fatalf("first run: a plugin failure must fail the uninstall, stdout:\n%s", stdout.String())
	}
	if checkEntireDirExists(context.Background()) {
		t.Fatal("the .entire/ step is isolated from the plugin failure and must still remove the directory")
	}

	t.Setenv("ENTIRE_TEST_FAIL_UNINSTALL_HOOKS", "")
	stdout.Reset()
	stderr.Reset()
	if err := runUninstall(context.Background(), &stdout, &stderr, true); err != nil {
		t.Fatalf("re-run error = %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "has been removed from this repository") {
		t.Errorf("re-run must reach the plugin without the external_agents gate and complete the uninstall, stdout:\n%s", stdout.String())
	}
}

// TestRunUninstall_UncheckableExternalAgentReported pins the plugin that cannot
// say whether its hooks are installed. Its hooks are not removed — asking a
// plugin that cannot answer to mutate state is not ours to do — so the run must
// say so, name the plugin, and hand over a command the user can run. .entire/ is
// about to go, and with it the setting that makes the plugin discoverable, so
// this run is the only place that can be said. An unknown state is a problem
// too: the run must not report success while a plugin's hooks are in doubt,
// so it exits non-zero.
func TestRunUninstall_UncheckableExternalAgentReported(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	const agentName = "ext-uninstall-unprobeable-test"
	installExternalAgentPluginForUninstall(t, agentName, true)
	t.Setenv("ENTIRE_TEST_PROBE", "fail")

	execLog := filepath.Join(t.TempDir(), "exec.log")
	t.Setenv("ENTIRE_TEST_EXEC_LOG", execLog)

	var stdout, stderr bytes.Buffer
	err := runUninstall(context.Background(), &stdout, &stderr, true)
	if err == nil {
		t.Fatalf("an unprobeable plugin must fail the uninstall, stdout:\n%s", stdout.String())
	}
	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Errorf("expected a SilentError, got %T: %v", err, err)
	}
	if !strings.Contains(stdout.String(), "did not complete") {
		t.Errorf("the closing line must say the uninstall did not complete, stdout:\n%s", stdout.String())
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "could not check whether") {
		t.Errorf("stderr must say the plugin could not be checked, got:\n%s", errOut)
	}
	if !strings.Contains(errOut, "mock probe failure") {
		t.Errorf("stderr must carry the probe's own error, got:\n%s", errOut)
	}
	assertLeftoverPluginReported(t, errOut, agentName, "hooks may still be installed")

	data, err := os.ReadFile(execLog)
	if err != nil {
		t.Fatalf("reading exec log: %v", err)
	}
	if strings.Contains(string(data), "uninstall-hooks") {
		t.Errorf("a plugin that could not be asked must not be told to uninstall, exec log:\n%s", data)
	}
	// Still one question per uninstall, even when the answer never arrives.
	if got := strings.Count(string(data), "are-hooks-installed"); got != 1 {
		t.Errorf("plugin queried for installed hooks %d times, want 1, exec log:\n%s", got, data)
	}
}

// TestRunUninstall_UncheckableExternalAgentGarbageJSON pins that a plugin
// printing junk is classified the same as one that crashes. Both mean we do not
// know, and only a clean answer means there is nothing to remove.
func TestRunUninstall_UncheckableExternalAgentGarbageJSON(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	const agentName = "ext-uninstall-garbage-probe-test"
	installExternalAgentPluginForUninstall(t, agentName, true)
	t.Setenv("ENTIRE_TEST_PROBE", "garbage")

	var stdout, stderr bytes.Buffer
	if err := runUninstall(context.Background(), &stdout, &stderr, true); err == nil {
		t.Fatalf("a plugin printing junk must fail the uninstall like one that crashes, stdout:\n%s", stdout.String())
	}

	if !strings.Contains(stderr.String(), "could not check whether") {
		t.Errorf("malformed JSON must be reported as unchecked, not treated as no hooks, stderr:\n%s", stderr.String())
	}
}

// TestRunUninstall_SummaryNamesUncheckableExternalAgent pins that the user
// approving the uninstall is told the plugin's state is unknown, on its own line:
// listing it as having hooks installed would assert the thing we could not find
// out, and omitting it hides part of the scope they are approving.
func TestRunUninstall_SummaryNamesUncheckableExternalAgent(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	const agentName = "ext-uninstall-unprobeable-summary-test"
	installExternalAgentPluginForUninstall(t, agentName, true)
	t.Setenv("ENTIRE_TEST_PROBE", "fail")

	var stdout, stderr bytes.Buffer
	if err := runUninstall(context.Background(), &stdout, &stderr, false); err == nil {
		t.Fatalf("expected the no-terminal confirmation guard to stop the uninstall, stdout:\n%s", stdout.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "could not be checked") {
		t.Fatalf("summary must list the plugin as unchecked, got:\n%s", out)
	}
	if !strings.Contains(out, agentName) {
		t.Errorf("summary must name the plugin %q, got:\n%s", agentName, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "agent hooks") && strings.Contains(line, agentName) {
			t.Errorf("an unchecked plugin must not be listed as having hooks installed, got:\n%s", out)
		}
	}
}

// TestGetAgentHookState_CancelledContextIsNotAPluginFault pins that our own
// cancellation is not reported as a plugin that could not answer. Every plugin on
// $PATH answers over a subprocess, so one Ctrl-C would otherwise diagnose all of
// them at once. Driven at the sweep rather than through runUninstall, which bails
// on a dead context long before it classifies anything.
func TestGetAgentHookState_CancelledContextIsNotAPluginFault(t *testing.T) {
	// Cannot use t.Parallel: mutates $PATH, cwd, and the agent registry.
	const agentName = "ext-hook-state-cancelled-test"
	installExternalAgentPluginForUninstall(t, agentName, true)
	external.DiscoverAndRegister(context.Background())

	// Registered under a live context, probed under a dead one: the probe fails
	// for a reason that has nothing to do with the plugin.
	live := getAgentHookState(context.Background())
	if len(live.installed) == 0 {
		t.Fatalf("plugin was not registered, so the cancelled case below would prove nothing")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	state := getAgentHookState(ctx)
	if len(state.unchecked) != 0 {
		t.Errorf("cancellation must not be charged to the plugin, got unchecked = %v", state.uncheckedNames())
	}
}

// TestPluginUninstallCommand_PerOS pins the recovery command's shape on both
// shell families. It is printed when it is the user's last chance to remove a
// plugin's hooks, so it must run in the shell they actually have: the POSIX
// `cd x && VAR=y bin` form is a syntax error in PowerShell, and would hand a
// Windows user a command that cannot run.
func TestPluginUninstallCommand_PerOS(t *testing.T) {
	t.Parallel()

	const name = types.AgentName("ext-cmd-test")

	posix := pluginUninstallCommandFor("linux", "/My Repo/it's here", name)
	wantPosix := `cd '/My Repo/it'\''s here' && ENTIRE_REPO_ROOT='/My Repo/it'\''s here' ` +
		fmt.Sprintf("ENTIRE_PROTOCOL_VERSION=%d entire-agent-%s uninstall-hooks", external.ProtocolVersion, name)
	if posix != wantPosix {
		t.Errorf("posix command =\n%s\nwant\n%s", posix, wantPosix)
	}

	win := pluginUninstallCommandFor("windows", `C:\My Repo\it's here`, name)
	wantWin := `cd 'C:\My Repo\it''s here'; $env:ENTIRE_REPO_ROOT = 'C:\My Repo\it''s here'; ` +
		fmt.Sprintf("$env:ENTIRE_PROTOCOL_VERSION = '%d'; entire-agent-%s uninstall-hooks", external.ProtocolVersion, name)
	if win != wantWin {
		t.Errorf("windows command =\n%s\nwant\n%s", win, wantWin)
	}
}

// assertLeftoverPluginReported checks the shape every leftover-hooks report
// shares on stderr: the plugin named, what state its hooks are in, and a
// runnable recovery command. The closing stdout verdict is each caller's own
// assertion.
func assertLeftoverPluginReported(t *testing.T, stderr, agentName, lead string) {
	t.Helper()

	if !strings.Contains(stderr, agentName) {
		t.Errorf("stderr must name the plugin whose hooks may survive, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, lead) {
		t.Errorf("stderr must say %q, got:\n%s", lead, stderr)
	}
	if !strings.Contains(stderr, "entire-agent-"+agentName+" uninstall-hooks") {
		t.Errorf("stderr must say how to remove the hooks, got:\n%s", stderr)
	}
	// The protocol guarantees every subcommand these, so a bare invocation is a
	// command a conforming plugin may reject — useless as the user's last resort.
	for _, want := range []string{"ENTIRE_REPO_ROOT=", "ENTIRE_PROTOCOL_VERSION="} {
		if !strings.Contains(stderr, want) {
			t.Errorf("recovery command must carry %s, got:\n%s", want, stderr)
		}
	}
}

// fakeBuiltinHookAgent is a built-in (non-external) hook-supporting agent whose
// UninstallHooks is observable. It embeds vogon so the Agent surface stays
// whatever the interface requires, and overrides only what these tests turn on.
type fakeBuiltinHookAgent struct {
	*vogon.Agent

	name           types.AgentName
	installed      bool
	uninstallCalls *int
	uninstallErr   error
	checkErr       error
}

func (f *fakeBuiltinHookAgent) Name() types.AgentName { return f.name }
func (f *fakeBuiltinHookAgent) Type() types.AgentType { return types.AgentType(f.name) }

// AreHooksInstalled reports the configured answer: installed for tests about a
// detected built-in whose removal then fails, checkErr for tests about a
// built-in that could not read its own config, and a plain false for tests
// about a built-in the installed-hooks sweep did not pick up.
func (f *fakeBuiltinHookAgent) AreHooksInstalled(context.Context) (bool, error) {
	return f.installed, f.checkErr
}

func (f *fakeBuiltinHookAgent) UninstallHooks(context.Context) error {
	*f.uninstallCalls++
	return f.uninstallErr
}

// registerFakeBuiltinHookAgent registers a built-in hook-supporting agent whose
// hooks report as not installed, and returns its UninstallHooks call count.
func registerFakeBuiltinHookAgent(t *testing.T, name types.AgentName, uninstallErr error) *int {
	t.Helper()
	return registerFakeBuiltinHookAgentEx(t, name, false, uninstallErr, nil)
}

func registerFakeBuiltinHookAgentEx(t *testing.T, name types.AgentName, installed bool, uninstallErr, checkErr error) *int {
	t.Helper()

	setupTestRepo(t)
	writeSettings(t, `{"enabled":true}`)
	t.Cleanup(agent.SnapshotRegistryForTesting())

	calls := 0
	agent.Register(name, func() agent.Agent {
		return &fakeBuiltinHookAgent{
			Agent:          &vogon.Agent{},
			name:           name,
			installed:      installed,
			uninstallCalls: &calls,
			uninstallErr:   uninstallErr,
			checkErr:       checkErr,
		}
	})
	return &calls
}

// TestRunUninstall_BuiltinNotDetectedIsLeftAlone pins that uninstall acts on
// exactly the sweep's answer: an agent that cleanly reported no hooks is in
// neither the installed nor the unchecked set, so its UninstallHooks is never
// called — the removal worklist is what the confirmation summary showed, not
// the whole registry.
func TestRunUninstall_BuiltinNotDetectedIsLeftAlone(t *testing.T) {
	// Cannot use t.Parallel: mutates cwd and the agent registry.
	calls := registerFakeBuiltinHookAgent(t, "fake-builtin-undetected", nil)

	var stdout, stderr bytes.Buffer
	if err := runUninstall(context.Background(), &stdout, &stderr, true); err != nil {
		t.Fatalf("runUninstall() error = %v\nstderr: %s", err, stderr.String())
	}

	if *calls != 0 {
		t.Errorf("a built-in that cleanly reported no hooks must not have UninstallHooks called, stdout:\n%s", stdout.String())
	}
}

// TestRunUninstall_BuiltinHookFailureFailsTheCommand pins that a built-in whose
// removal failed fails the uninstall. A built-in is Entire's own code: its
// removal failing means the uninstall did not do its job, and a success
// verdict would assert the opposite of the warning just printed.
func TestRunUninstall_BuiltinHookFailureFailsTheCommand(t *testing.T) {
	// Cannot use t.Parallel: mutates cwd and the agent registry.
	const agentName types.AgentName = "fake-builtin-failing"
	registerFakeBuiltinHookAgentEx(t, agentName, true, errors.New("mock builtin uninstall failure"), nil)

	var stdout, stderr bytes.Buffer
	err := runUninstall(context.Background(), &stdout, &stderr, true)
	if err == nil {
		t.Fatalf("a built-in hook removal failure must fail the uninstall, stdout:\n%s", stdout.String())
	}
	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Errorf("expected a SilentError (the message is already printed), got %T: %v", err, err)
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "failed to remove agent hooks") || !strings.Contains(errOut, string(agentName)) {
		t.Errorf("the failure must be warned about on stderr, naming the agent, got:\n%s", errOut)
	}

	out := stdout.String()
	if strings.Contains(out, "has been removed from this repository") {
		t.Errorf("uninstall must not report success when a built-in's hooks were not removed, stdout:\n%s", out)
	}
	if !strings.Contains(out, "did not complete") {
		t.Errorf("the closing line must say the uninstall did not complete, stdout:\n%s", out)
	}
}

// TestRunUninstall_UncheckableBuiltinFailsTheCommand pins the built-in that
// could not read its own config, e.g. a malformed or unreadable hooks.json.
// The reason is reported, no removal is attempted (an unverifiable check means
// the removal would read the same broken file), and no plugin command is
// offered — but the command must not report success, and the exit code has to
// say so.
func TestRunUninstall_UncheckableBuiltinFailsTheCommand(t *testing.T) {
	// Cannot use t.Parallel: mutates cwd and the agent registry.
	const agentName types.AgentName = "fake-builtin-unreadable"
	calls := registerFakeBuiltinHookAgentEx(t, agentName, false, nil, errors.New("parse hooks.json: unexpected end of input"))

	var stdout, stderr bytes.Buffer
	err := runUninstall(context.Background(), &stdout, &stderr, true)
	if err == nil {
		t.Fatalf("an unchecked built-in must fail the uninstall, stdout:\n%s", stdout.String())
	}
	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Errorf("expected a SilentError (the message is already printed), got %T: %v", err, err)
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "could not check whether") || !strings.Contains(errOut, "parse hooks.json") {
		t.Errorf("the reason must be reported, stderr:\n%s", errOut)
	}
	if strings.Contains(errOut, "entire-agent-"+string(agentName)) {
		t.Errorf("a built-in must not be handed a plugin command, stderr:\n%s", errOut)
	}
	if !strings.Contains(errOut, "may or may not remain") || !strings.Contains(errOut, string(agentName)) {
		t.Errorf("the warning must name the unverified agent, stderr:\n%s", errOut)
	}
	if *calls != 0 {
		t.Error("a built-in whose check failed must not be asked to uninstall")
	}

	out := stdout.String()
	if strings.Contains(out, "has been removed from this repository") {
		t.Errorf("uninstall must not report success while a built-in's hooks are unverified, stdout:\n%s", out)
	}
	if !strings.Contains(out, "did not complete") {
		t.Errorf("the closing line must say the uninstall did not complete, stdout:\n%s", out)
	}
}

// TestRunUninstall_UncheckedBuiltinAloneIsNotNotInstalled pins the second run
// of the unreadable-config repro: everything else already removed, the only
// trace left is a built-in whose config cannot be read. "Not installed" would
// assert the one thing the failed check makes unknowable, and would return
// before the warning that surfaces the reason.
func TestRunUninstall_UncheckedBuiltinAloneIsNotNotInstalled(t *testing.T) {
	// Cannot use t.Parallel: mutates cwd and the agent registry.
	const agentName types.AgentName = "fake-builtin-unreadable-only"
	registerFakeBuiltinHookAgentEx(t, agentName, false, nil, errors.New("read settings.json: permission denied"))
	// The helper writes .entire/settings.json; remove it so the unchecked
	// built-in is the only thing left, as after a first partial uninstall.
	if err := os.RemoveAll(paths.EntireDir); err != nil {
		t.Fatalf("removing .entire: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err := runUninstall(context.Background(), &stdout, &stderr, true)
	if err == nil {
		t.Fatalf("an unchecked built-in as the only leftover must fail the uninstall, stdout:\n%s", stdout.String())
	}

	out := stdout.String()
	if strings.Contains(out, "not installed in this repository") {
		t.Errorf("must not claim Entire is not installed when a built-in could not be checked, stdout:\n%s", out)
	}
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Errorf("the unreadable-config reason must reach the user, stderr:\n%s", stderr.String())
	}
}

// cancellingHookAgent is a built-in whose UninstallHooks cancels the run's
// context before returning, simulating Ctrl-C arriving mid-removal.
type cancellingHookAgent struct {
	*fakeBuiltinHookAgent

	cancel context.CancelFunc
}

func (c *cancellingHookAgent) UninstallHooks(ctx context.Context) error {
	*c.uninstallCalls++
	c.cancel()
	return ctx.Err()
}

// TestRunUninstall_CancellationIsNotAnAgentFault pins that a ctx cancelled
// mid-removal (Ctrl-C) is reported as an interruption, not charged to the
// agent whose removal it killed: no failure warning naming the agent, no
// recovery command asserting its hooks are still installed — just the
// interruption notice and a did-not-complete verdict, since a re-run finishes
// the job.
func TestRunUninstall_CancellationIsNotAnAgentFault(t *testing.T) {
	// Cannot use t.Parallel: mutates cwd and the agent registry.
	const agentName types.AgentName = "fake-builtin-cancelling"
	setupTestRepo(t)
	writeSettings(t, `{"enabled":true}`)
	t.Cleanup(agent.SnapshotRegistryForTesting())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	agent.Register(agentName, func() agent.Agent {
		return &cancellingHookAgent{
			fakeBuiltinHookAgent: &fakeBuiltinHookAgent{
				Agent:          &vogon.Agent{},
				name:           agentName,
				installed:      true,
				uninstallCalls: &calls,
			},
			cancel: cancel,
		}
	})

	var stdout, stderr bytes.Buffer
	err := runUninstall(ctx, &stdout, &stderr, true)
	if err == nil {
		t.Fatalf("an interrupted uninstall must not report success, stdout:\n%s", stdout.String())
	}
	if calls != 1 {
		t.Errorf("UninstallHooks calls = %d, want 1", calls)
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "interrupted while removing agent hooks") {
		t.Errorf("stderr must report the interruption, got:\n%s", errOut)
	}
	if strings.Contains(errOut, "failed to remove agent hooks") {
		t.Errorf("cancellation must not be reported as an agent failure, stderr:\n%s", errOut)
	}
	if strings.Contains(errOut, "uninstall-hooks") {
		t.Errorf("cancellation must not print a plugin recovery command, stderr:\n%s", errOut)
	}

	out := stdout.String()
	if strings.Contains(out, "has been removed from this repository") {
		t.Errorf("an interrupted uninstall must not close with the success verdict, stdout:\n%s", out)
	}
	if !strings.Contains(out, "did not complete") {
		t.Errorf("the closing line must say the uninstall did not complete, stdout:\n%s", out)
	}
}

func TestRunUninstall_NotAGitRepo(t *testing.T) {
	// Create a temp directory without git init
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	var stdout, stderr bytes.Buffer
	err := runUninstall(context.Background(), &stdout, &stderr, true)

	// Should return an error (silent error)
	if err == nil {
		t.Fatal("runUninstall() should return error for non-git directory")
	}

	// Should print message to stderr
	errOutput := stderr.String()
	if !strings.Contains(errOutput, "Not a git repository") {
		t.Errorf("Expected error message about not being a git repo, got: %s", errOutput)
	}
}

func TestCheckEntireDirExists(t *testing.T) {
	setupTestDir(t)

	// Should be false when directory doesn't exist
	if checkEntireDirExists(context.Background()) {
		t.Error("checkEntireDirExists(context.Background()) should return false when .entire doesn't exist")
	}

	// Create the directory
	if err := os.MkdirAll(paths.EntireDir, 0o755); err != nil {
		t.Fatalf("Failed to create .entire dir: %v", err)
	}

	// Should be true now
	if !checkEntireDirExists(context.Background()) {
		t.Error("checkEntireDirExists(context.Background()) should return true when .entire exists")
	}
}

func TestCountSessionStates(t *testing.T) {
	setupTestRepo(t)

	// Should be 0 when no session states exist
	count := countSessionStates(context.Background())
	if count != 0 {
		t.Errorf("countSessionStates(context.Background()) = %d, want 0", count)
	}
}

func TestCountShadowBranches(t *testing.T) {
	setupTestRepo(t)

	// Should be 0 when no shadow branches exist
	count := countShadowBranches(context.Background())
	if count != 0 {
		t.Errorf("countShadowBranches(context.Background()) = %d, want 0", count)
	}
}

func TestRemoveEntireDirectory(t *testing.T) {
	setupTestDir(t)

	// Create .entire directory with some files
	entireDir := paths.EntireDir
	if err := os.MkdirAll(filepath.Join(entireDir, "subdir"), 0o755); err != nil {
		t.Fatalf("Failed to create .entire/subdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "test.txt"), []byte("test"), 0o644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Remove the directory
	if err := removeEntireDirectory(context.Background()); err != nil {
		t.Fatalf("removeEntireDirectory(context.Background()) error = %v", err)
	}

	// Verify it's removed
	if _, err := os.Stat(entireDir); !os.IsNotExist(err) {
		t.Error(".entire directory should be removed")
	}
}

func TestShellCompletionTarget(t *testing.T) {
	tests := []struct {
		name             string
		shell            string
		createBashProf   bool
		wantShell        string
		wantRCBase       string // basename of rc file
		wantCompletion   string
		wantErrUnsupport bool
	}{
		{
			name:           "zsh",
			shell:          "/bin/zsh",
			wantShell:      "Zsh",
			wantRCBase:     ".zshrc",
			wantCompletion: "autoload -Uz compinit && compinit && source <(entire completion zsh)",
		},
		{
			name:           "bash_no_profile",
			shell:          "/bin/bash",
			wantShell:      "Bash",
			wantRCBase:     ".bashrc",
			wantCompletion: "source <(entire completion bash)",
		},
		{
			name:           "bash_with_profile",
			shell:          "/bin/bash",
			createBashProf: true,
			wantShell:      "Bash",
			wantRCBase:     ".bash_profile",
			wantCompletion: "source <(entire completion bash)",
		},
		{
			name:           "fish",
			shell:          "/usr/bin/fish",
			wantShell:      "Fish",
			wantRCBase:     filepath.Join(".config", "fish", "config.fish"),
			wantCompletion: "entire completion fish | source",
		},
		{
			name:             "empty_shell",
			shell:            "",
			wantErrUnsupport: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SHELL", tt.shell)

			if tt.createBashProf {
				if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(""), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			shellName, rcFile, completion, err := shellCompletionTarget()

			if tt.wantErrUnsupport {
				if !errors.Is(err, errUnsupportedShell) {
					t.Fatalf("got err=%v, want errUnsupportedShell", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if shellName != tt.wantShell {
				t.Errorf("shellName = %q, want %q", shellName, tt.wantShell)
			}
			wantRC := filepath.Join(home, tt.wantRCBase)
			if rcFile != wantRC {
				t.Errorf("rcFile = %q, want %q", rcFile, wantRC)
			}
			if completion != tt.wantCompletion {
				t.Errorf("completion = %q, want %q", completion, tt.wantCompletion)
			}
		})
	}
}

func TestAppendShellCompletion(t *testing.T) {
	tests := []struct {
		name           string
		rcFileRelPath  string
		completionLine string
		preExisting    string // existing content in rc file; empty means file doesn't exist
		createParent   bool   // whether parent dir already exists
	}{
		{
			name:           "zsh_new_file",
			rcFileRelPath:  ".zshrc",
			completionLine: "source <(entire completion zsh)",
			createParent:   true,
		},
		{
			name:           "zsh_existing_file",
			rcFileRelPath:  ".zshrc",
			completionLine: "source <(entire completion zsh)",
			preExisting:    "# existing zshrc content\n",
			createParent:   true,
		},
		{
			name:           "fish_no_parent_dir",
			rcFileRelPath:  filepath.Join(".config", "fish", "config.fish"),
			completionLine: "entire completion fish | source",
			createParent:   false,
		},
		{
			name:           "fish_existing_dir",
			rcFileRelPath:  filepath.Join(".config", "fish", "config.fish"),
			completionLine: "entire completion fish | source",
			createParent:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			rcFile := filepath.Join(home, tt.rcFileRelPath)

			if tt.createParent {
				if err := os.MkdirAll(filepath.Dir(rcFile), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tt.preExisting != "" {
				if err := os.WriteFile(rcFile, []byte(tt.preExisting), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			if err := appendShellCompletion(rcFile, tt.completionLine); err != nil {
				t.Fatalf("appendShellCompletion() error: %v", err)
			}

			// Verify the file was created and contains the completion line.
			data, err := os.ReadFile(rcFile)
			if err != nil {
				t.Fatalf("reading rc file: %v", err)
			}
			content := string(data)

			if !strings.Contains(content, shellCompletionComment) {
				t.Errorf("rc file missing comment %q", shellCompletionComment)
			}
			if !strings.Contains(content, tt.completionLine) {
				t.Errorf("rc file missing completion line %q", tt.completionLine)
			}
			if tt.preExisting != "" && !strings.HasPrefix(content, tt.preExisting) {
				t.Errorf("pre-existing content was overwritten")
			}

			// Verify parent directory permissions.
			info, err := os.Stat(filepath.Dir(rcFile))
			if err != nil {
				t.Fatalf("stat parent dir: %v", err)
			}
			if !info.IsDir() {
				t.Fatal("parent path is not a directory")
			}
		})
	}
}

func TestRemoveEntireDirectory_NotExists(t *testing.T) {
	setupTestDir(t)

	// Should not error when directory doesn't exist
	if err := removeEntireDirectory(context.Background()); err != nil {
		t.Fatalf("removeEntireDirectory(context.Background()) should not error when directory doesn't exist: %v", err)
	}
}

func TestPrintMissingAgentError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	printMissingAgentError(&buf)
	output := buf.String()

	if !strings.Contains(output, "Missing agent name") {
		t.Error("expected 'Missing agent name' in output")
	}
	for _, a := range agent.List() {
		if !strings.Contains(output, string(a)) {
			t.Errorf("expected agent %q listed in output", a)
		}
	}
	if !strings.Contains(output, "(default)") {
		t.Error("expected default annotation in output")
	}
	if !strings.Contains(output, "Usage: entire enable --agent") {
		t.Error("expected usage line in output")
	}
}

func TestPrintWrongAgentError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	printWrongAgentError(&buf, "not-an-agent")
	output := buf.String()

	if !strings.Contains(output, `Unknown agent "not-an-agent"`) {
		t.Error("expected unknown agent name in output")
	}
	for _, a := range agent.List() {
		if !strings.Contains(output, string(a)) {
			t.Errorf("expected agent %q listed in output", a)
		}
	}
	if !strings.Contains(output, "(default)") {
		t.Error("expected default annotation in output")
	}
	if !strings.Contains(output, "Usage: entire enable --agent") {
		t.Error("expected usage line in output")
	}
}

func TestEnableCmd_AgentFlagNoValue(t *testing.T) {
	setupTestRepo(t)

	cmd := newEnableCmd()
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"--agent"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --agent is used without a value")
	}

	output := stderr.String()
	if !strings.Contains(output, "Missing agent name") {
		t.Errorf("expected helpful error message, got: %s", output)
	}
	if !strings.Contains(output, string(agent.DefaultAgentName)) {
		t.Errorf("expected default agent listed, got: %s", output)
	}
	if strings.Contains(output, "flag needs an argument") {
		t.Error("should not contain default cobra/pflag error message")
	}
}

func TestEnableCmd_AgentFlagEmptyValue(t *testing.T) {
	setupTestRepo(t)

	cmd := newEnableCmd()
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"--agent="})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --agent= is used with empty value")
	}

	output := stderr.String()
	if !strings.Contains(output, "Missing agent name") {
		t.Errorf("expected helpful error message, got: %s", output)
	}
	if strings.Contains(output, "flag needs an argument") {
		t.Error("should not contain default cobra/pflag error message")
	}
}

func TestEnableUsesSetupFlow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		args      []string
		agentName string
		want      bool
	}{
		{name: "bare enable", args: nil, want: false},
		{name: "project only", args: []string{"--project"}, want: false},
		{name: "local only", args: []string{"--local"}, want: false},
		{name: "force", args: []string{"--force"}, want: true},
		{name: "absolute hook path", args: []string{"--absolute-git-hook-path"}, want: true},
		{name: "telemetry changed", args: []string{"--telemetry=false"}, want: true},
		{name: "checkpoint remote", args: []string{"--checkpoint-remote", "github:org/repo"}, want: true},
		{name: "skip push sessions", args: []string{"--skip-push-sessions"}, want: true},
		{name: "search skill", args: []string{"--search-skill"}, want: true},
		{name: "agent flag", args: []string{"--agent", "claude-code"}, agentName: "claude-code", want: true},
		{name: "yes flag", args: []string{"--yes"}, want: true},
		{name: "yes short flag", args: []string{"-y"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := newEnableCmd()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tt.args)
			if err := cmd.ParseFlags(tt.args); err != nil {
				t.Fatalf("ParseFlags() error = %v", err)
			}

			if got := enableUsesSetupFlow(cmd, tt.agentName); got != tt.want {
				t.Fatalf("enableUsesSetupFlow(%v, %q) = %v, want %v", tt.args, tt.agentName, got, tt.want)
			}
		})
	}
}

func TestEnableCmd_ForceOnConfiguredRepo_UsesConfigureFlow(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	writeClaudeHooksFixture(t)

	cmd := newEnableCmd()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--force"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("enable --force error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Cannot show agent selection in non-interactive mode.") {
		t.Fatalf("expected enable --force to route to configure flow, got: %s", output)
	}
	if strings.Contains(output, "Entire is already enabled.") {
		t.Fatalf("expected enable --force to avoid the lightweight re-enable path, got: %s", output)
	}
}

func TestEnableCmd_ForceOnConfiguredDisabledRepo_Reenables(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsDisabled)
	writeClaudeHooksFixture(t)

	cmd := newEnableCmd()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--force"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("enable --force error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Cannot show agent selection in non-interactive mode.") {
		t.Fatalf("expected enable --force to route through manage agents before enabling, got: %s", output)
	}
	if !strings.Contains(output, "Entire is now enabled.") {
		t.Fatalf("expected enable --force to still enable the repo, got: %s", output)
	}

	enabled, err := IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
	}
	if !enabled {
		t.Fatal("expected repo to be enabled after enable --force")
	}
}

func TestEnableCmd_ForceAndStrategyFlagsOnConfiguredDisabledRepo_ReenablesAndUpdatesSettings(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsDisabled)
	writeClaudeHooksFixture(t)

	cmd := newEnableCmd()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--force", "--checkpoint-remote", "github:org/repo", "--skip-push-sessions"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("enable with force and strategy flags error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Settings updated") {
		t.Fatalf("expected strategy flags to be applied, got: %s", output)
	}
	if !strings.Contains(output, "Cannot show agent selection in non-interactive mode.") {
		t.Fatalf("expected force handling to still reach manage agents, got: %s", output)
	}
	if !strings.Contains(output, "Entire is now enabled.") {
		t.Fatalf("expected repo to be enabled after updating settings, got: %s", output)
	}

	enabled, err := IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
	}
	if !enabled {
		t.Fatal("expected repo to be enabled after enable with strategy flags")
	}

	s, err := LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadEntireSettings() error = %v", err)
	}
	if got := s.StrategyOptions["push_sessions"]; got != false {
		t.Fatalf("push_sessions = %v, want false", got)
	}
	checkpointRemote, ok := s.StrategyOptions["checkpoint_remote"].(map[string]interface{})
	if !ok {
		t.Fatalf("checkpoint_remote = %#v, want map", s.StrategyOptions["checkpoint_remote"])
	}
	if checkpointRemote["provider"] != "github" || checkpointRemote["repo"] != "org/repo" {
		t.Fatalf("checkpoint_remote = %#v, want github/org/repo", checkpointRemote)
	}
}

// Regression: `entire enable --checkpoint-remote ...` (no --project)
// on a repo disabled at the project level must re-enable the project
// settings.json, not write the enabled flag to a shadow settings.local.json —
// which left the file the user disabled still enabled=false.
func TestEnableCmd_StrategyFlagsOnDisabledProjectRepo_EnablesProjectFile(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsDisabled) // settings.json: {"enabled": false}
	writeClaudeHooksFixture(t)

	cmd := newEnableCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--checkpoint-remote", "github:org/repo", "--skip-push-sessions"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("enable error = %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	// The project file the user disabled must be enabled again.
	projectS, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("load project settings: %v", err)
	}
	if !projectS.Enabled {
		t.Errorf("settings.json still enabled=false after enable; the enabled flag went to the wrong file")
	}
}

// Tests for detectOrSelectAgent

func TestDetectOrSelectAgent_AgentDetected(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir
	setupTestRepo(t)

	// Create .claude directory so Claude Code agent is detected
	if err := os.MkdirAll(".claude", 0o755); err != nil {
		t.Fatalf("Failed to create .claude directory: %v", err)
	}

	// No TTY here, so this exercises the non-interactive fallback: the single
	// detected agent is used without a picker. The interactive path pre-selects
	// it in the multi-select instead (see FirstRun_SingleBuiltIn test below).
	var buf bytes.Buffer
	agents, err := detectOrSelectAgent(context.Background(), &buf, nil)
	if err != nil {
		t.Fatalf("detectOrSelectAgent() error = %v", err)
	}

	// Should detect Claude Code
	if len(agents) != 1 {
		t.Fatalf("detectOrSelectAgent() returned %d agents, want 1", len(agents))
	}
	if agents[0].Name() != agent.AgentNameClaudeCode {
		t.Errorf("detectOrSelectAgent() agent name = %v, want %v", agents[0].Name(), agent.AgentNameClaudeCode)
	}

	output := buf.String()
	if !strings.Contains(output, "Detected agent:") {
		t.Errorf("Expected output to contain 'Detected agent:', got: %s", output)
	}
	if !strings.Contains(output, string(agent.AgentTypeClaudeCode)) {
		t.Errorf("Expected output to contain '%s', got: %s", agent.AgentTypeClaudeCode, output)
	}
}

func TestDetectOrSelectAgent_GeminiDetected(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir
	setupTestRepo(t)

	// Create .gemini directory so Gemini agent is detected
	if err := os.MkdirAll(".gemini", 0o755); err != nil {
		t.Fatalf("Failed to create .gemini directory: %v", err)
	}

	var buf bytes.Buffer
	agents, err := detectOrSelectAgent(context.Background(), &buf, nil)
	if err != nil {
		t.Fatalf("detectOrSelectAgent() error = %v", err)
	}

	// Should detect Gemini
	if len(agents) != 1 {
		t.Fatalf("detectOrSelectAgent() returned %d agents, want 1", len(agents))
	}
	if agents[0].Name() != agent.AgentNameGemini {
		t.Errorf("detectOrSelectAgent() agent name = %v, want %v", agents[0].Name(), agent.AgentNameGemini)
	}

	output := buf.String()
	if !strings.Contains(output, "Detected agent:") {
		t.Errorf("Expected output to contain 'Detected agent:', got: %s", output)
	}
}

func TestDetectOrSelectAgent_FirstRun_SingleBuiltIn_ShowsPickerPreSelected(t *testing.T) {
	// Not parallel: uses t.Chdir/t.Setenv and swaps the package-level
	// promptAgentSelection seam.
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")

	// Create .claude directory so exactly one built-in agent (Claude Code) is detected.
	if err := os.MkdirAll(".claude", 0o755); err != nil {
		t.Fatalf("Failed to create .claude directory: %v", err)
	}

	// First run: no hooks installed yet.
	if installed := GetAgentsWithHooksInstalled(context.Background()); len(installed) != 0 {
		t.Fatalf("Expected no installed hooks on first run, got %v", installed)
	}

	// Stub the real picker so we can assert it is shown (rather than the agent
	// being auto-used) and inspect which options it was given. Driving the
	// selectFn == nil path is what makes this a real regression guard: the old
	// shortcut returned early precisely when selectFn == nil, so a test that
	// injected a selectFn would have passed even before the fix.
	prev := promptAgentSelection
	t.Cleanup(func() { promptAgentSelection = prev })
	var offered []string
	var shown bool
	promptAgentSelection = func(options []huh.Option[string]) ([]string, error) {
		shown = true
		for _, o := range options {
			offered = append(offered, o.Value)
		}
		return []string{string(agent.AgentNameClaudeCode)}, nil
	}

	var buf bytes.Buffer
	agents, err := detectOrSelectAgent(context.Background(), &buf, nil)
	if err != nil {
		t.Fatalf("detectOrSelectAgent() error = %v", err)
	}

	// A lone detected built-in agent must no longer be auto-used: the picker
	// must be shown so the user can confirm it or add more.
	if !shown {
		t.Fatal("Expected the picker to be shown for a single detected agent, but it was auto-used")
	}
	if !slices.Contains(offered, string(agent.AgentNameClaudeCode)) {
		t.Errorf("Expected the detected agent among the picker options, got %v", offered)
	}
	if len(agents) != 1 || agents[0].Name() != agent.AgentNameClaudeCode {
		t.Fatalf("Expected the picked agent [claude-code] to be returned, got %v", agents)
	}
}

func TestDetectOrSelectAgent_OnlyExternalDetected_WithTTY_PromptsUser(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir, t.Setenv, and global agent registration
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")

	externalAgentName := "ext-prompt-pi"
	externalDir := t.TempDir()
	writeExternalAgentBinary(t, externalDir, externalAgentName)
	t.Setenv("ENTIRE_TEST_EXTERNAL_PRESENT", "1")
	t.Setenv("PATH", externalDir)

	external.DiscoverAndRegisterAlways(context.Background())

	var receivedAvailable []string
	selectFn := func(available []string) ([]string, error) {
		receivedAvailable = available
		return []string{string(agent.AgentNameClaudeCode)}, nil
	}

	var buf bytes.Buffer
	agents, err := detectOrSelectAgent(context.Background(), &buf, selectFn)
	if err != nil {
		t.Fatalf("detectOrSelectAgent() error = %v", err)
	}

	if len(receivedAvailable) == 0 {
		t.Fatal("Expected interactive prompt when only an external agent is detected")
	}
	if !slices.Contains(receivedAvailable, externalAgentName) {
		t.Fatalf("Expected external agent %q in options, got %v", externalAgentName, receivedAvailable)
	}
	if !slices.Contains(receivedAvailable, string(agent.AgentNameClaudeCode)) {
		t.Fatalf("Expected built-in agent options alongside external agent, got %v", receivedAvailable)
	}
	if len(agents) != 1 || agents[0].Name() != agent.AgentNameClaudeCode {
		t.Fatalf("Expected selected Claude Code agent, got %v", agents)
	}
	if strings.Contains(buf.String(), "Detected agent:") {
		t.Errorf("Expected external-only detection to prompt instead of auto-selecting, got output: %s", buf.String())
	}
}

func TestIsBuiltInAgent_ExternalAgent_False(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	setupTestRepo(t)

	externalAgentName := "ext-preselect-pi"
	externalDir := t.TempDir()
	writeExternalAgentBinary(t, externalDir, externalAgentName)
	t.Setenv("ENTIRE_TEST_EXTERNAL_PRESENT", "1")
	t.Setenv("PATH", externalDir)

	external.DiscoverAndRegisterAlways(context.Background())

	externalAgent, err := agent.Get(types.AgentName(externalAgentName))
	if err != nil {
		t.Fatalf("failed to get external agent %q: %v", externalAgentName, err)
	}

	if isBuiltInAgent(externalAgent) {
		t.Fatalf("expected external agent %q to not be treated as built-in", externalAgentName)
	}
}

func TestIsBuiltInAgent_BuiltInAgent_True(t *testing.T) {
	t.Parallel()

	claudeAgent, err := agent.Get(agent.AgentNameClaudeCode)
	if err != nil {
		t.Fatalf("failed to get claude agent: %v", err)
	}

	if !isBuiltInAgent(claudeAgent) {
		t.Fatal("expected built-in agent to be treated as built-in")
	}
}

func TestDetectOrSelectAgent_NoDetection_NoTTY_FallsBackToDefault(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)

	// No .claude or .gemini directory - detection will fail

	var buf bytes.Buffer
	agents, err := detectOrSelectAgent(context.Background(), &buf, nil)
	if err != nil {
		t.Fatalf("detectOrSelectAgent() error = %v", err)
	}

	// Should fall back to default agent (Claude Code)
	if len(agents) != 1 {
		t.Fatalf("detectOrSelectAgent() returned %d agents, want 1", len(agents))
	}
	if agents[0].Name() != agent.DefaultAgentName {
		t.Errorf("detectOrSelectAgent() agent name = %v, want default %v", agents[0].Name(), agent.DefaultAgentName)
	}

	output := buf.String()
	if !strings.Contains(output, "Agent:") {
		t.Errorf("Expected output to contain 'Agent:', got: %s", output)
	}
	if !strings.Contains(output, "(use --agent to change)") {
		t.Errorf("Expected output to contain '(use --agent to change)', got: %s", output)
	}
}

func TestDetectOrSelectAgent_NoDetection_WithTTY_ShowsPromptMessages(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")

	// No .claude or .gemini directory - detection will fail

	// Inject selector to avoid blocking on interactive form.Run().
	// The selector receives available agent names so tests can validate the options.
	selectFn := func(available []string) ([]string, error) {
		if len(available) == 0 {
			t.Error("selectFn received no available agents")
		}
		return []string{string(agent.AgentNameClaudeCode)}, nil
	}

	var buf bytes.Buffer
	agents, err := detectOrSelectAgent(context.Background(), &buf, selectFn)
	if err != nil {
		t.Fatalf("detectOrSelectAgent() error = %v", err)
	}

	// Should return the mock-selected agent
	if len(agents) != 1 {
		t.Fatalf("detectOrSelectAgent() returned %d agents, want 1", len(agents))
	}
	if agents[0].Name() != agent.AgentNameClaudeCode {
		t.Errorf("detectOrSelectAgent() agent = %v, want %v", agents[0].Name(), agent.AgentNameClaudeCode)
	}

	output := buf.String()
	if !strings.Contains(output, "Selected agents:") {
		t.Errorf("Expected output to contain 'Selected agents:', got: %s", output)
	}
}

func TestDetectOrSelectAgent_SelectionCancelled(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")

	selectFn := func(_ []string) ([]string, error) {
		return nil, errors.New("user cancelled")
	}

	var buf bytes.Buffer
	_, err := detectOrSelectAgent(context.Background(), &buf, selectFn)
	if err == nil {
		t.Fatal("expected error when selection is cancelled")
	}
	if !strings.Contains(err.Error(), "user cancelled") {
		t.Errorf("expected 'user cancelled' in error, got: %v", err)
	}
}

func TestDetectOrSelectAgent_NoneSelected(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")

	selectFn := func(_ []string) ([]string, error) {
		return []string{}, nil // user deselected everything
	}

	var buf bytes.Buffer
	_, err := detectOrSelectAgent(context.Background(), &buf, selectFn)
	if err == nil {
		t.Fatal("expected error when no agents selected")
	}
	if !strings.Contains(err.Error(), "no agents selected") {
		t.Errorf("expected 'no agents selected' in error, got: %v", err)
	}
}

func TestDetectOrSelectAgent_BothDirectoriesExist_PromptsUser(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")

	// Create both .claude and .gemini directories
	if err := os.MkdirAll(".claude", 0o755); err != nil {
		t.Fatalf("Failed to create .claude directory: %v", err)
	}
	if err := os.MkdirAll(".gemini", 0o755); err != nil {
		t.Fatalf("Failed to create .gemini directory: %v", err)
	}

	// Inject selector — receives available names, returns both
	selectFn := func(available []string) ([]string, error) {
		if len(available) < 2 {
			t.Errorf("expected at least 2 available agents, got %d", len(available))
		}
		return []string{string(agent.AgentNameClaudeCode), string(agent.AgentNameGemini)}, nil
	}

	var buf bytes.Buffer
	agents, err := detectOrSelectAgent(context.Background(), &buf, selectFn)
	if err != nil {
		t.Fatalf("detectOrSelectAgent() error = %v", err)
	}

	// Should return both selected agents
	if len(agents) != 2 {
		t.Fatalf("detectOrSelectAgent() returned %d agents, want 2", len(agents))
	}

	output := buf.String()
	if !strings.Contains(output, "Detected multiple agents:") {
		t.Errorf("Expected output to contain 'Detected multiple agents:', got: %s", output)
	}
	if !strings.Contains(output, "Claude Code") {
		t.Errorf("Expected output to mention Claude Code, got: %s", output)
	}
	if !strings.Contains(output, "Gemini CLI") {
		t.Errorf("Expected output to mention Gemini CLI, got: %s", output)
	}
	if !strings.Contains(output, "Selected agents:") {
		t.Errorf("Expected output to contain 'Selected agents:', got: %s", output)
	}
}

func TestDetectOrSelectAgent_BothDirectoriesExist_NoTTY_UsesAll(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)

	// Create both .claude and .gemini directories
	if err := os.MkdirAll(".claude", 0o755); err != nil {
		t.Fatalf("Failed to create .claude directory: %v", err)
	}
	if err := os.MkdirAll(".gemini", 0o755); err != nil {
		t.Fatalf("Failed to create .gemini directory: %v", err)
	}

	var buf bytes.Buffer
	agents, err := detectOrSelectAgent(context.Background(), &buf, nil)
	if err != nil {
		t.Fatalf("detectOrSelectAgent() error = %v", err)
	}

	// With no TTY and multiple detected, should return all detected agents
	if len(agents) != 2 {
		t.Errorf("detectOrSelectAgent() returned %d agents, want 2", len(agents))
	}
}

// writeClaudeHooksFixture writes a minimal .claude/settings.json with Entire hooks installed.
// Only the Stop hook is needed — AreHooksInstalled() checks for it first.
func writeClaudeHooksFixture(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(".claude", 0o755); err != nil {
		t.Fatalf("Failed to create .claude directory: %v", err)
	}
	hooksJSON := `{
		"hooks": {
			"Stop": [{"hooks": [{"type": "command", "command": "entire hooks claude-code stop"}]}]
		}
	}`
	if err := os.WriteFile(".claude/settings.json", []byte(hooksJSON), 0o644); err != nil {
		t.Fatalf("Failed to write .claude/settings.json: %v", err)
	}
}

// writeGeminiHooksFixture writes a minimal .gemini/settings.json with Entire hooks installed.
// AreHooksInstalled() checks for any hook command starting with "entire ".
func writeGeminiHooksFixture(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(".gemini", 0o755); err != nil {
		t.Fatalf("Failed to create .gemini directory: %v", err)
	}
	hooksJSON := `{
		"hooks": {
			"enabled": true,
			"SessionStart": [{"hooks": [{"type": "command", "command": "entire hooks gemini session-start"}]}]
		}
	}`
	if err := os.WriteFile(".gemini/settings.json", []byte(hooksJSON), 0o644); err != nil {
		t.Fatalf("Failed to write .gemini/settings.json: %v", err)
	}
}

func TestDetectOrSelectAgent_ReRun_AlwaysPromptsWithInstalledPreSelected(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")

	// Install Claude Code hooks (simulates a previous `entire enable` run)
	writeClaudeHooksFixture(t)

	// Verify hooks are detected as installed
	installed := GetAgentsWithHooksInstalled(context.Background())
	if len(installed) == 0 {
		t.Fatal("Expected Claude Code hooks to be detected as installed")
	}

	// Track what the selector receives
	var receivedAvailable []string
	selectFn := func(available []string) ([]string, error) {
		receivedAvailable = available
		// User keeps claude-code selected
		return []string{string(agent.AgentNameClaudeCode)}, nil
	}

	var buf bytes.Buffer
	agents, err := detectOrSelectAgent(context.Background(), &buf, selectFn)
	if err != nil {
		t.Fatalf("detectOrSelectAgent() error = %v", err)
	}

	// Should have been prompted (selectFn called) even though only one agent is detected
	if len(receivedAvailable) == 0 {
		t.Fatal("Expected interactive prompt to be shown on re-run, but selectFn was not called")
	}

	// Should return the selected agent
	if len(agents) != 1 || agents[0].Name() != agent.AgentNameClaudeCode {
		t.Errorf("Expected [claude-code], got %v", agents)
	}

	// Should NOT contain "Detected agent:" (the auto-use message for first run)
	output := buf.String()
	if strings.Contains(output, "Detected agent:") {
		t.Errorf("Re-run should not auto-use agent, but got: %s", output)
	}
}

func TestDetectOrSelectAgent_ReRun_NoTTY_KeepsInstalled(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)

	// Install Claude Code hooks
	writeClaudeHooksFixture(t)

	var buf bytes.Buffer
	agents, err := detectOrSelectAgent(context.Background(), &buf, nil)
	if err != nil {
		t.Fatalf("detectOrSelectAgent() error = %v", err)
	}

	// Should keep currently installed agents without prompting
	if len(agents) != 1 {
		t.Fatalf("Expected 1 agent, got %d", len(agents))
	}
	if agents[0].Name() != agent.AgentNameClaudeCode {
		t.Errorf("Expected claude-code, got %v", agents[0].Name())
	}
}

// checkClaudeCodeHooksInstalled checks if Claude Code hooks are installed.
func checkClaudeCodeHooksInstalled() bool {
	ag, err := agent.Get(agent.AgentNameClaudeCode)
	if err != nil {
		return false
	}
	hookAgent, ok := agent.AsHookSupport(ag)
	if !ok {
		return false
	}
	installed, err := hookAgent.AreHooksInstalled(context.Background())
	return err == nil && installed
}

// checkGeminiCLIHooksInstalled checks if Gemini CLI hooks are installed.
func checkGeminiCLIHooksInstalled() bool {
	ag, err := agent.Get(agent.AgentNameGemini)
	if err != nil {
		return false
	}
	hookAgent, ok := agent.AsHookSupport(ag)
	if !ok {
		return false
	}
	installed, err := hookAgent.AreHooksInstalled(context.Background())
	return err == nil && installed
}

func TestUninstallDeselectedAgentHooks(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir
	setupTestRepo(t)

	// Install Claude Code hooks
	writeClaudeHooksFixture(t)

	// Verify hooks are installed
	if !checkClaudeCodeHooksInstalled() {
		t.Fatal("Expected Claude Code hooks to be installed before test")
	}

	// Call uninstallDeselectedAgentHooks with an empty selection (deselect claude-code)
	var buf bytes.Buffer
	err := uninstallDeselectedAgentHooks(context.Background(), &buf, []agent.Agent{})
	if err != nil {
		t.Fatalf("uninstallDeselectedAgentHooks() error = %v", err)
	}

	// Hooks should be uninstalled
	if checkClaudeCodeHooksInstalled() {
		t.Error("Expected Claude Code hooks to be uninstalled after deselection")
	}

	output := buf.String()
	if !strings.Contains(output, "Removed") {
		t.Errorf("Expected output to mention removal, got: %s", output)
	}
}

func TestRunUninstall_CodexLinkedWorktreeDoesNotRemovePrimaryHooks(t *testing.T) {
	setupCodexRepositoryWithLinkedWorktree(t)
	repoRoot, err := paths.WorktreeRoot(t.Context())
	if err != nil {
		t.Fatalf("resolve primary worktree: %v", err)
	}
	linkedRoot := filepath.Join(filepath.Dir(repoRoot), "linked")
	if err := os.RemoveAll(filepath.Join(linkedRoot, ".codex")); err != nil {
		t.Fatalf("remove linked Codex project layer: %v", err)
	}
	t.Chdir(linkedRoot)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()

	ag, err := agent.Get(agent.AgentNameCodex)
	if err != nil {
		t.Fatalf("get Codex agent: %v", err)
	}
	hookAgent, ok := agent.AsHookSupport(ag)
	if !ok {
		t.Fatal("Codex agent does not support hooks")
	}
	installed, err := hookAgent.AreHooksInstalled(t.Context())
	if err != nil {
		t.Fatalf("check Codex hooks: %v", err)
	}
	if installed {
		t.Fatal("Codex hooks must be inactive without the linked checkout's .codex project layer")
	}
	freshness, ok := ag.(agent.HookFreshness)
	if !ok || freshness.CheckHookConfig(t.Context()) != agent.HooksAbsent {
		t.Fatal("primary-checkout Codex hooks must not count as local removable configuration")
	}

	var stdout, stderr bytes.Buffer
	if err := runUninstall(t.Context(), &stdout, &stderr, true); err != nil {
		t.Fatalf("uninstall Entire: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".codex", "hooks.json")); err != nil {
		t.Fatalf("primary-checkout Codex hooks changed after uninstall from linked worktree: %v", err)
	}
	if strings.Contains(stdout.String(), "Removed Codex hooks") {
		t.Fatalf("uninstall reported removing non-local Codex hooks: %s", stdout.String())
	}
}

func TestUninstallDeselectedAgentHooks_CodexIgnoresPrimaryOnlyHooks(t *testing.T) {
	setupCodexRepositoryWithLinkedWorktree(t)
	repoRoot, err := paths.WorktreeRoot(t.Context())
	if err != nil {
		t.Fatalf("resolve primary worktree: %v", err)
	}
	linkedRoot := filepath.Join(filepath.Dir(repoRoot), "linked")
	if err := os.RemoveAll(filepath.Join(linkedRoot, ".codex")); err != nil {
		t.Fatalf("remove linked Codex project layer: %v", err)
	}
	t.Chdir(linkedRoot)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()

	ag, err := agent.Get(agent.AgentNameCodex)
	if err != nil {
		t.Fatalf("get Codex agent: %v", err)
	}
	hookAgent, ok := agent.AsHookSupport(ag)
	if !ok {
		t.Fatal("Codex agent does not support hooks")
	}
	installed, err := hookAgent.AreHooksInstalled(t.Context())
	if err != nil {
		t.Fatalf("check Codex hooks: %v", err)
	}
	if installed {
		t.Fatal("Codex hooks must be inactive without the linked checkout's .codex project layer")
	}
	freshness, ok := ag.(agent.HookFreshness)
	if !ok || freshness.CheckHookConfig(t.Context()) != agent.HooksAbsent {
		t.Fatal("primary-checkout Codex hooks must not count as local removable configuration")
	}

	var output bytes.Buffer
	if err := uninstallDeselectedAgentHooks(t.Context(), &output, nil); err != nil {
		t.Fatalf("uninstallDeselectedAgentHooks() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(repoRoot, ".codex", "hooks.json")); err != nil {
		t.Fatalf("primary-checkout Codex hooks changed after deselection from linked worktree: %v", err)
	}
	if strings.Contains(output.String(), "Removed Codex hooks") {
		t.Fatalf("deselection reported removing non-local Codex hooks: %s", output.String())
	}
}

func TestUninstallDeselectedAgentHooks_CodexLinkedWorktreeOwnership(t *testing.T) {
	t.Parallel()
	assertCodexLinkedWorktreeCleanupOwnership(t, "deselect", "deselection")
}

func TestRemoveAgentHooks_CodexLinkedWorktreeOwnership(t *testing.T) {
	t.Parallel()
	assertCodexLinkedWorktreeCleanupOwnership(t, "clean", "clean")
}

func assertCodexLinkedWorktreeCleanupOwnership(t *testing.T, action, description string) {
	t.Helper()
	repoRoot, linkedRoot := setupCodexOwnershipWorktrees(t)
	primaryPath := filepath.Join(repoRoot, ".codex", "hooks.json")
	linkedPath := filepath.Join(linkedRoot, ".codex", "hooks.json")
	primaryBefore := readSetupTestFile(t, primaryPath)

	runSetupCodexOwnershipHelper(t, linkedRoot, action)

	if got := readSetupTestFile(t, primaryPath); got != primaryBefore {
		t.Fatalf("primary hooks changed after linked-worktree %s\nwant: %s\n got: %s", description, primaryBefore, got)
	}
	if data := readSetupTestFile(t, linkedPath); strings.Contains(data, "entire hooks codex") {
		t.Fatalf("linked-worktree Entire hooks still exist after %s: %s", description, data)
	}
}

func TestSetupCodexLinkedWorktreeOwnershipHelper(t *testing.T) {
	t.Parallel()
	action := os.Getenv("ENTIRE_SETUP_CODEX_OWNERSHIP_HELPER")
	if action == "" {
		t.Skip("subprocess helper")
	}
	var output bytes.Buffer
	switch action {
	case "deselect":
		if err := uninstallDeselectedAgentHooks(t.Context(), &output, nil); err != nil {
			t.Fatal(err)
		}
	case "clean":
		// uninstallAgentHooks is what `entire disable --uninstall` runs; it
		// replaced removeAgentHooks, so the ownership guarantee is asserted
		// against the function that actually removes hooks today.
		repoRoot, err := paths.WorktreeRoot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		p := newUninstallPrinter(&output, &output)
		if !uninstallAgentHooks(t.Context(), p, repoRoot, getAgentHookState(t.Context())) {
			t.Fatalf("uninstall agent hooks reported failure: %s", output.String())
		}
	default:
		t.Fatalf("unknown ownership helper action %q", action)
	}
}

func setupCodexOwnershipWorktrees(t *testing.T) (repoRoot, linkedRoot string) {
	t.Helper()
	tmp := t.TempDir()
	repoRoot = filepath.Join(tmp, "repo")
	linkedRoot = filepath.Join(tmp, "linked")
	testutil.InitRepo(t, repoRoot)
	testutil.WriteFile(t, repoRoot, "README.md", "initial\n")
	testutil.GitAdd(t, repoRoot, "README.md")
	testutil.GitCommit(t, repoRoot, "initial")
	cmd := exec.CommandContext(t.Context(), "git", "-C", repoRoot, "worktree", "add", "-b", "feature", linkedRoot)
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("create linked worktree: %v: %s", err, output)
	}
	for _, root := range []string{repoRoot, linkedRoot} {
		hooksPath := filepath.Join(root, ".codex", "hooks.json")
		if err := os.MkdirAll(filepath.Dir(hooksPath), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(hooksPath, []byte(canonicalCodexHooksJSON()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return repoRoot, linkedRoot
}

func runSetupCodexOwnershipHelper(t *testing.T, dir, action string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestSetupCodexLinkedWorktreeOwnershipHelper$", "-test.count=1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"ENTIRE_SETUP_CODEX_OWNERSHIP_HELPER="+action,
		"ENTIRE_TEST_TTY=0",
		"CODEX_HOME="+filepath.Join(t.TempDir(), "codex-home"),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run ownership helper: %v: %s", err, output)
	}
}

func readSetupTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func setupCodexRepositoryWithLinkedWorktree(t *testing.T) {
	t.Helper()
	tmp := setupTestDir(t)
	repoRoot := filepath.Join(tmp, "repo")
	linkedRoot := filepath.Join(tmp, "linked")
	testutil.InitRepo(t, repoRoot)
	testutil.WriteFile(t, repoRoot, "README.md", "initial\n")
	testutil.GitAdd(t, repoRoot, "README.md")
	testutil.GitCommit(t, repoRoot, "initial")

	cmd := exec.CommandContext(t.Context(), "git", "-C", repoRoot, "worktree", "add", "-b", "feature", linkedRoot)
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("create linked worktree: %v: %s", err, output)
	}
	if err := os.MkdirAll(filepath.Join(linkedRoot, ".codex"), 0o750); err != nil {
		t.Fatalf("create linked Codex project layer: %v", err)
	}

	t.Chdir(repoRoot)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	t.Setenv("CODEX_HOME", filepath.Join(tmp, "codex-home"))

	ag, err := agent.Get(agent.AgentNameCodex)
	if err != nil {
		t.Fatalf("get Codex agent: %v", err)
	}
	hookAgent, ok := agent.AsHookSupport(ag)
	if !ok {
		t.Fatal("Codex agent does not support hooks")
	}
	if _, err := hookAgent.InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("install Codex hooks: %v", err)
	}
}

func TestUninstallDeselectedAgentHooks_KeepsSelectedAgents(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir
	setupTestRepo(t)

	// Install Claude Code hooks
	writeClaudeHooksFixture(t)

	// Call uninstallDeselectedAgentHooks with claude-code still selected
	claudeAgent, err := agent.Get(agent.AgentNameClaudeCode)
	if err != nil {
		t.Fatalf("Failed to get claude-code agent: %v", err)
	}

	var buf bytes.Buffer
	err = uninstallDeselectedAgentHooks(context.Background(), &buf, []agent.Agent{claudeAgent})
	if err != nil {
		t.Fatalf("uninstallDeselectedAgentHooks() error = %v", err)
	}

	// Hooks should still be installed
	if !checkClaudeCodeHooksInstalled() {
		t.Error("Expected Claude Code hooks to remain installed when still selected")
	}

	output := buf.String()
	if strings.Contains(output, "Removed") {
		t.Errorf("Should not mention removal when agent is still selected, got: %s", output)
	}
}

func TestUninstallDeselectedAgentHooks_MultipleInstalled_DeselectOne(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir
	setupTestRepo(t)

	// Install both Claude Code and Gemini hooks
	writeClaudeHooksFixture(t)
	writeGeminiHooksFixture(t)

	// Verify both are installed
	installed := GetAgentsWithHooksInstalled(context.Background())
	if len(installed) < 2 {
		t.Fatalf("Expected at least 2 agents installed, got %d", len(installed))
	}

	// Keep only Claude Code selected (deselect Gemini)
	claudeAgent, err := agent.Get(agent.AgentNameClaudeCode)
	if err != nil {
		t.Fatalf("Failed to get claude-code agent: %v", err)
	}

	var buf bytes.Buffer
	err = uninstallDeselectedAgentHooks(context.Background(), &buf, []agent.Agent{claudeAgent})
	if err != nil {
		t.Fatalf("uninstallDeselectedAgentHooks() error = %v", err)
	}

	// Claude Code hooks should remain
	if !checkClaudeCodeHooksInstalled() {
		t.Error("Expected Claude Code hooks to remain installed")
	}

	// Gemini hooks should be removed
	if checkGeminiCLIHooksInstalled() {
		t.Error("Expected Gemini CLI hooks to be uninstalled after deselection")
	}

	output := buf.String()
	if !strings.Contains(output, "Removed") {
		t.Errorf("Expected output to mention removal, got: %s", output)
	}
}

func TestManageAgents_DeselectRemovesAgent(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")
	writeSettings(t, testSettingsEnabled)

	// Install Claude Code hooks
	writeClaudeHooksFixture(t)

	if !checkClaudeCodeHooksInstalled() {
		t.Fatal("Expected Claude Code hooks to be installed before test")
	}

	// Deselect claude-code, select gemini instead
	selectFn := func(_ []string) ([]string, error) {
		return []string{string(agent.AgentNameGemini)}, nil
	}

	var buf bytes.Buffer
	err := runManageAgents(context.Background(), &buf, EnableOptions{}, selectFn)
	if err != nil {
		t.Fatalf("runManageAgents() error = %v", err)
	}

	output := buf.String()

	// Claude Code hooks should be removed
	if checkClaudeCodeHooksInstalled() {
		t.Error("Expected Claude Code hooks to be uninstalled after deselection")
	}

	if !strings.Contains(output, "Removed agents") {
		t.Errorf("Expected output to mention removed agents, got: %s", output)
	}
}

func TestManageAgents_DeselectAll_RemovesAllAndShowsGuidance(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")
	writeSettings(t, testSettingsEnabled)
	writeClaudeHooksFixture(t)

	if !checkClaudeCodeHooksInstalled() {
		t.Fatal("Expected Claude Code hooks to be installed before test")
	}

	selectFn := func(_ []string) ([]string, error) {
		return []string{}, nil
	}

	var buf bytes.Buffer
	err := runManageAgents(context.Background(), &buf, EnableOptions{}, selectFn)
	if err != nil {
		t.Fatalf("runManageAgents() error = %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "All agents have been removed.") {
		t.Errorf("Expected 'All agents have been removed.' message, got: %s", output)
	}
	if !strings.Contains(output, "entire agent add") {
		t.Errorf("Expected guidance on how to re-add agents, got: %s", output)
	}

	if checkClaudeCodeHooksInstalled() {
		t.Error("Expected Claude Code hooks to be uninstalled after deselecting all")
	}
}

func TestManageAgents_DeselectIgnoresPrimaryOnlyCodexHooks(t *testing.T) {
	setupCodexRepositoryWithLinkedWorktree(t)
	repoRoot, err := paths.WorktreeRoot(t.Context())
	if err != nil {
		t.Fatalf("resolve primary worktree: %v", err)
	}
	linkedRoot := filepath.Join(filepath.Dir(repoRoot), "linked")
	if err := os.RemoveAll(filepath.Join(linkedRoot, ".codex")); err != nil {
		t.Fatalf("remove linked Codex project layer: %v", err)
	}
	t.Chdir(linkedRoot)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()

	ag, err := agent.Get(agent.AgentNameCodex)
	if err != nil {
		t.Fatalf("get Codex agent: %v", err)
	}
	hookAgent, ok := agent.AsHookSupport(ag)
	if !ok {
		t.Fatal("Codex agent does not support hooks")
	}
	installed, err := hookAgent.AreHooksInstalled(t.Context())
	if err != nil {
		t.Fatalf("check Codex hooks: %v", err)
	}
	if installed {
		t.Fatal("Codex hooks must be inactive without the linked checkout's .codex project layer")
	}
	freshness, ok := ag.(agent.HookFreshness)
	if !ok || freshness.CheckHookConfig(t.Context()) != agent.HooksAbsent {
		t.Fatal("primary-checkout Codex hooks must not count as local removable configuration")
	}

	var output bytes.Buffer
	selectNone := func(_ []string) ([]string, error) { return nil, nil }
	if err := runManageAgents(t.Context(), &output, EnableOptions{}, selectNone); err != nil {
		t.Fatalf("runManageAgents() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".codex", "hooks.json")); err != nil {
		t.Fatalf("primary-checkout Codex hooks changed after deselection: %v", err)
	}
	if strings.Contains(output.String(), "Removed Codex hooks") {
		t.Fatalf("deselection reported removing non-local Codex hooks: %s", output.String())
	}
}

func TestManageAgents_DoesNotRemoveInvalidCodexFile(t *testing.T) {
	setupCodexRepositoryWithLinkedWorktree(t)
	repoRoot, err := paths.WorktreeRoot(t.Context())
	if err != nil {
		t.Fatalf("resolve primary worktree: %v", err)
	}
	hooksPath := filepath.Join(repoRoot, ".codex", "hooks.json")
	const malformed = `{not-json`
	if err := os.WriteFile(hooksPath, []byte(malformed), 0o600); err != nil {
		t.Fatalf("write malformed Codex hooks: %v", err)
	}

	var output bytes.Buffer
	selectNone := func(_ []string) ([]string, error) { return nil, nil }
	if err := runManageAgents(t.Context(), &output, EnableOptions{}, selectNone); err != nil {
		t.Fatalf("runManageAgents() error = %v", err)
	}
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read malformed Codex hooks: %v", err)
	}
	if string(data) != malformed {
		t.Fatalf("invalid Codex file was changed: %q", data)
	}
	if strings.Contains(output.String(), "Removed Codex") {
		t.Fatalf("invalid Codex file was offered for removal: %s", output.String())
	}
}

func TestManageAgents_NoChanges(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")
	writeSettings(t, testSettingsEnabled)
	writeClaudeHooksFixture(t)

	// Keep the same selection
	selectFn := func(_ []string) ([]string, error) {
		return []string{string(agent.AgentNameClaudeCode)}, nil
	}

	var buf bytes.Buffer
	err := runManageAgents(context.Background(), &buf, EnableOptions{}, selectFn)
	if err != nil {
		t.Fatalf("runManageAgents() error = %v", err)
	}

	if !strings.Contains(buf.String(), "No changes made.") {
		t.Errorf("Expected 'No changes made.' output, got: %s", buf.String())
	}
}

func TestManageAgents_NoChanges_StillPersistsVercelSetting(t *testing.T) {
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")
	writeSettings(t, testSettingsEnabled)
	writeClaudeHooksFixture(t)

	if err := os.WriteFile("vercel.json", []byte(`{
  "git": {
    "deploymentEnabled": {
      "entire/**": false
    }
  }
}`), 0o644); err != nil {
		t.Fatalf("write vercel.json: %v", err)
	}

	selectFn := func(_ []string) ([]string, error) {
		return []string{string(agent.AgentNameClaudeCode)}, nil
	}

	var buf bytes.Buffer
	err := runManageAgents(context.Background(), &buf, EnableOptions{}, selectFn)
	if err != nil {
		t.Fatalf("runManageAgents() error = %v", err)
	}

	if strings.Contains(buf.String(), "No changes made.") {
		t.Fatalf("did not expect no-op output when settings changed, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), ".entire/settings.json") {
		t.Fatalf("expected settings update output, got: %s", buf.String())
	}

	s, err := settings.Load(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if !s.Vercel {
		t.Fatal("expected vercel setting to be enabled")
	}
}

func TestManageAgents_ForceReinstallsSelectedAgentHooks(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")
	writeSettings(t, testSettingsEnabled)
	writeClaudeHooksFixture(t)

	// Simulate a stale or locally modified Entire-managed Claude hook.
	modifiedHooksJSON := `{
		"hooks": {
			"Stop": [{"hooks": [{"type": "command", "command": "entire hooks claude-code stop --stale"}]}]
		}
	}`
	if err := os.WriteFile(".claude/settings.json", []byte(modifiedHooksJSON), 0o644); err != nil {
		t.Fatalf("Failed to mutate .claude/settings.json: %v", err)
	}

	selectFn := func(_ []string) ([]string, error) {
		return []string{string(agent.AgentNameClaudeCode)}, nil
	}

	var buf bytes.Buffer
	err := runManageAgents(context.Background(), &buf, EnableOptions{ForceHooks: true}, selectFn)
	if err != nil {
		t.Fatalf("runManageAgents() error = %v", err)
	}

	data, err := os.ReadFile(".claude/settings.json")
	if err != nil {
		t.Fatalf("Failed to read .claude/settings.json: %v", err)
	}
	content := string(data)

	if strings.Contains(content, "stop --stale") {
		t.Errorf("Expected force reinstall to rewrite stale Claude hook, got: %s", content)
	}
	if !strings.Contains(content, "entire hooks claude-code stop") {
		t.Errorf("Expected force reinstall to restore canonical Claude hook, got: %s", content)
	}
	if strings.Contains(buf.String(), "No changes made.") {
		t.Errorf("Force reinstall should not be treated as no-op, got: %s", buf.String())
	}
}

func TestManageAgents_ForceReportsReinstalledAgentsSeparately(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")
	writeSettings(t, testSettingsEnabled)
	writeClaudeHooksFixture(t)

	selectFn := func(_ []string) ([]string, error) {
		return []string{string(agent.AgentNameClaudeCode)}, nil
	}

	var buf bytes.Buffer
	err := runManageAgents(context.Background(), &buf, EnableOptions{ForceHooks: true}, selectFn)
	if err != nil {
		t.Fatalf("runManageAgents() error = %v", err)
	}

	if !strings.Contains(buf.String(), "Reinstalled agents") {
		t.Errorf("Expected force reinstall summary to mention reinstalled agents, got: %s", buf.String())
	}
	if strings.Contains(buf.String(), "Added agents") {
		t.Errorf("Force reinstall should not be reported as added agents, got: %s", buf.String())
	}
}

func TestManageAgents_AddAndRemove(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")
	writeSettings(t, testSettingsEnabled)

	// Install Claude Code hooks
	writeClaudeHooksFixture(t)

	// Deselect claude-code, add gemini
	selectFn := func(_ []string) ([]string, error) {
		return []string{string(agent.AgentNameGemini)}, nil
	}

	var buf bytes.Buffer
	err := runManageAgents(context.Background(), &buf, EnableOptions{}, selectFn)
	if err != nil {
		t.Fatalf("runManageAgents() error = %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Added agents") {
		t.Errorf("Expected 'Added agents' in output, got: %s", output)
	}
	if !strings.Contains(output, "Removed agents") {
		t.Errorf("Expected 'Removed agents' in output, got: %s", output)
	}

	// Verify hooks on disk: Claude removed, Gemini added
	if checkClaudeCodeHooksInstalled() {
		t.Error("Expected Claude Code hooks to be uninstalled after deselection")
	}
	if !checkGeminiCLIHooksInstalled() {
		t.Error("Expected Gemini CLI hooks to be installed after selection")
	}
}

func TestMaybePromptVercelDeploymentDisable_MergesExistingConfig(t *testing.T) {
	setupTestRepo(t)

	requireWriteFile := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	requireWriteFile("vercel.json", `{
  "cleanUrls": true,
  "git": {
    "deploymentEnabled": {
      "main": true
    }
  }
}`)

	var prompted bool
	var buf bytes.Buffer
	changed, err := maybePromptVercelDeploymentDisable(context.Background(), &buf, settings.EntireSettingsFile, func() (bool, error) {
		prompted = true
		return true, nil
	})
	if err != nil {
		t.Fatalf("maybePromptVercelDeploymentDisable() error = %v", err)
	}
	if !changed {
		t.Fatal("expected Vercel setting change")
	}
	if !prompted {
		t.Fatal("expected Vercel prompt to run")
	}

	projectSettings, err := settings.Load(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if !projectSettings.Vercel {
		t.Fatal("expected vercel setting to be enabled")
	}
}

func TestMaybePromptVercelDeploymentDisable_CreatesConfigWhenVercelDetected(t *testing.T) {
	setupTestRepo(t)

	if err := os.MkdirAll(".vercel", 0o755); err != nil {
		t.Fatalf("mkdir .vercel: %v", err)
	}

	var buf bytes.Buffer
	changed, err := maybePromptVercelDeploymentDisable(context.Background(), &buf, settings.EntireSettingsFile, func() (bool, error) {
		return true, nil
	})
	if err != nil {
		t.Fatalf("maybePromptVercelDeploymentDisable() error = %v", err)
	}
	if !changed {
		t.Fatal("expected Vercel setting change")
	}

	projectSettings, err := settings.Load(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if !projectSettings.Vercel {
		t.Fatal("expected vercel setting to be enabled")
	}
}

func TestMaybePromptVercelDeploymentDisable_SkipsPromptWhenAlreadyDisabledInVercelJSON(t *testing.T) {
	setupTestRepo(t)

	if err := os.WriteFile("vercel.json", []byte(`{
  "git": {
    "deploymentEnabled": {
      "entire/**": false
    }
  }
}`), 0o644); err != nil {
		t.Fatalf("write vercel.json: %v", err)
	}

	promptCalled := false
	var buf bytes.Buffer
	changed, err := maybePromptVercelDeploymentDisable(context.Background(), &buf, settings.EntireSettingsFile, func() (bool, error) {
		promptCalled = true
		return true, nil
	})
	if err != nil {
		t.Fatalf("maybePromptVercelDeploymentDisable() error = %v", err)
	}
	if !changed {
		t.Fatal("expected Vercel setting change from existing vercel.json")
	}
	if promptCalled {
		t.Fatal("expected Vercel prompt to be skipped when already configured")
	}
	if !strings.Contains(buf.String(), ".entire/settings.json") {
		t.Fatalf("expected settings update output, got %q", buf.String())
	}

	projectSettings, err := settings.Load(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if !projectSettings.Vercel {
		t.Fatal("expected vercel setting to be enabled from existing vercel.json")
	}
}

func TestMaybePromptVercelDeploymentDisable_WritesLocalSettingsWhenRequested(t *testing.T) {
	setupTestRepo(t)

	if err := os.MkdirAll(filepath.Dir(settings.EntireSettingsLocalFile), 0o755); err != nil {
		t.Fatalf("mkdir settings dir: %v", err)
	}
	if err := os.WriteFile("vercel.json", []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write vercel.json: %v", err)
	}

	var buf bytes.Buffer
	changed, err := maybePromptVercelDeploymentDisable(context.Background(), &buf, settings.EntireSettingsLocalFile, func() (bool, error) {
		return true, nil
	})
	if err != nil {
		t.Fatalf("maybePromptVercelDeploymentDisable() error = %v", err)
	}
	if !changed {
		t.Fatal("expected Vercel setting change")
	}
	if !strings.Contains(buf.String(), settings.EntireSettingsLocalFile) {
		t.Fatalf("expected local settings update output, got %q", buf.String())
	}

	localSettingsPath := filepath.Join(".", settings.EntireSettingsLocalFile)
	localSettings, err := settings.LoadFromFile(localSettingsPath)
	if err != nil {
		t.Fatalf("load local settings: %v", err)
	}
	if !localSettings.Vercel {
		t.Fatal("expected vercel setting in local settings")
	}

	projectSettingsPath := filepath.Join(".", settings.EntireSettingsFile)
	projectSettings, err := settings.LoadFromFile(projectSettingsPath)
	if err != nil {
		t.Fatalf("load project settings: %v", err)
	}
	if projectSettings.Vercel {
		t.Fatal("expected project settings to remain unchanged")
	}
}

func TestDetectOrSelectAgent_ReRun_NewlyDetectedAgentAvailableNotPreSelected(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")

	// Simulate: Claude Code hooks installed from a previous run
	writeClaudeHooksFixture(t)

	// Simulate: user added .gemini directory since last enable (detected but not installed)
	if err := os.MkdirAll(".gemini", 0o755); err != nil {
		t.Fatalf("Failed to create .gemini directory: %v", err)
	}

	// Track which agents the selector receives
	var receivedAvailable []string
	selectFn := func(available []string) ([]string, error) {
		receivedAvailable = available
		// Only select the installed agent (simulate user not checking the new one)
		return []string{string(agent.AgentNameClaudeCode)}, nil
	}

	var buf bytes.Buffer
	agents, err := detectOrSelectAgent(context.Background(), &buf, selectFn)
	if err != nil {
		t.Fatalf("detectOrSelectAgent() error = %v", err)
	}

	// Should have prompted (re-run always prompts)
	if len(receivedAvailable) == 0 {
		t.Fatal("Expected interactive prompt on re-run")
	}

	// Newly detected agent should be available as an option
	if len(receivedAvailable) < 2 {
		t.Errorf("Expected at least 2 available agents (detected agent should be an option), got %d", len(receivedAvailable))
	}

	// Only the installed agent should be returned (user didn't select the new one)
	if len(agents) != 1 || agents[0].Name() != agent.AgentNameClaudeCode {
		t.Errorf("Expected only [claude-code], got %v", agents)
	}
}

func TestDetectOrSelectAgent_ReRun_EmptySelection_ReturnsError(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")

	// Install Claude Code hooks (re-run scenario)
	writeClaudeHooksFixture(t)

	selectFn := func(_ []string) ([]string, error) {
		return []string{}, nil // user deselected everything
	}

	var buf bytes.Buffer
	_, err := detectOrSelectAgent(context.Background(), &buf, selectFn)
	if err == nil {
		t.Fatal("Expected error when no agents selected on re-run")
	}
	if !strings.Contains(err.Error(), "no agents selected") {
		t.Errorf("Expected 'no agents selected' error, got: %v", err)
	}
}

// Tests for configure --checkpoint-remote

func TestConfigureCmd_CheckpointRemote_UpdatesProjectSettings(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	cmd := newSetupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--checkpoint-remote", "github:ashtom/zeugs-checkpoints"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --checkpoint-remote failed: %v", err)
	}

	if !strings.Contains(stdout.String(), "Settings updated") {
		t.Errorf("expected 'Settings updated' output, got: %s", stdout.String())
	}

	// Verify the setting was written to settings.json
	s, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to load settings: %v", err)
	}
	remote := s.GetCheckpointRemote()
	if remote == nil {
		t.Fatal("expected checkpoint_remote to be set")
		return
	}
	if remote.Provider != "github" || remote.Repo != "ashtom/zeugs-checkpoints" {
		t.Errorf("unexpected checkpoint_remote: %+v", remote)
	}
}

func TestConfigureCmd_CheckpointRemote_WritesToLocalFile(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	cmd := newSetupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--local", "--checkpoint-remote", "github:org/repo"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --local --checkpoint-remote failed: %v", err)
	}

	if !strings.Contains(stdout.String(), "settings.local.json") {
		t.Errorf("expected output to reference settings.local.json, got: %s", stdout.String())
	}

	// Verify the setting was written to settings.local.json, not settings.json
	localS, err := settings.LoadFromFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("failed to load local settings: %v", err)
	}
	remote := localS.GetCheckpointRemote()
	if remote == nil {
		t.Fatal("expected checkpoint_remote in local settings")
	}

	// Project settings should be unchanged
	projectS, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to load project settings: %v", err)
	}
	if projectS.GetCheckpointRemote() != nil {
		t.Error("checkpoint_remote should not leak into project settings")
	}
}

func TestConfigureCmd_CheckpointRemote_LocalOnlyRepo(t *testing.T) {
	setupTestRepo(t)
	// Only local settings exist — no settings.json
	writeLocalSettings(t, testSettingsEnabled)

	cmd := newSetupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--checkpoint-remote", "github:org/repo"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --checkpoint-remote on local-only repo failed: %v", err)
	}

	// Should NOT create settings.json
	if _, err := os.Stat(EntireSettingsFile); err == nil {
		t.Error("settings.json should not be created in a local-only repo")
	}

	// Should write to settings.local.json
	localS, err := settings.LoadFromFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("failed to load local settings: %v", err)
	}
	if localS.GetCheckpointRemote() == nil {
		t.Error("expected checkpoint_remote in local settings")
	}
}

// Tests for configure --summarize-timeout-seconds (issue #1198)

func TestConfigureCmd_SummarizeTimeoutSeconds_WritesProjectSettings(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	cmd := newSetupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--summarize-timeout-seconds", "300"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --summarize-timeout-seconds failed: %v", err)
	}

	if !strings.Contains(stdout.String(), "Settings updated") {
		t.Errorf("expected 'Settings updated' output, got: %s", stdout.String())
	}

	s, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to load settings: %v", err)
	}
	if s.SummaryTimeoutSeconds != 300 {
		t.Errorf("SummaryTimeoutSeconds = %d, want 300", s.SummaryTimeoutSeconds)
	}
}

func TestConfigureCmd_SummarizeTimeoutSeconds_WritesLocalSettings(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	cmd := newSetupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--local", "--summarize-timeout-seconds", "600"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --local --summarize-timeout-seconds failed: %v", err)
	}

	if !strings.Contains(stdout.String(), "settings.local.json") {
		t.Errorf("expected output to reference settings.local.json, got: %s", stdout.String())
	}

	localS, err := settings.LoadFromFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("failed to load local settings: %v", err)
	}
	if localS.SummaryTimeoutSeconds != 600 {
		t.Errorf("local SummaryTimeoutSeconds = %d, want 600", localS.SummaryTimeoutSeconds)
	}

	// Project settings must not have been mutated.
	projectS, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to load project settings: %v", err)
	}
	if projectS.SummaryTimeoutSeconds != 0 {
		t.Errorf("project SummaryTimeoutSeconds = %d, want 0 (unchanged)", projectS.SummaryTimeoutSeconds)
	}
}

func TestConfigureCmd_SummarizeTimeoutSeconds_ClearsValue(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, `{"enabled":true,"summary_timeout_seconds":300}`)

	cmd := newSetupCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--summarize-timeout-seconds", "0"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --summarize-timeout-seconds 0 failed: %v", err)
	}

	s, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to load settings: %v", err)
	}
	if s.SummaryTimeoutSeconds != 0 {
		t.Errorf("SummaryTimeoutSeconds = %d, want 0 (cleared)", s.SummaryTimeoutSeconds)
	}
}

func TestConfigureCmd_SummarizeTimeoutSeconds_RejectsNegative(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	cmd := newSetupCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--summarize-timeout-seconds", "-5"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for negative --summarize-timeout-seconds")
	}
	if !strings.Contains(err.Error(), "non-negative") {
		t.Errorf("expected 'non-negative' in error, got: %v", err)
	}
}

func TestConfigureCmd_CheckpointRemote_InvalidFormat(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	cmd := newSetupCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--checkpoint-remote", "invalid-format"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid --checkpoint-remote format")
	}
}

func TestConfigureCmd_CheckpointRemote_DoesNotLeakMergedSettings(t *testing.T) {
	setupTestRepo(t)
	// Project has enabled=true, local has log_level override
	writeSettings(t, testSettingsEnabled)
	writeLocalSettings(t, `{"log_level": "debug"}`)

	cmd := newSetupCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--project", "--checkpoint-remote", "github:org/repo"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --project --checkpoint-remote failed: %v", err)
	}

	// Project settings should NOT contain log_level from local
	data, err := os.ReadFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to read settings: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to parse settings: %v", err)
	}
	if _, exists := raw["log_level"]; exists {
		t.Error("log_level from local settings leaked into project settings")
	}
}

func stubCLIAvailable(t *testing.T) {
	t.Helper()
	orig := isSummaryCLIAvailable
	isSummaryCLIAvailable = func(types.AgentName) bool { return true }
	t.Cleanup(func() { isSummaryCLIAvailable = orig })
}

func TestConfigureCmd_SummarizeProvider_UpdatesProjectSettings(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	stubCLIAvailable(t)

	cmd := newSetupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--summarize-provider", "codex", "--summarize-model", "gpt-5"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --summarize-provider failed: %v", err)
	}

	if !strings.Contains(stdout.String(), "Settings updated") {
		t.Errorf("expected 'Settings updated' output, got: %s", stdout.String())
	}

	s, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to load settings: %v", err)
	}
	if s.SummaryGeneration == nil {
		t.Fatal("expected summary_generation to be set")
	}
	if s.SummaryGeneration.Provider != "codex" {
		t.Fatalf("summary provider = %q, want %q", s.SummaryGeneration.Provider, "codex")
	}
	if s.SummaryGeneration.Model != "gpt-5" {
		t.Fatalf("summary model = %q, want %q", s.SummaryGeneration.Model, "gpt-5")
	}
}

func TestConfigureCmd_SummarizeProvider_WritesToLocalFile(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	stubCLIAvailable(t)

	cmd := newSetupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--local", "--summarize-provider", "claude-code", "--summarize-model", "sonnet"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --local --summarize-provider failed: %v", err)
	}

	if !strings.Contains(stdout.String(), "settings.local.json") {
		t.Errorf("expected output to reference settings.local.json, got: %s", stdout.String())
	}

	localS, err := settings.LoadFromFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("failed to load local settings: %v", err)
	}
	if localS.SummaryGeneration == nil {
		t.Fatal("expected local summary_generation to be set")
	}
	if localS.SummaryGeneration.Provider != "claude-code" {
		t.Fatalf("local summary provider = %q, want %q", localS.SummaryGeneration.Provider, "claude-code")
	}

	projectS, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to load project settings: %v", err)
	}
	if projectS.SummaryGeneration != nil {
		t.Fatal("summary_generation should not leak into project settings")
	}
}

func TestConfigureCmd_SummarizeProvider_ExternalEnablesExternalAgents(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	const provider = "external-summary-config"
	externalDir := t.TempDir()
	writeExternalSummaryAgentBinary(t, externalDir, provider)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cmd := newSetupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--summarize-provider", provider})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --summarize-provider external failed: %v", err)
	}

	s, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to load settings: %v", err)
	}
	if s.SummaryGeneration == nil {
		t.Fatal("expected summary_generation to be set")
	}
	if s.SummaryGeneration.Provider != provider {
		t.Fatalf("summary provider = %q, want %q", s.SummaryGeneration.Provider, provider)
	}
	if !s.ExternalAgents {
		t.Fatal("external summary provider should enable external_agents")
	}
	if !strings.Contains(stdout.String(), externalAgentsAutoEnabledNotice) {
		t.Fatalf("expected notice surfacing the external_agents flip, got stdout:\n%s", stdout.String())
	}
}

func TestConfigureCmd_SummarizeProvider_ExternalAlreadyEnabled_NoNotice(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	setupTestRepo(t)
	writeSettings(t, `{"enabled": true, "external_agents": true}`)

	const provider = "external-summary-already-on"
	externalDir := t.TempDir()
	writeExternalSummaryAgentBinary(t, externalDir, provider)
	t.Setenv("PATH", externalDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cmd := newSetupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--summarize-provider", provider})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --summarize-provider external failed: %v", err)
	}

	if strings.Contains(stdout.String(), externalAgentsAutoEnabledNotice) {
		t.Fatalf("notice should not fire when external_agents was already enabled, got stdout:\n%s", stdout.String())
	}
}

func TestConfigureCmd_SummarizeProvider_InvalidProvider(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	cmd := newSetupCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--summarize-provider", "opencode"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for unsupported summary provider")
	}
}

func TestConfigureCmd_SummarizeProvider_SwitchClearsStaleModel(t *testing.T) {
	stubCLIAvailable(t)
	setupTestRepo(t)
	writeSettings(t, `{"enabled": true, "summary_generation": {"provider": "claude-code", "model": "sonnet"}}`)

	cmd := newSetupCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--summarize-provider", "codex"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --summarize-provider codex failed: %v", err)
	}

	s, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to load settings: %v", err)
	}
	if s.SummaryGeneration == nil {
		t.Fatal("expected summary_generation to be set")
	}
	if s.SummaryGeneration.Provider != "codex" {
		t.Fatalf("summary provider = %q, want %q", s.SummaryGeneration.Provider, "codex")
	}
	if s.SummaryGeneration.Model != "" {
		t.Fatalf("summary model = %q, want empty after provider switch", s.SummaryGeneration.Model)
	}
}

func TestConfigureCmd_SummarizeModel_RequiresProvider(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	cmd := newSetupCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--summarize-model", "sonnet"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for summarize-model without provider")
	}
}

func TestConfigureCmd_SummarizeModel_LocalInheritsProviderFromProject(t *testing.T) {
	setupTestRepo(t)
	stubCLIAvailable(t)
	// Project settings define the provider; local override only sets the model.
	writeSettings(t, `{"enabled": true, "summary_generation": {"provider": "claude-code"}}`)

	cmd := newSetupCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--local", "--summarize-model", "sonnet"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --local --summarize-model failed: %v", err)
	}

	localS, err := settings.LoadFromFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("failed to load local settings: %v", err)
	}
	if localS.SummaryGeneration == nil {
		t.Fatal("expected local summary_generation to be set")
	}
	if localS.SummaryGeneration.Model != "sonnet" {
		t.Fatalf("local summary model = %q, want %q", localS.SummaryGeneration.Model, "sonnet")
	}

	// Project settings must not be modified.
	projectS, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to load project settings: %v", err)
	}
	if projectS.SummaryGeneration.Model != "" {
		t.Fatalf("project model = %q, should remain empty", projectS.SummaryGeneration.Model)
	}
}

func TestConfigureCmd_SummarizeModel_UsesExistingProvider(t *testing.T) {
	setupTestRepo(t)
	stubCLIAvailable(t)
	writeSettings(t, `{"enabled": true, "summary_generation": {"provider": "claude-code"}}`)

	cmd := newSetupCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--summarize-model", "sonnet"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --summarize-model failed: %v", err)
	}

	s, err := settings.LoadFromFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("failed to load settings: %v", err)
	}
	if s.SummaryGeneration == nil {
		t.Fatal("expected summary_generation to be set")
	}
	if s.SummaryGeneration.Provider != "claude-code" {
		t.Fatalf("summary provider = %q, want %q", s.SummaryGeneration.Provider, "claude-code")
	}
	if s.SummaryGeneration.Model != "sonnet" {
		t.Fatalf("summary model = %q, want %q", s.SummaryGeneration.Model, "sonnet")
	}
}

func TestSelectAllAgents_ReturnsAll(t *testing.T) {
	t.Parallel()
	available := []string{"claude-code", "gemini-cli", "opencode"}
	selected, err := selectAllAgents(available)
	if err != nil {
		t.Fatalf("selectAllAgents() error = %v", err)
	}
	if !slices.Equal(selected, available) {
		t.Errorf("selectAllAgents() = %v, want %v", selected, available)
	}
}

func TestSelectAllAgents_EmptyReturnsError(t *testing.T) {
	t.Parallel()
	_, err := selectAllAgents(nil)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestDetectOrSelectAgent_YesSelectsAll(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "1")

	var buf bytes.Buffer
	agents, err := detectOrSelectAgent(context.Background(), &buf, selectAllAgents)
	if err != nil {
		t.Fatalf("detectOrSelectAgent() with selectAllAgents error = %v", err)
	}

	// Should return at least 2 agents (claude-code + gemini-cli are registered in test imports)
	if len(agents) < 2 {
		t.Errorf("expected at least 2 agents with selectAllAgents, got %d", len(agents))
	}

	output := buf.String()
	if !strings.Contains(output, "Selected agents:") {
		t.Errorf("Expected output to contain 'Selected agents:', got: %s", output)
	}
}

func TestManageAgents_YesWorksNonInteractive(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)

	// Install claude-code hooks so there's something installed
	writeClaudeHooksFixture(t)

	// Use a selectFn that only picks built-in agents to avoid failures
	// from stale external agent binaries registered by other tests.
	selectBuiltIn := func(available []string) ([]string, error) {
		var selected []string
		for _, name := range available {
			ag, err := agent.Get(types.AgentName(name))
			if err != nil {
				continue
			}
			if isBuiltInAgent(ag) {
				selected = append(selected, name)
			}
		}
		if len(selected) == 0 {
			return nil, errors.New("no built-in agents available")
		}
		return selected, nil
	}

	var buf bytes.Buffer
	err := runManageAgents(context.Background(), &buf, EnableOptions{}, selectBuiltIn)
	if err != nil {
		t.Fatalf("runManageAgents() with selectFn in non-interactive mode error = %v", err)
	}

	output := buf.String()
	// Should NOT print the non-interactive bail-out message
	if strings.Contains(output, "Cannot show agent selection in non-interactive mode") {
		t.Error("selectFn should bypass the interactivity check, but got non-interactive message")
	}
}

func TestEnableYes_TelemetryRespectsOptOut(t *testing.T) {
	// Cannot use t.Parallel() because subtests use t.Setenv

	t.Run("yes with telemetry=false", func(t *testing.T) {
		s := &EntireSettings{}
		opts := EnableOptions{Telemetry: false}
		if !opts.Telemetry || os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
			f := false
			s.Telemetry = &f
		} else if s.Telemetry == nil {
			tr := true
			s.Telemetry = &tr
		}
		if s.Telemetry == nil || *s.Telemetry != false {
			t.Errorf("expected telemetry=false when --yes --telemetry=false, got %v", s.Telemetry)
		}
	})

	t.Run("yes with ENTIRE_TELEMETRY_OPTOUT", func(t *testing.T) {
		t.Setenv("ENTIRE_TELEMETRY_OPTOUT", "1")
		s := &EntireSettings{}
		opts := EnableOptions{Telemetry: true}
		if !opts.Telemetry || os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
			f := false
			s.Telemetry = &f
		} else if s.Telemetry == nil {
			tr := true
			s.Telemetry = &tr
		}
		if s.Telemetry == nil || *s.Telemetry != false {
			t.Errorf("expected telemetry=false with ENTIRE_TELEMETRY_OPTOUT, got %v", s.Telemetry)
		}
	})

	t.Run("yes defaults to telemetry enabled", func(t *testing.T) {
		s := &EntireSettings{}
		opts := EnableOptions{Telemetry: true}
		if !opts.Telemetry {
			f := false
			s.Telemetry = &f
		} else if s.Telemetry == nil {
			tr := true
			s.Telemetry = &tr
		}
		if s.Telemetry == nil || *s.Telemetry != true {
			t.Errorf("expected telemetry=true with --yes (default), got %v", s.Telemetry)
		}
	})

	t.Run("yes preserves existing telemetry setting", func(t *testing.T) {
		existing := false
		s := &EntireSettings{Telemetry: &existing}
		opts := EnableOptions{Telemetry: true}
		if !opts.Telemetry || os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
			f := false
			s.Telemetry = &f
		} else if s.Telemetry == nil {
			tr := true
			s.Telemetry = &tr
		}
		if *s.Telemetry != false {
			t.Errorf("expected existing telemetry=false to be preserved, got %v", *s.Telemetry)
		}
	})
}

func TestEnableCmd_YesFreshRepo_SkipsPromptsAndEnables(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	testutil.WriteFile(t, ".", "f.txt", "init")
	testutil.GitAdd(t, ".", "f.txt")
	testutil.GitCommit(t, ".", "init")

	// Use --yes with --agent to test the realistic CI scenario.
	// The --yes flag skips telemetry/Vercel prompts while --agent selects a specific agent.
	// The pure --yes-selects-all-agents path is covered by TestDetectOrSelectAgent_YesSelectsAll.
	cmd := newEnableCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--yes", "--agent", "claude-code"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("enable --yes --agent claude-code error = %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Ready.") {
		t.Errorf("expected 'Ready.' in output, got: %s", output)
	}

	// Verify settings were saved with telemetry enabled (--yes default)
	s, err := LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("failed to load settings: %v", err)
	}
	if !s.Enabled {
		t.Error("expected enabled=true")
	}
}

func TestEnableCmd_YesWithAgent_AgentTakesPrecedence(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	testutil.WriteFile(t, ".", "f.txt", "init")
	testutil.GitAdd(t, ".", "f.txt")
	testutil.GitCommit(t, ".", "init")

	cmd := newEnableCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--yes", "--agent", "claude-code"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("enable --yes --agent claude-code error = %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	output := stdout.String()
	// --agent takes precedence — should show single-agent non-interactive output
	if !strings.Contains(output, "Agent: Claude Code") {
		t.Errorf("expected 'Agent: Claude Code' in output, got: %s", output)
	}
	// Should NOT have shown multi-select output
	if strings.Contains(output, "Selected agents:") {
		t.Errorf("--agent should bypass multi-select, but got 'Selected agents:' in: %s", output)
	}
}

func TestEnableCmd_YesOnConfiguredRepo_ManagesAgents(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	writeClaudeHooksFixture(t)

	cmd := newEnableCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--yes"})

	// May partially fail due to stale external agents in global registry,
	// but the key behavior is that it doesn't bail out with the non-interactive message.
	_ = cmd.Execute() //nolint:errcheck // partial failure from stale test agents is expected

	output := stdout.String()
	// Should NOT bail out with non-interactive message
	if strings.Contains(output, "Cannot show agent selection in non-interactive mode") {
		t.Error("--yes should bypass non-interactive check, but got bail-out message")
	}
}

func TestEnableCmd_YesWithTelemetryFalse(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	testutil.WriteFile(t, ".", "f.txt", "init")
	testutil.GitAdd(t, ".", "f.txt")
	testutil.GitCommit(t, ".", "init")

	cmd := newEnableCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--yes", "--agent", "claude-code", "--telemetry=false"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("enable --yes --telemetry=false error = %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	// Verify telemetry was disabled despite --yes
	s, err := LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("failed to load settings: %v", err)
	}
	if s.Telemetry == nil || *s.Telemetry != false {
		t.Errorf("expected telemetry=false when --yes --telemetry=false, got %v", s.Telemetry)
	}
}

func TestConfigureCmd_BarePrintsHelpHint(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	writeClaudeHooksFixture(t)

	cmd := newSetupCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "entire agent") {
		t.Errorf("expected hint about 'entire agent' in help output, got: %s", output)
	}
	// Bare configure must not run the agent picker.
	if strings.Contains(output, "Cannot show agent selection in non-interactive mode") {
		t.Errorf("bare configure should not invoke agent picker, got: %s", output)
	}
}

func TestConfigureCmd_AgentFlagRemoved(t *testing.T) {
	t.Parallel()
	cmd := newSetupCmd()
	if cmd.Flags().Lookup("agent") != nil {
		t.Error("'configure' must not expose --agent (use 'entire agent add')")
	}
	if cmd.Flags().Lookup("remove") != nil {
		t.Error("'configure' must not expose --remove (use 'entire agent remove')")
	}
	if cmd.Flags().Lookup("yes") != nil {
		t.Error("'configure' must not expose --yes (lives on 'entire enable')")
	}
}

func TestConfigureCmd_TelemetryFlag_PersistsSetting(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	cmd := newSetupCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--telemetry=false"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --telemetry=false error = %v", err)
	}

	s, err := LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if s.Telemetry == nil || *s.Telemetry != false {
		t.Errorf("expected telemetry=false, got %v", s.Telemetry)
	}
}

func TestConfigureCmd_AbsoluteGitHookPathFlag_PersistsAndReinstallsHook(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	cmd := newSetupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--absolute-git-hook-path"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --absolute-git-hook-path error = %v", err)
	}

	s, err := LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if !s.AbsoluteGitHookPath {
		t.Error("expected absolute_git_hook_path=true after configure --absolute-git-hook-path")
	}
	if !strings.Contains(stdout.String(), "Reinstalled git hook") {
		t.Errorf("expected hook reinstall message, got: %s", stdout.String())
	}
}

func TestConfigureCmd_TelemetryAlone_DoesNotReinstallHook(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	cmd := newSetupCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--telemetry=false"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("configure --telemetry=false error = %v", err)
	}

	if strings.Contains(stdout.String(), "Reinstalled git hook") {
		t.Errorf("--telemetry alone should not trigger hook reinstall, got: %s", stdout.String())
	}
}

func TestConfigureCmd_FreshRepo_PointsAtEnable(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir
	setupTestRepo(t)
	// No settings written — fresh repo.

	cmd := newSetupCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--telemetry=false"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected configure on fresh repo to fail")
	}
	if !strings.Contains(stderr.String(), "entire enable") {
		t.Errorf("expected hint pointing at 'entire enable', got stderr: %s", stderr.String())
	}
}

func TestCleanRemoteURLForReport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rawURL  string
		want    string
		wantErr bool
	}{
		{
			name:   "https without credentials is normalized",
			rawURL: "https://github.com/entireio/cli.git",
			want:   "https://github.com/entireio/cli.git",
		},
		{
			name:   "https token credentials are stripped",
			rawURL: "https://ghp_secrettoken@github.com/entireio/cli.git",
			want:   "https://github.com/entireio/cli.git",
		},
		{
			name:   "https user:password credentials are stripped",
			rawURL: "https://x-access-token:ghp_secret@github.com/entireio/cli.git",
			want:   "https://github.com/entireio/cli.git",
		},
		{
			name:   "query parameters are dropped",
			rawURL: "https://github.com/entireio/cli.git?token=secret",
			want:   "https://github.com/entireio/cli.git",
		},
		{
			name:   "scp-style ssh remote is normalized to https and the user is dropped",
			rawURL: "git@github.com:entireio/cli.git",
			want:   "https://github.com/entireio/cli.git",
		},
		{
			name:   "missing .git suffix is added",
			rawURL: "https://github.com/entireio/cli",
			want:   "https://github.com/entireio/cli.git",
		},
		{
			name:   "entire:// mirror origin maps the forge back to its real host",
			rawURL: "entire://aws-us-east-2.entire.io/gh/entireio/cli",
			want:   "https://github.com/entireio/cli.git",
		},
		{
			name:   "unknown forge host is preserved (self-hosted enterprise)",
			rawURL: "git@ghe.corp.example.com:entireio/cli.git",
			want:   "https://ghe.corp.example.com/entireio/cli.git",
		},
		{
			name:    "unparseable single-segment path errors",
			rawURL:  "https://github.com/onlyowner.git",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			info, err := gitremote.ParseURL(tt.rawURL)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected ParseURL error for %q", tt.rawURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error parsing %q: %v", tt.rawURL, err)
			}
			got := cleanRemoteURLForReport(info)
			if got != tt.want {
				t.Errorf("cleanRemoteURLForReport(%q) = %q, want %q", tt.rawURL, got, tt.want)
			}
			// The cleaned URL must never carry the original credentials.
			for _, secret := range []string{"ghp_secrettoken", "ghp_secret", "x-access-token", "token=secret"} {
				if strings.Contains(got, secret) {
					t.Errorf("cleaned URL %q leaked credential %q", got, secret)
				}
			}
		})
	}
}

// TestRunRemoveAgent_AntigravityWarnsAboutGlobalTeeRemoval pins the
// machine-global side effect of a repo-scoped command: removing the
// Antigravity agent uninstalls the title-tee from agy's GLOBAL settings, which
// disables token capture for every other repo still using Antigravity. The
// user must be told, since the breakage is otherwise silent (zero-token
// checkpoints) until doctor or agent add runs in the other repo.
func TestRunRemoveAgent_AntigravityWarnsAboutGlobalTeeRemoval(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)

	agentsDir := filepath.Join(dir, ".agents")
	if err := os.MkdirAll(agentsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "hooks.json"), []byte(antigravityHooksJSON()), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgDir := filepath.Join(t.TempDir(), "agy")
	if err := os.MkdirAll(cfgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	teeSettings := `{"title":{"type":"command","command":"entire hooks antigravity title-tee"}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(teeSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENTIRE_ANTIGRAVITY_CONFIG_DIR", cfgDir)

	var out bytes.Buffer
	if err := runRemoveAgent(context.Background(), &out, "antigravity"); err != nil {
		t.Fatalf("runRemoveAgent: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "Removed Antigravity hooks.") {
		t.Errorf("missing removal confirmation: %q", got)
	}
	if !strings.Contains(got, "token capture") || !strings.Contains(got, "other repositories") {
		t.Errorf("missing global title-tee removal warning: %q", got)
	}
}

// First-time setups get the git-refs checkpoint backend written explicitly
// into the new settings.json — new users must not answer a storage-topology
// question (the old wizard prompt), and the explicit write is what keeps the
// choice durable. The config-less runtime default (git-branch, see
// checkpoint.resolvePrimaryType) is deliberately untouched so repos set up
// before this change keep their behavior.
func TestRunEnableInteractive_FirstRunDefaultsToGitRefs(t *testing.T) {
	enable := func(t *testing.T, opts EnableOptions) *settings.CheckpointsConfig {
		t.Helper()
		ag, err := agent.Get(types.AgentName("claude-code"))
		if err != nil {
			t.Fatalf("agent.Get(claude-code) error = %v", err)
		}
		var buf bytes.Buffer
		if err := runEnableInteractive(context.Background(), &buf, []agent.Agent{ag}, opts); err != nil {
			t.Fatalf("runEnableInteractive() error = %v", err)
		}
		s, err := settings.Load(context.Background())
		if err != nil {
			t.Fatalf("settings.Load() error = %v", err)
		}
		return s.Checkpoints
	}

	t.Run("first run writes git-refs explicitly", func(t *testing.T) {
		setupTestRepo(t)
		cfg := enable(t, EnableOptions{Yes: true, Telemetry: true})
		if cfg == nil || cfg.Primary.Type != checkpoint.BackendTypeGitRefs {
			t.Errorf("Checkpoints = %+v, want explicit git-refs primary", cfg)
		}
	})

	t.Run("explicit --checkpoint-backend branch wins", func(t *testing.T) {
		setupTestRepo(t)
		cfg := enable(t, EnableOptions{Yes: true, Telemetry: true, CheckpointBackend: "branch"})
		if cfg == nil || cfg.Primary.Type != checkpoint.BackendTypeGitBranch {
			t.Errorf("Checkpoints = %+v, want explicit git-branch primary", cfg)
		}
	})

	t.Run("env override suppresses the first-run default", func(t *testing.T) {
		setupTestRepo(t)
		// ENTIRE_CHECKPOINTS_PRIMARY fully replaces the settings block, so
		// writing the refs default under it would persist config diverging
		// from the backend actually in use (and break harnesses pinning
		// git-branch via the env).
		t.Setenv(settings.EnvCheckpointsPrimary, "git-branch")
		cfg := enable(t, EnableOptions{Yes: true, Telemetry: true})
		if cfg != nil {
			t.Errorf("Checkpoints = %+v, want none written under the env override", cfg)
		}
	})

	t.Run("re-run of an existing config-less repo stays config-less", func(t *testing.T) {
		setupTestRepo(t)
		// A repo set up before this change: settings.json exists, no
		// checkpoints block. Re-running setup must not inject git-refs.
		writeSettings(t, testSettingsEnabled)
		cfg := enable(t, EnableOptions{Yes: true, Telemetry: true})
		if cfg != nil {
			t.Errorf("Checkpoints = %+v, want none added on a pre-existing setup", cfg)
		}
	})
}

// TestPluginUninstallCommand_QuotesRepoRoot pins that the recovery command
// survives an ordinary repo path. A space is the common case; the metacharacters
// matter because this line is meant to be pasted into a shell, where an
// unquoted path silently runs `cd` against the wrong argument.
func TestPluginUninstallCommand_QuotesRepoRoot(t *testing.T) {
	t.Parallel()

	got := pluginUninstallCommand("/Users/me/My Repo", "flaky")
	if !strings.Contains(got, "cd '/Users/me/My Repo'") {
		t.Errorf("repo root must be quoted for the shell, got:\n%s", got)
	}
	if !strings.Contains(got, "ENTIRE_REPO_ROOT='/Users/me/My Repo'") {
		t.Errorf("env value must be quoted too, got:\n%s", got)
	}
	// A single quote in a path terminates the quoting unless escaped.
	if got := pluginUninstallCommand("/tmp/it's", "flaky"); !strings.Contains(got, `'/tmp/it'\''s'`) {
		t.Errorf("a single quote in the path must be escaped, got:\n%s", got)
	}
}

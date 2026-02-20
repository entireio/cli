package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/go-git/go-git/v5"
)

// Note: Tests for hook manipulation functions (addHookToMatcher, hookCommandExists, etc.)
// have been moved to the agent/claudecode package where these functions now reside.
// See cmd/entire/cli/agent/claudecode/hooks_test.go for those tests.

// setupTestDir creates a temp directory, changes to it, and returns it.
// It also registers cleanup to restore the original directory.
func setupTestDir(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	paths.ClearRepoRootCache()
	return tmpDir
}

// setupTestRepo creates a temp directory with a git repo initialized.
func setupTestRepo(t *testing.T) {
	t.Helper()
	tmpDir := setupTestDir(t)
	if _, err := git.PlainInit(tmpDir, false); err != nil {
		t.Fatalf("Failed to init repo: %v", err)
	}
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

func TestRunEnable(t *testing.T) {
	setupTestDir(t)
	writeSettings(t, testSettingsDisabled)

	var stdout bytes.Buffer
	if err := runEnable(&stdout); err != nil {
		t.Fatalf("runEnable() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "enabled") {
		t.Errorf("Expected output to contain 'enabled', got: %s", stdout.String())
	}

	enabled, err := IsEnabled()
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
	}
	if !enabled {
		t.Error("Entire should be enabled after running enable command")
	}
}

func TestRunEnable_AlreadyEnabled(t *testing.T) {
	setupTestDir(t)
	writeSettings(t, testSettingsEnabled)

	var stdout bytes.Buffer
	if err := runEnable(&stdout); err != nil {
		t.Fatalf("runEnable() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "enabled") {
		t.Errorf("Expected output to mention enabled state, got: %s", stdout.String())
	}
}

func TestRunDisable(t *testing.T) {
	setupTestDir(t)
	writeSettings(t, testSettingsEnabled)

	var stdout bytes.Buffer
	if err := runDisable(&stdout, false); err != nil {
		t.Fatalf("runDisable() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "disabled") {
		t.Errorf("Expected output to contain 'disabled', got: %s", stdout.String())
	}

	enabled, err := IsEnabled()
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
	}
	if enabled {
		t.Error("Entire should be disabled after running disable command")
	}
}

func TestRunDisable_AlreadyDisabled(t *testing.T) {
	setupTestDir(t)
	writeSettings(t, testSettingsDisabled)

	var stdout bytes.Buffer
	if err := runDisable(&stdout, false); err != nil {
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
	if checkDisabledGuard(&stdout) {
		t.Error("checkDisabledGuard() should return false when no settings file exists")
	}
	if stdout.String() != "" {
		t.Errorf("checkDisabledGuard() should not print anything when enabled, got: %s", stdout.String())
	}

	// Settings with enabled: true
	writeSettings(t, testSettingsEnabled)
	stdout.Reset()
	if checkDisabledGuard(&stdout) {
		t.Error("checkDisabledGuard() should return false when enabled")
	}

	// Settings with enabled: false
	writeSettings(t, testSettingsDisabled)
	stdout.Reset()
	if !checkDisabledGuard(&stdout) {
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
	if err := runDisable(&stdout, false); err != nil {
		t.Fatalf("runDisable() error = %v", err)
	}

	// Should be disabled because runDisable updates local settings when it exists
	enabled, err := IsEnabled()
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
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
	if err := runDisable(&stdout, true); err != nil {
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

	// Local settings should still be enabled (untouched)
	localContent, err := os.ReadFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("Failed to read local settings: %v", err)
	}
	if !strings.Contains(string(localContent), `"enabled": true`) && !strings.Contains(string(localContent), `"enabled":true`) {
		t.Errorf("Local settings should still have enabled:true, got: %s", localContent)
	}
}

// TestRunDisable_CreatesLocalSettingsWhenMissing verifies that running
// `entire disable` without --project creates settings.local.json when it
// doesn't exist, rather than writing to settings.json.
func TestRunDisable_CreatesLocalSettingsWhenMissing(t *testing.T) {
	setupTestDir(t)
	// Only create project settings (no local settings)
	writeSettings(t, testSettingsEnabled)

	var stdout bytes.Buffer
	if err := runDisable(&stdout, false); err != nil {
		t.Fatalf("runDisable() error = %v", err)
	}

	// Should be disabled
	enabled, err := IsEnabled()
	if err != nil {
		t.Fatalf("IsEnabled() error = %v", err)
	}
	if enabled {
		t.Error("Entire should be disabled after running disable command")
	}

	// Local settings file should be created with enabled:false
	localContent, err := os.ReadFile(EntireSettingsLocalFile)
	if err != nil {
		t.Fatalf("Local settings file should have been created: %v", err)
	}
	if !strings.Contains(string(localContent), `"enabled":false`) && !strings.Contains(string(localContent), `"enabled": false`) {
		t.Errorf("Local settings should have enabled:false, got: %s", localContent)
	}

	// Project settings should remain unchanged (still enabled)
	projectContent, err := os.ReadFile(EntireSettingsFile)
	if err != nil {
		t.Fatalf("Failed to read project settings: %v", err)
	}
	if !strings.Contains(string(projectContent), `"enabled":true`) && !strings.Contains(string(projectContent), `"enabled": true`) {
		t.Errorf("Project settings should still have enabled:true, got: %s", projectContent)
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

func TestRunEnableWithStrategy_PreservesExistingSettings(t *testing.T) {
	setupTestRepo(t)

	// Create initial settings with strategy_options (like push enabled)
	initialSettings := `{
		"strategy": "manual-commit",
		"enabled": true,
		"strategy_options": {
			"push": true,
			"some_other_option": "value"
		}
	}`
	writeSettings(t, initialSettings)

	// Run enable with a different strategy — pass agents directly (no TTY needed)
	defaultAgent := agent.Default()
	var stdout bytes.Buffer
	err := runEnableWithStrategy(&stdout, []agent.Agent{defaultAgent}, "auto-commit", false, false, true, false, false, false)
	if err != nil {
		t.Fatalf("runEnableWithStrategy() error = %v", err)
	}

	// Load the saved settings and verify strategy_options were preserved
	settings, err := LoadEntireSettings()
	if err != nil {
		t.Fatalf("LoadEntireSettings() error = %v", err)
	}

	// Strategy should be updated
	if settings.Strategy != "auto-commit" {
		t.Errorf("Strategy should be 'auto-commit', got %q", settings.Strategy)
	}

	// strategy_options should be preserved
	if settings.StrategyOptions == nil {
		t.Fatal("strategy_options should be preserved, but got nil")
	}
	if settings.StrategyOptions["push"] != true {
		t.Errorf("strategy_options.push should be true, got %v", settings.StrategyOptions["push"])
	}
	if settings.StrategyOptions["some_other_option"] != "value" {
		t.Errorf("strategy_options.some_other_option should be 'value', got %v", settings.StrategyOptions["some_other_option"])
	}
}

func TestRunEnableWithStrategy_PreservesLocalSettings(t *testing.T) {
	setupTestRepo(t)

	// Create project settings
	writeSettings(t, `{"strategy": "manual-commit", "enabled": true}`)

	// Create local settings with strategy_options
	localSettings := `{
		"strategy_options": {
			"push": true
		}
	}`
	writeLocalSettings(t, localSettings)

	// Run enable with --local flag — pass agents directly (no TTY needed)
	defaultAgent := agent.Default()
	var stdout bytes.Buffer
	err := runEnableWithStrategy(&stdout, []agent.Agent{defaultAgent}, "auto-commit", false, true, false, false, false, false)
	if err != nil {
		t.Fatalf("runEnableWithStrategy() error = %v", err)
	}

	// Load the merged settings (project + local)
	settings, err := LoadEntireSettings()
	if err != nil {
		t.Fatalf("LoadEntireSettings() error = %v", err)
	}

	// Strategy should be updated (from local)
	if settings.Strategy != "auto-commit" {
		t.Errorf("Strategy should be 'auto-commit', got %q", settings.Strategy)
	}

	// strategy_options.push should be preserved
	if settings.StrategyOptions == nil {
		t.Fatal("strategy_options should be preserved, but got nil")
	}
	if settings.StrategyOptions["push"] != true {
		t.Errorf("strategy_options.push should be true, got %v", settings.StrategyOptions["push"])
	}
}

// Tests for runUninstall and helper functions

func TestRunUninstall_Force_NothingInstalled(t *testing.T) {
	setupTestRepo(t)

	var stdout, stderr bytes.Buffer
	err := runUninstall(&stdout, &stderr, true)
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
	err := runUninstall(&stdout, &stderr, true)
	if err != nil {
		t.Fatalf("runUninstall() error = %v", err)
	}

	// Verify directory is removed
	if _, err := os.Stat(entireDir); !os.IsNotExist(err) {
		t.Error(".entire directory should be removed after uninstall")
	}

	output := stdout.String()
	if !strings.Contains(output, "uninstalled successfully") {
		t.Errorf("Expected success message, got: %s", output)
	}
}

func TestRunUninstall_Force_RemovesGitHooks(t *testing.T) {
	setupTestRepo(t)

	// Create .entire directory (required for git hooks)
	writeSettings(t, testSettingsEnabled)

	// Install git hooks
	if _, err := strategy.InstallGitHook(true); err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}

	// Verify hooks are installed
	if !strategy.IsGitHookInstalled() {
		t.Fatal("git hooks should be installed before uninstall")
	}

	var stdout, stderr bytes.Buffer
	err := runUninstall(&stdout, &stderr, true)
	if err != nil {
		t.Fatalf("runUninstall() error = %v", err)
	}

	// Verify hooks are removed
	if strategy.IsGitHookInstalled() {
		t.Error("git hooks should be removed after uninstall")
	}

	output := stdout.String()
	if !strings.Contains(output, "Removed git hooks") {
		t.Errorf("Expected output to mention removed git hooks, got: %s", output)
	}
}

func TestRunUninstall_NotAGitRepo(t *testing.T) {
	// Create a temp directory without git init
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	paths.ClearRepoRootCache()

	var stdout, stderr bytes.Buffer
	err := runUninstall(&stdout, &stderr, true)

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
	if checkEntireDirExists() {
		t.Error("checkEntireDirExists() should return false when .entire doesn't exist")
	}

	// Create the directory
	if err := os.MkdirAll(paths.EntireDir, 0o755); err != nil {
		t.Fatalf("Failed to create .entire dir: %v", err)
	}

	// Should be true now
	if !checkEntireDirExists() {
		t.Error("checkEntireDirExists() should return true when .entire exists")
	}
}

func TestCountSessionStates(t *testing.T) {
	setupTestRepo(t)

	// Should be 0 when no session states exist
	count := countSessionStates()
	if count != 0 {
		t.Errorf("countSessionStates() = %d, want 0", count)
	}
}

func TestCountShadowBranches(t *testing.T) {
	setupTestRepo(t)

	// Should be 0 when no shadow branches exist
	count := countShadowBranches()
	if count != 0 {
		t.Errorf("countShadowBranches() = %d, want 0", count)
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
	if err := removeEntireDirectory(); err != nil {
		t.Fatalf("removeEntireDirectory() error = %v", err)
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
	if err := removeEntireDirectory(); err != nil {
		t.Fatalf("removeEntireDirectory() should not error when directory doesn't exist: %v", err)
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

// Tests for canPromptInteractively

func TestCanPromptInteractively_EnvVar_True(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Setenv
	t.Setenv("ENTIRE_TEST_TTY", "1")

	if !canPromptInteractively() {
		t.Error("canPromptInteractively() = false, want true when ENTIRE_TEST_TTY=1")
	}
}

func TestCanPromptInteractively_EnvVar_False(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Setenv
	t.Setenv("ENTIRE_TEST_TTY", "0")

	if canPromptInteractively() {
		t.Error("canPromptInteractively() = true, want false when ENTIRE_TEST_TTY=0")
	}
}

func TestCanPromptInteractively_EnvVar_OtherValue(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Setenv
	t.Setenv("ENTIRE_TEST_TTY", "yes") // Not "1", so should be false

	if canPromptInteractively() {
		t.Error("canPromptInteractively() = true, want false when ENTIRE_TEST_TTY is set but not '1'")
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

	var buf bytes.Buffer
	agents, err := detectOrSelectAgent(&buf, nil)
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
	agents, err := detectOrSelectAgent(&buf, nil)
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

func TestDetectOrSelectAgent_NoDetection_NoTTY_FallsBackToDefault(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir and t.Setenv
	setupTestRepo(t)
	t.Setenv("ENTIRE_TEST_TTY", "0") // No TTY available

	// No .claude or .gemini directory - detection will fail

	var buf bytes.Buffer
	agents, err := detectOrSelectAgent(&buf, nil)
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
	agents, err := detectOrSelectAgent(&buf, selectFn)
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
	if !strings.Contains(output, "No agent configuration detected") {
		t.Errorf("Expected output to contain 'No agent configuration detected', got: %s", output)
	}
	if !strings.Contains(output, "This is normal") {
		t.Errorf("Expected output to contain 'This is normal', got: %s", output)
	}
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
	_, err := detectOrSelectAgent(&buf, selectFn)
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
	_, err := detectOrSelectAgent(&buf, selectFn)
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
	agents, err := detectOrSelectAgent(&buf, selectFn)
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
	t.Setenv("ENTIRE_TEST_TTY", "0") // No TTY available

	// Create both .claude and .gemini directories
	if err := os.MkdirAll(".claude", 0o755); err != nil {
		t.Fatalf("Failed to create .claude directory: %v", err)
	}
	if err := os.MkdirAll(".gemini", 0o755); err != nil {
		t.Fatalf("Failed to create .gemini directory: %v", err)
	}

	var buf bytes.Buffer
	agents, err := detectOrSelectAgent(&buf, nil)
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
	installed := GetAgentsWithHooksInstalled()
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
	agents, err := detectOrSelectAgent(&buf, selectFn)
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
	t.Setenv("ENTIRE_TEST_TTY", "0") // No TTY available

	// Install Claude Code hooks
	writeClaudeHooksFixture(t)

	var buf bytes.Buffer
	agents, err := detectOrSelectAgent(&buf, nil)
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
	hookAgent, ok := ag.(agent.HookSupport)
	if !ok {
		return false
	}
	return hookAgent.AreHooksInstalled()
}

// checkGeminiCLIHooksInstalled checks if Gemini CLI hooks are installed.
func checkGeminiCLIHooksInstalled() bool {
	ag, err := agent.Get(agent.AgentNameGemini)
	if err != nil {
		return false
	}
	hookAgent, ok := ag.(agent.HookSupport)
	if !ok {
		return false
	}
	return hookAgent.AreHooksInstalled()
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
	err := uninstallDeselectedAgentHooks(&buf, []agent.Agent{})
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
	err = uninstallDeselectedAgentHooks(&buf, []agent.Agent{claudeAgent})
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
	installed := GetAgentsWithHooksInstalled()
	if len(installed) < 2 {
		t.Fatalf("Expected at least 2 agents installed, got %d", len(installed))
	}

	// Keep only Claude Code selected (deselect Gemini)
	claudeAgent, err := agent.Get(agent.AgentNameClaudeCode)
	if err != nil {
		t.Fatalf("Failed to get claude-code agent: %v", err)
	}

	var buf bytes.Buffer
	err = uninstallDeselectedAgentHooks(&buf, []agent.Agent{claudeAgent})
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
	agents, err := detectOrSelectAgent(&buf, selectFn)
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
	_, err := detectOrSelectAgent(&buf, selectFn)
	if err == nil {
		t.Fatal("Expected error when no agents selected on re-run")
	}
	if !strings.Contains(err.Error(), "no agents selected") {
		t.Errorf("Expected 'no agents selected' error, got: %v", err)
	}
}

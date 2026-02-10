package cli

import (
	"bytes"
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

	// Run enable with a different strategy
	var stdout bytes.Buffer
	err := runEnableWithStrategy(&stdout, "auto-commit", false, false, false, true, false, false, false)
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

	// Run enable with --local flag
	var stdout bytes.Buffer
	err := runEnableWithStrategy(&stdout, "auto-commit", false, false, true, false, false, false, false)
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

func TestIsFullyEnabled_NotEnabled(t *testing.T) {
	setupTestDir(t)

	// No settings, no hooks, no directory - should not be fully enabled
	enabled, _, _ := isFullyEnabled()
	if enabled {
		t.Error("isFullyEnabled() should return false when nothing is set up")
	}
}

func TestIsFullyEnabled_SettingsDisabled(t *testing.T) {
	setupTestDir(t)
	writeSettings(t, testSettingsDisabled)

	enabled, _, _ := isFullyEnabled()
	if enabled {
		t.Error("isFullyEnabled() should return false when settings have enabled=false")
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
				if err != errUnsupportedShell {
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

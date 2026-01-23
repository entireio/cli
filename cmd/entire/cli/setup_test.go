package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/paths"
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

func TestRunStatus_Enabled(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	var stdout bytes.Buffer
	if err := runStatus(&stdout); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "Project, enabled") {
		t.Errorf("Expected output to show 'Project, enabled', got: %s", stdout.String())
	}
}

func TestRunStatus_Disabled(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsDisabled)

	var stdout bytes.Buffer
	if err := runStatus(&stdout); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "Project, disabled") {
		t.Errorf("Expected output to show 'Project, disabled', got: %s", stdout.String())
	}
}

func TestRunStatus_NotSetUp(t *testing.T) {
	setupTestRepo(t)

	var stdout bytes.Buffer
	if err := runStatus(&stdout); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "○ not set up") {
		t.Errorf("Expected output to show '○ not set up', got: %s", output)
	}
	if !strings.Contains(output, "entire enable") {
		t.Errorf("Expected output to mention 'entire enable', got: %s", output)
	}
}

func TestRunStatus_NotGitRepository(t *testing.T) {
	setupTestDir(t) // No git init

	var stdout bytes.Buffer
	if err := runStatus(&stdout); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "✕ not a git repository") {
		t.Errorf("Expected output to show '✕ not a git repository', got: %s", stdout.String())
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
		},
		"agent_options": {
			"claude-code": {
				"ignore_untracked": true
			}
		}
	}`
	writeSettings(t, initialSettings)

	// Run enable with a different strategy
	var stdout bytes.Buffer
	err := runEnableWithStrategy(&stdout, "auto-commit", false, false, false, true, false, false, false, false, false)
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

	// agent_options should be preserved
	if settings.AgentOptions == nil {
		t.Fatal("agent_options should be preserved, but got nil")
	}
	claudeOpts, ok := settings.AgentOptions["claude-code"].(map[string]interface{})
	if !ok {
		t.Fatal("agent_options.claude-code should exist")
	}
	if claudeOpts["ignore_untracked"] != true {
		t.Errorf("agent_options.claude-code.ignore_untracked should be true, got %v", claudeOpts["ignore_untracked"])
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
	err := runEnableWithStrategy(&stdout, "auto-commit", false, false, true, false, false, false, false, false, false)
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

func TestRunStatus_LocalSettingsOnly(t *testing.T) {
	setupTestRepo(t)
	writeLocalSettings(t, `{"strategy": "auto-commit", "enabled": true}`)

	var stdout bytes.Buffer
	if err := runStatus(&stdout); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Local, enabled") {
		t.Errorf("Expected output to show 'Local, enabled', got: %s", output)
	}
	if strings.Contains(output, "Project,") {
		t.Errorf("Should not show Project settings when only local exists, got: %s", output)
	}
}

func TestRunStatus_BothProjectAndLocal(t *testing.T) {
	setupTestRepo(t)
	// Project: enabled=true, strategy=manual-commit
	// Local: enabled=false, strategy=auto-commit
	// Merged: enabled=false (local overrides), strategy=auto-commit (local overrides)
	writeSettings(t, `{"strategy": "manual-commit", "enabled": true}`)
	writeLocalSettings(t, `{"strategy": "auto-commit", "enabled": false}`)

	var stdout bytes.Buffer
	if err := runStatus(&stdout); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	// Should show merged settings with "Project + Local" source label
	if !strings.Contains(output, "Project + Local, disabled (auto-commit)") {
		t.Errorf("Expected output to show 'Project + Local, disabled (auto-commit)', got: %s", output)
	}
}

func TestRunStatus_ShowsStrategy(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, `{"strategy": "auto-commit", "enabled": true}`)

	var stdout bytes.Buffer
	if err := runStatus(&stdout); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "(auto-commit)") {
		t.Errorf("Expected output to show strategy '(auto-commit)', got: %s", output)
	}
}

func TestRunStatus_ShowsManualCommitStrategy(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, `{"strategy": "manual-commit", "enabled": false}`)

	var stdout bytes.Buffer
	if err := runStatus(&stdout); err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "Project, disabled (manual-commit)") {
		t.Errorf("Expected output to show 'Project, disabled (manual-commit)', got: %s", output)
	}
}

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/go-git/go-git/v5"
)

// Note: Tests for hook manipulation functions (addHookToMatcher, hookCommandExists, etc.)
// have been moved to the agent/claudecode package where these functions now reside.
// See cmd/entire/cli/agent/claudecode/hooks_test.go for those tests.

func TestFindStanzaVersion(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{"empty", "", 0},
		{"no stanza", "some random content\nmore lines\n", 0},
		{"v1", "# Entire CLI completions (v1) {{{\nsome line\n# }}}", 1},
		{"v2", "# Entire CLI completions (v2) {{{\nsome line\n# }}}", 2},
		{"legacy only", "# Entire CLI shell completion\nsource <(entire completion zsh)\n", 0},
		{"with surrounding content", "export PATH=/usr/bin\n# Entire CLI completions (v1) {{{\nautoload\n# }}}\nalias ls=ll\n", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findStanzaVersion(tt.content); got != tt.want {
				t.Errorf("findStanzaVersion() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCheckStanzaStatus(t *testing.T) {
	tests := []struct {
		name           string
		content        string
		currentVersion int
		want           stanzaStatus
	}{
		{"not found", "some content\n", 1, stanzaNotFound},
		{"current exact", "# Entire CLI completions (v1) {{{\nline\n# }}}", 1, stanzaCurrent},
		{"current higher version", "# Entire CLI completions (v3) {{{\nline\n# }}}", 2, stanzaCurrent},
		{"outdated", "# Entire CLI completions (v1) {{{\nline\n# }}}", 2, stanzaOutdated},
		{"legacy", "# Entire CLI shell completion\nsource <(entire completion zsh)\n", 1, stanzaLegacy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := checkStanzaStatus(tt.content, tt.currentVersion); got != tt.want {
				t.Errorf("checkStanzaStatus() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBuildStanza(t *testing.T) {
	got := buildStanza(1, "autoload -Uz compinit && compinit && source <(entire completion zsh)")
	want := "# Entire CLI completions (v1) {{{\nautoload -Uz compinit && compinit && source <(entire completion zsh)\n# }}}"
	if got != want {
		t.Errorf("buildStanza() =\n%s\nwant:\n%s", got, want)
	}
}

func TestCountStanzaOpeners(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{"none", "some content\n", 0},
		{"one", "# Entire CLI completions (v1) {{{\nline\n# }}}", 1},
		{"two", "# Entire CLI completions (v1) {{{\nline\n# }}}\n# Entire CLI completions (v1) {{{\nline\n# }}}", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countStanzaOpeners(tt.content); got != tt.want {
				t.Errorf("countStanzaOpeners() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestReplaceStanza(t *testing.T) {
	tests := []struct {
		name    string
		content string
		version int
		line    string
		want    string
		wantErr error
	}{
		{
			name:    "happy path",
			content: "before\n# Entire CLI completions (v1) {{{\nold line\n# }}}\nafter",
			version: 2,
			line:    "new line",
			want:    "before\n# Entire CLI completions (v2) {{{\nnew line\n# }}}\nafter",
		},
		{
			name:    "no stanza",
			content: "just some content\n",
			version: 1,
			line:    "line",
			want:    "just some content\n",
		},
		{
			name:    "corrupted no close",
			content: "# Entire CLI completions (v1) {{{\nsome line\nno close marker",
			version: 2,
			line:    "line",
			wantErr: errStanzaCorrupted,
		},
		{
			name:    "multiple stanzas",
			content: "# Entire CLI completions (v1) {{{\nline\n# }}}\n# Entire CLI completions (v1) {{{\nline\n# }}}",
			version: 2,
			line:    "line",
			wantErr: errMultipleStanzas,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := replaceStanza(tt.content, tt.version, tt.line)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("replaceStanza() error = %v, wantErr %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("replaceStanza() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("replaceStanza() =\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestRemoveStanza(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
		wantErr error
	}{
		{
			name:    "happy path",
			content: "before\n# Entire CLI completions (v1) {{{\nold line\n# }}}\nafter",
			want:    "before\nafter",
		},
		{
			name:    "with blank line after",
			content: "before\n# Entire CLI completions (v1) {{{\nold line\n# }}}\n\nafter",
			want:    "before\nafter",
		},
		{
			name:    "no stanza",
			content: "just some content\n",
			want:    "just some content\n",
		},
		{
			name:    "corrupted",
			content: "# Entire CLI completions (v1) {{{\nno close",
			wantErr: errStanzaCorrupted,
		},
		{
			name:    "multiple stanzas",
			content: "# Entire CLI completions (v1) {{{\na\n# }}}\n# Entire CLI completions (v1) {{{\nb\n# }}}",
			wantErr: errMultipleStanzas,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := removeStanza(tt.content)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("removeStanza() error = %v, wantErr %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("removeStanza() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("removeStanza() =\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

func TestRemoveLegacyCompletion(t *testing.T) {
	completionLine := "source <(entire completion bash)"
	tests := []struct {
		name        string
		content     string
		wantContent string
		wantRemoved bool
	}{
		{
			name:        "legacy present",
			content:     "before\n# Entire CLI shell completion\nsource <(entire completion bash)\nafter",
			wantContent: "before\nafter",
			wantRemoved: true,
		},
		{
			name:        "not present",
			content:     "before\nafter",
			wantContent: "before\nafter",
			wantRemoved: false,
		},
		{
			name:        "comment without matching next line",
			content:     "before\n# Entire CLI shell completion\nsome other line\nafter",
			wantContent: "before\nsome other line\nafter",
			wantRemoved: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, removed := removeLegacyCompletion(tt.content, completionLine)
			if removed != tt.wantRemoved {
				t.Errorf("removeLegacyCompletion() removed = %v, want %v", removed, tt.wantRemoved)
			}
			if got != tt.wantContent {
				t.Errorf("removeLegacyCompletion() =\n%q\nwant:\n%q", got, tt.wantContent)
			}
		})
	}
}

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

func TestRemoveEntireDirectory_NotExists(t *testing.T) {
	setupTestDir(t)

	// Should not error when directory doesn't exist
	if err := removeEntireDirectory(); err != nil {
		t.Fatalf("removeEntireDirectory() should not error when directory doesn't exist: %v", err)
	}
}

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testSettingsEnabled  = `{"enabled": true}`
	testSettingsDisabled = `{"enabled": false}`
)

func TestLoadEntireSettings_EnabledDefaultsToTrue(t *testing.T) {
	// Create a temporary directory and change to it (auto-restored after test)
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Test 1: No settings file exists - should default to enabled
	settings, err := LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadEntireSettings(context.Background()) error = %v", err)
	}
	if !settings.Enabled {
		t.Error("Enabled should default to true when no settings file exists")
	}

	// Test 2: Settings file exists without enabled field - should default to true
	settingsDir := filepath.Dir(EntireSettingsFile)
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatalf("Failed to create settings dir: %v", err)
	}
	settingsContent := `{}`
	if err := os.WriteFile(EntireSettingsFile, []byte(settingsContent), 0o644); err != nil {
		t.Fatalf("Failed to write settings file: %v", err)
	}

	settings, err = LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadEntireSettings(context.Background()) error = %v", err)
	}
	if !settings.Enabled {
		t.Error("Enabled should default to true when field is missing from JSON")
	}

	// Test 3: Settings file with enabled: false - should be false
	settingsContent = testSettingsDisabled
	if err := os.WriteFile(EntireSettingsFile, []byte(settingsContent), 0o644); err != nil {
		t.Fatalf("Failed to write settings file: %v", err)
	}

	settings, err = LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadEntireSettings(context.Background()) error = %v", err)
	}
	if settings.Enabled {
		t.Error("Enabled should be false when explicitly set to false")
	}

	// Test 4: Settings file with enabled: true - should be true
	settingsContent = testSettingsEnabled
	if err := os.WriteFile(EntireSettingsFile, []byte(settingsContent), 0o644); err != nil {
		t.Fatalf("Failed to write settings file: %v", err)
	}

	settings, err = LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadEntireSettings(context.Background()) error = %v", err)
	}
	if !settings.Enabled {
		t.Error("Enabled should be true when explicitly set to true")
	}
}

func TestSaveEntireSettings_PreservesEnabled(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Save settings with Enabled = false
	settings := &EntireSettings{
		Enabled: false,
	}
	if err := SaveEntireSettings(context.Background(), settings); err != nil {
		t.Fatalf("SaveEntireSettings() error = %v", err)
	}

	// Load and verify
	loaded, err := LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadEntireSettings(context.Background()) error = %v", err)
	}
	if loaded.Enabled {
		t.Error("Enabled should be false after saving as false")
	}
}

func TestIsEnabled(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Test 1: No settings file - should return true (default)
	enabled, err := IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled(context.Background()) error = %v", err)
	}
	if !enabled {
		t.Error("IsEnabled(context.Background()) should return true when no settings file exists")
	}

	// Test 2: Settings with enabled: false - should return false
	settingsDir := filepath.Dir(EntireSettingsFile)
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatalf("Failed to create settings dir: %v", err)
	}
	settingsContent := `{"enabled": false}`
	if err := os.WriteFile(EntireSettingsFile, []byte(settingsContent), 0o644); err != nil {
		t.Fatalf("Failed to write settings file: %v", err)
	}

	enabled, err = IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled(context.Background()) error = %v", err)
	}
	if enabled {
		t.Error("IsEnabled(context.Background()) should return false when disabled")
	}

	// Test 3: Settings with enabled: true - should return true
	settingsContent = testSettingsEnabled
	if err := os.WriteFile(EntireSettingsFile, []byte(settingsContent), 0o644); err != nil {
		t.Fatalf("Failed to write settings file: %v", err)
	}

	enabled, err = IsEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsEnabled(context.Background()) error = %v", err)
	}
	if !enabled {
		t.Error("IsEnabled(context.Background()) should return true when enabled")
	}
}

// setupLocalOverrideTestDir creates a temp directory with .entire folder for testing
func setupLocalOverrideTestDir(t *testing.T) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	settingsDir := filepath.Dir(EntireSettingsFile)
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatalf("Failed to create settings dir: %v", err)
	}
}

func TestLoadEntireSettings_LocalOverridesStrategy(t *testing.T) {
	setupLocalOverrideTestDir(t)

	baseSettings := testSettingsEnabled
	if err := os.WriteFile(EntireSettingsFile, []byte(baseSettings), 0o644); err != nil {
		t.Fatalf("Failed to write settings file: %v", err)
	}

	localSettings := testSettingsEnabled
	if err := os.WriteFile(EntireSettingsLocalFile, []byte(localSettings), 0o644); err != nil {
		t.Fatalf("Failed to write local settings file: %v", err)
	}

	settings, err := LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadEntireSettings(context.Background()) error = %v", err)
	}
	if !settings.Enabled {
		t.Error("Enabled should remain true from base settings")
	}
}

func TestLoadEntireSettings_LocalOverridesEnabled(t *testing.T) {
	setupLocalOverrideTestDir(t)

	baseSettings := testSettingsEnabled
	if err := os.WriteFile(EntireSettingsFile, []byte(baseSettings), 0o644); err != nil {
		t.Fatalf("Failed to write settings file: %v", err)
	}

	localSettings := `{"enabled": false}`
	if err := os.WriteFile(EntireSettingsLocalFile, []byte(localSettings), 0o644); err != nil {
		t.Fatalf("Failed to write local settings file: %v", err)
	}

	settings, err := LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadEntireSettings(context.Background()) error = %v", err)
	}
	if settings.Enabled {
		t.Error("Enabled should be false from local override")
	}
}

func TestLoadEntireSettings_LocalOverridesLocalDev(t *testing.T) {
	setupLocalOverrideTestDir(t)

	baseSettings := testSettingsEnabled
	if err := os.WriteFile(EntireSettingsFile, []byte(baseSettings), 0o644); err != nil {
		t.Fatalf("Failed to write settings file: %v", err)
	}

	localSettings := `{"local_dev": true}`
	if err := os.WriteFile(EntireSettingsLocalFile, []byte(localSettings), 0o644); err != nil {
		t.Fatalf("Failed to write local settings file: %v", err)
	}

	settings, err := LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadEntireSettings(context.Background()) error = %v", err)
	}
	if !settings.LocalDev {
		t.Error("LocalDev should be true from local override")
	}
}

func TestLoadEntireSettings_LocalMergesStrategyOptions(t *testing.T) {
	setupLocalOverrideTestDir(t)

	baseSettings := `{"enabled": true, "strategy_options": {"key1": "value1", "key2": "value2"}}`
	if err := os.WriteFile(EntireSettingsFile, []byte(baseSettings), 0o644); err != nil {
		t.Fatalf("Failed to write settings file: %v", err)
	}

	localSettings := `{"strategy_options": {"key2": "overridden", "key3": "value3"}}`
	if err := os.WriteFile(EntireSettingsLocalFile, []byte(localSettings), 0o644); err != nil {
		t.Fatalf("Failed to write local settings file: %v", err)
	}

	settings, err := LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadEntireSettings(context.Background()) error = %v", err)
	}

	if settings.StrategyOptions["key1"] != "value1" {
		t.Errorf("key1 should remain 'value1', got %v", settings.StrategyOptions["key1"])
	}
	if settings.StrategyOptions["key2"] != "overridden" {
		t.Errorf("key2 should be 'overridden', got %v", settings.StrategyOptions["key2"])
	}
	if settings.StrategyOptions["key3"] != "value3" {
		t.Errorf("key3 should be 'value3', got %v", settings.StrategyOptions["key3"])
	}
}

func TestLoadEntireSettings_OnlyLocalFileExists(t *testing.T) {
	setupLocalOverrideTestDir(t)

	// No base settings file
	localSettings := testSettingsEnabled
	if err := os.WriteFile(EntireSettingsLocalFile, []byte(localSettings), 0o644); err != nil {
		t.Fatalf("Failed to write local settings file: %v", err)
	}

	settings, err := LoadEntireSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadEntireSettings(context.Background()) error = %v", err)
	}
	if !settings.Enabled {
		t.Error("Enabled should default to true")
	}
}

func TestLoadEntireSettings_RejectsUnknownKeysInBase(t *testing.T) {
	setupLocalOverrideTestDir(t)

	baseSettings := `{"bogus_key": true}`
	if err := os.WriteFile(EntireSettingsFile, []byte(baseSettings), 0o644); err != nil {
		t.Fatalf("Failed to write settings file: %v", err)
	}

	_, err := LoadEntireSettings(context.Background())
	if err == nil {
		t.Fatal("LoadEntireSettings(context.Background()) should return error for unknown key")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("Error should mention 'unknown field', got: %v", err)
	}
}

// TestGetStrategy_HasBlobFetcher verifies that GetStrategy returns a strategy
// with blob fetching enabled. Without this, checkpoint reads after a treeless
// fetch (--filter=blob:none) silently fail because blobs are not local and
// FetchingTree has no fetcher to download them — causing "session log not
// available" errors during resume.
//
// Regression test for the bug introduced in b92b37b3 where Read and
// ReadSessionContent were changed to use FetchingTree but GetStrategy did not
// configure a blob fetcher on the strategy.
func TestGetStrategy_HasBlobFetcher(t *testing.T) {
	t.Parallel()

	strat := GetStrategy(context.Background())
	if !strat.HasBlobFetcher() {
		t.Fatal("GetStrategy must return a strategy with a blob fetcher configured; " +
			"without it, checkpoint reads fail after treeless fetches")
	}
}

func TestLoadEntireSettings_RejectsUnknownKeysInLocal(t *testing.T) {
	setupLocalOverrideTestDir(t)

	baseSettings := `{}`
	if err := os.WriteFile(EntireSettingsFile, []byte(baseSettings), 0o644); err != nil {
		t.Fatalf("Failed to write settings file: %v", err)
	}

	localSettings := `{"bogus_key": "value"}`
	if err := os.WriteFile(EntireSettingsLocalFile, []byte(localSettings), 0o644); err != nil {
		t.Fatalf("Failed to write local settings file: %v", err)
	}

	_, err := LoadEntireSettings(context.Background())
	if err == nil {
		t.Fatal("LoadEntireSettings(context.Background()) should return error for unknown key in local settings")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("Error should mention 'unknown field', got: %v", err)
	}
}

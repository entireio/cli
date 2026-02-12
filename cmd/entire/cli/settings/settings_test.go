package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/redact"
)

func TestLoad_RejectsUnknownKeys(t *testing.T) {
	// Create a temporary directory
	tmpDir := t.TempDir()

	// Create .entire directory
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	// Create settings.json with an unknown key
	settingsFile := filepath.Join(entireDir, "settings.json")
	settingsContent := `{"strategy": "manual-commit", "unknown_key": "value"}`
	if err := os.WriteFile(settingsFile, []byte(settingsContent), 0644); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}

	// Initialize a git repo (required by paths.AbsPath)
	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0755); err != nil {
		t.Fatalf("failed to create .git directory: %v", err)
	}

	// Change to the temp directory
	t.Chdir(tmpDir)

	// Try to load settings - should fail due to unknown key
	_, err := Load()
	if err == nil {
		t.Error("expected error for unknown key, got nil")
	} else if !containsUnknownField(err.Error()) {
		t.Errorf("expected unknown field error, got: %v", err)
	}
}

func TestLoad_AcceptsValidKeys(t *testing.T) {
	// Create a temporary directory
	tmpDir := t.TempDir()

	// Create .entire directory
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	// Create settings.json with all valid keys
	settingsFile := filepath.Join(entireDir, "settings.json")
	settingsContent := `{
		"strategy": "auto-commit",
		"enabled": true,
		"local_dev": false,
		"log_level": "debug",
		"strategy_options": {"key": "value"},
		"telemetry": true
	}`
	if err := os.WriteFile(settingsFile, []byte(settingsContent), 0644); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}

	// Initialize a git repo (required by paths.AbsPath)
	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0755); err != nil {
		t.Fatalf("failed to create .git directory: %v", err)
	}

	// Change to the temp directory
	t.Chdir(tmpDir)

	// Load settings - should succeed
	settings, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify values
	if settings.Strategy != "auto-commit" {
		t.Errorf("expected strategy 'auto-commit', got %q", settings.Strategy)
	}
	if !settings.Enabled {
		t.Error("expected enabled to be true")
	}
	if settings.LogLevel != "debug" {
		t.Errorf("expected log_level 'debug', got %q", settings.LogLevel)
	}
	if settings.Telemetry == nil || !*settings.Telemetry {
		t.Error("expected telemetry to be true")
	}
}

func TestLoad_LocalSettingsRejectsUnknownKeys(t *testing.T) {
	// Create a temporary directory
	tmpDir := t.TempDir()

	// Create .entire directory
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	// Create valid settings.json
	settingsFile := filepath.Join(entireDir, "settings.json")
	settingsContent := `{"strategy": "manual-commit"}`
	if err := os.WriteFile(settingsFile, []byte(settingsContent), 0644); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}

	// Create settings.local.json with an unknown key
	localSettingsFile := filepath.Join(entireDir, "settings.local.json")
	localSettingsContent := `{"bad_key": true}`
	if err := os.WriteFile(localSettingsFile, []byte(localSettingsContent), 0644); err != nil {
		t.Fatalf("failed to write local settings file: %v", err)
	}

	// Initialize a git repo (required by paths.AbsPath)
	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0755); err != nil {
		t.Fatalf("failed to create .git directory: %v", err)
	}

	// Change to the temp directory
	t.Chdir(tmpDir)

	// Try to load settings - should fail due to unknown key in local settings
	_, err := Load()
	if err == nil {
		t.Error("expected error for unknown key in local settings, got nil")
	} else if !containsUnknownField(err.Error()) {
		t.Errorf("expected unknown field error, got: %v", err)
	}
}

// containsUnknownField checks if the error message indicates an unknown field
func containsUnknownField(msg string) bool {
	// Go's json package reports unknown fields with this message format
	return strings.Contains(msg, "unknown field")
}

func TestEntireSettings_GetShowcaseConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		settings *EntireSettings
		wantNil  bool
		validate func(*testing.T, *redact.ShowcaseConfig)
	}{
		{
			name: "showcase config present with all fields",
			settings: &EntireSettings{
				StrategyOptions: map[string]any{
					"showcase": map[string]any{
						"redact_paths":        true,
						"redact_usernames":    true,
						"redact_project_info": false,
						"allowed_paths":       []any{"src/", "lib/"},
						"allowed_domains":     []any{"@example.com"},
						"custom_blocklist":    []any{"acme-corp", "project-*"},
					},
				},
			},
			wantNil: false,
			validate: func(t *testing.T, cfg *redact.ShowcaseConfig) {
				if !cfg.RedactPaths {
					t.Error("expected RedactPaths to be true")
				}
				if !cfg.RedactUsernames {
					t.Error("expected RedactUsernames to be true")
				}
				if cfg.RedactProjectInfo {
					t.Error("expected RedactProjectInfo to be false")
				}
				if len(cfg.AllowedPaths) != 2 {
					t.Errorf("expected 2 allowed paths, got %d", len(cfg.AllowedPaths))
				}
				if len(cfg.AllowedDomains) != 1 {
					t.Errorf("expected 1 allowed domain, got %d", len(cfg.AllowedDomains))
				}
				if len(cfg.CustomBlocklist) != 2 {
					t.Errorf("expected 2 blocklist items, got %d", len(cfg.CustomBlocklist))
				}
			},
		},
		{
			name: "showcase config with defaults",
			settings: &EntireSettings{
				StrategyOptions: map[string]any{
					"showcase": map[string]any{},
				},
			},
			wantNil: false,
			validate: func(t *testing.T, cfg *redact.ShowcaseConfig) {
				// Should return defaults from DefaultShowcaseConfig()
				defaults := redact.DefaultShowcaseConfig()
				if cfg.RedactPaths != defaults.RedactPaths {
					t.Error("expected default RedactPaths")
				}
				if cfg.RedactUsernames != defaults.RedactUsernames {
					t.Error("expected default RedactUsernames")
				}
				if cfg.RedactProjectInfo != defaults.RedactProjectInfo {
					t.Error("expected default RedactProjectInfo")
				}
			},
		},
		{
			name: "no showcase config",
			settings: &EntireSettings{
				StrategyOptions: map[string]any{
					"other": map[string]any{},
				},
			},
			wantNil: true,
		},
		{
			name:     "nil StrategyOptions",
			settings: &EntireSettings{},
			wantNil:  true,
		},
		{
			name: "showcase is not a map",
			settings: &EntireSettings{
				StrategyOptions: map[string]any{
					"showcase": "invalid",
				},
			},
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.settings.GetShowcaseConfig()
			if tt.wantNil {
				if cfg != nil {
					t.Errorf("expected nil config, got %+v", cfg)
				}
			} else {
				if cfg == nil {
					t.Fatal("expected non-nil config, got nil")
				}
				if tt.validate != nil {
					tt.validate(t, cfg)
				}
			}
		})
	}
}

func TestEntireSettings_IsShowcaseEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		settings *EntireSettings
		want     bool
	}{
		{
			name: "showcase config present",
			settings: &EntireSettings{
				StrategyOptions: map[string]any{
					"showcase": map[string]any{
						"redact_paths": true,
					},
				},
			},
			want: true,
		},
		{
			name: "showcase config missing",
			settings: &EntireSettings{
				StrategyOptions: map[string]any{
					"other": map[string]any{},
				},
			},
			want: false,
		},
		{
			name:     "nil StrategyOptions",
			settings: &EntireSettings{},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.settings.IsShowcaseEnabled()
			if got != tt.want {
				t.Errorf("IsShowcaseEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetShowcaseConfig_ArrayParsing(t *testing.T) {
	t.Parallel()

	settings := &EntireSettings{
		StrategyOptions: map[string]any{
			"showcase": map[string]any{
				"allowed_paths":    []any{"src/", "lib/", "cmd/"},
				"allowed_domains":  []any{"@example.com", "@test.org"},
				"custom_blocklist": []any{"term1", "term2", "term3"},
			},
		},
	}

	cfg := settings.GetShowcaseConfig()
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}

	// Check allowed_paths
	if len(cfg.AllowedPaths) != 3 {
		t.Errorf("expected 3 allowed paths, got %d", len(cfg.AllowedPaths))
	}
	expectedPaths := []string{"src/", "lib/", "cmd/"}
	for i, expected := range expectedPaths {
		if i >= len(cfg.AllowedPaths) || cfg.AllowedPaths[i] != expected {
			t.Errorf("allowed_paths[%d] = %q, want %q", i, cfg.AllowedPaths[i], expected)
		}
	}

	// Check allowed_domains
	if len(cfg.AllowedDomains) != 2 {
		t.Errorf("expected 2 allowed domains, got %d", len(cfg.AllowedDomains))
	}

	// Check custom_blocklist
	if len(cfg.CustomBlocklist) != 3 {
		t.Errorf("expected 3 blocklist items, got %d", len(cfg.CustomBlocklist))
	}
}

func TestGetShowcaseConfig_SkipsNonStringArrayElements(t *testing.T) {
	t.Parallel()

	settings := &EntireSettings{
		StrategyOptions: map[string]any{
			"showcase": map[string]any{
				"allowed_paths": []any{"src/", 123, "lib/", nil, "cmd/"},
			},
		},
	}

	cfg := settings.GetShowcaseConfig()
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}

	// Should only extract string values
	if len(cfg.AllowedPaths) != 3 {
		t.Errorf("expected 3 allowed paths (skipping non-strings), got %d", len(cfg.AllowedPaths))
	}
}

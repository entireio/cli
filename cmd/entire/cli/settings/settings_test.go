package settings

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	settingsContent := `{"enabled": true, "unknown_key": "value"}`
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
	_, err := Load(context.Background())
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
	settings, err := Load(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify values
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
	settingsContent := `{"enabled": true}`
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
	_, err := Load(context.Background())
	if err == nil {
		t.Error("expected error for unknown key in local settings, got nil")
	} else if !containsUnknownField(err.Error()) {
		t.Errorf("expected unknown field error, got: %v", err)
	}
}

func TestLoad_AcceptsDeprecatedStrategyField(t *testing.T) {
	tmpDir := t.TempDir()

	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	settingsFile := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"enabled": true, "strategy": "auto-commit"}`), 0o644); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755); err != nil {
		t.Fatalf("failed to create .git directory: %v", err)
	}

	t.Chdir(tmpDir)

	s, err := Load(context.Background())
	if err != nil {
		t.Fatalf("expected no error for deprecated strategy field, got: %v", err)
	}
	if s.Strategy != "auto-commit" {
		t.Errorf("expected strategy 'auto-commit', got %q", s.Strategy)
	}
}

func TestGetCommitLinking_DefaultsToPrompt(t *testing.T) {
	s := &EntireSettings{Enabled: true}
	if got := s.GetCommitLinking(); got != CommitLinkingPrompt {
		t.Errorf("GetCommitLinking() = %q, want %q", got, CommitLinkingPrompt)
	}
}

func TestGetCommitLinking_ReturnsExplicitValue(t *testing.T) {
	s := &EntireSettings{Enabled: true, CommitLinking: CommitLinkingAlways}
	if got := s.GetCommitLinking(); got != CommitLinkingAlways {
		t.Errorf("GetCommitLinking() = %q, want %q", got, CommitLinkingAlways)
	}

	s.CommitLinking = CommitLinkingPrompt
	if got := s.GetCommitLinking(); got != CommitLinkingPrompt {
		t.Errorf("GetCommitLinking() = %q, want %q", got, CommitLinkingPrompt)
	}
}

func TestLoad_CommitLinkingField(t *testing.T) {
	tmpDir := t.TempDir()

	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	settingsFile := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"enabled": true, "commit_linking": "always"}`), 0o644); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755); err != nil {
		t.Fatalf("failed to create .git directory: %v", err)
	}

	t.Chdir(tmpDir)

	s, err := Load(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.CommitLinking != CommitLinkingAlways {
		t.Errorf("CommitLinking = %q, want %q", s.CommitLinking, CommitLinkingAlways)
	}
	if got := s.GetCommitLinking(); got != CommitLinkingAlways {
		t.Errorf("GetCommitLinking() = %q, want %q", got, CommitLinkingAlways)
	}
}

func TestMergeJSON_CommitLinking(t *testing.T) {
	tmpDir := t.TempDir()

	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	// Base settings without commit_linking
	settingsFile := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"enabled": true}`), 0o644); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}

	// Local override with commit_linking
	localFile := filepath.Join(entireDir, "settings.local.json")
	if err := os.WriteFile(localFile, []byte(`{"commit_linking": "always"}`), 0o644); err != nil {
		t.Fatalf("failed to write local settings file: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755); err != nil {
		t.Fatalf("failed to create .git directory: %v", err)
	}

	t.Chdir(tmpDir)

	s, err := Load(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.CommitLinking != CommitLinkingAlways {
		t.Errorf("CommitLinking = %q, want %q (expected local override)", s.CommitLinking, CommitLinkingAlways)
	}
}

func TestGetBranchPrefix_DefaultsToEntire(t *testing.T) {
	s := &EntireSettings{Enabled: true}
	if got := s.GetBranchPrefix(); got != DefaultBranchPrefix {
		t.Errorf("GetBranchPrefix() = %q, want %q", got, DefaultBranchPrefix)
	}
}

func TestGetBranchPrefix_ReturnsExplicitValue(t *testing.T) {
	s := &EntireSettings{Enabled: true, BranchPrefix: "jfrog/"}
	if got := s.GetBranchPrefix(); got != "jfrog/" {
		t.Errorf("GetBranchPrefix() = %q, want %q", got, "jfrog/")
	}
}

func TestGetCommitPrefix_DefaultsToCheckpoint(t *testing.T) {
	s := &EntireSettings{Enabled: true}
	if got := s.GetCommitPrefix(); got != DefaultCommitPrefix {
		t.Errorf("GetCommitPrefix() = %q, want %q", got, DefaultCommitPrefix)
	}
}

func TestGetCommitPrefix_ReturnsExplicitValue(t *testing.T) {
	s := &EntireSettings{Enabled: true, CommitPrefix: "JFrog Checkpoint"}
	if got := s.GetCommitPrefix(); got != "JFrog Checkpoint" {
		t.Errorf("GetCommitPrefix() = %q, want %q", got, "JFrog Checkpoint")
	}
}

func TestGetMetadataBranchName_Default(t *testing.T) {
	s := &EntireSettings{Enabled: true}
	if got := s.GetMetadataBranchName(); got != "entire/checkpoints/v1" {
		t.Errorf("GetMetadataBranchName() = %q, want %q", got, "entire/checkpoints/v1")
	}
}

func TestGetMetadataBranchName_CustomPrefix(t *testing.T) {
	s := &EntireSettings{Enabled: true, BranchPrefix: "jfrog/"}
	if got := s.GetMetadataBranchName(); got != "jfrog/checkpoints/v1" {
		t.Errorf("GetMetadataBranchName() = %q, want %q", got, "jfrog/checkpoints/v1")
	}
}

func TestLoad_BranchPrefixField(t *testing.T) {
	tmpDir := t.TempDir()

	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	settingsFile := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"enabled": true, "branch_prefix": "myco/"}`), 0o644); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755); err != nil {
		t.Fatalf("failed to create .git directory: %v", err)
	}

	t.Chdir(tmpDir)

	s, err := Load(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.BranchPrefix != "myco/" {
		t.Errorf("BranchPrefix = %q, want %q", s.BranchPrefix, "myco/")
	}
	if got := s.GetMetadataBranchName(); got != "myco/checkpoints/v1" {
		t.Errorf("GetMetadataBranchName() = %q, want %q", got, "myco/checkpoints/v1")
	}
}

func TestLoad_BranchPrefixValidation(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		wantErr bool
	}{
		{"valid prefix", "jfrog/", false},
		{"missing trailing slash", "jfrog", true},
		{"contains double dot", "my../prefix/", true},
		{"contains space", "my prefix/", true},
		{"contains tilde", "my~prefix/", true},
		{"empty (uses default)", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			entireDir := filepath.Join(tmpDir, ".entire")
			if err := os.MkdirAll(entireDir, 0o755); err != nil {
				t.Fatalf("failed to create .entire directory: %v", err)
			}

			content := `{"enabled": true}`
			if tt.prefix != "" {
				content = `{"enabled": true, "branch_prefix": "` + tt.prefix + `"}`
			}
			settingsFile := filepath.Join(entireDir, "settings.json")
			if err := os.WriteFile(settingsFile, []byte(content), 0o644); err != nil {
				t.Fatalf("failed to write settings file: %v", err)
			}

			if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755); err != nil {
				t.Fatalf("failed to create .git directory: %v", err)
			}

			t.Chdir(tmpDir)

			_, err := Load(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("Load() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoad_CommitPrefixField(t *testing.T) {
	tmpDir := t.TempDir()

	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	settingsFile := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"enabled": true, "commit_prefix": "JFrog Checkpoint"}`), 0o644); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755); err != nil {
		t.Fatalf("failed to create .git directory: %v", err)
	}

	t.Chdir(tmpDir)

	s, err := Load(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.CommitPrefix != "JFrog Checkpoint" {
		t.Errorf("CommitPrefix = %q, want %q", s.CommitPrefix, "JFrog Checkpoint")
	}
}

func TestMergeJSON_BranchAndCommitPrefix(t *testing.T) {
	tmpDir := t.TempDir()

	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	// Base settings without prefixes
	settingsFile := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"enabled": true}`), 0o644); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}

	// Local override with prefixes
	localFile := filepath.Join(entireDir, "settings.local.json")
	if err := os.WriteFile(localFile, []byte(`{"branch_prefix": "jfrog/", "commit_prefix": "JFrog"}`), 0o644); err != nil {
		t.Fatalf("failed to write local settings file: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755); err != nil {
		t.Fatalf("failed to create .git directory: %v", err)
	}

	t.Chdir(tmpDir)

	s, err := Load(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.BranchPrefix != "jfrog/" {
		t.Errorf("BranchPrefix = %q, want %q (expected local override)", s.BranchPrefix, "jfrog/")
	}
	if s.CommitPrefix != "JFrog" {
		t.Errorf("CommitPrefix = %q, want %q (expected local override)", s.CommitPrefix, "JFrog")
	}
}

// containsUnknownField checks if the error message indicates an unknown field
func containsUnknownField(msg string) bool {
	// Go's json package reports unknown fields with this message format
	return strings.Contains(msg, "unknown field")
}

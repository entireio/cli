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
		"telemetry": true,
		"redaction": {"pii": {"enabled": true, "email": true, "phone": false}},
		"external_agents": true
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
	if settings.Redaction == nil {
		t.Fatal("expected redaction to be non-nil")
	}
	if settings.Redaction.PII == nil {
		t.Fatal("expected redaction.pii to be non-nil")
	}
	if !settings.Redaction.PII.Enabled {
		t.Error("expected redaction.pii.enabled to be true")
	}
	if settings.Redaction.PII.Email == nil || !*settings.Redaction.PII.Email {
		t.Error("expected redaction.pii.email to be true")
	}
	if settings.Redaction.PII.Phone == nil || *settings.Redaction.PII.Phone {
		t.Error("expected redaction.pii.phone to be false")
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

func TestLoad_MissingRedactionIsNil(t *testing.T) {
	tmpDir := t.TempDir()
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	settingsFile := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"enabled": true}`), 0o644); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755); err != nil {
		t.Fatalf("failed to create .git directory: %v", err)
	}
	t.Chdir(tmpDir)

	settings, err := Load(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if settings.Redaction != nil {
		t.Error("expected redaction to be nil when not in settings")
	}
}

func TestLoad_LocalOverridesRedaction(t *testing.T) {
	tmpDir := t.TempDir()
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	// Base settings: PII disabled
	settingsFile := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"enabled": true, "redaction": {"pii": {"enabled": false}}}`), 0o644); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}

	// Local override: PII enabled with custom patterns
	localFile := filepath.Join(entireDir, "settings.local.json")
	localContent := `{"redaction": {"pii": {"enabled": true, "custom_patterns": {"employee_id": "EMP-\\d{6}"}}}}`
	if err := os.WriteFile(localFile, []byte(localContent), 0o644); err != nil {
		t.Fatalf("failed to write local settings file: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755); err != nil {
		t.Fatalf("failed to create .git directory: %v", err)
	}
	t.Chdir(tmpDir)

	settings, err := Load(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if settings.Redaction == nil || settings.Redaction.PII == nil {
		t.Fatal("expected redaction.pii to be non-nil after local override")
	}
	if !settings.Redaction.PII.Enabled {
		t.Error("expected local override to enable PII")
	}
	if settings.Redaction.PII.CustomPatterns == nil {
		t.Fatal("expected custom_patterns to be non-nil")
	}
	if settings.Redaction.PII.CustomPatterns["employee_id"] != `EMP-\d{6}` {
		t.Errorf("expected employee_id pattern, got %v", settings.Redaction.PII.CustomPatterns)
	}
}

func TestLoad_LocalMergesRedactionSubfields(t *testing.T) {
	tmpDir := t.TempDir()
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	// Base: PII enabled with email=true, phone=true
	baseContent := `{"enabled":true,"redaction":{"pii":{"enabled":true,"email":true,"phone":true}}}`
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(baseContent), 0o644); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}

	// Local: adds custom_patterns only — should NOT erase email/phone from base
	localContent := `{"redaction":{"pii":{"enabled":true,"custom_patterns":{"ssn":"\\d{3}-\\d{2}-\\d{4}"}}}}`
	if err := os.WriteFile(filepath.Join(entireDir, "settings.local.json"), []byte(localContent), 0o644); err != nil {
		t.Fatalf("failed to write local settings file: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755); err != nil {
		t.Fatalf("failed to create .git directory: %v", err)
	}
	t.Chdir(tmpDir)

	settings, err := Load(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if settings.Redaction == nil || settings.Redaction.PII == nil {
		t.Fatal("expected redaction.pii to be non-nil")
	}
	// email and phone from base should survive local merge
	if settings.Redaction.PII.Email == nil || !*settings.Redaction.PII.Email {
		t.Error("expected email=true from base to survive local merge")
	}
	if settings.Redaction.PII.Phone == nil || !*settings.Redaction.PII.Phone {
		t.Error("expected phone=true from base to survive local merge")
	}
	// custom_patterns from local should be present
	if settings.Redaction.PII.CustomPatterns == nil {
		t.Fatal("expected custom_patterns from local to be present")
	}
	if _, ok := settings.Redaction.PII.CustomPatterns["ssn"]; !ok {
		t.Error("expected ssn pattern from local override")
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

func TestExternalAgents_DefaultsFalse(t *testing.T) {
	s := &EntireSettings{}
	if s.ExternalAgents {
		t.Error("expected ExternalAgents to default to false")
	}
}

func TestLoad_ExternalAgentsField(t *testing.T) {
	tmpDir := t.TempDir()

	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	settingsFile := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"enabled": true, "external_agents": true}`), 0o644); err != nil {
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
	if !s.ExternalAgents {
		t.Error("expected ExternalAgents to be true")
	}
}

func TestMergeJSON_ExternalAgents(t *testing.T) {
	tmpDir := t.TempDir()

	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	// Base settings without external_agents
	settingsFile := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"enabled": true}`), 0o644); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}

	// Local override enables external_agents
	localFile := filepath.Join(entireDir, "settings.local.json")
	if err := os.WriteFile(localFile, []byte(`{"external_agents": true}`), 0o644); err != nil {
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
	if !s.ExternalAgents {
		t.Error("expected ExternalAgents to be true from local override")
	}
}

// containsUnknownField checks if the error message indicates an unknown field
func containsUnknownField(msg string) bool {
	// Go's json package reports unknown fields with this message format
	return strings.Contains(msg, "unknown field")
}

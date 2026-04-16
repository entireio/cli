package settings

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	baseSettingsClaudeSonnet = `{"enabled": true, "summary_generation": {"provider": "claude-code", "model": "sonnet"}}`
	providerCodex            = "codex"
)

// setupSettingsDir creates a temp repo directory with the provided settings
// contents and chdirs into it. Pass empty strings to skip the base or local
// file. DRYs up the merge/load integration tests that otherwise all repeat
// the same ~12 lines of tmpdir + .entire + .git + chdir boilerplate.
func setupSettingsDir(t *testing.T, base, local string) {
	t.Helper()
	tmpDir := t.TempDir()
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}
	if base != "" {
		if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(base), 0o644); err != nil {
			t.Fatalf("failed to write settings file: %v", err)
		}
	}
	if local != "" {
		if err := os.WriteFile(filepath.Join(entireDir, "settings.local.json"), []byte(local), 0o644); err != nil {
			t.Fatalf("failed to write local settings file: %v", err)
		}
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755); err != nil {
		t.Fatalf("failed to create .git directory: %v", err)
	}
	t.Chdir(tmpDir)
}

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
		"summary_generation": {"provider": "claude-code", "model": "sonnet"},
		"telemetry": true,
		"redaction": {"pii": {"enabled": true, "email": true, "phone": false}},
		"external_agents": true,
		"vercel": true
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
	if settings.SummaryGeneration == nil {
		t.Fatal("expected summary_generation to be non-nil")
	}
	if settings.SummaryGeneration.Provider != "claude-code" {
		t.Errorf("expected summary_generation.provider 'claude-code', got %q", settings.SummaryGeneration.Provider)
	}
	if settings.SummaryGeneration.Model != "sonnet" { //nolint:goconst // test literal
		t.Errorf("expected summary_generation.model 'sonnet', got %q", settings.SummaryGeneration.Model)
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
	if !settings.Vercel {
		t.Error("expected vercel to be true")
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

func TestLoad_MergesLocalOverrides(t *testing.T) {
	tmpDir := t.TempDir()
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(`{"enabled": true, "vercel": true}`), 0o644); err != nil {
		t.Fatalf("failed to write settings.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "settings.local.json"), []byte(`{"log_level": "debug"}`), 0o644); err != nil {
		t.Fatalf("failed to write settings.local.json: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755); err != nil {
		t.Fatalf("failed to create .git directory: %v", err)
	}

	t.Chdir(tmpDir)

	s, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !s.Vercel {
		t.Error("expected vercel to be true")
	}
	if s.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", s.LogLevel, "debug")
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

func TestLoad_SummaryGenerationModelWithoutProviderRejected(t *testing.T) {
	setupSettingsDir(t, `{"enabled": true, "summary_generation": {"model": "sonnet"}}`, "")

	_, err := Load(context.Background())
	if err == nil {
		t.Fatal("expected error for summary_generation.model without provider")
	}
	if !strings.Contains(err.Error(), "summary_generation.model") || !strings.Contains(err.Error(), "without summary_generation.provider") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

// TestLoad_MergedSettingsRejectsInvalidCombination verifies that the merged
// result of base + local settings is validated, not just each file in
// isolation. A base with no summary_generation and a local override that
// sets only a model (no provider) produces a merged state that is invalid
// per SummaryGenerationSettings.Validate(), and the load path must reject
// it rather than letting it reach the provider-resolution code.
func TestLoad_MergedSettingsRejectsInvalidCombination(t *testing.T) {
	setupSettingsDir(t, `{"enabled": true}`, `{"summary_generation": {"model": "sonnet"}}`)

	_, err := Load(context.Background())
	if err == nil {
		t.Fatal("expected error for merged model-without-provider combination")
	}
	if !strings.Contains(err.Error(), "merged settings invalid") {
		t.Fatalf("expected wrapped 'merged settings invalid' error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "summary_generation.model") {
		t.Fatalf("expected inner error to mention summary_generation.model, got: %v", err)
	}
}

func TestLoadFromFile_AcceptsModelWithoutProvider(t *testing.T) {
	t.Parallel()

	// A local override file may legitimately contain only a model; the
	// provider comes from the project settings after merge. LoadFromFile
	// must not reject this — validation happens post-merge in Load().
	tmpDir := t.TempDir()
	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}
	localFile := filepath.Join(entireDir, "settings.local.json")
	if err := os.WriteFile(localFile, []byte(`{"summary_generation": {"model": "sonnet"}}`), 0o644); err != nil {
		t.Fatalf("failed to write local settings: %v", err)
	}

	s, err := LoadFromFile(localFile)
	if err != nil {
		t.Fatalf("LoadFromFile should accept model-only file, got error: %v", err)
	}
	if s.SummaryGeneration == nil || s.SummaryGeneration.Model != "sonnet" {
		t.Fatalf("expected model 'sonnet', got %+v", s.SummaryGeneration)
	}
}

func TestSummaryGenerationSettings_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		s       *SummaryGenerationSettings
		wantErr bool
	}{
		{name: "nil receiver is valid", s: nil, wantErr: false},
		{name: "provider and model is valid", s: &SummaryGenerationSettings{Provider: "claude-code", Model: "sonnet"}, wantErr: false},
		{name: "model without provider is invalid", s: &SummaryGenerationSettings{Model: "sonnet"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.s.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestMergeJSON_SummaryGeneration_ProviderSwitchClearsStaleModel verifies that
// switching providers via a local override clears a model from the base that
// was tuned to the old provider. Without this, local `{"provider":"codex"}`
// on base `{"provider":"claude-code","model":"sonnet"}` would produce
// `provider=codex, model=sonnet`, which codex would reject at CLI time.
func TestMergeJSON_SummaryGeneration_ProviderSwitchClearsStaleModel(t *testing.T) {
	setupSettingsDir(t, baseSettingsClaudeSonnet, `{"summary_generation": {"provider": "codex"}}`)

	s, err := Load(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.SummaryGeneration == nil {
		t.Fatal("expected SummaryGeneration to be non-nil")
	}
	if s.SummaryGeneration.Provider != providerCodex {
		t.Errorf("SummaryGeneration.Provider = %q, want %q", s.SummaryGeneration.Provider, providerCodex)
	}
	if s.SummaryGeneration.Model != "" {
		t.Errorf("SummaryGeneration.Model = %q, want \"\" (stale Claude model should be cleared on provider switch)", s.SummaryGeneration.Model)
	}
}

// TestMergeJSON_SummaryGeneration_ProviderSwitchWithExplicitModelPreserved
// checks the complementary case: if the override sets BOTH provider and model,
// we preserve the explicit model rather than clearing it.
func TestMergeJSON_SummaryGeneration_ProviderSwitchWithExplicitModelPreserved(t *testing.T) {
	setupSettingsDir(t, baseSettingsClaudeSonnet, `{"summary_generation": {"provider": "codex", "model": "gpt-5"}}`)

	s, err := Load(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.SummaryGeneration.Provider != "codex" || s.SummaryGeneration.Model != "gpt-5" {
		t.Errorf("Provider/Model = %q/%q, want codex/gpt-5", s.SummaryGeneration.Provider, s.SummaryGeneration.Model)
	}
}

// TestMergeJSON_SummaryGeneration_SameProviderPreservesModel confirms we only
// clear the model on provider *change*, not on any provider override. A local
// override that pins the provider to the same value as the base must not
// clobber the base's model.
func TestMergeJSON_SummaryGeneration_SameProviderPreservesModel(t *testing.T) {
	setupSettingsDir(t, baseSettingsClaudeSonnet, `{"summary_generation": {"provider": "claude-code"}}`)

	s, err := Load(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.SummaryGeneration.Provider != "claude-code" || s.SummaryGeneration.Model != "sonnet" {
		t.Errorf("Provider/Model = %q/%q, want claude-code/sonnet", s.SummaryGeneration.Provider, s.SummaryGeneration.Model)
	}
}

func TestIsCheckpointsV2Enabled_DefaultsFalse(t *testing.T) {
	t.Parallel()
	s := &EntireSettings{Enabled: true}
	if s.IsCheckpointsV2Enabled() {
		t.Error("expected IsCheckpointsV2Enabled to default to false")
	}
}

func TestIsCheckpointsV2Enabled_EmptyStrategyOptions(t *testing.T) {
	t.Parallel()
	s := &EntireSettings{Enabled: true, StrategyOptions: map[string]any{}}
	if s.IsCheckpointsV2Enabled() {
		t.Error("expected IsCheckpointsV2Enabled to be false with empty strategy_options")
	}
}

func TestIsCheckpointsV2Enabled_True(t *testing.T) {
	t.Parallel()
	s := &EntireSettings{
		Enabled:         true,
		StrategyOptions: map[string]any{"checkpoints_v2": true},
	}
	if !s.IsCheckpointsV2Enabled() {
		t.Error("expected IsCheckpointsV2Enabled to be true")
	}
}

func TestIsCheckpointsV2Enabled_ExplicitlyFalse(t *testing.T) {
	t.Parallel()
	s := &EntireSettings{
		Enabled:         true,
		StrategyOptions: map[string]any{"checkpoints_v2": false},
	}
	if s.IsCheckpointsV2Enabled() {
		t.Error("expected IsCheckpointsV2Enabled to be false when explicitly set to false")
	}
}

func TestIsCheckpointsV2Enabled_WrongType(t *testing.T) {
	t.Parallel()
	s := &EntireSettings{
		Enabled:         true,
		StrategyOptions: map[string]any{"checkpoints_v2": "yes"},
	}
	if s.IsCheckpointsV2Enabled() {
		t.Error("expected IsCheckpointsV2Enabled to be false for non-bool value")
	}
}

func TestIsCheckpointsV2Enabled_LoadFromFile(t *testing.T) {
	tmpDir := t.TempDir()

	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	settingsFile := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"enabled": true, "strategy_options": {"checkpoints_v2": true}}`), 0o644); err != nil {
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
	if !s.IsCheckpointsV2Enabled() {
		t.Error("expected IsCheckpointsV2Enabled to be true after loading from file")
	}
}

func TestIsCheckpointsV2Enabled_LocalOverride(t *testing.T) {
	tmpDir := t.TempDir()

	entireDir := filepath.Join(tmpDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}

	// Base settings without checkpoints_v2
	settingsFile := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"enabled": true}`), 0o644); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}

	// Local override enables checkpoints_v2
	localFile := filepath.Join(entireDir, "settings.local.json")
	if err := os.WriteFile(localFile, []byte(`{"strategy_options": {"checkpoints_v2": true}}`), 0o644); err != nil {
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
	if !s.IsCheckpointsV2Enabled() {
		t.Error("expected IsCheckpointsV2Enabled to be true from local override")
	}
}

func TestIsPushV2RefsEnabled_DefaultsFalse(t *testing.T) {
	t.Parallel()
	s := &EntireSettings{Enabled: true}
	if s.IsPushV2RefsEnabled() {
		t.Error("expected IsPushV2RefsEnabled to default to false")
	}
}

func TestIsPushV2RefsEnabled_RequiresBothFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		opts     map[string]any
		expected bool
	}{
		{"both true", map[string]any{"checkpoints_v2": true, "push_v2_refs": true}, true},
		{"only checkpoints_v2", map[string]any{"checkpoints_v2": true}, false},
		{"only push_v2_refs", map[string]any{"push_v2_refs": true}, false},
		{"both false", map[string]any{"checkpoints_v2": false, "push_v2_refs": false}, false},
		{"push_v2_refs wrong type", map[string]any{"checkpoints_v2": true, "push_v2_refs": "yes"}, false},
		{"empty options", map[string]any{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := &EntireSettings{
				Enabled:         true,
				StrategyOptions: tt.opts,
			}
			if got := s.IsPushV2RefsEnabled(); got != tt.expected {
				t.Errorf("IsPushV2RefsEnabled() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestIsFilteredFetchesEnabled_DefaultsFalse(t *testing.T) {
	t.Parallel()
	s := &EntireSettings{Enabled: true}
	if s.IsFilteredFetchesEnabled() {
		t.Error("expected IsFilteredFetchesEnabled to default to false")
	}
}

func TestIsFilteredFetchesEnabled_True(t *testing.T) {
	t.Parallel()
	s := &EntireSettings{
		Enabled:         true,
		StrategyOptions: map[string]any{"filtered_fetches": true},
	}
	if !s.IsFilteredFetchesEnabled() {
		t.Error("expected IsFilteredFetchesEnabled to be true")
	}
}

func TestIsFilteredFetchesEnabled_WrongType(t *testing.T) {
	t.Parallel()
	s := &EntireSettings{
		Enabled:         true,
		StrategyOptions: map[string]any{"filtered_fetches": "yes"},
	}
	if s.IsFilteredFetchesEnabled() {
		t.Error("expected IsFilteredFetchesEnabled to be false for non-bool value")
	}
}

// containsUnknownField checks if the error message indicates an unknown field
func containsUnknownField(msg string) bool {
	// Go's json package reports unknown fields with this message format
	return strings.Contains(msg, "unknown field")
}

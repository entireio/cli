package settings

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	debugLevel       = "debug"
	providerClaudeCC = "claude-code"
	modelSonnet      = "sonnet"
)

func boolPtr(b bool) *bool { return &b }

type synthCase struct {
	name   string
	legacy *EntireSettings
	check  func(t *testing.T, got *Settings)
}

func runSynthCases(t *testing.T, cases []synthCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := synthesizeFromLegacy(tc.legacy)
			if got == nil {
				t.Fatal("synthesizeFromLegacy returned nil")
			}
			if got.Schema != CurrentSchemaVersion {
				t.Fatalf("Schema = %d, want %d", got.Schema, CurrentSchemaVersion)
			}
			tc.check(t, got)
		})
	}
}

// TestSynthesizeFromLegacy_BackendSelection covers how the Primary backend
// type and Mirrors are derived from legacy strategy_options.
func TestSynthesizeFromLegacy_BackendSelection(t *testing.T) {
	t.Parallel()
	runSynthCases(t, []synthCase{
		{
			name:   "nil legacy returns defaults",
			legacy: nil,
			check: func(t *testing.T, got *Settings) {
				if !got.Enabled {
					t.Fatal("Enabled = false, want true")
				}
			},
		},
		{
			name:   "empty legacy → primary v1, no mirrors",
			legacy: &EntireSettings{Enabled: true},
			check: func(t *testing.T, got *Settings) {
				if got.Checkpoints.Primary.Type != BackendTypeV1 {
					t.Fatalf("Primary.Type = %q, want %q", got.Checkpoints.Primary.Type, BackendTypeV1)
				}
				if len(got.Checkpoints.Mirrors) != 0 {
					t.Fatalf("Mirrors = %v, want empty", got.Checkpoints.Mirrors)
				}
			},
		},
		{
			name: "checkpoints_v2 alone → primary v2",
			legacy: &EntireSettings{
				Enabled:         true,
				StrategyOptions: map[string]any{"checkpoints_v2": true},
			},
			check: func(t *testing.T, got *Settings) {
				if got.Checkpoints.Primary.Type != BackendTypeV2 {
					t.Fatalf("Primary.Type = %q, want %q", got.Checkpoints.Primary.Type, BackendTypeV2)
				}
			},
		},
		{
			name: "checkpoints_version=2 → primary v2",
			legacy: &EntireSettings{
				Enabled:         true,
				StrategyOptions: map[string]any{"checkpoints_version": float64(2)},
			},
			check: func(t *testing.T, got *Settings) {
				if got.Checkpoints.Primary.Type != BackendTypeV2 {
					t.Fatalf("Primary.Type = %q, want %q", got.Checkpoints.Primary.Type, BackendTypeV2)
				}
			},
		},
		{
			name: "checkpoints_version=1 → primary v1",
			legacy: &EntireSettings{
				Enabled:         true,
				StrategyOptions: map[string]any{"checkpoints_version": float64(1)},
			},
			check: func(t *testing.T, got *Settings) {
				if got.Checkpoints.Primary.Type != BackendTypeV1 {
					t.Fatalf("Primary.Type = %q, want %q", got.Checkpoints.Primary.Type, BackendTypeV1)
				}
			},
		},
		{
			name: "gmeta alone → primary v1, gmeta mirror",
			legacy: &EntireSettings{
				Enabled:         true,
				StrategyOptions: map[string]any{"gmeta": true},
			},
			check: func(t *testing.T, got *Settings) {
				if got.Checkpoints.Primary.Type != BackendTypeV1 {
					t.Fatalf("Primary.Type = %q, want v1", got.Checkpoints.Primary.Type)
				}
				if len(got.Checkpoints.Mirrors) != 1 || got.Checkpoints.Mirrors[0].Type != BackendTypeGmeta {
					t.Fatalf("Mirrors = %v, want [{gmeta}]", got.Checkpoints.Mirrors)
				}
			},
		},
		{
			name: "checkpoints_v2 + gmeta → primary v2, gmeta mirror",
			legacy: &EntireSettings{
				Enabled: true,
				StrategyOptions: map[string]any{
					"checkpoints_v2": true,
					"gmeta":          true,
				},
			},
			check: func(t *testing.T, got *Settings) {
				if got.Checkpoints.Primary.Type != BackendTypeV2 {
					t.Fatalf("Primary.Type = %q, want v2", got.Checkpoints.Primary.Type)
				}
				if len(got.Checkpoints.Mirrors) != 1 || got.Checkpoints.Mirrors[0].Type != BackendTypeGmeta {
					t.Fatalf("Mirrors = %v, want [{gmeta}]", got.Checkpoints.Mirrors)
				}
			},
		},
		{
			name: "push_v2_refs is silently dropped (no v2 destination)",
			legacy: &EntireSettings{
				Enabled: true,
				StrategyOptions: map[string]any{
					"checkpoints_v2": true,
					"push_v2_refs":   true,
				},
			},
			check: func(t *testing.T, got *Settings) {
				if got.Checkpoints.Primary.Type != BackendTypeV2 {
					t.Fatalf("Primary.Type = %q, want v2", got.Checkpoints.Primary.Type)
				}
			},
		},
		{
			name: "deprecated 'strategy' field is silently dropped",
			legacy: &EntireSettings{
				Enabled:  true,
				Strategy: "auto-commit",
			},
			check: func(t *testing.T, got *Settings) {
				if got.Checkpoints.Primary.Type != BackendTypeV1 {
					t.Fatalf("Primary.Type = %q, want v1", got.Checkpoints.Primary.Type)
				}
			},
		},
	})
}

// TestSynthesizeFromLegacy_CheckpointFields covers Checkpoints sub-fields
// other than Primary/Mirrors: remote, retention, signing, filtered fetches,
// push_sessions tri-state.
func TestSynthesizeFromLegacy_CheckpointFields(t *testing.T) {
	t.Parallel()
	runSynthCases(t, []synthCase{
		{
			name: "checkpoint_remote → checkpoints.remote",
			legacy: &EntireSettings{
				Enabled: true,
				StrategyOptions: map[string]any{
					"checkpoint_remote": map[string]any{
						"provider": "github",
						"repo":     "org/checkpoints",
					},
				},
			},
			check: func(t *testing.T, got *Settings) {
				if got.Checkpoints.Git == nil {
					t.Fatal("Git = nil, want populated")
				}
				if got.Checkpoints.Git.Provider != "github" || got.Checkpoints.Git.Repo != "org/checkpoints" {
					t.Fatalf("Git = %+v, want github/org/checkpoints", got.Checkpoints.Git)
				}
			},
		},
		{
			name: "filtered_fetches → checkpoints.filtered_fetches",
			legacy: &EntireSettings{
				Enabled:         true,
				StrategyOptions: map[string]any{"filtered_fetches": true},
			},
			check: func(t *testing.T, got *Settings) {
				if !got.Checkpoints.FilteredFetches {
					t.Fatal("FilteredFetches = false, want true")
				}
			},
		},
		{
			name: "push_sessions=false → explicit false pointer",
			legacy: &EntireSettings{
				Enabled:         true,
				StrategyOptions: map[string]any{"push_sessions": false},
			},
			check: func(t *testing.T, got *Settings) {
				if got.Checkpoints.PushSessions == nil || *got.Checkpoints.PushSessions {
					t.Fatalf("PushSessions = %v, want explicit false", got.Checkpoints.PushSessions)
				}
			},
		},
		{
			name: "push_sessions=true → explicit true pointer",
			legacy: &EntireSettings{
				Enabled:         true,
				StrategyOptions: map[string]any{"push_sessions": true},
			},
			check: func(t *testing.T, got *Settings) {
				if got.Checkpoints.PushSessions == nil || !*got.Checkpoints.PushSessions {
					t.Fatalf("PushSessions = %v, want explicit true", got.Checkpoints.PushSessions)
				}
			},
		},
		{
			name:   "push_sessions absent → nil pointer",
			legacy: &EntireSettings{Enabled: true},
			check: func(t *testing.T, got *Settings) {
				if got.Checkpoints.PushSessions != nil {
					t.Fatalf("PushSessions = %v, want nil", got.Checkpoints.PushSessions)
				}
			},
		},
		{
			name: "full_transcript_generation_retention_days set → preserved",
			legacy: &EntireSettings{
				Enabled:         true,
				StrategyOptions: map[string]any{"full_transcript_generation_retention_days": float64(90)},
			},
			check: func(t *testing.T, got *Settings) {
				if got.Checkpoints.FullTranscriptRetentionDays != 90 {
					t.Fatalf("FullTranscriptRetentionDays = %d, want 90", got.Checkpoints.FullTranscriptRetentionDays)
				}
			},
		},
		{
			name:   "full_transcript_generation_retention_days absent → 0",
			legacy: &EntireSettings{Enabled: true},
			check: func(t *testing.T, got *Settings) {
				if got.Checkpoints.FullTranscriptRetentionDays != 0 {
					t.Fatalf("FullTranscriptRetentionDays = %d, want 0", got.Checkpoints.FullTranscriptRetentionDays)
				}
			},
		},
		{
			name: "sign_checkpoint_commits=false → checkpoints.sign_commits=false",
			legacy: &EntireSettings{
				Enabled:               true,
				SignCheckpointCommits: boolPtr(false),
			},
			check: func(t *testing.T, got *Settings) {
				if got.Checkpoints.SignCommits == nil || *got.Checkpoints.SignCommits {
					t.Fatalf("SignCommits = %v, want explicit false", got.Checkpoints.SignCommits)
				}
			},
		},
		{
			name:   "sign_checkpoint_commits unset → checkpoints.sign_commits nil",
			legacy: &EntireSettings{Enabled: true},
			check: func(t *testing.T, got *Settings) {
				if got.Checkpoints.SignCommits != nil {
					t.Fatalf("SignCommits = %v, want nil", got.Checkpoints.SignCommits)
				}
			},
		},
	})
}

// TestSynthesizeFromLegacy_TopLevelFields covers logging, hooks, features,
// redaction, and telemetry mappings.
func TestSynthesizeFromLegacy_TopLevelFields(t *testing.T) {
	t.Parallel()
	runSynthCases(t, []synthCase{
		{
			name: "log_level → logging.level",
			legacy: &EntireSettings{
				Enabled:  true,
				LogLevel: debugLevel,
			},
			check: func(t *testing.T, got *Settings) {
				if got.Logging.Level != debugLevel {
					t.Fatalf("Logging.Level = %q, want %q", got.Logging.Level, debugLevel)
				}
			},
		},
		{
			name: "summarize → features.summarize",
			legacy: &EntireSettings{
				Enabled: true,
				StrategyOptions: map[string]any{
					"summarize": map[string]any{"enabled": true},
				},
			},
			check: func(t *testing.T, got *Settings) {
				if !got.Features.Summarize {
					t.Fatal("Features.Summarize = false, want true")
				}
			},
		},
		{
			name: "external_agents → features.external_agents",
			legacy: &EntireSettings{
				Enabled:        true,
				ExternalAgents: true,
			},
			check: func(t *testing.T, got *Settings) {
				if !got.Features.ExternalAgents {
					t.Fatal("Features.ExternalAgents = false, want true")
				}
			},
		},
		{
			name: "vercel → features.vercel",
			legacy: &EntireSettings{
				Enabled: true,
				Vercel:  true,
			},
			check: func(t *testing.T, got *Settings) {
				if !got.Features.Vercel {
					t.Fatal("Features.Vercel = false, want true")
				}
			},
		},
		{
			name: "commit_linking → hooks.commit_linking",
			legacy: &EntireSettings{
				Enabled:       true,
				CommitLinking: CommitLinkingAlways,
			},
			check: func(t *testing.T, got *Settings) {
				if got.Hooks.CommitLinking != CommitLinkingAlways {
					t.Fatalf("Hooks.CommitLinking = %q, want %q", got.Hooks.CommitLinking, CommitLinkingAlways)
				}
			},
		},
		{
			name: "absolute_git_hook_path → hooks.absolute_git_hook_path",
			legacy: &EntireSettings{
				Enabled:             true,
				AbsoluteGitHookPath: true,
			},
			check: func(t *testing.T, got *Settings) {
				if !got.Hooks.AbsoluteGitHookPath {
					t.Fatal("Hooks.AbsoluteGitHookPath = false, want true")
				}
			},
		},
		{
			name: "redaction passes through",
			legacy: &EntireSettings{
				Enabled: true,
				Redaction: &RedactionSettings{
					PII: &PIISettings{Enabled: true, Email: boolPtr(true)},
				},
			},
			check: func(t *testing.T, got *Settings) {
				if got.Redaction == nil || got.Redaction.PII == nil || !got.Redaction.PII.Enabled {
					t.Fatalf("Redaction = %+v, want PII enabled", got.Redaction)
				}
			},
		},
		{
			name: "telemetry pointer passes through",
			legacy: &EntireSettings{
				Enabled:   true,
				Telemetry: boolPtr(true),
			},
			check: func(t *testing.T, got *Settings) {
				if got.Telemetry == nil || !*got.Telemetry {
					t.Fatalf("Telemetry = %v, want explicit true", got.Telemetry)
				}
			},
		},
	})
}

// TestSynthesizeFromLegacy_SummaryGeneration covers the summary_generation
// group consolidation (provider, model, timeout).
func TestSynthesizeFromLegacy_SummaryGeneration(t *testing.T) {
	t.Parallel()
	runSynthCases(t, []synthCase{
		{
			name: "summary_timeout_seconds → summary_generation.timeout_seconds",
			legacy: &EntireSettings{
				Enabled:               true,
				SummaryTimeoutSeconds: 30,
			},
			check: func(t *testing.T, got *Settings) {
				if got.SummaryGeneration == nil {
					t.Fatal("SummaryGeneration = nil, want allocated")
				}
				if got.SummaryGeneration.TimeoutSeconds != 30 {
					t.Fatalf("TimeoutSeconds = %d, want 30", got.SummaryGeneration.TimeoutSeconds)
				}
			},
		},
		{
			name: "provider/model preserved",
			legacy: &EntireSettings{
				Enabled: true,
				SummaryGeneration: &SummaryGenerationSettings{
					Provider: providerClaudeCC,
					Model:    modelSonnet,
				},
			},
			check: func(t *testing.T, got *Settings) {
				if got.SummaryGeneration == nil {
					t.Fatal("SummaryGeneration = nil")
				}
				if got.SummaryGeneration.Provider != providerClaudeCC || got.SummaryGeneration.Model != modelSonnet {
					t.Fatalf("SummaryGeneration = %+v, want claude-code/sonnet", got.SummaryGeneration)
				}
			},
		},
		{
			name:   "no summary fields → summary_generation nil",
			legacy: &EntireSettings{Enabled: true},
			check: func(t *testing.T, got *Settings) {
				if got.SummaryGeneration != nil {
					t.Fatalf("SummaryGeneration = %+v, want nil", got.SummaryGeneration)
				}
			},
		},
	})
}

// TestLoadV2_FileScenarios covers how LoadV2 reads from disk.
// These tests use t.Chdir, so they cannot run in parallel.
func TestLoadV2_NoFiles(t *testing.T) {
	setupSettingsDir(t, "", "")
	got, err := LoadV2(context.Background())
	if err != nil {
		t.Fatalf("LoadV2: %v", err)
	}
	if got.Schema != CurrentSchemaVersion || !got.Enabled {
		t.Fatalf("defaults wrong: %+v", got)
	}
	if got.Checkpoints.Primary.Type != BackendTypeV1 {
		t.Fatalf("expected default primary v1 when no files present, got %q", got.Checkpoints.Primary.Type)
	}
}

func TestLoadV2_LegacyMainOnly(t *testing.T) {
	setupSettingsDir(t, `{"enabled": true, "strategy_options": {"checkpoints_v2": true}}`, "")
	got, err := LoadV2(context.Background())
	if err != nil {
		t.Fatalf("LoadV2: %v", err)
	}
	if got.Checkpoints.Primary.Type != BackendTypeV2 {
		t.Fatalf("Primary.Type = %q, want v2", got.Checkpoints.Primary.Type)
	}
}

func TestLoadV2_LegacyMainAndLocal(t *testing.T) {
	setupSettingsDir(t,
		`{"enabled": true}`,
		`{"log_level": "`+debugLevel+`"}`,
	)
	got, err := LoadV2(context.Background())
	if err != nil {
		t.Fatalf("LoadV2: %v", err)
	}
	if got.Logging.Level != debugLevel {
		t.Fatalf("Logging.Level = %q, want %q", got.Logging.Level, debugLevel)
	}
}

func TestLoadV2_V2MainOnly(t *testing.T) {
	setupSettingsDir(t, `{
		"schema": 2,
		"enabled": true,
		"checkpoints": {"primary": {"type": "v2"}, "mirrors": [{"type": "gmeta"}]}
	}`, "")
	got, err := LoadV2(context.Background())
	if err != nil {
		t.Fatalf("LoadV2: %v", err)
	}
	if got.Checkpoints.Primary.Type != BackendTypeV2 {
		t.Fatalf("Primary.Type = %q, want v2", got.Checkpoints.Primary.Type)
	}
	if len(got.Checkpoints.Mirrors) != 1 || got.Checkpoints.Mirrors[0].Type != BackendTypeGmeta {
		t.Fatalf("Mirrors = %v, want [gmeta]", got.Checkpoints.Mirrors)
	}
}

func TestLoadV2_V2MainAndLocalOverride(t *testing.T) {
	setupSettingsDir(t,
		`{"schema": 2, "enabled": true, "logging": {"level": "info"}}`,
		`{"schema": 2, "logging": {"level": "`+debugLevel+`"}}`,
	)
	got, err := LoadV2(context.Background())
	if err != nil {
		t.Fatalf("LoadV2: %v", err)
	}
	if got.Logging.Level != debugLevel {
		t.Fatalf("Logging.Level = %q, want %q (overridden)", got.Logging.Level, debugLevel)
	}
	if !got.Enabled {
		t.Fatal("Enabled should be preserved across override")
	}
}

// TestLoadV2_V2MainLegacyLocal covers the common migration scenario:
// the tracked settings.json has been migrated to schema 2, but the
// gitignored settings.local.json is still in legacy shape. The legacy
// override translates to v2 fields without forcing the user to migrate
// the local file first.
func TestLoadV2_V2MainLegacyLocal(t *testing.T) {
	setupSettingsDir(t,
		`{"schema": 2, "enabled": true, "checkpoints": {"primary": {"type": "v2"}}}`,
		`{"log_level": "`+debugLevel+`"}`,
	)
	got, err := LoadV2(context.Background())
	if err != nil {
		t.Fatalf("LoadV2: %v", err)
	}
	if got.Checkpoints.Primary.Type != BackendTypeV2 {
		t.Fatalf("Primary.Type = %q, want v2 (preserved from main)", got.Checkpoints.Primary.Type)
	}
	if got.Logging.Level != debugLevel {
		t.Fatalf("Logging.Level = %q, want %q (from legacy local)", got.Logging.Level, debugLevel)
	}
}

// TestLoadV2_LegacyMainV2Local covers the inverse: legacy main, v2 local.
// Less common but should also work without rejection.
func TestLoadV2_LegacyMainV2Local(t *testing.T) {
	setupSettingsDir(t,
		`{"enabled": true, "strategy_options": {"checkpoints_v2": true}}`,
		`{"schema": 2, "logging": {"level": "`+debugLevel+`"}}`,
	)
	got, err := LoadV2(context.Background())
	if err != nil {
		t.Fatalf("LoadV2: %v", err)
	}
	if got.Checkpoints.Primary.Type != BackendTypeV2 {
		t.Fatalf("Primary.Type = %q, want v2 (synthesized from main)", got.Checkpoints.Primary.Type)
	}
	if got.Logging.Level != debugLevel {
		t.Fatalf("Logging.Level = %q, want %q (from v2 local)", got.Logging.Level, debugLevel)
	}
}

func TestLoadV2FromBytes_Empty(t *testing.T) {
	t.Parallel()
	got, err := LoadV2FromBytes(nil)
	if err != nil {
		t.Fatalf("LoadV2FromBytes(nil): %v", err)
	}
	if got.Schema != CurrentSchemaVersion || !got.Enabled {
		t.Fatalf("defaults wrong: %+v", got)
	}
}

func TestLoadV2FromBytes_V2(t *testing.T) {
	t.Parallel()
	got, err := LoadV2FromBytes([]byte(`{"schema": 2, "enabled": true, "checkpoints": {"primary": {"type": "v2"}}}`))
	if err != nil {
		t.Fatalf("LoadV2FromBytes: %v", err)
	}
	if got.Checkpoints.Primary.Type != BackendTypeV2 {
		t.Fatalf("Primary.Type = %q, want v2", got.Checkpoints.Primary.Type)
	}
}

func TestLoadV2FromBytes_Legacy(t *testing.T) {
	t.Parallel()
	got, err := LoadV2FromBytes([]byte(`{"enabled": true, "strategy_options": {"checkpoints_v2": true}}`))
	if err != nil {
		t.Fatalf("LoadV2FromBytes: %v", err)
	}
	if got.Checkpoints.Primary.Type != BackendTypeV2 {
		t.Fatalf("Primary.Type = %q, want v2", got.Checkpoints.Primary.Type)
	}
}

func TestLoadV2FromBytes_UnknownFieldRejected(t *testing.T) {
	t.Parallel()
	_, err := LoadV2FromBytes([]byte(`{"schema": 2, "totally_unknown": true}`))
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !containsUnknownField(err.Error()) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadV2FromBytes_InvalidJSON(t *testing.T) {
	t.Parallel()
	_, err := LoadV2FromBytes([]byte(`{not json`))
	if err == nil {
		t.Fatal("expected JSON parse error")
	}
}

// TestSynthesizeFromLegacy_RoundTripFromBytes verifies the fully-loaded
// legacy parser feeds the synthesizer correctly. Catches mismatches between
// accessors and the synthesizer that unit cases on synthesize alone would miss.
func TestSynthesizeFromLegacy_RoundTripFromBytes(t *testing.T) {
	t.Parallel()
	legacyJSON := `{
		"enabled": true,
		"log_level": "` + debugLevel + `",
		"commit_linking": "always",
		"absolute_git_hook_path": true,
		"external_agents": true,
		"vercel": true,
		"telemetry": true,
		"sign_checkpoint_commits": false,
		"summary_timeout_seconds": 45,
		"summary_generation": {"provider": "` + providerClaudeCC + `", "model": "sonnet"},
		"redaction": {"pii": {"enabled": true}},
		"strategy_options": {
			"checkpoints_version": 2,
			"gmeta": true,
			"summarize": {"enabled": true},
			"checkpoint_remote": {"provider": "github", "repo": "org/repo"},
			"filtered_fetches": true,
			"push_sessions": false,
			"full_transcript_generation_retention_days": 30
		}
	}`
	legacy, err := LoadFromBytes([]byte(legacyJSON))
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}

	got := synthesizeFromLegacy(legacy)

	want := &Settings{
		Schema:   CurrentSchemaVersion,
		Enabled:  true,
		Logging:  LoggingConfig{Level: debugLevel},
		Hooks:    HooksConfig{CommitLinking: CommitLinkingAlways, AbsoluteGitHookPath: true},
		Features: FeaturesConfig{Summarize: true, ExternalAgents: true, Vercel: true},
		Checkpoints: CheckpointsConfig{
			Primary:                     BackendConfig{Type: BackendTypeV2},
			Mirrors:                     []BackendConfig{{Type: BackendTypeGmeta}},
			Git:                         &CheckpointRemoteConfig{Provider: "github", Repo: "org/repo"},
			FullTranscriptRetentionDays: 30,
			SignCommits:                 boolPtr(false),
			FilteredFetches:             true,
			PushSessions:                boolPtr(false),
		},
		Redaction: &RedactionSettings{PII: &PIISettings{Enabled: true}},
		Telemetry: boolPtr(true),
		SummaryGeneration: &SummaryGenerationConfig{
			Provider:       providerClaudeCC,
			Model:          modelSonnet,
			TimeoutSeconds: 45,
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\n got: %+v\nwant: %+v", got, want)
	}
}

// TestLoadV2_PartialCheckpointsOverride is the regression test for the
// finding that wholesale top-level group replacement could silently erase
// the primary backend. The local override here only specifies mirrors;
// primary, retention, and other fields must be preserved from the main file.
func TestLoadV2_PartialCheckpointsOverride(t *testing.T) {
	setupSettingsDir(t,
		`{
			"schema": 2,
			"checkpoints": {
				"primary": {"type": "v2"},
				"full_transcript_retention_days": 90,
				"filtered_fetches": true
			}
		}`,
		`{
			"schema": 2,
			"checkpoints": {"mirrors": [{"type": "gmeta"}]}
		}`,
	)
	got, err := LoadV2(context.Background())
	if err != nil {
		t.Fatalf("LoadV2: %v", err)
	}
	if got.Checkpoints.Primary.Type != BackendTypeV2 {
		t.Fatalf("Primary.Type = %q, want v2 (preserved)", got.Checkpoints.Primary.Type)
	}
	if got.Checkpoints.FullTranscriptRetentionDays != 90 {
		t.Fatalf("FullTranscriptRetentionDays = %d, want 90 (preserved)", got.Checkpoints.FullTranscriptRetentionDays)
	}
	if !got.Checkpoints.FilteredFetches {
		t.Fatal("FilteredFetches = false, want true (preserved)")
	}
	if len(got.Checkpoints.Mirrors) != 1 || got.Checkpoints.Mirrors[0].Type != BackendTypeGmeta {
		t.Fatalf("Mirrors = %v, want [{gmeta}] (added by override)", got.Checkpoints.Mirrors)
	}
}

// TestLoadV2_PartialNestedFieldsPreserved is a stronger version of the
// above that exercises every nested group with a partial override. Acts
// as a sanity check on the assumption that Go's json decoder preserves
// struct fields not mentioned in the JSON.
func TestLoadV2_PartialNestedFieldsPreserved(t *testing.T) {
	setupSettingsDir(t,
		`{
			"schema": 2,
			"enabled": true,
			"logging": {"level": "info"},
			"checkpoints": {
				"primary": {"type": "v2"},
				"full_transcript_retention_days": 60
			},
			"hooks": {"commit_linking": "always", "absolute_git_hook_path": true},
			"features": {"summarize": true, "external_agents": true}
		}`,
		`{
			"schema": 2,
			"hooks": {"commit_linking": "prompt"},
			"features": {"vercel": true}
		}`,
	)
	got, err := LoadV2(context.Background())
	if err != nil {
		t.Fatalf("LoadV2: %v", err)
	}
	if got.Hooks.CommitLinking != CommitLinkingPrompt {
		t.Fatalf("Hooks.CommitLinking = %q, want prompt (overridden)", got.Hooks.CommitLinking)
	}
	if !got.Hooks.AbsoluteGitHookPath {
		t.Fatal("Hooks.AbsoluteGitHookPath = false, want true (preserved)")
	}
	if !got.Features.Vercel {
		t.Fatal("Features.Vercel = false, want true (overridden)")
	}
	if !got.Features.Summarize {
		t.Fatal("Features.Summarize = false, want true (preserved)")
	}
	if !got.Features.ExternalAgents {
		t.Fatal("Features.ExternalAgents = false, want true (preserved)")
	}
	if got.Checkpoints.Primary.Type != BackendTypeV2 {
		t.Fatalf("Primary.Type = %q, want v2 (preserved)", got.Checkpoints.Primary.Type)
	}
	if got.Checkpoints.FullTranscriptRetentionDays != 60 {
		t.Fatalf("FullTranscriptRetentionDays = %d, want 60 (preserved)", got.Checkpoints.FullTranscriptRetentionDays)
	}
	if got.Logging.Level != "info" {
		t.Fatalf("Logging.Level = %q, want info (preserved)", got.Logging.Level)
	}
}

// TestLoadV2_LegacyOverridePartial verifies that a legacy override only
// touches the v2 fields it explicitly mentions, leaving the rest of the v2
// base intact. Mirror of TestLoadV2_PartialCheckpointsOverride for the
// legacy-shape override path.
func TestLoadV2_LegacyOverridePartial(t *testing.T) {
	setupSettingsDir(t,
		`{
			"schema": 2,
			"checkpoints": {"primary": {"type": "v2"}, "filtered_fetches": true},
			"hooks": {"commit_linking": "always"}
		}`,
		`{"log_level": "`+debugLevel+`"}`,
	)
	got, err := LoadV2(context.Background())
	if err != nil {
		t.Fatalf("LoadV2: %v", err)
	}
	if got.Logging.Level != debugLevel {
		t.Fatalf("Logging.Level = %q, want %q", got.Logging.Level, debugLevel)
	}
	if got.Checkpoints.Primary.Type != BackendTypeV2 {
		t.Fatalf("Primary.Type = %q, want v2 (preserved)", got.Checkpoints.Primary.Type)
	}
	if !got.Checkpoints.FilteredFetches {
		t.Fatal("FilteredFetches = false, want true (preserved)")
	}
	if got.Hooks.CommitLinking != CommitLinkingAlways {
		t.Fatalf("Hooks.CommitLinking = %q, want always (preserved)", got.Hooks.CommitLinking)
	}
}

// TestLoadV2_LegacyOverridePartialStrategyOptions is the legacy-override
// counterpart for strategy_options-keyed fields that map onto Checkpoints.
func TestLoadV2_LegacyOverridePartialStrategyOptions(t *testing.T) {
	setupSettingsDir(t,
		`{
			"schema": 2,
			"checkpoints": {
				"primary": {"type": "v1"},
				"full_transcript_retention_days": 30,
				"filtered_fetches": true
			}
		}`,
		`{"strategy_options": {"checkpoints_v2": true}}`,
	)
	got, err := LoadV2(context.Background())
	if err != nil {
		t.Fatalf("LoadV2: %v", err)
	}
	if got.Checkpoints.Primary.Type != BackendTypeV2 {
		t.Fatalf("Primary.Type = %q, want v2 (overridden)", got.Checkpoints.Primary.Type)
	}
	if got.Checkpoints.FullTranscriptRetentionDays != 30 {
		t.Fatalf("FullTranscriptRetentionDays = %d, want 30 (preserved)", got.Checkpoints.FullTranscriptRetentionDays)
	}
	if !got.Checkpoints.FilteredFetches {
		t.Fatal("FilteredFetches = false, want true (preserved)")
	}
}

// TestSettings_Validate covers the semantic validation rules.
func TestSettings_Validate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		s       *Settings
		wantErr string
	}{
		{
			name:    "nil settings",
			s:       nil,
			wantErr: "nil",
		},
		{
			name: "wrong schema",
			s: &Settings{
				Schema:      1,
				Checkpoints: CheckpointsConfig{Primary: BackendConfig{Type: BackendTypeV1}},
			},
			wantErr: "schema",
		},
		{
			name: "empty primary type",
			s: &Settings{
				Schema: CurrentSchemaVersion,
			},
			wantErr: "checkpoints.primary.type",
		},
		{
			name: "unknown primary type",
			s: &Settings{
				Schema:      CurrentSchemaVersion,
				Checkpoints: CheckpointsConfig{Primary: BackendConfig{Type: "totally-fake"}},
			},
			wantErr: "checkpoints.primary.type",
		},
		{
			name: "unknown mirror type",
			s: &Settings{
				Schema: CurrentSchemaVersion,
				Checkpoints: CheckpointsConfig{
					Primary: BackendConfig{Type: BackendTypeV2},
					Mirrors: []BackendConfig{{Type: "unknown-backend"}},
				},
			},
			wantErr: "checkpoints.mirrors[0].type",
		},
		{
			name: "summary model without provider",
			s: &Settings{
				Schema:            CurrentSchemaVersion,
				Checkpoints:       CheckpointsConfig{Primary: BackendConfig{Type: BackendTypeV1}},
				SummaryGeneration: &SummaryGenerationConfig{Model: modelSonnet},
			},
			wantErr: "summary_generation.model",
		},
		{
			name: "valid: defaults",
			s:    defaultSettings(),
		},
		{
			name: "valid: full v2 with gmeta mirror",
			s: &Settings{
				Schema: CurrentSchemaVersion,
				Checkpoints: CheckpointsConfig{
					Primary: BackendConfig{Type: BackendTypeV2},
					Mirrors: []BackendConfig{{Type: BackendTypeGmeta}},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.s.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestLoadV2_RejectsInvalidV2 verifies that LoadV2 surfaces validation
// errors as load-time failures rather than letting them through to first use.
func TestLoadV2_RejectsInvalidV2(t *testing.T) {
	setupSettingsDir(t, `{
		"schema": 2,
		"checkpoints": {"primary": {"type": "totally-fake"}}
	}`, "")
	_, err := LoadV2(context.Background())
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "checkpoints.primary.type") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestLoadV2_LegacyOverrideRedactionGranular is the regression test for
// the wholesale-replacement bug on the legacy-override path: a legacy
// override that mentions only one PII field should preserve the v2 base's
// other PII fields and the rest of the redaction config.
func TestLoadV2_LegacyOverrideRedactionGranular(t *testing.T) {
	setupSettingsDir(t,
		`{
			"schema": 2,
			"checkpoints": {"primary": {"type": "v2"}},
			"redaction": {
				"pii": {"enabled": true, "email": true, "phone": true, "address": true}
			}
		}`,
		`{"redaction": {"pii": {"address": false}}}`,
	)
	got, err := LoadV2(context.Background())
	if err != nil {
		t.Fatalf("LoadV2: %v", err)
	}
	if got.Redaction == nil || got.Redaction.PII == nil {
		t.Fatalf("Redaction = %+v, want populated", got.Redaction)
	}
	pii := got.Redaction.PII
	if !pii.Enabled {
		t.Fatal("PII.Enabled = false, want true (preserved from base)")
	}
	if pii.Email == nil || !*pii.Email {
		t.Fatalf("PII.Email = %v, want true (preserved from base)", pii.Email)
	}
	if pii.Phone == nil || !*pii.Phone {
		t.Fatalf("PII.Phone = %v, want true (preserved from base)", pii.Phone)
	}
	if pii.Address == nil || *pii.Address {
		t.Fatalf("PII.Address = %v, want explicit false (overridden)", pii.Address)
	}
}

// TestLoadV2_LegacyOverrideSummaryTimeoutOnly verifies that a legacy
// summary_timeout_seconds-only override preserves the v2 base's provider
// and model. Previously this was wholesale-replaced.
func TestLoadV2_LegacyOverrideSummaryTimeoutOnly(t *testing.T) {
	setupSettingsDir(t,
		`{
			"schema": 2,
			"checkpoints": {"primary": {"type": "v2"}},
			"summary_generation": {"provider": "`+providerClaudeCC+`", "model": "sonnet", "timeout_seconds": 60}
		}`,
		`{"summary_timeout_seconds": 30}`,
	)
	got, err := LoadV2(context.Background())
	if err != nil {
		t.Fatalf("LoadV2: %v", err)
	}
	if got.SummaryGeneration == nil {
		t.Fatal("SummaryGeneration = nil, want preserved")
	}
	if got.SummaryGeneration.Provider != providerClaudeCC {
		t.Fatalf("Provider = %q, want %q (preserved)", got.SummaryGeneration.Provider, providerClaudeCC)
	}
	if got.SummaryGeneration.Model != modelSonnet {
		t.Fatalf("Model = %q, want sonnet (preserved)", got.SummaryGeneration.Model)
	}
	if got.SummaryGeneration.TimeoutSeconds != 30 {
		t.Fatalf("TimeoutSeconds = %d, want 30 (overridden)", got.SummaryGeneration.TimeoutSeconds)
	}
}

// TestLoadV2_LegacyOverrideSummaryProviderOnly verifies that an override
// of just summary_generation.provider preserves the v2 base's existing
// timeout and only updates the explicitly-mentioned field.
func TestLoadV2_LegacyOverrideSummaryProviderOnly(t *testing.T) {
	setupSettingsDir(t,
		`{
			"schema": 2,
			"checkpoints": {"primary": {"type": "v2"}},
			"summary_generation": {"provider": "`+providerClaudeCC+`", "model": "sonnet", "timeout_seconds": 45}
		}`,
		`{"summary_generation": {"provider": "codex"}}`,
	)
	got, err := LoadV2(context.Background())
	if err != nil {
		t.Fatalf("LoadV2: %v", err)
	}
	if got.SummaryGeneration == nil {
		t.Fatal("SummaryGeneration = nil")
	}
	if got.SummaryGeneration.Provider != "codex" {
		t.Fatalf("Provider = %q, want codex (overridden)", got.SummaryGeneration.Provider)
	}
	if got.SummaryGeneration.Model != modelSonnet {
		t.Fatalf("Model = %q, want sonnet (preserved)", got.SummaryGeneration.Model)
	}
	if got.SummaryGeneration.TimeoutSeconds != 45 {
		t.Fatalf("TimeoutSeconds = %d, want 45 (preserved)", got.SummaryGeneration.TimeoutSeconds)
	}
}

// TestLoadV2_TestdataExamples loads each canonical example file under
// testdata/ via LoadV2FromBytes and asserts the parsed structure. These
// examples double as hand-readable documentation of the v2 shape.
func TestLoadV2_TestdataExamples(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		file  string
		check func(t *testing.T, s *Settings)
	}{
		{
			name: "minimal",
			file: "v2-minimal.json",
			check: func(t *testing.T, s *Settings) {
				if s.Checkpoints.Primary.Type != BackendTypeV2 {
					t.Fatalf("Primary.Type = %q, want v2", s.Checkpoints.Primary.Type)
				}
				if len(s.Checkpoints.Mirrors) != 0 {
					t.Fatalf("Mirrors = %v, want empty", s.Checkpoints.Mirrors)
				}
			},
		},
		{
			name: "with gmeta mirror",
			file: "v2-with-gmeta-mirror.json",
			check: func(t *testing.T, s *Settings) {
				if len(s.Checkpoints.Mirrors) != 1 || s.Checkpoints.Mirrors[0].Type != BackendTypeGmeta {
					t.Fatalf("Mirrors = %v, want [{gmeta}]", s.Checkpoints.Mirrors)
				}
			},
		},
		{
			name: "with git config",
			file: "v2-with-git-config.json",
			check: func(t *testing.T, s *Settings) {
				if s.Checkpoints.Git == nil {
					t.Fatal("Git = nil, want populated")
				}
				if s.Checkpoints.Git.Provider != "github" || s.Checkpoints.Git.Repo != "myorg/myrepo-checkpoints" {
					t.Fatalf("Git = %+v", s.Checkpoints.Git)
				}
			},
		},
		{
			name: "kitchen sink",
			file: "v2-kitchen-sink.json",
			check: func(t *testing.T, s *Settings) {
				if s.Logging.Level != debugLevel {
					t.Fatalf("Logging.Level = %q", s.Logging.Level)
				}
				if s.Checkpoints.Git == nil || s.Checkpoints.FullTranscriptRetentionDays != 90 {
					t.Fatalf("Checkpoints = %+v", s.Checkpoints)
				}
				if !s.Features.Summarize || !s.Features.ExternalAgents {
					t.Fatalf("Features = %+v", s.Features)
				}
				if s.SummaryGeneration == nil || s.SummaryGeneration.Provider != providerClaudeCC {
					t.Fatalf("SummaryGeneration = %+v", s.SummaryGeneration)
				}
			},
		},
		{
			name: "legacy equivalent",
			file: "legacy-equivalent.json",
			check: func(t *testing.T, s *Settings) {
				if s.Checkpoints.Primary.Type != BackendTypeV2 {
					t.Fatalf("Primary.Type = %q, want v2 (synthesized)", s.Checkpoints.Primary.Type)
				}
				if len(s.Checkpoints.Mirrors) != 1 || s.Checkpoints.Mirrors[0].Type != BackendTypeGmeta {
					t.Fatalf("Mirrors = %v, want [{gmeta}]", s.Checkpoints.Mirrors)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := readTestdataFile(t, tt.file)
			got, err := LoadV2FromBytes(data)
			if err != nil {
				t.Fatalf("LoadV2FromBytes(%s): %v", tt.file, err)
			}
			tt.check(t, got)
		})
	}
}

// TestLoadV2_TestdataLegacyEquivalentMatchesKitchenSink verifies that
// legacy-equivalent.json synthesizes to the same Settings as parsing
// v2-kitchen-sink.json directly. This pair documents the legacy → v2
// migration map concretely; if a new legacy key gets added to the
// synthesizer, both files should be updated and this test will catch
// drift between them.
func TestLoadV2_TestdataLegacyEquivalentMatchesKitchenSink(t *testing.T) {
	t.Parallel()
	v2, err := LoadV2FromBytes(readTestdataFile(t, "v2-kitchen-sink.json"))
	if err != nil {
		t.Fatalf("LoadV2FromBytes(v2-kitchen-sink): %v", err)
	}
	legacy, err := LoadV2FromBytes(readTestdataFile(t, "legacy-equivalent.json"))
	if err != nil {
		t.Fatalf("LoadV2FromBytes(legacy-equivalent): %v", err)
	}
	if !reflect.DeepEqual(v2, legacy) {
		t.Fatalf("legacy-equivalent does not match v2-kitchen-sink:\n v2:     %+v\n legacy: %+v", v2, legacy)
	}
}

func readTestdataFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

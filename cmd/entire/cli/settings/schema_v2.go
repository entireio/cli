// Schema-v2 settings types. See synthesizeFromLegacy in load_v2.go for
// the v1→v2 mapping.

package settings

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// CurrentSchemaVersion is the schema version emitted by v2 writers.
// Files marked with this version are parsed via the v2 path; older files
// are loaded via the legacy parser and synthesized into Settings on the fly.
const CurrentSchemaVersion = 2

// Settings is the schema-v2 representation of .entire/settings.json.
//
// All fields are JSON-omitempty where defaults are well-defined so that
// hand-written settings files stay minimal. Pointer types are used where
// "unset" must be distinguishable from "explicit false" (e.g. Telemetry,
// SignCommits, PushSessions).
type Settings struct {
	// Schema identifies the settings file schema version. Always 2 in this struct.
	Schema int `json:"schema"`

	// Enabled indicates whether Entire is active. When false, CLI commands
	// show a disabled message and hooks exit silently. Defaults to true.
	Enabled bool `json:"enabled"`

	// LocalDev indicates whether to use "go run" instead of the "entire"
	// binary. Used during development when the binary is not installed.
	LocalDev bool `json:"local_dev,omitempty"`

	// Logging configures runtime logging.
	Logging LoggingConfig `json:"logging,omitempty"`

	// Checkpoints configures permanent checkpoint storage and related ops.
	Checkpoints CheckpointsConfig `json:"checkpoints,omitempty"`

	// Hooks configures git hook behavior.
	Hooks HooksConfig `json:"hooks,omitempty"`

	// Features toggles optional behaviors.
	Features FeaturesConfig `json:"features,omitempty"`

	// Redaction configures PII redaction beyond the default secret detection.
	Redaction *RedactionSettings `json:"redaction,omitempty"`

	// Telemetry controls anonymous usage analytics.
	// nil = not asked yet, true = opted in, false = opted out.
	Telemetry *bool `json:"telemetry,omitempty"`

	// SummaryGeneration configures provider selection and timeout for
	// `entire explain --generate`.
	SummaryGeneration *SummaryGenerationConfig `json:"summary_generation,omitempty"`
}

// LoggingConfig configures runtime logging verbosity.
type LoggingConfig struct {
	// Level is the logging verbosity (debug, info, warn, error).
	// Can be overridden by ENTIRE_LOG_LEVEL. Defaults to "info" when unset.
	Level string `json:"level,omitempty"`
}

// CheckpointsConfig configures checkpoint storage and related operations.
//
// Primary serves all reads and is the authoritative writer. Mirrors receive
// best-effort fan-out writes (warn on failure, never serve reads). This
// shape replaces the legacy strategy_options{checkpoints_version, gmeta, ...}
// soup with explicit, typed selection.
type CheckpointsConfig struct {
	// Primary is the authoritative checkpoint backend.
	// Reads always come from Primary. Writes that fail here are fatal.
	Primary BackendConfig `json:"primary"`

	// Mirrors are best-effort write targets.
	// Mirror failures are logged but do not fail the operation. Mirrors
	// never serve reads — they are export targets, not sources of truth.
	Mirrors []BackendConfig `json:"mirrors,omitempty"`

	// Git configures the destination for git-based backends (v1, v2, gmeta).
	// Optional; when unset, the default origin is used. Per-backend overrides
	// can be added by giving BackendConfig its own Git field if a future use
	// case needs different destinations for different git backends — today
	// this single value applies to all of them.
	Git *CheckpointRemoteConfig `json:"git,omitempty"`

	// FullTranscriptRetentionDays is the retention window (in days) for
	// archived raw-transcript generations. Zero/negative falls back to
	// the documented default (60 days).
	FullTranscriptRetentionDays int `json:"full_transcript_retention_days,omitempty"`

	// SignCommits controls whether checkpoint commits are signed.
	// nil/true = sign (default), false = skip signing.
	SignCommits *bool `json:"sign_commits,omitempty"`

	// FilteredFetches enables --filter=blob:none on checkpoint fetches.
	FilteredFetches bool `json:"filtered_fetches,omitempty"`

	// PushSessions controls whether session refs are pushed to remotes.
	// nil = push (default), explicit false = do not push.
	PushSessions *bool `json:"push_sessions,omitempty"`
}

// BackendConfig identifies and configures a single checkpoint backend.
//
// The Type field selects the backend implementation (e.g. "v1", "v2",
// "gmeta"). Backend-specific configuration is added as additional fields
// here as backends are introduced (e.g. an "s3" backend would carry
// bucket/region; today every supported backend needs only Type).
type BackendConfig struct {
	// Type is the backend identifier. Recognized values: "v1", "v2", "gmeta".
	Type string `json:"type"`
}

// HooksConfig configures git hook behavior.
type HooksConfig struct {
	// CommitLinking controls how commits are linked to agent sessions.
	// "always" = auto-link without prompting, "prompt" = ask each commit.
	// Defaults to "prompt" when unset.
	CommitLinking string `json:"commit_linking,omitempty"`

	// AbsoluteGitHookPath embeds the full binary path in git hooks instead
	// of bare "entire". Needed for GUI git clients (Xcode, Tower, etc.)
	// that don't source shell profiles and can't find "entire" on PATH.
	AbsoluteGitHookPath bool `json:"absolute_git_hook_path,omitempty"`
}

// FeaturesConfig toggles optional product behaviors.
type FeaturesConfig struct {
	// Summarize enables AI-generated checkpoint summaries.
	Summarize bool `json:"summarize,omitempty"`

	// ExternalAgents enables discovery and registration of external agent
	// plugins (entire-agent-* binaries on $PATH).
	ExternalAgents bool `json:"external_agents,omitempty"`

	// Vercel marks the repository as using Vercel; the metadata branch
	// then includes a vercel.json that disables deployments for Entire branches.
	Vercel bool `json:"vercel,omitempty"`
}

// SummaryGenerationConfig configures `entire explain --generate`.
//
// Replaces legacy SummaryGenerationSettings + the top-level
// SummaryTimeoutSeconds field, grouping all summary-generation knobs.
type SummaryGenerationConfig struct {
	// Provider is the agent name for summary generation
	// (e.g. "claude-code", "codex", "gemini").
	Provider string `json:"provider,omitempty"`

	// Model is an optional model hint passed to the selected provider.
	Model string `json:"model,omitempty"`

	// TimeoutSeconds is an optional hard deadline for summary generation.
	// Zero or negative means "unset" — the caller picks the default.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// knownBackendTypes is the set of backend type identifiers Validate accepts.
// Kept here next to BackendConfig so adding a new backend type is a single
// place to update — Validate's error messages format from this map.
var knownBackendTypes = map[string]struct{}{
	BackendTypeV1:    {},
	BackendTypeV2:    {},
	BackendTypeGmeta: {},
}

// knownBackendTypesList returns the backend types in sorted order for use
// in error messages. Sorting keeps test assertions deterministic.
func knownBackendTypesList() string {
	names := make([]string, 0, len(knownBackendTypes))
	for n := range knownBackendTypes {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Validate checks the Settings for semantic correctness beyond what JSON
// decoding catches. Run this after parsing or merging to surface invalid
// configurations at load time rather than at first use.
//
// Current rules:
//   - Schema must be exactly CurrentSchemaVersion. Writers emit only the
//     current value, and Validate rejects others. (isSchemaV2 accepts >=
//     as a shape probe, but strict decode plus this check enforce the
//     actual contract.)
//   - Checkpoints.Primary.Type must be a known backend.
//   - Each Mirror's Type must be a known backend.
//   - SummaryGeneration.Model requires SummaryGeneration.Provider, matching
//     the legacy SummaryGenerationSettings.Validate semantics.
func (s *Settings) Validate() error {
	if s == nil {
		return errors.New("settings: nil")
	}
	if s.Schema != CurrentSchemaVersion {
		return fmt.Errorf("settings: schema = %d, want %d", s.Schema, CurrentSchemaVersion)
	}
	if _, ok := knownBackendTypes[s.Checkpoints.Primary.Type]; !ok {
		return fmt.Errorf("checkpoints.primary.type = %q: must be one of %s", s.Checkpoints.Primary.Type, knownBackendTypesList())
	}
	for i, m := range s.Checkpoints.Mirrors {
		if _, ok := knownBackendTypes[m.Type]; !ok {
			return fmt.Errorf("checkpoints.mirrors[%d].type = %q: must be one of %s", i, m.Type, knownBackendTypesList())
		}
	}
	if s.SummaryGeneration != nil && s.SummaryGeneration.Model != "" && s.SummaryGeneration.Provider == "" {
		return fmt.Errorf("summary_generation.model %q set without summary_generation.provider", s.SummaryGeneration.Model)
	}
	return nil
}

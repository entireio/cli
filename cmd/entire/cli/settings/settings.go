// Package settings provides configuration loading for Entire.
// This package is separate from cli to allow strategy package to import it
// without creating an import cycle (cli imports strategy).
package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

const (
	// EntireSettingsFile is the path to the Entire settings file
	EntireSettingsFile = ".entire/settings.json"
	// EntireSettingsLocalFile is the path to the local settings override file (not committed)
	EntireSettingsLocalFile = ".entire/settings.local.json"
	// ClonePreferencesFile is the path inside the git common dir for clone-local preferences.
	ClonePreferencesFile = "entire/preferences.json"
)

var (
	checkpointsVersionWarningOnce sync.Once
)

type worktreeRootContextKey struct{}

// WithWorktreeRoot returns a context that makes settings.Load resolve project
// and clone-local settings relative to worktreeRoot instead of the process cwd.
func WithWorktreeRoot(ctx context.Context, worktreeRoot string) context.Context {
	if worktreeRoot == "" {
		return ctx
	}
	return context.WithValue(ctx, worktreeRootContextKey{}, filepath.Clean(worktreeRoot))
}

func worktreeRootFromContext(ctx context.Context) (string, bool) {
	root, ok := ctx.Value(worktreeRootContextKey{}).(string)
	return root, ok && root != ""
}

// Commit linking mode constants.
const (
	// CommitLinkingAlways auto-links commits to sessions without prompting.
	CommitLinkingAlways = "always"
	// CommitLinkingPrompt prompts the user on each commit (default for existing users).
	CommitLinkingPrompt = "prompt"
)

// EntireSettings represents the .entire/settings.json configuration
type EntireSettings struct {
	// Enabled indicates whether Entire is active. When false, CLI commands
	// show a disabled message and hooks exit silently. Defaults to true.
	Enabled bool `json:"enabled"`

	// LocalDev indicates whether to use "go run" instead of the "entire" binary
	// This is used for development when the binary is not installed
	LocalDev bool `json:"local_dev,omitempty"`

	// LogLevel sets the logging verbosity (debug, info, warn, error).
	// Can be overridden by ENTIRE_LOG_LEVEL environment variable.
	// Defaults to "info".
	LogLevel string `json:"log_level,omitempty"`

	// StrategyOptions contains strategy-specific configuration
	StrategyOptions map[string]any `json:"strategy_options,omitempty"`

	// AbsoluteGitHookPath embeds the full binary path in git hooks instead of
	// bare "entire". This is needed for GUI git clients (Xcode, Tower, etc.)
	// that don't source shell profiles and can't find "entire" on PATH.
	AbsoluteGitHookPath bool `json:"absolute_git_hook_path,omitempty"`

	// Telemetry controls anonymous usage analytics.
	// nil = not asked yet (show prompt), true = opted in, false = opted out
	Telemetry *bool `json:"telemetry,omitempty"`

	// Redaction configures PII redaction behavior for transcripts and metadata.
	Redaction *RedactionSettings `json:"redaction,omitempty"`

	// Review maps agent name (e.g. "claude-code") to the review config for
	// that agent. When empty, `entire review` triggers the first-run picker.
	Review map[string]ReviewConfig `json:"review,omitempty"`

	// ReviewFixAgent is the default agent used when applying aggregate or
	// multi-agent review findings with `entire review --fix`.
	ReviewFixAgent string `json:"review_fix_agent,omitempty"`

	// CommitLinking controls how commits are linked to agent sessions.
	// "always" = auto-link without prompting, "prompt" = ask on each commit.
	// Defaults to "prompt" (preserves existing user behavior).
	CommitLinking string `json:"commit_linking,omitempty"`

	// ExternalAgents enables discovery and registration of external agent
	// plugins (entire-agent-* binaries on $PATH). Defaults to false.
	ExternalAgents bool `json:"external_agents,omitempty"`

	// SummaryGeneration stores provider preferences for explain --generate.
	// This is separate from strategy_options.summarize, which controls
	// checkpoint auto-summarize behavior.
	SummaryGeneration *SummaryGenerationSettings `json:"summary_generation,omitempty"`

	// Vercel indicates that the repository uses Vercel and the metadata branch
	// should include a vercel.json that disables deployments for Entire branches.
	Vercel bool `json:"vercel,omitempty"`

	// SummaryTimeoutSeconds is an optional hard deadline (in seconds) for
	// `entire explain --generate` summary generation. Zero or negative means
	// "unset" -- falls back to the per-run --summary-timeout-seconds flag
	// (if set) or the package default (5 minutes). Raise for very large
	// transcripts; lower (e.g. 30) for fast-fail in CI.
	SummaryTimeoutSeconds int `json:"summary_timeout_seconds,omitempty"`

	// SignCheckpointCommits controls whether checkpoint commits are signed.
	// nil/true = sign (default), false = skip signing.
	SignCheckpointCommits *bool `json:"sign_checkpoint_commits,omitempty"`

	// Deprecated: no longer used. Exists to tolerate old settings files
	// that still contain "strategy": "auto-commit" or similar.
	Strategy string `json:"strategy,omitempty"`
}

// ClonePreferences stores clone-local, uncommitted preferences that should be
// shared by linked worktrees in the same git clone.
//
// Stored in the git common dir (not the worktree) so multiple worktrees of the
// same clone see the same preferences. Not committed because the file lives
// inside .git/.
type ClonePreferences struct {
	Review         map[string]ReviewConfig `json:"review,omitempty"`
	ReviewFixAgent string                  `json:"review_fix_agent,omitempty"`

	// ReviewMigrationDismissed records that the user declined the one-shot
	// migration of review keys from project settings to clone-local prefs.
	// Once true, `entire review` stops prompting on every invocation; the
	// user can re-enable by editing this file or deleting the key.
	ReviewMigrationDismissed bool `json:"review_migration_dismissed,omitempty"`
}

// SummaryGenerationSettings configures provider selection for on-demand
// checkpoint summaries generated by explain --generate.
type SummaryGenerationSettings struct {
	// Provider is the selected summary provider agent name
	// (for example "claude-code", "codex", or "gemini").
	Provider string `json:"provider,omitempty"`

	// Model is an optional model hint passed to the selected provider.
	Model string `json:"model,omitempty"`
}

// Validate returns an error if the settings combination is semantically invalid.
// A model without a provider is meaningless: the model hint needs a provider to
// route to. The load path calls Validate() after merging, catching hand-edited
// files that land in this state.
func (s *SummaryGenerationSettings) Validate() error {
	if s == nil {
		return nil
	}
	if s.Model != "" && s.Provider == "" {
		return fmt.Errorf("summary_generation.model %q set without summary_generation.provider", s.Model)
	}
	return nil
}

// SetProvider updates the provider and optionally the model, clearing any stale
// model from the previous provider when switching without a replacement.
// An empty newProvider preserves the current provider; an empty newModel
// preserves the current model unless the provider is changing, in which case
// the old model is cleared to avoid passing (say) a Claude model to Codex.
func (s *SummaryGenerationSettings) SetProvider(newProvider, newModel string) {
	if s == nil {
		return
	}
	if newProvider != "" && s.Provider != "" && s.Provider != newProvider && newModel == "" {
		s.Model = ""
	}
	if newProvider != "" {
		s.Provider = newProvider
	}
	if newModel != "" {
		s.Model = newModel
	}
}

// RedactionSettings configures redaction behavior beyond the default secret detection.
type RedactionSettings struct {
	PII *PIISettings `json:"pii,omitempty"`

	// CustomRedactions is a label → RE2 regex map for user-defined patterns
	// to scrub from transcripts. Use it for internal credential shapes the
	// bundled detectors don't know about, project codenames, or any other
	// string pattern you don't want stored. Each match is replaced with the
	// bare "REDACTED" token used by the built-in secret layers, not the
	// "[REDACTED_<LABEL>]" token used by PII. Failed regex compilations are
	// logged via slog.Warn and the rule is skipped.
	CustomRedactions map[string]string `json:"custom_redactions,omitempty"`
}

// PIISettings configures PII detection categories.
// When Enabled is true, email and phone default to true; address defaults to false.
type PIISettings struct {
	Enabled        bool              `json:"enabled"`
	Email          *bool             `json:"email,omitempty"`
	Phone          *bool             `json:"phone,omitempty"`
	Address        *bool             `json:"address,omitempty"`
	CustomPatterns map[string]string `json:"custom_patterns,omitempty"`
}

// GetCommitLinking returns the effective commit linking mode.
// Returns the explicit value if set, otherwise defaults to "prompt"
// to preserve existing user behavior.
func (s *EntireSettings) GetCommitLinking() string {
	if s.CommitLinking != "" {
		return s.CommitLinking
	}
	return CommitLinkingPrompt
}

// SummaryTimeoutValue returns the configured hard deadline for
// `entire explain --generate` summary generation. Zero means "unset" --
// the caller picks the default. Negative values are treated as unset.
func (s *EntireSettings) SummaryTimeoutValue() time.Duration {
	if s.SummaryTimeoutSeconds < 1 {
		return 0
	}
	return time.Duration(s.SummaryTimeoutSeconds) * time.Second
}

// ReviewConfig holds the per-agent review configuration. Both fields are
// optional; together they describe what `entire review` should ask the
// agent to do.
//
// Precedence when composing the review prompt sent to the agent:
//   - If Prompt is non-empty, it is used verbatim.
//   - Otherwise, Skills are composed into a default template
//     ("Please run these review skills in order: 1. /X 2. /Y").
//
// Skills are always recorded on the checkpoint metadata regardless of
// which path composed the prompt — they're the structured, queryable
// tag alongside ReviewPrompt (which is the ground truth).
type ReviewConfig struct {
	// Skills is the list of slash-prefixed skill invocations configured
	// for this agent. May be empty when Prompt carries the full request.
	Skills []string `json:"skills,omitempty"`

	// Prompt, when non-empty, carries saved review instructions. When
	// Skills is non-empty it is appended after the selected skills; when
	// Skills is empty it is the full prompt for prompt-only review configs.
	Prompt string `json:"prompt,omitempty"`
}

// IsZero reports whether the config is effectively unset.
func (c ReviewConfig) IsZero() bool {
	return len(c.Skills) == 0 && c.Prompt == ""
}

// ReviewConfigFor returns the configured review config for the given agent.
// Returns a zero-value config when the agent has no entry; callers should
// check IsZero (or the individual fields) to decide whether configuration
// is present.
func (s *EntireSettings) ReviewConfigFor(agentName string) ReviewConfig {
	if s == nil {
		return ReviewConfig{}
	}
	return s.Review[agentName]
}

// Load loads the Entire settings from .entire/settings.json, then applies
// clone-local preferences from the git common dir, then applies any overrides
// from .entire/settings.local.json if it exists.
// Returns default settings if no settings or preferences file exists.
// Works correctly from any subdirectory within the repository.
func Load(ctx context.Context) (*EntireSettings, error) {
	if worktreeRoot, ok := worktreeRootFromContext(ctx); ok {
		return loadForWorktreeRoot(ctx, worktreeRoot)
	}

	// Get absolute paths for settings files
	settingsFileAbs, err := paths.AbsPath(ctx, EntireSettingsFile)
	if err != nil {
		settingsFileAbs = EntireSettingsFile // Fallback to relative
	}
	preferencesFileAbs := ""
	if path, prefErr := ClonePreferencesPath(ctx); prefErr == nil {
		preferencesFileAbs = path
	} else {
		// Log at Debug rather than silently dropping the preferences layer.
		// "Not in a git repo" is a legitimate case (some commands run outside
		// a repo), but a git PATH issue or .git/ permission failure is worth
		// finding via `ENTIRE_LOG_LEVEL=debug` when users report "my picker
		// choices vanished".
		logging.Debug(ctx, "clone preferences path unresolved; skipping preferences layer",
			slog.String("error", prefErr.Error()))
	}
	localSettingsFileAbs, err := paths.AbsPath(ctx, EntireSettingsLocalFile)
	if err != nil {
		localSettingsFileAbs = EntireSettingsLocalFile // Fallback to relative
	}

	return loadMergedSettings(settingsFileAbs, preferencesFileAbs, localSettingsFileAbs)
}

func loadForWorktreeRoot(ctx context.Context, worktreeRoot string) (*EntireSettings, error) {
	settingsFileAbs := filepath.Join(worktreeRoot, EntireSettingsFile)
	preferencesFileAbs := ""
	if path, prefErr := clonePreferencesPathForWorktreeRoot(ctx, worktreeRoot); prefErr == nil {
		preferencesFileAbs = path
	} else {
		logging.Debug(ctx, "clone preferences path unresolved; skipping preferences layer",
			slog.String("error", prefErr.Error()))
	}
	localSettingsFileAbs := filepath.Join(worktreeRoot, EntireSettingsLocalFile)
	return loadMergedSettings(settingsFileAbs, preferencesFileAbs, localSettingsFileAbs)
}

func clonePreferencesPathForWorktreeRoot(ctx context.Context, worktreeRoot string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", worktreeRoot, "rev-parse", "--git-common-dir")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}

	commonDir := strings.TrimSpace(string(output))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(worktreeRoot, commonDir)
	}
	return filepath.Join(filepath.Clean(commonDir), ClonePreferencesFile), nil
}

func loadMergedSettings(settingsFileAbs, preferencesFileAbs, localSettingsFileAbs string) (*EntireSettings, error) {
	// Load base settings
	settings, err := loadFromFile(settingsFileAbs)
	if err != nil {
		return nil, fmt.Errorf("reading settings file: %w", err)
	}

	if preferencesFileAbs != "" {
		preferences, err := loadClonePreferencesFromFile(preferencesFileAbs)
		if err != nil {
			return nil, fmt.Errorf("reading clone preferences file: %w", err)
		}
		applyClonePreferences(settings, preferences)
	}

	// Apply local overrides if they exist
	localData, err := os.ReadFile(localSettingsFileAbs) //nolint:gosec // path is from AbsPath or constant
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading local settings file: %w", err)
		}
		// Local file doesn't exist, continue without overrides
	} else {
		if err := mergeJSON(settings, localData); err != nil {
			return nil, fmt.Errorf("merging local settings: %w", err)
		}
	}

	// Re-validate after merge. Individual files are validated by loadFromFile,
	// but mergeJSON patches fields independently and can produce combinations
	// (e.g. model without provider when the local override sets only a model
	// on top of a base with no provider) that neither file alone contained.
	if err := settings.SummaryGeneration.Validate(); err != nil {
		return nil, fmt.Errorf("merged settings invalid: %w", err)
	}

	return settings, nil
}

// LoadFromFile loads settings from a specific file path without merging local overrides.
// Returns default settings if the file doesn't exist.
// Use this when you need to display individual settings files separately.
func LoadFromFile(filePath string) (*EntireSettings, error) {
	return loadFromFile(filePath)
}

// LoadProjectRaw reads .entire/settings.json as a generic JSON object so
// callers can inspect or mutate individual keys without losing unrelated
// fields to round-trip decoding.
//
// Returns:
//   - path: absolute path of the project settings file.
//   - raw: parsed JSON object, or an empty map when the file is missing.
//   - exists: false when the file does not exist (raw is empty); true otherwise.
//   - err: parse error or read error other than ENOENT.
//
// Pair with SaveProjectRaw for read-modify-write flows like the review-key
// migration. Owning the path resolution and raw IO here keeps callers from
// duplicating settings parsing in violation of the "Settings access must go
// through the settings package" rule in CLAUDE.md.
func LoadProjectRaw(ctx context.Context) (path string, raw map[string]json.RawMessage, exists bool, err error) {
	path, err = paths.AbsPath(ctx, EntireSettingsFile)
	if err != nil {
		path = EntireSettingsFile
	}
	data, readErr := os.ReadFile(path) //nolint:gosec // path is from AbsPath or a project-relative constant
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return path, map[string]json.RawMessage{}, false, nil
		}
		return path, nil, false, fmt.Errorf("reading project settings: %w", readErr)
	}
	raw = map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return path, nil, true, fmt.Errorf("parsing project settings: %w", err)
	}
	return path, raw, true, nil
}

// LoadLocalRaw reads .entire/settings.local.json as a generic JSON object,
// mirroring LoadProjectRaw for the per-developer overrides file. Returns
// exists=false (and an empty raw map) when the file does not exist — the
// common case for users who haven't created the local override file.
//
// Pair with the migration flow: callers can use this to detect when local
// overrides would mask a freshly-migrated setting, then warn the user
// before performing the migration.
func LoadLocalRaw(ctx context.Context) (path string, raw map[string]json.RawMessage, exists bool, err error) {
	path, err = paths.AbsPath(ctx, EntireSettingsLocalFile)
	if err != nil {
		path = EntireSettingsLocalFile
	}
	data, readErr := os.ReadFile(path) //nolint:gosec // path is from AbsPath or a project-relative constant
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return path, map[string]json.RawMessage{}, false, nil
		}
		return path, nil, false, fmt.Errorf("reading local settings: %w", readErr)
	}
	raw = map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return path, nil, true, fmt.Errorf("parsing local settings: %w", err)
	}
	return path, raw, true, nil
}

// SaveProjectRaw writes a generic JSON object back to .entire/settings.json
// atomically (temp file + rename). Callers should mutate the map returned by
// LoadProjectRaw and pass it back here so unrelated fields are preserved.
func SaveProjectRaw(path string, raw map[string]json.RawMessage) error {
	data, err := jsonutil.MarshalIndentWithNewline(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal project settings: %w", err)
	}
	if err := jsonutil.WriteFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("writing project settings: %w", err)
	}
	return nil
}

// ClonePreferencesPath returns the clone-local preferences path in the git common dir.
func ClonePreferencesPath(ctx context.Context) (string, error) {
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}
	return filepath.Join(commonDir, ClonePreferencesFile), nil
}

// LoadClonePreferences loads clone-local preferences from the git common dir.
func LoadClonePreferences(ctx context.Context) (*ClonePreferences, error) {
	path, err := ClonePreferencesPath(ctx)
	if err != nil {
		return nil, err
	}
	return loadClonePreferencesFromFile(path)
}

// SaveClonePreferences saves clone-local preferences to the git common dir.
func SaveClonePreferences(ctx context.Context, prefs *ClonePreferences) error {
	path, err := ClonePreferencesPath(ctx)
	if err != nil {
		return err
	}
	return saveClonePreferencesToFile(prefs, path)
}

// LoadFromBytes parses settings from raw JSON bytes without merging local overrides.
// Use this when you have settings content from a non-file source (e.g., git show).
func LoadFromBytes(data []byte) (*EntireSettings, error) {
	s := &EntireSettings{Enabled: true}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(s); err != nil {
		return nil, fmt.Errorf("parsing settings: %w", err)
	}
	return s, nil
}

// loadFromFile loads settings from a specific file path.
// Returns default settings if the file doesn't exist.
func loadFromFile(filePath string) (*EntireSettings, error) {
	settings := &EntireSettings{
		Enabled: true, // Default to enabled
	}

	data, err := os.ReadFile(filePath) //nolint:gosec // path is from caller
	if err != nil {
		if os.IsNotExist(err) {
			return settings, nil
		}
		return nil, fmt.Errorf("%w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(settings); err != nil {
		return nil, fmt.Errorf("parsing settings file: %w", err)
	}

	// Validate commit_linking if set
	if settings.CommitLinking != "" && settings.CommitLinking != CommitLinkingAlways && settings.CommitLinking != CommitLinkingPrompt {
		return nil, fmt.Errorf("invalid commit_linking value %q: must be %q or %q", settings.CommitLinking, CommitLinkingAlways, CommitLinkingPrompt)
	}

	// SummaryGeneration is NOT validated here — individual files may
	// legitimately contain only a model (provider comes from another file).
	// Validation happens after merge in Load().

	return settings, nil
}

func loadClonePreferencesFromFile(filePath string) (*ClonePreferences, error) {
	prefs := &ClonePreferences{}

	data, err := os.ReadFile(filePath) //nolint:gosec // path is from caller
	if err != nil {
		if os.IsNotExist(err) {
			return prefs, nil
		}
		return nil, fmt.Errorf("%w", err)
	}

	// Lenient decoding here (vs. strict via DisallowUnknownFields in
	// loadFromFile for EntireSettings). Two reasons clone preferences need
	// the looser contract:
	//   1. They are rewritten on every picker save — a newer binary can
	//      introduce a field the older binary then sees as unknown, which
	//      under strict decoding would brick settings.Load for that older
	//      binary across the whole clone.
	//   2. The file lives in .git/, so users rarely hand-edit it; the
	//      typo-silently-ignored downside is theoretical here.
	// EntireSettings stays strict because it's committed and team-edited,
	// where unknown keys usually mean typos worth surfacing immediately.
	if err := json.Unmarshal(data, prefs); err != nil {
		return nil, fmt.Errorf("parsing preferences file: %w", err)
	}
	return prefs, nil
}

func saveClonePreferencesToFile(prefs *ClonePreferences, filePath string) error {
	if prefs == nil {
		prefs = &ClonePreferences{}
	}
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating preferences directory: %w", err)
	}

	data, err := jsonutil.MarshalIndentWithNewline(prefs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling preferences: %w", err)
	}

	if err := jsonutil.WriteFileAtomic(filePath, data, 0o644); err != nil {
		return fmt.Errorf("writing preferences file: %w", err)
	}
	return nil
}

func applyClonePreferences(settings *EntireSettings, prefs *ClonePreferences) {
	if prefs == nil {
		return
	}
	if prefs.Review != nil {
		settings.Review = prefs.Review
	}
	if prefs.ReviewFixAgent != "" {
		settings.ReviewFixAgent = prefs.ReviewFixAgent
	}
}

// mergeJSON merges JSON data into existing settings.
// Most fields only apply non-zero values from JSON. The review map is replaced
// whenever the key is present, so override files can clear or fully replace
// project-level review configuration.
func mergeJSON(settings *EntireSettings, data []byte) error {
	// Validate that there are no unknown keys using strict decoding.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var temp EntireSettings
	if err := dec.Decode(&temp); err != nil {
		return fmt.Errorf("parsing JSON: %w", err)
	}

	// Parse into a map to check which fields are present.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parsing JSON: %w", err)
	}

	if err := mergeScalarFields(settings, raw); err != nil {
		return err
	}
	if err := mergeStrategyOptions(settings, raw); err != nil {
		return err
	}
	if err := mergeSummaryGeneration(settings, raw); err != nil {
		return err
	}
	if err := mergeCommitLinking(settings, raw); err != nil {
		return err
	}
	if reviewRaw, ok := raw["review"]; ok {
		var review map[string]ReviewConfig
		if err := json.Unmarshal(reviewRaw, &review); err != nil {
			return fmt.Errorf("parsing review field: %w", err)
		}
		settings.Review = review
	}

	// Merge redaction sub-fields if present (field-level, not wholesale replace).
	if redactionRaw, ok := raw["redaction"]; ok {
		if settings.Redaction == nil {
			settings.Redaction = &RedactionSettings{}
		}
		if err := mergeRedaction(settings.Redaction, redactionRaw); err != nil {
			return fmt.Errorf("parsing redaction field: %w", err)
		}
	}

	return nil
}

// mergeScalarFields merges simple bool, *bool, string, and int fields from raw JSON.
func mergeScalarFields(settings *EntireSettings, raw map[string]json.RawMessage) error {
	if err := mergeRawBool(raw, "enabled", &settings.Enabled); err != nil {
		return err
	}
	if err := mergeRawBool(raw, "local_dev", &settings.LocalDev); err != nil {
		return err
	}
	if err := mergeRawBool(raw, "absolute_git_hook_path", &settings.AbsoluteGitHookPath); err != nil {
		return err
	}
	if err := mergeRawBool(raw, "external_agents", &settings.ExternalAgents); err != nil {
		return err
	}
	if err := mergeRawBool(raw, "vercel", &settings.Vercel); err != nil {
		return err
	}
	if err := mergeRawBoolPtr(raw, "telemetry", &settings.Telemetry); err != nil {
		return err
	}
	if err := mergeRawBoolPtr(raw, "sign_checkpoint_commits", &settings.SignCheckpointCommits); err != nil {
		return err
	}
	if err := mergeRawStringNonEmpty(raw, "log_level", &settings.LogLevel); err != nil {
		return err
	}
	if err := mergeRawStringNonEmpty(raw, "review_fix_agent", &settings.ReviewFixAgent); err != nil {
		return err
	}
	if err := mergeRawInt(raw, "summary_timeout_seconds", &settings.SummaryTimeoutSeconds); err != nil {
		return err
	}
	return nil
}

func mergeRawBool(raw map[string]json.RawMessage, key string, dst *bool) error {
	v, ok := raw[key]
	if !ok {
		return nil
	}
	return unmarshalField(key, v, dst)
}

func mergeRawBoolPtr(raw map[string]json.RawMessage, key string, dst **bool) error {
	v, ok := raw[key]
	if !ok {
		return nil
	}
	var b bool
	if err := unmarshalField(key, v, &b); err != nil {
		return err
	}
	*dst = &b
	return nil
}

func mergeRawStringNonEmpty(raw map[string]json.RawMessage, key string, dst *string) error {
	v, ok := raw[key]
	if !ok {
		return nil
	}
	var s string
	if err := unmarshalField(key, v, &s); err != nil {
		return err
	}
	if s != "" {
		*dst = s
	}
	return nil
}

func mergeRawInt(raw map[string]json.RawMessage, key string, dst *int) error {
	v, ok := raw[key]
	if !ok {
		return nil
	}
	return unmarshalField(key, v, dst)
}

func unmarshalField(key string, data json.RawMessage, dst any) error {
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("parsing %s field: %w", key, err)
	}
	return nil
}

func mergeStrategyOptions(settings *EntireSettings, raw map[string]json.RawMessage) error {
	optionsRaw, ok := raw["strategy_options"]
	if !ok {
		return nil
	}
	var opts map[string]any
	if err := unmarshalField("strategy_options", optionsRaw, &opts); err != nil {
		return err
	}
	if settings.StrategyOptions == nil {
		settings.StrategyOptions = opts
	} else {
		for k, v := range opts {
			settings.StrategyOptions[k] = v
		}
	}
	return nil
}

func mergeSummaryGeneration(settings *EntireSettings, raw map[string]json.RawMessage) error {
	summaryRaw, ok := raw["summary_generation"]
	if !ok {
		return nil
	}
	if settings.SummaryGeneration == nil {
		settings.SummaryGeneration = &SummaryGenerationSettings{}
	}

	var summaryFields map[string]json.RawMessage
	if err := unmarshalField("summary_generation", summaryRaw, &summaryFields); err != nil {
		return err
	}

	_, modelInOverride := summaryFields["model"]

	if providerRaw, ok := summaryFields["provider"]; ok {
		var provider string
		if err := unmarshalField("summary_generation.provider", providerRaw, &provider); err != nil {
			return err
		}
		// If the override switches providers without also setting a model,
		// the base's model was tuned to the old provider and would likely
		// cause a runtime failure when handed to the new one (e.g. codex
		// rejecting "sonnet"). Clear it so the new provider falls back to
		// its own default.
		if provider != settings.SummaryGeneration.Provider && !modelInOverride {
			settings.SummaryGeneration.Model = ""
		}
		settings.SummaryGeneration.Provider = provider
	}

	if modelRaw, ok := summaryFields["model"]; ok {
		var model string
		if err := unmarshalField("summary_generation.model", modelRaw, &model); err != nil {
			return err
		}
		settings.SummaryGeneration.Model = model
	}
	return nil
}

func mergeCommitLinking(settings *EntireSettings, raw map[string]json.RawMessage) error {
	commitLinkingRaw, ok := raw["commit_linking"]
	if !ok {
		return nil
	}
	var cl string
	if err := unmarshalField("commit_linking", commitLinkingRaw, &cl); err != nil {
		return err
	}
	if cl == "" {
		return nil
	}
	switch cl {
	case CommitLinkingAlways, CommitLinkingPrompt:
		settings.CommitLinking = cl
	default:
		return fmt.Errorf("invalid commit_linking value %q: must be %q or %q", cl, CommitLinkingAlways, CommitLinkingPrompt)
	}
	return nil
}

// mergeRedaction merges redaction overrides into existing RedactionSettings.
// Only fields present in the override JSON are applied.
func mergeRedaction(dst *RedactionSettings, data json.RawMessage) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parsing redaction: %w", err)
	}
	if piiRaw, ok := raw["pii"]; ok {
		if dst.PII == nil {
			dst.PII = &PIISettings{}
		}
		if err := mergePIISettings(dst.PII, piiRaw); err != nil {
			return err
		}
	}
	if csRaw, ok := raw["custom_redactions"]; ok {
		var cs map[string]string
		if err := json.Unmarshal(csRaw, &cs); err != nil {
			return fmt.Errorf("parsing redaction.custom_redactions: %w", err)
		}
		if dst.CustomRedactions == nil {
			dst.CustomRedactions = cs
		} else {
			for k, v := range cs {
				dst.CustomRedactions[k] = v
			}
		}
	}
	return nil
}

// mergePIISettings merges PII overrides into existing PIISettings.
// Only fields present in the override JSON are applied; missing fields
// are preserved from the base settings.
func mergePIISettings(dst *PIISettings, data json.RawMessage) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parsing pii: %w", err)
	}
	if v, ok := raw["enabled"]; ok {
		if err := json.Unmarshal(v, &dst.Enabled); err != nil {
			return fmt.Errorf("parsing pii.enabled: %w", err)
		}
	}
	if v, ok := raw["email"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return fmt.Errorf("parsing pii.email: %w", err)
		}
		dst.Email = &b
	}
	if v, ok := raw["phone"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return fmt.Errorf("parsing pii.phone: %w", err)
		}
		dst.Phone = &b
	}
	if v, ok := raw["address"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return fmt.Errorf("parsing pii.address: %w", err)
		}
		dst.Address = &b
	}
	if v, ok := raw["custom_patterns"]; ok {
		var cp map[string]string
		if err := json.Unmarshal(v, &cp); err != nil {
			return fmt.Errorf("parsing pii.custom_patterns: %w", err)
		}
		if dst.CustomPatterns == nil {
			dst.CustomPatterns = cp
		} else {
			for k, val := range cp {
				dst.CustomPatterns[k] = val
			}
		}
	}
	return nil
}

// IsSetUp returns true if Entire has been set up in the current repository.
// This checks if .entire/settings.json exists.
// Use this to avoid creating files/directories in repos where Entire was never enabled.
func IsSetUp(ctx context.Context) bool {
	settingsFileAbs, err := paths.AbsPath(ctx, EntireSettingsFile)
	if err != nil {
		return false
	}
	_, err = os.Stat(settingsFileAbs)
	return err == nil
}

// IsSetUpAny returns true if Entire has been set up in the current repository,
// checking both .entire/settings.json and .entire/settings.local.json.
// Use this to detect any prior setup, even if only local settings exist.
func IsSetUpAny(ctx context.Context) bool {
	if IsSetUp(ctx) {
		return true
	}
	localFileAbs, err := paths.AbsPath(ctx, EntireSettingsLocalFile)
	if err != nil {
		return false
	}
	_, err = os.Lstat(localFileAbs)
	return err == nil
}

// IsSetUpAndEnabled returns true if Entire is both set up and enabled.
// This checks if .entire/settings.json exists AND has enabled: true.
// Use this for hooks that should be no-ops when Entire is not active.
func IsSetUpAndEnabled(ctx context.Context) bool {
	if !IsSetUp(ctx) {
		return false
	}
	s, err := Load(ctx)
	if err != nil {
		return false
	}
	return s.Enabled
}

// IsCheckpointsV2Enabled checks if checkpoints v2 is enabled in settings.
// Returns false by default if settings cannot be loaded or the key is missing.
func IsCheckpointsV2Enabled(ctx context.Context) bool {
	settings, err := Load(ctx)
	if err != nil {
		return false
	}
	return settings.IsCheckpointsV2Enabled()
}

// CheckpointsVersion returns the configured checkpoints format version, or 1
// if settings cannot be loaded or the value is unset/invalid.
func CheckpointsVersion(ctx context.Context) int {
	s, err := Load(ctx)
	if err != nil {
		return 1
	}
	version := s.CheckpointsVersion()
	if s.StrategyOptions != nil {
		if configured, ok := s.StrategyOptions["checkpoints_version"]; ok {
			if _, supported := parseCheckpointsVersion(configured); !supported {
				checkpointsVersionWarningOnce.Do(func() {
					fmt.Fprintf(os.Stderr,
						"[entire] unsupported strategy_options.checkpoints_version %v detected in settings. Falling back to the default version (1).\n",
						configured,
					)
				})
			}
		}
	}
	return version
}

// WarnIfCheckpointsV2Disallowed emits the user-facing fallback warning when a
// settings file still requests checkpoints v2. Call this from push-time flows
// so users learn why v1 metadata is being pushed instead.
func WarnIfCheckpointsV2Disallowed(ctx context.Context) {
	s, err := Load(ctx)
	if err != nil {
		return
	}
	s.WarnIfCheckpointsV2Disallowed()
}

// IsFilteredFetchesEnabled checks if filtered fetches should be used.
// When enabled, filtered fetches always resolve remote names to URLs first so
// git does not persist promisor settings onto named remotes in local config.
// Returns false by default.
func IsFilteredFetchesEnabled(ctx context.Context) bool {
	s, err := Load(ctx)
	if err != nil {
		return false
	}
	return s.IsFilteredFetchesEnabled()
}

// IsSummarizeEnabled checks if auto-summarize is enabled in settings.
// Returns false by default if settings cannot be loaded or the key is missing.
func IsSummarizeEnabled(ctx context.Context) bool {
	settings, err := Load(ctx)
	if err != nil {
		return false
	}
	return settings.IsSummarizeEnabled()
}

// IsSummarizeEnabled checks if auto-summarize is enabled in this settings instance.
func (s *EntireSettings) IsSummarizeEnabled() bool {
	if s.StrategyOptions == nil {
		return false
	}
	summarizeOpts, ok := s.StrategyOptions["summarize"].(map[string]any)
	if !ok {
		return false
	}
	enabled, ok := summarizeOpts["enabled"].(bool)
	if !ok {
		return false
	}
	return enabled
}

// CheckpointRemoteConfig holds the structured checkpoint remote configuration.
// Stored in strategy_options.checkpoint_remote as {"provider": "github", "repo": "org/repo"}.
type CheckpointRemoteConfig struct {
	Provider string // e.g., "github"
	Repo     string // e.g., "org/checkpoints-repo"
}

// Owner returns the owner portion of the repo field (before the slash).
// Returns empty string if the repo field doesn't contain a slash.
func (c *CheckpointRemoteConfig) Owner() string {
	parts := strings.SplitN(c.Repo, "/", 2)
	if len(parts) < 2 {
		return ""
	}
	return parts[0]
}

// GetCheckpointRemote returns the configured checkpoint remote.
// Expects a structured object: {"provider": "github", "repo": "org/repo"}.
// Returns nil if not configured, wrong type, or missing required fields.
func (s *EntireSettings) GetCheckpointRemote() *CheckpointRemoteConfig {
	if s.StrategyOptions == nil {
		return nil
	}
	val, ok := s.StrategyOptions["checkpoint_remote"]
	if !ok {
		return nil
	}
	m, ok := val.(map[string]any)
	if !ok {
		return nil
	}
	provider, providerOK := m["provider"].(string)
	repo, repoOK := m["repo"].(string)
	if !providerOK || !repoOK || provider == "" || repo == "" {
		return nil
	}
	if !strings.Contains(repo, "/") {
		return nil
	}
	return &CheckpointRemoteConfig{Provider: provider, Repo: repo}
}

// IsCheckpointsV2Enabled checks if checkpoints v2 is enabled for read paths.
// Existing v2 checkpoint metadata remains readable while new writes use v1.
func (s *EntireSettings) IsCheckpointsV2Enabled() bool {
	if s.StrategyOptions == nil {
		return false
	}
	if val, ok := s.StrategyOptions["checkpoints_version"]; ok {
		version, supported := parseCheckpointsVersion(val)
		if supported && version == 2 {
			return true
		}
	}
	val, ok := s.StrategyOptions["checkpoints_v2"].(bool)
	return ok && val
}

// CheckpointsVersion returns the configured checkpoints format version from
// strategy_options.checkpoints_version. Returns 1 when unset, invalid, or
// unsupported. Version 2 is no longer an exclusive storage mode; reads use
// IsCheckpointsV2Enabled to enable dual v2/v1 lookup when legacy settings are
// present.
func (s *EntireSettings) CheckpointsVersion() int {
	if s.StrategyOptions == nil {
		return 1
	}
	val, ok := s.StrategyOptions["checkpoints_version"]
	if !ok {
		return 1
	}
	version, ok := parseCheckpointsVersion(val)
	if ok && version == 1 {
		return 1
	}
	return 1
}

// WarnIfCheckpointsV2Disallowed emits the v2 fallback warning when any legacy
// settings key requests v2 writes or pushes.
func (s *EntireSettings) WarnIfCheckpointsV2Disallowed() {
	if val, ok := s.disallowedCheckpointsV2Value(); ok {
		warnCheckpointsV2Disallowed(val)
	}
}

func (s *EntireSettings) disallowedCheckpointsV2Value() (any, bool) {
	if s.StrategyOptions == nil {
		return nil, false
	}
	if val, ok := s.StrategyOptions["checkpoints_version"]; ok {
		version, supported := parseCheckpointsVersion(val)
		if supported && version == 2 {
			return val, true
		}
	}
	for _, key := range []string{"checkpoints_v2", "push_v2_refs", "push_v2"} {
		if val, ok := s.StrategyOptions[key].(bool); ok && val {
			return 2, true
		}
	}
	return nil, false
}

func warnCheckpointsV2Disallowed(val any) {
	fmt.Fprintf(os.Stderr,
		"[entire] strategy_options.checkpoints_version %v is no longer supported. Falling back to version 1\n",
		val,
	)
}

func parseCheckpointsVersion(val any) (int, bool) {
	v, ok := val.(int)
	if ok && (v == 1 || v == 2) {
		return v, true
	}
	floatV, ok := val.(float64)
	if ok && (floatV == 1 || floatV == 2) {
		return int(floatV), true
	}
	stringV, ok := val.(string)
	if ok {
		parsed, err := strconv.Atoi(stringV)
		if err == nil && (parsed == 1 || parsed == 2) {
			return parsed, true
		}
	}
	return 1, false
}

// IsFilteredFetchesEnabled checks if fetches should use --filter=blob:none.
// When enabled, filtered fetches always use resolved URLs rather than remote
// names to avoid persisting promisor settings onto named remotes.
func (s *EntireSettings) IsFilteredFetchesEnabled() bool {
	if s.StrategyOptions == nil {
		return false
	}
	val, ok := s.StrategyOptions["filtered_fetches"].(bool)
	return ok && val
}

// IsPushSessionsDisabled checks if push_sessions is disabled in settings.
// Returns true if push_sessions is explicitly set to false.
func (s *EntireSettings) IsPushSessionsDisabled() bool {
	if s.StrategyOptions == nil {
		return false
	}
	val, exists := s.StrategyOptions["push_sessions"]
	if !exists {
		return false
	}
	if boolVal, ok := val.(bool); ok {
		return !boolVal // disabled = !push_sessions
	}
	return false
}

// IsExternalAgentsEnabled checks if external agent discovery is enabled in settings.
// Returns false by default if settings cannot be loaded or the key is missing.
func IsExternalAgentsEnabled(ctx context.Context) bool {
	s, err := Load(ctx)
	if err != nil {
		return false
	}
	return s.ExternalAgents
}

// IsSignCheckpointCommitsEnabled returns true if checkpoint commits should be signed.
// Defaults to true when the setting is not explicitly set.
func (s *EntireSettings) IsSignCheckpointCommitsEnabled() bool {
	return s.SignCheckpointCommits == nil || *s.SignCheckpointCommits
}

// IsSignCheckpointCommitsEnabled checks if checkpoint commit signing is enabled in settings.
// Returns true by default if settings cannot be loaded or the key is missing.
func IsSignCheckpointCommitsEnabled(ctx context.Context) bool {
	s, err := Load(ctx)
	if err != nil {
		return true
	}
	return s.IsSignCheckpointCommitsEnabled()
}

// Save saves the settings to .entire/settings.json.
func Save(ctx context.Context, settings *EntireSettings) error {
	return saveToFile(ctx, settings, EntireSettingsFile)
}

// SaveLocal saves the settings to .entire/settings.local.json.
func SaveLocal(ctx context.Context, settings *EntireSettings) error {
	return saveToFile(ctx, settings, EntireSettingsLocalFile)
}

// saveToFile saves settings to the specified file path.
func saveToFile(ctx context.Context, settings *EntireSettings, filePath string) error {
	// Get absolute path for the file
	filePathAbs, err := paths.AbsPath(ctx, filePath)
	if err != nil {
		filePathAbs = filePath // Fallback to relative
	}

	// Ensure directory exists
	dir := filepath.Dir(filePathAbs)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating settings directory: %w", err)
	}

	data, err := jsonutil.MarshalIndentWithNewline(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling settings: %w", err)
	}

	if err := jsonutil.WriteFileAtomic(filePathAbs, data, 0o644); err != nil {
		return fmt.Errorf("writing settings file: %w", err)
	}
	return nil
}

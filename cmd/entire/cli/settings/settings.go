// Package settings provides configuration loading for Entire.
// This package is separate from cli to allow strategy package to import it
// without creating an import cycle (cli imports strategy).
package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/redact"
)

const (
	// EntireSettingsFile is the path to the Entire settings file
	EntireSettingsFile = ".entire/settings.json"
	// EntireSettingsLocalFile is the path to the local settings override file (not committed)
	EntireSettingsLocalFile = ".entire/settings.local.json"
	// ClonePreferencesFile is the path inside the git common dir for clone-local preferences.
	ClonePreferencesFile = "entire/preferences.json"
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

	// ReviewProfiles maps profile names (e.g. "general", "security") to
	// named review setups. `entire review` runs one profile: its canonical task
	// is fanned out to the configured agents, then an optional master agent
	// consolidates the worker reports.
	ReviewProfiles map[string]ReviewProfileConfig `json:"review_profiles,omitempty"`

	// ReviewDefaultProfile is the profile used by `entire review` when no
	// profile is supplied. If empty, `general` is used when present, otherwise
	// the single configured profile is used.
	ReviewDefaultProfile string `json:"review_default_profile,omitempty"`

	// Deprecated: legacy pre-profile review settings. Kept so old config files
	// still parse. `entire review` reads this only as a compatibility fallback
	// when no review_profiles are configured, exposing it as the general profile.
	Review map[string]ReviewConfig `json:"review,omitempty"`

	// ReviewFixAgent is a legacy saved fix-agent preference. The `entire review
	// --fix` flow has been removed; this field is retained only so older
	// settings/preferences files still parse. It is no longer read by
	// `entire review`.
	ReviewFixAgent string `json:"review_fix_agent,omitempty"`

	// Investigate holds configuration for `entire investigate`. Empty means
	// `entire investigate` triggers the first-run picker.
	Investigate *InvestigateConfig `json:"investigate,omitempty"`

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

	// Checkpoints selects checkpoint storage backends (a primary plus optional
	// write-only mirrors). checkpoint.Open consumes it via the lenient
	// LoadCheckpointsConfig loader; the field also lives here so the strict
	// settings loader (DisallowUnknownFields) accepts a "checkpoints" key.
	Checkpoints *CheckpointsConfig `json:"checkpoints,omitempty"`

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
	ReviewProfiles       map[string]ReviewProfileConfig `json:"review_profiles,omitempty"`
	ReviewDefaultProfile string                         `json:"review_default_profile,omitempty"`

	// Deprecated: legacy pre-profile review settings. Kept so old preference
	// files parse. New review setup writes ReviewProfiles instead, while
	// `entire review` may read Review as a fallback when profiles are absent.
	Review         map[string]ReviewConfig `json:"review,omitempty"`
	ReviewFixAgent string                  `json:"review_fix_agent,omitempty"`

	// ReviewMigrationDismissed records that the user declined the one-shot
	// migration of review keys from project settings to clone-local prefs.
	// Once true, `entire review` stops prompting on every invocation; the
	// user can re-enable by editing this file or deleting the key.
	ReviewMigrationDismissed bool `json:"review_migration_dismissed,omitempty"`

	// TrailsEnabled caches whether trails are enabled for this repository on the
	// API. Pointer shape distinguishes "unknown/not refreshed yet" (nil) from a
	// definitive false. This is clone-local and not committed so hook-time agent
	// context injection can avoid network/auth work on the prompt path.
	TrailsEnabled *bool `json:"trails_enabled,omitempty"`

	// Freshness and scope for TrailsEnabled.
	TrailsEnabledCheckedAt *time.Time `json:"trails_enabled_checked_at,omitempty"`
	TrailsEnabledRepoKey   string     `json:"trails_enabled_repo_key,omitempty"`
	TrailsEnabledAPIBase   string     `json:"trails_enabled_api_base,omitempty"`
	TrailsEnabledAuthKey   string     `json:"trails_enabled_auth_key,omitempty"`
}

// SummaryGenerationSettings configures provider selection for on-demand
// checkpoint summaries generated by explain --generate.
type SummaryGenerationSettings struct {
	// Provider is the selected summary provider agent name
	// (for example "claude-code", "codex", "gemini", or "pi").
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

	// OpenAIPrivacyFilter is the optional 8th redaction layer (opt-in).
	// See docs/security-and-privacy.md.
	OpenAIPrivacyFilter *OPFSettings `json:"openai_privacy_filter,omitempty"`
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

// OPFSettings configures the optional OpenAI Privacy Filter detection layer.
// Disabled by default. Runs only at condensation/export boundaries — see
// docs/security-and-privacy.md.
//
// There is intentionally no "on_failure" field: warn-only is the only mode
// the runtime currently supports, and DisallowUnknownFields will reject any
// future user who tries to set it. Adding the field again should land in
// lockstep with the runtime enforcement.
type OPFSettings struct {
	Enabled        bool            `json:"enabled,omitempty"`
	Categories     map[string]bool `json:"categories,omitempty"`
	Command        string          `json:"command,omitempty"`
	TimeoutSeconds int             `json:"timeout_seconds,omitempty"`

	// PromptDefault controls whether the pre-push hook asks the user
	// before running OPF. "" (default) and "ask" both surface the
	// interactive prompt; "never" skips OPF and pushes 7-layer content;
	// "always" runs without asking. ENTIRE_OPF=yes|no on the push
	// invocation overrides this setting per-push.
	PromptDefault string `json:"prompt_default,omitempty"`
}

// Valid PromptDefault values. Empty == OPFPromptAsk.
const (
	OPFPromptAsk    = "ask"
	OPFPromptNever  = "never"
	OPFPromptAlways = "always"
)

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

// ReviewProfileConfig is a named review setup. The profile-level Task is the
// canonical task every reviewer agent is asked to run; per-agent ReviewConfig
// entries adapt that task to agent-specific mechanics such as slash commands
// or additional instructions. Judge names the single agent that consolidates
// the reviewers' reports into the final verdict in a closing round.
//
// Example:
//
//	"review_profiles": {
//	  "security": {
//	    "task": "Review this change for auth, injection, secrets, and privilege-boundary bugs.",
//	    "agents": {
//	      "claude-sonnet": {"agent": "claude-code", "model": "sonnet", "skills": ["/security-review"]},
//	      "codex": {"model": "gpt-5-codex", "skills": ["/review"], "prompt": "Focus on security."}
//	    },
//	    "judge": {"agent": "claude-code", "model": "opus"}
//	  }
//	}
//
// ReviewProfileConfig is intentionally small: the review package owns built-in
// default task text for conventional profile names like "general".
type ReviewProfileConfig struct {
	Task   string                  `json:"task,omitempty"`
	Agents map[string]ReviewConfig `json:"agents,omitempty"`
	// Judge is the single agent (plus optional model) that consolidates the
	// reviewers' reports into the final verdict. It is optional: a
	// one-reviewer profile needs no judge (the lone report is the result),
	// and a multi-reviewer profile with no judge set falls back to an
	// auto-selected reviewer that can write a verdict.
	Judge *ReviewConfig `json:"judge,omitempty"`
	// Output selects where the final review verdict is delivered: "local"
	// (printed and saved to the local review manifest — the default) or
	// "trail" (additionally posted to the branch's trail as a finding via
	// the data API). Empty means local.
	Output string `json:"output,omitempty"`
}

// IsZero reports whether the profile is effectively unset.
func (c ReviewProfileConfig) IsZero() bool {
	return c.Task == "" && len(c.Agents) == 0 && (c.Judge == nil || c.Judge.IsZero())
}

// ReviewConfig holds one worker's configuration within a review profile.
// The profile's agents map is keyed by worker id. For simple configs the worker
// id is also the agent registry name (for example "claude-code"). To run the
// same agent more than once with different models, use stable worker ids and set
// Agent to the underlying registry name.
//
// Skills are agent-specific invocations passed before the task. Prompt is
// additional agent-specific instruction appended after the profile task; it is
// no longer a verbatim replacement for the whole review prompt.
type ReviewConfig struct {
	// Agent is the underlying agent registry key for this worker. Empty means
	// the profile map key is the agent name. Set this when the map key is an
	// alias such as "claude-sonnet" or "claude-opus".
	Agent string `json:"agent,omitempty"`

	// Model is an optional model hint passed to the agent CLI for this worker.
	// Empty means use the agent's own default.
	Model string `json:"model,omitempty"`

	// Skills is the list of slash-prefixed skill invocations configured
	// for this agent. May be empty for prompt/model-driven workers (e.g. Pi),
	// in which case the profile task plus Prompt drive the review.
	Skills []string `json:"skills,omitempty"`

	// Prompt, when non-empty, carries saved agent-specific instructions. It is
	// appended after the profile task (and after any Skills); it is not a
	// verbatim replacement for the whole review prompt.
	Prompt string `json:"prompt,omitempty"`
}

// IsZero reports whether the config is effectively unset.
func (c ReviewConfig) IsZero() bool {
	return c.Agent == "" && c.Model == "" && len(c.Skills) == 0 && c.Prompt == ""
}

// InvestigateConfig holds the configuration for `entire investigate`.
// Unlike ReviewConfig, investigate runs the same shared prompt across
// all configured agents, so the schema is a flat agent list with global
// loop knobs rather than per-agent skill lists.
type InvestigateConfig struct {
	// Agents is the ordered list of agent names to round-robin during the loop.
	Agents []string `json:"agents,omitempty"`

	// MaxTurns is the per-agent turn budget. Defaults to 2 when zero
	// (see investigate.defaultMaxTurns).
	MaxTurns int `json:"max_turns,omitempty"`

	// Quorum is the count of `approve` stances needed to terminate the loop.
	// Zero means "all agents must approve" (matches marvin's default).
	Quorum int `json:"quorum,omitempty"`

	// AlwaysPrompt is appended to every turn's composed prompt, parallel
	// to ReviewConfig.Prompt.
	AlwaysPrompt string `json:"always_prompt,omitempty"`
}

// IsZero reports whether the config is effectively unset.
func (c *InvestigateConfig) IsZero() bool {
	if c == nil {
		return true
	}
	return len(c.Agents) == 0 && c.MaxTurns == 0 && c.Quorum == 0 && c.AlwaysPrompt == ""
}

// InvestigateConfig returns the configured investigate config. Returns nil
// when no configuration is present; callers should check IsZero (or guard
// for nil) to decide whether configuration is present.
func (s *EntireSettings) InvestigateConfig() *InvestigateConfig {
	if s == nil {
		return nil
	}
	return s.Investigate
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

	settingsFileAbs, localSettingsFileAbs := settingsAbsPaths(ctx)
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

	return loadMergedSettings(settingsFileAbs, preferencesFileAbs, localSettingsFileAbs)
}

// settingsAbsPaths resolves the base and local settings file paths relative to
// the current working directory, falling back to the relative path when
// absolute resolution fails.
func settingsAbsPaths(ctx context.Context) (base, local string) {
	base, err := paths.AbsPath(ctx, EntireSettingsFile)
	if err != nil {
		base = EntireSettingsFile // Fallback to relative
	}
	local, err = paths.AbsPath(ctx, EntireSettingsLocalFile)
	if err != nil {
		local = EntireSettingsLocalFile // Fallback to relative
	}
	return base, local
}

// worktreeSettingsPaths resolves the base and local settings file paths under
// an explicit worktree root.
func worktreeSettingsPaths(worktreeRoot string) (base, local string) {
	return filepath.Join(worktreeRoot, EntireSettingsFile), filepath.Join(worktreeRoot, EntireSettingsLocalFile)
}

func loadForWorktreeRoot(ctx context.Context, worktreeRoot string) (*EntireSettings, error) {
	settingsFileAbs, localSettingsFileAbs := worktreeSettingsPaths(worktreeRoot)
	preferencesFileAbs := ""
	if path, prefErr := clonePreferencesPathForWorktreeRoot(ctx, worktreeRoot); prefErr == nil {
		preferencesFileAbs = path
	} else {
		logging.Debug(ctx, "clone preferences path unresolved; skipping preferences layer",
			slog.String("error", prefErr.Error()))
	}
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
	localData, err := readConfined(localSettingsFileAbs)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
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
// Pair with SaveProjectRaw for read-modify-write flows that need to preserve
// unrelated keys. Owning the path resolution and raw IO here keeps callers
// from duplicating settings parsing in violation of the "Settings access must
// go through the settings package" rule in CLAUDE.md.
func LoadProjectRaw(ctx context.Context) (path string, raw map[string]json.RawMessage, exists bool, err error) {
	path, err = paths.AbsPath(ctx, EntireSettingsFile)
	if err != nil {
		path = EntireSettingsFile
	}
	data, readErr := readConfined(path)
	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
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
// Pair with SaveProjectRaw for read-modify-write flows that need to preserve
// unrelated keys in the per-developer override file.
func LoadLocalRaw(ctx context.Context) (path string, raw map[string]json.RawMessage, exists bool, err error) {
	path, err = paths.AbsPath(ctx, EntireSettingsLocalFile)
	if err != nil {
		path = EntireSettingsLocalFile
	}
	data, readErr := readConfined(path)
	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
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

// SaveLocalRaw writes a generic JSON object back to .entire/settings.local.json
// atomically (temp file + rename). Mirrors SaveProjectRaw for the per-developer
// overrides file; the only difference is the error wording, which says "local
// settings" so failure messages match the file actually being written.
//
// Pair with LoadLocalRaw for read-modify-write flows that target the local
// override (e.g. persisting an interactive prompt's "always" choice without
// touching the project-wide settings file).
func SaveLocalRaw(path string, raw map[string]json.RawMessage) error {
	data, err := jsonutil.MarshalIndentWithNewline(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal local settings: %w", err)
	}
	if err := jsonutil.WriteFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("writing local settings: %w", err)
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

// ModifyClonePreferences runs a read-modify-write under the preferences lock.
func ModifyClonePreferences(ctx context.Context, fn func(*ClonePreferences) error) error {
	path, err := ClonePreferencesPath(ctx)
	if err != nil {
		return err
	}
	return modifyClonePreferencesFile(path, fn)
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
	if s.Redaction != nil {
		if err := validateOPFSettings(s.Redaction.OpenAIPrivacyFilter); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// readConfined reads filePath through an os.Root anchored at its parent
// directory. The root confines the open to that directory, so the read cannot
// be redirected outside it by a swapped or symlinked path between resolution and
// open (TOCTOU) — unlike a bare os.ReadFile of an absolute path. A symlink that
// escapes the directory surfaces as a non-ENOENT error. Callers must classify
// "missing" with errors.Is(err, fs.ErrNotExist) rather than os.IsNotExist,
// since the returned errors are wrapped.
func readConfined(filePath string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(filePath))
	if err != nil {
		return nil, fmt.Errorf("open settings dir: %w", err)
	}
	defer root.Close()

	f, err := root.Open(filepath.Base(filePath))
	if err != nil {
		return nil, fmt.Errorf("open settings file: %w", err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read settings file: %w", err)
	}
	return data, nil
}

// loadFromFile loads settings from a specific file path.
// Returns default settings if the file doesn't exist.
func loadFromFile(filePath string) (*EntireSettings, error) {
	settings := &EntireSettings{
		Enabled: true, // Default to enabled
	}

	data, err := readConfined(filePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
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

	if settings.Redaction != nil {
		if err := validateOPFSettings(settings.Redaction.OpenAIPrivacyFilter); err != nil {
			return nil, err
		}
	}

	return settings, nil
}

func loadClonePreferencesFromFile(filePath string) (*ClonePreferences, error) {
	prefs := &ClonePreferences{}

	data, err := readConfined(filePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
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

// mergeReviewProfiles overlays src review profiles onto base by name, returning
// a new map. A profile from a higher-precedence layer (src) overrides the
// same-named one from a lower layer (base), but profiles unique to each layer
// are all preserved. This lets a team keep shared profiles in
// .entire/settings.json while individuals add or override profiles in
// clone-local preferences or .entire/settings.local.json, without one layer
// hiding the others' profiles.
//
// Neither input map is mutated: callers (and the maps they own, e.g. a freshly
// loaded ClonePreferences) can rely on their maps being left untouched. The
// result is always a fresh, non-nil map (empty when both inputs are empty), so
// callers never receive nil from a non-nil input.
func mergeReviewProfiles(base, src map[string]ReviewProfileConfig) map[string]ReviewProfileConfig {
	out := make(map[string]ReviewProfileConfig, len(base)+len(src))
	for name, cfg := range base {
		out[name] = cfg
	}
	for name, cfg := range src {
		out[name] = cfg
	}
	return out
}

func modifyClonePreferencesFile(filePath string, fn func(*ClonePreferences) error) error {
	if err := os.MkdirAll(filepath.Dir(filePath), 0o750); err != nil {
		return fmt.Errorf("creating preferences directory: %w", err)
	}
	release, err := flock.Acquire(filePath + ".lock")
	if err != nil {
		return fmt.Errorf("lock preferences file: %w", err)
	}
	defer release()

	prefs, err := loadClonePreferencesFromFile(filePath)
	if err != nil {
		return err
	}
	if err := fn(prefs); err != nil {
		return err
	}
	return saveClonePreferencesToFile(prefs, filePath)
}

func applyClonePreferences(settings *EntireSettings, prefs *ClonePreferences) {
	if prefs == nil {
		return
	}
	if prefs.ReviewProfiles != nil {
		settings.ReviewProfiles = mergeReviewProfiles(settings.ReviewProfiles, prefs.ReviewProfiles)
	}
	if prefs.ReviewDefaultProfile != "" {
		settings.ReviewDefaultProfile = prefs.ReviewDefaultProfile
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
	if profilesRaw, ok := raw["review_profiles"]; ok {
		var profiles map[string]ReviewProfileConfig
		if err := json.Unmarshal(profilesRaw, &profiles); err != nil {
			return fmt.Errorf("parsing review_profiles field: %w", err)
		}
		// Merge per-profile so a local override file adds to / overrides shared
		// profiles by name rather than replacing the whole set.
		settings.ReviewProfiles = mergeReviewProfiles(settings.ReviewProfiles, profiles)
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

	if err := mergeInvestigate(settings, raw); err != nil {
		return err
	}

	return nil
}

// mergeInvestigate replaces the investigate config from the override (whole-object
// replacement, parallel to how summary_generation is handled but simpler — the
// investigate schema is small and lacks per-field merge semantics).
func mergeInvestigate(settings *EntireSettings, raw map[string]json.RawMessage) error {
	investigateRaw, ok := raw["investigate"]
	if !ok {
		return nil
	}
	var cfg InvestigateConfig
	if err := unmarshalField("investigate", investigateRaw, &cfg); err != nil {
		return err
	}
	settings.Investigate = &cfg
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
	if err := mergeRawStringNonEmpty(raw, "review_default_profile", &settings.ReviewDefaultProfile); err != nil {
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
	if opfRaw, ok := raw["openai_privacy_filter"]; ok {
		if dst.OpenAIPrivacyFilter == nil {
			dst.OpenAIPrivacyFilter = &OPFSettings{}
		}
		if err := mergeOPFSettings(dst.OpenAIPrivacyFilter, opfRaw); err != nil {
			return err
		}
	}
	return nil
}

// validateOPFSettings rejects unknown category names so typos surface at
// parse time. Silent zero-detection of a privacy category is effectively
// a correctness bug — the user thinks they're protected but they're not.
func validateOPFSettings(opf *OPFSettings) error {
	if opf == nil {
		return nil
	}
	for name := range opf.Categories {
		if !redact.IsKnownOPFCategory(name) {
			return fmt.Errorf("openai_privacy_filter.categories has unknown key %q (see docs/security-and-privacy.md for the supported set)", name)
		}
	}
	if opf.TimeoutSeconds < 0 {
		return fmt.Errorf("openai_privacy_filter.timeout_seconds must be greater than or equal to 0 (got %d)", opf.TimeoutSeconds)
	}
	switch opf.PromptDefault {
	case "", OPFPromptAsk, OPFPromptNever, OPFPromptAlways:
		// ok
	default:
		return fmt.Errorf("openai_privacy_filter.prompt_default must be one of %q, %q, %q (got %q)",
			OPFPromptAsk, OPFPromptNever, OPFPromptAlways, opf.PromptDefault)
	}
	return nil
}

// mergeOPFSettings merges OPF overrides into existing OPFSettings. Only
// fields present in the override JSON are applied; missing fields preserve
// the base value.
func mergeOPFSettings(dst *OPFSettings, data json.RawMessage) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parsing openai_privacy_filter: %w", err)
	}
	if v, ok := raw["enabled"]; ok {
		if err := json.Unmarshal(v, &dst.Enabled); err != nil {
			return fmt.Errorf("parsing openai_privacy_filter.enabled: %w", err)
		}
	}
	if v, ok := raw["categories"]; ok {
		var cats map[string]bool
		if err := json.Unmarshal(v, &cats); err != nil {
			return fmt.Errorf("parsing openai_privacy_filter.categories: %w", err)
		}
		if dst.Categories == nil {
			dst.Categories = make(map[string]bool, len(cats))
		}
		for k, b := range cats {
			dst.Categories[k] = b
		}
	}
	if v, ok := raw["command"]; ok {
		if err := json.Unmarshal(v, &dst.Command); err != nil {
			return fmt.Errorf("parsing openai_privacy_filter.command: %w", err)
		}
	}
	if v, ok := raw["timeout_seconds"]; ok {
		if err := json.Unmarshal(v, &dst.TimeoutSeconds); err != nil {
			return fmt.Errorf("parsing openai_privacy_filter.timeout_seconds: %w", err)
		}
	}
	if v, ok := raw["prompt_default"]; ok {
		if err := json.Unmarshal(v, &dst.PromptDefault); err != nil {
			return fmt.Errorf("parsing openai_privacy_filter.prompt_default: %w", err)
		}
	}
	return validateOPFSettings(dst)
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
	_, err = os.Lstat(settingsFileAbs)
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

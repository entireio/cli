// Schema-v2 loader and legacy synthesizer.
// See synthesizeFromLegacy for the v1→v2 mapping.

package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Backend type constants used in BackendConfig.Type.
const (
	BackendTypeV1    = "v1"
	BackendTypeV2    = "v2"
	BackendTypeGmeta = "gmeta"
)

// Legacy strategy_options keys read by the synthesizer. The legacy package
// has no constants for these; defining them here keeps the synthesizer's
// references typo-proof without modifying legacy accessors.
const (
	legacyKeyGmeta                       = "gmeta"
	legacyKeyPushSessions                = "push_sessions"
	legacyKeyFullTranscriptRetentionDays = "full_transcript_generation_retention_days"
	legacyKeyCheckpointsV2               = "checkpoints_v2"
	legacyKeyCheckpointsVersion          = "checkpoints_version"
	legacyKeyCheckpointRemote            = "checkpoint_remote"
	legacyKeyFilteredFetches             = "filtered_fetches"
	legacyKeySummarize                   = "summarize"
)

// LoadV2 loads settings as the v2 shape. Files declaring "schema": 2 are
// parsed natively; older files are loaded via the legacy parser and
// synthesized. Mixed shapes (one file v2, one legacy) are supported: the
// legacy file is translated field-by-field on top of the v2 base.
//
// Returns Settings with documented defaults if neither settings file exists.
// Validates the result before returning so semantic errors surface at load time.
func LoadV2(ctx context.Context) (*Settings, error) {
	mainAbs, err := paths.AbsPath(ctx, EntireSettingsFile)
	if err != nil {
		mainAbs = EntireSettingsFile
	}
	localAbs, err := paths.AbsPath(ctx, EntireSettingsLocalFile)
	if err != nil {
		localAbs = EntireSettingsLocalFile
	}

	mainData, mainErr := readSettingsFileIfExists(mainAbs)
	if mainErr != nil {
		return nil, fmt.Errorf("reading settings file: %w", mainErr)
	}
	localData, localErr := readSettingsFileIfExists(localAbs)
	if localErr != nil {
		return nil, fmt.Errorf("reading local settings file: %w", localErr)
	}

	settings, err := buildSettings(mainData, localData, mainAbs, localAbs)
	if err != nil {
		return nil, err
	}
	if err := settings.Validate(); err != nil {
		return nil, fmt.Errorf("settings invalid: %w", err)
	}
	return settings, nil
}

// LoadV2FromBytes parses a single settings document from raw bytes.
// Accepts either schema-v2 JSON or legacy JSON; the shape is auto-detected
// via the schema field. No local-override merge is performed.
func LoadV2FromBytes(data []byte) (*Settings, error) {
	if len(data) == 0 {
		return defaultSettings(), nil
	}
	var (
		settings *Settings
		err      error
	)
	if isSchemaV2(data) {
		settings, err = parseV2Bytes(data)
	} else {
		var legacy *EntireSettings
		legacy, err = LoadFromBytes(data)
		if err == nil {
			settings = synthesizeFromLegacy(legacy)
		}
	}
	if err != nil {
		return nil, err
	}
	if err := settings.Validate(); err != nil {
		return nil, fmt.Errorf("settings invalid: %w", err)
	}
	return settings, nil
}

// buildSettings constructs the merged Settings from raw main and local
// bytes. Each file may independently be v2 or legacy; this is what makes
// incremental migration of the gitignored local override safe.
//
// When both files are legacy, we delegate to the legacy loadMergedSettings
// so the legacy granular-merge behavior (PII fields, summary_generation
// provider/model interaction) is preserved verbatim before synthesis.
func buildSettings(mainData, localData []byte, mainAbs, localAbs string) (*Settings, error) {
	if mainData == nil && localData == nil {
		return defaultSettings(), nil
	}
	if !isSchemaV2(mainData) && !isSchemaV2(localData) {
		legacy, err := loadMergedSettings(mainAbs, localAbs)
		if err != nil {
			return nil, err
		}
		return synthesizeFromLegacy(legacy), nil
	}

	// At least one file is v2. Establish the base from the main file (v2
	// or synthesized legacy), then apply the local file as the right kind
	// of override.
	base, err := buildBase(mainData)
	if err != nil {
		return nil, err
	}
	if localData != nil {
		if err := applyOverride(base, localData); err != nil {
			return nil, err
		}
	}
	return base, nil
}

// buildBase parses the main file into a Settings, choosing between the v2
// parser and the legacy synthesizer based on shape.
func buildBase(data []byte) (*Settings, error) {
	if data == nil {
		return defaultSettings(), nil
	}
	if isSchemaV2(data) {
		settings, err := parseV2Bytes(data)
		if err != nil {
			return nil, fmt.Errorf("parsing settings file: %w", err)
		}
		return settings, nil
	}
	legacy, err := LoadFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parsing settings file: %w", err)
	}
	return synthesizeFromLegacy(legacy), nil
}

// applyOverride merges an override file (v2 or legacy) onto an existing v2
// base. Routes to the appropriate per-shape applier.
func applyOverride(base *Settings, data []byte) error {
	if isSchemaV2(data) {
		return mergeV2Override(base, data)
	}
	return applyLegacyOverride(base, data)
}

// readSettingsFileIfExists returns the file contents, or (nil, nil) if the
// file does not exist. Other I/O errors propagate.
func readSettingsFileIfExists(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is from AbsPath or constant
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err //nolint:wrapcheck // caller wraps with file context
	}
	return data, nil
}

// isSchemaV2 reports whether a settings JSON document declares schema >= 2.
// Returns false for nil/empty input or invalid JSON; callers handle those
// cases explicitly downstream.
func isSchemaV2(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	var probe struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	return probe.Schema >= CurrentSchemaVersion
}

// parseV2Bytes parses a single schema-v2 JSON document into a fresh Settings
// (with defaults). Strict decoding rejects unknown fields so typos become
// loud failures.
func parseV2Bytes(data []byte) (*Settings, error) {
	settings := defaultSettings()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(settings); err != nil {
		return nil, fmt.Errorf("parsing settings: %w", err)
	}
	return settings, nil
}

// mergeV2Override merges a schema-v2 override on top of an existing
// Settings. Decoding into the live struct in place gives us field-level
// granular merge for free: Go's json package leaves struct fields not
// mentioned in the JSON unchanged, including nested fields. So an override
// of {"checkpoints": {"mirrors": [...]}} preserves base.Checkpoints.Primary
// and the rest of base.Checkpoints, only replacing Mirrors.
//
// Caveats: pointer fields and slices are decoded by value-replacement, not
// granular merge. If a user wants to change one field of base.Redaction
// they must spell out enough of the override to reconstruct the desired
// shape. This matches what JSON decoding naturally does and is consistent
// with how config tooling generally behaves.
func mergeV2Override(base *Settings, data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(base); err != nil {
		return fmt.Errorf("merging local settings: %w", err)
	}
	return nil
}

// applyLegacyOverride merges a legacy-shape override on top of an existing
// v2 Settings. Translates each legacy key present in the override to its
// v2 destination. Fields not present in the override are left untouched —
// this is what makes "v2 main + legacy local" usable without forcing the
// user to migrate the local file before the main one.
func applyLegacyOverride(base *Settings, data []byte) error {
	legacy, err := LoadFromBytes(data)
	if err != nil {
		return fmt.Errorf("parsing local settings: %w", err)
	}

	var present map[string]json.RawMessage
	if err := json.Unmarshal(data, &present); err != nil {
		return fmt.Errorf("parsing local settings: %w", err)
	}

	overlay := synthesizeFromLegacy(legacy)
	if err := applyTopLevelLegacyKeys(base, legacy, overlay, present); err != nil {
		return err
	}

	soRaw, ok := present["strategy_options"]
	if !ok {
		return nil
	}
	var so map[string]json.RawMessage
	if err := json.Unmarshal(soRaw, &so); err != nil {
		return fmt.Errorf("parsing local strategy_options: %w", err)
	}
	applyStrategyOptionLegacyKeys(base, overlay, so)
	return nil
}

// applyTopLevelLegacyKeys copies overlay → base for top-level legacy keys
// present in the override. Each branch maps one legacy key to its v2
// destination. Pointer-typed nested fields (Redaction, SummaryGeneration)
// merge granularly so an override that mentions only sub-fields preserves
// the rest of the v2 base — wholesale replacement would silently erase
// fields the user did not intend to change.
func applyTopLevelLegacyKeys(base *Settings, legacy *EntireSettings, overlay *Settings, present map[string]json.RawMessage) error {
	if _, ok := present["enabled"]; ok {
		base.Enabled = overlay.Enabled
	}
	if _, ok := present["local_dev"]; ok {
		base.LocalDev = overlay.LocalDev
	}
	if _, ok := present["log_level"]; ok {
		base.Logging.Level = overlay.Logging.Level
	}
	if _, ok := present["commit_linking"]; ok {
		base.Hooks.CommitLinking = overlay.Hooks.CommitLinking
	}
	if _, ok := present["absolute_git_hook_path"]; ok {
		base.Hooks.AbsoluteGitHookPath = overlay.Hooks.AbsoluteGitHookPath
	}
	if _, ok := present["external_agents"]; ok {
		base.Features.ExternalAgents = overlay.Features.ExternalAgents
	}
	if _, ok := present["vercel"]; ok {
		base.Features.Vercel = overlay.Features.Vercel
	}
	if _, ok := present["telemetry"]; ok {
		base.Telemetry = overlay.Telemetry
	}
	if _, ok := present["sign_checkpoint_commits"]; ok {
		base.Checkpoints.SignCommits = overlay.Checkpoints.SignCommits
	}
	if err := mergeLegacyRedactionOverride(base, present); err != nil {
		return err
	}
	return mergeLegacySummaryGenerationOverride(base, legacy, present)
}

// mergeLegacyRedactionOverride applies a legacy redaction override to the
// v2 base granularly. Reuses the existing legacy mergeRedaction helper so
// the per-PII-field merge logic stays in a single source of truth.
func mergeLegacyRedactionOverride(base *Settings, present map[string]json.RawMessage) error {
	redactRaw, ok := present["redaction"]
	if !ok {
		return nil
	}
	if base.Redaction == nil {
		base.Redaction = &RedactionSettings{}
	}
	if err := mergeRedaction(base.Redaction, redactRaw); err != nil {
		return fmt.Errorf("merging redaction override: %w", err)
	}
	return nil
}

// mergeLegacySummaryGenerationOverride applies a legacy summary-generation
// override to the v2 base granularly. The legacy schema splits the timeout
// into a top-level field and provider/model into a nested object; both
// translate into the unified v2 SummaryGenerationConfig.
//
// Per-field presence preserves base values that the override does not
// mention. For example, an override of just summary_timeout_seconds keeps
// the v2 base's Provider and Model intact.
func mergeLegacySummaryGenerationOverride(base *Settings, legacy *EntireSettings, present map[string]json.RawMessage) error {
	sgRaw, hasSg := present["summary_generation"]
	_, hasTimeout := present["summary_timeout_seconds"]
	if !hasSg && !hasTimeout {
		return nil
	}
	if base.SummaryGeneration == nil {
		base.SummaryGeneration = &SummaryGenerationConfig{}
	}
	if hasTimeout {
		base.SummaryGeneration.TimeoutSeconds = legacy.SummaryTimeoutSeconds
	}
	if !hasSg {
		return nil
	}
	var sg map[string]json.RawMessage
	if err := json.Unmarshal(sgRaw, &sg); err != nil {
		return fmt.Errorf("merging summary_generation override: %w", err)
	}
	if legacy.SummaryGeneration == nil {
		return nil
	}
	if _, ok := sg["provider"]; ok {
		base.SummaryGeneration.Provider = legacy.SummaryGeneration.Provider
	}
	if _, ok := sg["model"]; ok {
		base.SummaryGeneration.Model = legacy.SummaryGeneration.Model
	}
	return nil
}

// applyStrategyOptionLegacyKeys copies overlay → base for strategy_options
// keys present in the override. Each legacy strategy_options key maps to a
// specific v2 destination on base.Checkpoints or base.Features.
func applyStrategyOptionLegacyKeys(base, overlay *Settings, so map[string]json.RawMessage) {
	if _, ok := so[legacyKeyCheckpointsV2]; ok {
		base.Checkpoints.Primary = overlay.Checkpoints.Primary
	}
	if _, ok := so[legacyKeyCheckpointsVersion]; ok {
		base.Checkpoints.Primary = overlay.Checkpoints.Primary
	}
	if _, ok := so[legacyKeyGmeta]; ok {
		base.Checkpoints.Mirrors = overlay.Checkpoints.Mirrors
	}
	if _, ok := so[legacyKeyCheckpointRemote]; ok {
		base.Checkpoints.Git = overlay.Checkpoints.Git
	}
	if _, ok := so[legacyKeyFullTranscriptRetentionDays]; ok {
		base.Checkpoints.FullTranscriptRetentionDays = overlay.Checkpoints.FullTranscriptRetentionDays
	}
	if _, ok := so[legacyKeyFilteredFetches]; ok {
		base.Checkpoints.FilteredFetches = overlay.Checkpoints.FilteredFetches
	}
	if _, ok := so[legacyKeyPushSessions]; ok {
		base.Checkpoints.PushSessions = overlay.Checkpoints.PushSessions
	}
	if _, ok := so[legacyKeySummarize]; ok {
		base.Features.Summarize = overlay.Features.Summarize
	}
}

// defaultSettings returns the zero-value Settings with documented defaults
// applied (Schema = 2, Enabled = true, Primary = v1). v1 is the default
// primary for parity with the legacy "no version specified means v1"
// behavior; users on a fresh repo see V2 only after `entire enable` writes
// it explicitly.
func defaultSettings() *Settings {
	return &Settings{
		Schema:  CurrentSchemaVersion,
		Enabled: true,
		Checkpoints: CheckpointsConfig{
			Primary: BackendConfig{Type: BackendTypeV1},
		},
	}
}

// synthesizeFromLegacy maps an EntireSettings into a Settings, encoding the
// legacy → v2 migration. This is the single source of truth for the
// translation and is reused by `entire migrate-config`.
//
// Dropped (no v2 destination):
//   - push_v2_refs: was a dual-write transition knob; v2 ref pushes will
//     be a property of the v2 backend's CheckpointSyncer, not a settings flag.
//   - strategy: deprecated tolerator field.
//
// See the function body for field-by-field mapping.
func synthesizeFromLegacy(s *EntireSettings) *Settings {
	if s == nil {
		return defaultSettings()
	}

	out := &Settings{
		Schema:   CurrentSchemaVersion,
		Enabled:  s.Enabled,
		LocalDev: s.LocalDev,
		Logging: LoggingConfig{
			Level: s.LogLevel,
		},
		Hooks: HooksConfig{
			CommitLinking:       s.CommitLinking,
			AbsoluteGitHookPath: s.AbsoluteGitHookPath,
		},
		Features: FeaturesConfig{
			Summarize:      s.IsSummarizeEnabled(),
			ExternalAgents: s.ExternalAgents,
			Vercel:         s.Vercel,
		},
		Redaction: s.Redaction,
		Telemetry: s.Telemetry,
	}

	out.Checkpoints = synthesizeCheckpointsConfig(s)

	if hasSummaryGeneration(s) {
		out.SummaryGeneration = &SummaryGenerationConfig{
			TimeoutSeconds: s.SummaryTimeoutSeconds,
		}
		if s.SummaryGeneration != nil {
			out.SummaryGeneration.Provider = s.SummaryGeneration.Provider
			out.SummaryGeneration.Model = s.SummaryGeneration.Model
		}
	}

	return out
}

// synthesizeCheckpointsConfig builds the v2 Checkpoints group from the
// legacy EntireSettings. Split out for readability since the mapping pulls
// from both top-level fields and the strategy_options soup.
func synthesizeCheckpointsConfig(s *EntireSettings) CheckpointsConfig {
	cfg := CheckpointsConfig{
		Primary:                     BackendConfig{Type: legacyPrimaryBackend(s)},
		Git:                         s.GetCheckpointRemote(),
		FullTranscriptRetentionDays: legacyFullTranscriptRetention(s),
		SignCommits:                 s.SignCheckpointCommits,
		FilteredFetches:             s.IsFilteredFetchesEnabled(),
		PushSessions:                legacyPushSessions(s),
	}

	if legacyGmetaEnabled(s) {
		cfg.Mirrors = append(cfg.Mirrors, BackendConfig{Type: BackendTypeGmeta})
	}

	return cfg
}

// legacyPrimaryBackend returns "v2" if either checkpoints_version or
// checkpoints_v2 enables v2 in the legacy settings, "v1" otherwise.
func legacyPrimaryBackend(s *EntireSettings) string {
	if s.IsCheckpointsV2Enabled() {
		return BackendTypeV2
	}
	return BackendTypeV1
}

// legacyGmetaEnabled checks the strategy_options.gmeta flag. The flag does
// not exist on this branch (it ships with the gmeta mainport), but the
// synthesizer recognizes it ahead of time so settings files written by
// that branch round-trip cleanly.
func legacyGmetaEnabled(s *EntireSettings) bool {
	if s.StrategyOptions == nil {
		return false
	}
	val, ok := s.StrategyOptions[legacyKeyGmeta].(bool)
	return ok && val
}

// legacyFullTranscriptRetention returns the configured retention only when
// the legacy key is explicitly set; the legacy accessor would return a
// default for unset values, which we suppress here so the v2 file stays
// minimal.
func legacyFullTranscriptRetention(s *EntireSettings) int {
	if s.StrategyOptions == nil {
		return 0
	}
	if _, ok := s.StrategyOptions[legacyKeyFullTranscriptRetentionDays]; !ok {
		return 0
	}
	return s.GetFullTranscriptGenerationRetentionDays()
}

// legacyPushSessions extracts the explicit push_sessions boolean if set;
// otherwise returns nil so the v2 file stays minimal. Tri-state preserves
// the legacy "explicit false disables, otherwise default" semantics.
func legacyPushSessions(s *EntireSettings) *bool {
	if s.StrategyOptions == nil {
		return nil
	}
	val, ok := s.StrategyOptions[legacyKeyPushSessions]
	if !ok {
		return nil
	}
	b, ok := val.(bool)
	if !ok {
		return nil
	}
	return &b
}

// hasSummaryGeneration reports whether any summary-generation field on the
// legacy settings is non-default. Used to decide whether to allocate the
// v2 SummaryGenerationConfig at all (vs. leaving it nil).
func hasSummaryGeneration(s *EntireSettings) bool {
	if s.SummaryGeneration != nil && (s.SummaryGeneration.Provider != "" || s.SummaryGeneration.Model != "") {
		return true
	}
	return s.SummaryTimeoutSeconds > 0
}

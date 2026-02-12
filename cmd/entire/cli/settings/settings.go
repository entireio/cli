// Package settings provides configuration loading for Entire.
// This package is separate from cli to allow strategy package to import it
// without creating an import cycle (cli imports strategy).
package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
)

// DefaultStrategyName is the default strategy when none is configured.
// This is duplicated here to avoid importing the strategy package (which would create a cycle).
const DefaultStrategyName = "manual-commit"

const (
	// EntireSettingsFile is the path to the Entire settings file
	EntireSettingsFile = ".entire/settings.json"
	// EntireSettingsLocalFile is the path to the local settings override file (not committed)
	EntireSettingsLocalFile = ".entire/settings.local.json"
)

// EntireSettings represents the .entire/settings.json configuration
type EntireSettings struct {
	// Strategy is the name of the git strategy to use
	Strategy string `json:"strategy"`

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

	// Telemetry controls anonymous usage analytics.
	// nil = not asked yet (show prompt), true = opted in, false = opted out
	Telemetry *bool `json:"telemetry,omitempty"`
}

// Load loads the Entire settings from .entire/settings.json,
// then applies any overrides from .entire/settings.local.json if it exists.
// Returns default settings if neither file exists.
// Works correctly from any subdirectory within the repository.
func Load() (*EntireSettings, error) {
	// Get absolute paths for settings files
	settingsFileAbs, err := paths.AbsPath(EntireSettingsFile)
	if err != nil {
		settingsFileAbs = EntireSettingsFile // Fallback to relative
	}
	localSettingsFileAbs, err := paths.AbsPath(EntireSettingsLocalFile)
	if err != nil {
		localSettingsFileAbs = EntireSettingsLocalFile // Fallback to relative
	}

	// Load base settings
	settings, err := loadFromFile(settingsFileAbs)
	if err != nil {
		return nil, fmt.Errorf("reading settings file: %w", err)
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

	applyDefaults(settings)

	return settings, nil
}

// LoadFromFile loads settings from a specific file path without merging local overrides.
// Returns default settings if the file doesn't exist.
// Use this when you need to display individual settings files separately.
func LoadFromFile(filePath string) (*EntireSettings, error) {
	return loadFromFile(filePath)
}

// loadFromFile loads settings from a specific file path.
// Returns default settings if the file doesn't exist.
func loadFromFile(filePath string) (*EntireSettings, error) {
	settings := &EntireSettings{
		Strategy: DefaultStrategyName,
		Enabled:  true, // Default to enabled
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
	applyDefaults(settings)

	return settings, nil
}

// mergeJSON merges JSON data into existing settings.
// Only non-zero values from the JSON override existing settings.
func mergeJSON(settings *EntireSettings, data []byte) error {
	// First, validate that there are no unknown keys using strict decoding
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var temp EntireSettings
	if err := dec.Decode(&temp); err != nil {
		return fmt.Errorf("parsing JSON: %w", err)
	}

	// Parse into a map to check which fields are present
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parsing JSON: %w", err)
	}

	// Override strategy if present and non-empty
	if strategyRaw, ok := raw["strategy"]; ok {
		var s string
		if err := json.Unmarshal(strategyRaw, &s); err != nil {
			return fmt.Errorf("parsing strategy field: %w", err)
		}
		if s != "" {
			settings.Strategy = s
		}
	}

	// Override enabled if present
	if enabledRaw, ok := raw["enabled"]; ok {
		var e bool
		if err := json.Unmarshal(enabledRaw, &e); err != nil {
			return fmt.Errorf("parsing enabled field: %w", err)
		}
		settings.Enabled = e
	}

	// Override local_dev if present
	if localDevRaw, ok := raw["local_dev"]; ok {
		var ld bool
		if err := json.Unmarshal(localDevRaw, &ld); err != nil {
			return fmt.Errorf("parsing local_dev field: %w", err)
		}
		settings.LocalDev = ld
	}

	// Override log_level if present and non-empty
	if logLevelRaw, ok := raw["log_level"]; ok {
		var ll string
		if err := json.Unmarshal(logLevelRaw, &ll); err != nil {
			return fmt.Errorf("parsing log_level field: %w", err)
		}
		if ll != "" {
			settings.LogLevel = ll
		}
	}

	// Merge strategy_options if present
	if optionsRaw, ok := raw["strategy_options"]; ok {
		var opts map[string]any
		if err := json.Unmarshal(optionsRaw, &opts); err != nil {
			return fmt.Errorf("parsing strategy_options field: %w", err)
		}
		if settings.StrategyOptions == nil {
			settings.StrategyOptions = opts
		} else {
			for k, v := range opts {
				settings.StrategyOptions[k] = v
			}
		}
	}

	// Override telemetry if present
	if telemetryRaw, ok := raw["telemetry"]; ok {
		var t bool
		if err := json.Unmarshal(telemetryRaw, &t); err != nil {
			return fmt.Errorf("parsing telemetry field: %w", err)
		}
		settings.Telemetry = &t
	}

	return nil
}

func applyDefaults(settings *EntireSettings) {
	if settings.Strategy == "" {
		settings.Strategy = DefaultStrategyName
	}
}

// IsSummarizeEnabled checks if auto-summarize is enabled in settings.
// Returns false by default if settings cannot be loaded or the key is missing.
func IsSummarizeEnabled() bool {
	settings, err := Load()
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

// GetShowcaseConfig extracts showcase settings from StrategyOptions.
// Returns nil if showcase settings are not configured.
func (s *EntireSettings) GetShowcaseConfig() *redact.ShowcaseConfig {
	if s.StrategyOptions == nil {
		return nil
	}
	showcaseOpts, ok := s.StrategyOptions["showcase"].(map[string]any)
	if !ok {
		return nil
	}

	// Parse showcase options into ShowcaseConfig struct
	cfg := redact.DefaultShowcaseConfig()

	if redactPaths, ok := showcaseOpts["redact_paths"].(bool); ok {
		cfg.RedactPaths = redactPaths
	}
	if redactUsernames, ok := showcaseOpts["redact_usernames"].(bool); ok {
		cfg.RedactUsernames = redactUsernames
	}
	if redactProjectInfo, ok := showcaseOpts["redact_project_info"].(bool); ok {
		cfg.RedactProjectInfo = redactProjectInfo
	}

	// Parse allowed_paths array
	if allowedPaths, ok := showcaseOpts["allowed_paths"].([]any); ok {
		cfg.AllowedPaths = make([]string, 0, len(allowedPaths))
		for _, v := range allowedPaths {
			if s, ok := v.(string); ok {
				cfg.AllowedPaths = append(cfg.AllowedPaths, s)
			}
		}
	}

	// Parse allowed_domains array
	if allowedDomains, ok := showcaseOpts["allowed_domains"].([]any); ok {
		cfg.AllowedDomains = make([]string, 0, len(allowedDomains))
		for _, v := range allowedDomains {
			if s, ok := v.(string); ok {
				cfg.AllowedDomains = append(cfg.AllowedDomains, s)
			}
		}
	}

	// Parse custom_blocklist array
	if customBlocklist, ok := showcaseOpts["custom_blocklist"].([]any); ok {
		cfg.CustomBlocklist = make([]string, 0, len(customBlocklist))
		for _, v := range customBlocklist {
			if s, ok := v.(string); ok {
				cfg.CustomBlocklist = append(cfg.CustomBlocklist, s)
			}
		}
	}

	return &cfg
}

// IsShowcaseEnabled checks if showcase export is enabled in settings.
// Returns true if the showcase configuration exists (regardless of other settings).
func (s *EntireSettings) IsShowcaseEnabled() bool {
	return s.GetShowcaseConfig() != nil
}

// Save saves the settings to .entire/settings.json.
func Save(settings *EntireSettings) error {
	return saveToFile(settings, EntireSettingsFile)
}

// SaveLocal saves the settings to .entire/settings.local.json.
func SaveLocal(settings *EntireSettings) error {
	return saveToFile(settings, EntireSettingsLocalFile)
}

// saveToFile saves settings to the specified file path.
func saveToFile(settings *EntireSettings, filePath string) error {
	// Get absolute path for the file
	filePathAbs, err := paths.AbsPath(filePath)
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

	//nolint:gosec // G306: settings file is config, not secrets; 0o644 is appropriate
	if err := os.WriteFile(filePathAbs, data, 0o644); err != nil {
		return fmt.Errorf("writing settings file: %w", err)
	}
	return nil
}

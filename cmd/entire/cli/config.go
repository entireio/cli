package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/jsonutil"
	"entire.io/cli/cmd/entire/cli/logging"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"

	// Import claudecode to register the agent
	_ "entire.io/cli/cmd/entire/cli/agent/claudecode"
)

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
	StrategyOptions map[string]interface{} `json:"strategy_options,omitempty"`

	// Telemetry controls anonymous usage analytics.
	// nil = not asked yet (show prompt), true = opted in, false = opted out
	Telemetry *bool `json:"telemetry,omitempty"`
}

// LoadEntireSettings loads the Entire settings from .entire/settings.json,
// then applies any overrides from .entire/settings.local.json if it exists.
// Returns default settings if neither file exists.
// Works correctly from any subdirectory within the repository.
func LoadEntireSettings() (*EntireSettings, error) {
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
	settings, err := loadSettingsFromFile(settingsFileAbs)
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
		if err := mergeSettingsJSON(settings, localData); err != nil {
			return nil, fmt.Errorf("merging local settings: %w", err)
		}
	}

	applyDefaultStrategy(settings)

	return settings, nil
}

// mergeSettingsJSON merges JSON data into existing settings.
// Only non-zero values from the JSON override existing settings.
func mergeSettingsJSON(settings *EntireSettings, data []byte) error {
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
		var opts map[string]interface{}
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

// SaveEntireSettings saves the Entire settings to .entire/settings.json.
func SaveEntireSettings(settings *EntireSettings) error {
	return saveSettingsToFile(settings, EntireSettingsFile)
}

// SaveEntireSettingsLocal saves the Entire settings to .entire/settings.local.json.
func SaveEntireSettingsLocal(settings *EntireSettings) error {
	return saveSettingsToFile(settings, EntireSettingsLocalFile)
}

// loadSettingsFromFile loads settings from a specific file path.
// Returns default settings if the file doesn't exist.
func loadSettingsFromFile(filePath string) (*EntireSettings, error) {
	settings := &EntireSettings{
		Strategy: strategy.DefaultStrategyName,
		Enabled:  true, // Default to enabled
	}

	data, err := os.ReadFile(filePath) //nolint:gosec // path is from caller
	if err != nil {
		if os.IsNotExist(err) {
			return settings, nil
		}
		return nil, fmt.Errorf("%w", err)
	}

	if err := json.Unmarshal(data, settings); err != nil {
		return nil, fmt.Errorf("parsing settings file: %w", err)
	}
	applyDefaultStrategy(settings)

	return settings, nil
}

func applyDefaultStrategy(settings *EntireSettings) {
	// Apply defaults if not set
	if settings.Strategy == "" {
		settings.Strategy = strategy.DefaultStrategyName
	}
}

func saveSettingsToFile(settings *EntireSettings, filePath string) error {
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

// IsEnabled returns whether Entire is currently enabled.
// Returns true by default if settings cannot be loaded.
func IsEnabled() (bool, error) {
	settings, err := LoadEntireSettings()
	if err != nil {
		return true, err
	}
	return settings.Enabled, nil
}

// GetStrategy returns the configured strategy instance.
// Falls back to default if the configured strategy is not found.
//
//nolint:ireturn // Factory pattern requires returning the interface
func GetStrategy() strategy.Strategy {
	settings, err := LoadEntireSettings()
	if err != nil {
		// Fall back to default on error
		logging.Info(context.Background(), "falling back to default strategy - failed to load settings",
			slog.String("error", err.Error()))
		return strategy.Default()
	}

	s, err := strategy.Get(settings.Strategy)
	if err != nil {
		// Fall back to default if strategy not found
		logging.Info(context.Background(), "falling back to default strategy - configured strategy not found",
			slog.String("configured", settings.Strategy),
			slog.String("error", err.Error()))
		return strategy.Default()
	}

	return s
}

// GetLogLevel returns the configured log level from settings.
// Returns empty string if not configured (caller should use default).
// Note: ENTIRE_LOG_LEVEL env var takes precedence; check it first.
func GetLogLevel() string {
	settings, err := LoadEntireSettings()
	if err != nil {
		return ""
	}
	return settings.LogLevel
}

// IsMultiSessionWarningEnabled checks if multi-session warnings are enabled.
// Returns false (no warnings) by default - warnings are opt-in since shadow branch
// suffixes make concurrent sessions safe.
func IsMultiSessionWarningEnabled() bool {
	settings, err := LoadEntireSettings()
	if err != nil {
		return false // Default: no warnings
	}
	if settings.StrategyOptions == nil {
		return false
	}
	if enabled, ok := settings.StrategyOptions["enable_multisession_warning"].(bool); ok {
		return enabled
	}
	return false
}

// GetAgentsWithHooksInstalled returns names of agents that have hooks installed.
func GetAgentsWithHooksInstalled() []agent.AgentName {
	var installed []agent.AgentName
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		if hs, ok := ag.(agent.HookSupport); ok && hs.AreHooksInstalled() {
			installed = append(installed, name)
		}
	}
	return installed
}

// JoinAgentNames joins agent names into a comma-separated string.
func JoinAgentNames(names []agent.AgentName) string {
	strs := make([]string, len(names))
	for i, n := range names {
		strs[i] = string(n)
	}
	return strings.Join(strs, ",")
}

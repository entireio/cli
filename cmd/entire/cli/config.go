package cli

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	// Import agent packages to register them
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/codexcli"
)

// Package-level aliases to avoid shadowing the settings package with local variables named "settings".
const (
	EntireSettingsFile      = settings.EntireSettingsFile
	EntireSettingsLocalFile = settings.EntireSettingsLocalFile
)

// EntireSettings is an alias for settings.EntireSettings.
type EntireSettings = settings.EntireSettings

// LoadEntireSettings loads the Entire settings from .entire/settings.json,
// then applies any overrides from .entire/settings.local.json if it exists.
// Returns default settings if neither file exists.
// Works correctly from any subdirectory within the repository.
func LoadEntireSettings() (*settings.EntireSettings, error) {
	s, err := settings.Load()
	if err != nil {
		return nil, fmt.Errorf("loading settings: %w", err)
	}
	return s, nil
}

// SaveEntireSettings saves the Entire settings to .entire/settings.json.
func SaveEntireSettings(s *settings.EntireSettings) error {
	if err := settings.Save(s); err != nil {
		return fmt.Errorf("saving settings: %w", err)
	}
	return nil
}

// SaveEntireSettingsLocal saves the Entire settings to .entire/settings.local.json.
func SaveEntireSettingsLocal(s *settings.EntireSettings) error {
	if err := settings.SaveLocal(s); err != nil {
		return fmt.Errorf("saving local settings: %w", err)
	}
	return nil
}

// IsEnabled returns whether Entire is currently enabled.
// Returns true by default if settings cannot be loaded.
func IsEnabled() (bool, error) {
	s, err := settings.Load()
	if err != nil {
		return true, err //nolint:wrapcheck // already present in codebase
	}
	return s.Enabled, nil
}

// GetStrategy returns the configured strategy instance.
// Falls back to default if the configured strategy is not found.
//
//nolint:ireturn // Factory pattern requires returning the interface
func GetStrategy() strategy.Strategy {
	s, err := settings.Load()
	if err != nil {
		// Fall back to default on error
		logging.Info(context.Background(), "falling back to default strategy - failed to load settings",
			slog.String("error", err.Error()))
		return strategy.Default()
	}

	strat, err := strategy.Get(s.Strategy)
	if err != nil {
		// Fall back to default if strategy not found
		logging.Info(context.Background(), "falling back to default strategy - configured strategy not found",
			slog.String("configured", s.Strategy),
			slog.String("error", err.Error()))
		return strategy.Default()
	}

	return strat
}

// GetLogLevel returns the configured log level from settings.
// Returns empty string if not configured (caller should use default).
// Note: ENTIRE_LOG_LEVEL env var takes precedence; check it first.
func GetLogLevel() string {
	s, err := settings.Load()
	if err != nil {
		return ""
	}
	return s.LogLevel
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

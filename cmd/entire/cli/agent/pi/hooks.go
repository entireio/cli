package pi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure PiAgent implements HookSupport and HookHandler
var (
	_ agent.HookSupport = (*PiAgent)(nil)
	_ agent.HookHandler = (*PiAgent)(nil)
)

// Pi hook names - these become subcommands under `entire hooks pi`
const (
	HookNameSessionStart     = "session-start"
	HookNameSessionEnd       = "session-end"
	HookNameStop             = "stop"
	HookNameUserPromptSubmit = "user-prompt-submit"
)

// PiSettingsFileName is the settings file used by pi.
const PiSettingsFileName = "settings.json"

// entireExtensionPackage is the npm package for the pi-entire extension.
const entireExtensionPackage = "npm:@hjanuschka/pi-entire"

// GetHookNames returns the hook verbs Pi supports.
// These become subcommands: entire hooks pi <verb>
func (p *PiAgent) GetHookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameStop,
		HookNameUserPromptSubmit,
	}
}

// InstallHooks installs pi-entire extension in .pi/settings.json.
// Pi uses extensions rather than external hooks, so we add the extension package.
// Returns the number of changes made.
func (p *PiAgent) InstallHooks(localDev bool, force bool) (int, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot, err = os.Getwd()
		if err != nil {
			return 0, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	settingsPath := filepath.Join(repoRoot, ".pi", PiSettingsFileName)

	// Read existing settings if they exist
	// Use map[string]json.RawMessage to preserve unknown fields
	var rawSettings map[string]json.RawMessage
	var packages []string

	existingData, readErr := os.ReadFile(settingsPath) //nolint:gosec
	if readErr == nil {
		if err := json.Unmarshal(existingData, &rawSettings); err != nil {
			return 0, fmt.Errorf("failed to parse existing settings.json: %w", err)
		}
		// Extract packages array if it exists
		if packagesRaw, ok := rawSettings["packages"]; ok {
			if err := json.Unmarshal(packagesRaw, &packages); err != nil {
				return 0, fmt.Errorf("failed to parse packages in settings.json: %w", err)
			}
		}
	} else {
		rawSettings = make(map[string]json.RawMessage)
	}

	// Check if extension is already installed
	for _, pkg := range packages {
		if pkg == entireExtensionPackage {
			return 0, nil // Already installed
		}
	}

	// Add the extension package
	packages = append(packages, entireExtensionPackage)

	// Update the packages field in rawSettings
	packagesJSON, err := json.Marshal(packages)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal packages: %w", err)
	}
	rawSettings["packages"] = packagesJSON

	// Create directory if needed
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create .pi directory: %w", err)
	}

	// Write settings
	output, err := json.MarshalIndent(rawSettings, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, output, 0o644); err != nil {
		return 0, fmt.Errorf("failed to write settings.json: %w", err)
	}

	return 1, nil
}

// UninstallHooks removes pi-entire extension from settings.
func (p *PiAgent) UninstallHooks() error {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}

	settingsPath := filepath.Join(repoRoot, ".pi", PiSettingsFileName)
	data, err := os.ReadFile(settingsPath) //nolint:gosec
	if err != nil {
		return nil // No settings file means nothing to uninstall
	}

	// Use map[string]json.RawMessage to preserve unknown fields
	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return fmt.Errorf("failed to parse settings.json: %w", err)
	}

	// Extract and modify packages array
	var packages []string
	if packagesRaw, ok := rawSettings["packages"]; ok {
		if err := json.Unmarshal(packagesRaw, &packages); err != nil {
			return fmt.Errorf("failed to parse packages in settings.json: %w", err)
		}
	}

	// Remove the extension package
	newPackages := make([]string, 0, len(packages))
	for _, pkg := range packages {
		if pkg != entireExtensionPackage {
			newPackages = append(newPackages, pkg)
		}
	}

	// Update the packages field in rawSettings
	packagesJSON, err := json.Marshal(newPackages)
	if err != nil {
		return fmt.Errorf("failed to marshal packages: %w", err)
	}
	rawSettings["packages"] = packagesJSON

	// Write back
	output, err := json.MarshalIndent(rawSettings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, output, 0o644); err != nil {
		return fmt.Errorf("failed to write settings.json: %w", err)
	}

	return nil
}

// AreHooksInstalled checks if pi-entire extension is installed.
func (p *PiAgent) AreHooksInstalled() bool {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}

	settingsPath := filepath.Join(repoRoot, ".pi", PiSettingsFileName)
	data, err := os.ReadFile(settingsPath) //nolint:gosec
	if err != nil {
		return false
	}

	// Use map[string]json.RawMessage for consistency
	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return false
	}

	var packages []string
	if packagesRaw, ok := rawSettings["packages"]; ok {
		if err := json.Unmarshal(packagesRaw, &packages); err != nil {
			return false
		}
	}

	for _, pkg := range packages {
		if pkg == entireExtensionPackage {
			return true
		}
	}

	return false
}

// GetSupportedHooks returns the hook types Pi supports.
func (p *PiAgent) GetSupportedHooks() []agent.HookType {
	return []agent.HookType{
		agent.HookSessionStart,
		agent.HookSessionEnd,
		agent.HookUserPromptSubmit,
		agent.HookStop,
	}
}

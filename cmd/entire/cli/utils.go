package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/entireio/cli/redact"
)

// IsAccessibleMode returns true if accessibility mode should be enabled.
// This checks the ACCESSIBLE environment variable.
// Set ACCESSIBLE=1 (or any non-empty value) to enable accessible mode,
// which uses simpler prompts that work better with screen readers.
func IsAccessibleMode() bool {
	return os.Getenv("ACCESSIBLE") != ""
}

// entireTheme returns the Dracula theme for consistent styling.
func entireTheme() *huh.Theme {
	return huh.ThemeDracula()
}

// NewAccessibleForm creates a new huh form with accessibility mode
// enabled if the ACCESSIBLE environment variable is set.
// Note: WithAccessible() is only available on forms, not individual fields.
// Always wrap confirmations and other prompts in a form to enable accessibility.
func NewAccessibleForm(groups ...*huh.Group) *huh.Form {
	form := huh.NewForm(groups...).WithTheme(entireTheme())
	if IsAccessibleMode() {
		form = form.WithAccessible(true)
	}
	return form
}

// fileExists checks if a file exists
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// copyFile copies a file from src to dst
func copyFile(src, dst string) error {
	input, err := os.ReadFile(src) //nolint:gosec // Reading from controlled git metadata path
	if err != nil {
		return err //nolint:wrapcheck // already present in codebase
	}
	if err := os.WriteFile(dst, input, 0o600); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	return nil
}

// copyFileRedacted copies a file from src to dst with secret redaction applied.
// Uses format-aware redaction based on file extension:
//   - .jsonl: JSONL-aware redaction (line-delimited JSON, e.g. Claude Code transcripts)
//   - .json:  JSON-aware redaction (single JSON document, e.g. Gemini CLI transcripts)
//   - other:  plain-text redaction
func copyFileRedacted(src, dst string) error {
	data, err := os.ReadFile(src) //nolint:gosec // Reading from controlled git metadata path
	if err != nil {
		return err //nolint:wrapcheck // already present in codebase
	}
	switch {
	case strings.HasSuffix(src, ".jsonl"):
		data, err = redact.JSONLBytes(data)
		if err != nil {
			return fmt.Errorf("failed to redact JSONL: %w", err)
		}
	case strings.HasSuffix(src, ".json"):
		data, err = redact.JSONBytes(data)
		if err != nil {
			return fmt.Errorf("failed to redact JSON: %w", err)
		}
	default:
		data = redact.Bytes(data)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	return nil
}

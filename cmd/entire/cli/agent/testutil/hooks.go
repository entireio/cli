// Package testutil provides shared test utilities for agent packages.
package testutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ReadRawHooks reads the raw hooks map from a settings file.
// settingsDir is the directory name (e.g., ".claude" or ".gemini").
func ReadRawHooks(t *testing.T, tempDir, settingsDir string) map[string]json.RawMessage {
	t.Helper()
	settingsPath := filepath.Join(tempDir, settingsDir, "settings.json")
	data, err := os.ReadFile(settingsPath) //nolint:gosec // Test utility, path constructed from test tempDir
	if err != nil {
		t.Fatalf("failed to read settings.json: %v", err)
	}

	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		t.Fatalf("failed to parse settings.json: %v", err)
	}

	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := rawSettings["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			t.Fatalf("failed to parse hooks: %v", err)
		}
	}
	return rawHooks
}

// GetKeys returns the keys of a map as a slice.
func GetKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

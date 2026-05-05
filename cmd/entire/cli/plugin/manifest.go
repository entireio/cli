// Package plugin implements the gh-style plugin system: external executables
// named `entire-<name>` that the CLI discovers and dispatches to when an
// unknown subcommand is invoked.
//
// See docs/plugin-system-plan.md for the architecture, and gh's
// pkg/cmd/extension for the design we mirror.
package plugin

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ManifestFileName is the filename of the binary plugin manifest within a
// plugin directory. Field-for-field parity with gh's binManifest.
const ManifestFileName = "manifest.yml"

// BinaryManifest describes a plugin installed from a GitHub release asset.
// Field-for-field parity with gh's binManifest so user intuition transfers.
type BinaryManifest struct {
	Owner    string `yaml:"owner"`
	Name     string `yaml:"name"`
	Host     string `yaml:"host"`
	Tag      string `yaml:"tag"`
	IsPinned bool   `yaml:"isPinned"`
	Path     string `yaml:"path"`
}

// LoadBinaryManifest reads a manifest from disk.
func LoadBinaryManifest(path string) (*BinaryManifest, error) {
	// G304: path is constructed from the plugins root + a controlled prefix.
	data, err := os.ReadFile(path) //nolint:gosec // controlled path under plugins root
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m BinaryManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// Save writes the manifest to disk at path.
func (m *BinaryManifest) Save(path string) error {
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

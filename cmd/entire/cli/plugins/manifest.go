// Package plugins implements the embedded Lua plugin runtime for the Entire
// CLI. It lets third parties author no-build-step plugins that subscribe to
// lifecycle/git hooks and contribute commands, layered on top of the existing
// kubectl-style `entire-<name>` binary plugins without replacing them.
//
// The runtime is pure Go (github.com/yuin/gopher-lua) so it builds with
// CGO_ENABLED=0 on every target. Plugins run inside a sandboxed Lua state with
// a curated standard library and are gated by an explicit per-plugin allow-list
// in settings (see settings.PluginSettings); repo-local plugins never auto-run.
package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// DefaultEntry is the plugin entry script used when a manifest omits "entry".
const DefaultEntry = "main.lua"

// ManifestFileName is the on-disk name of a plugin's manifest.
const ManifestFileName = "plugin.json"

// Manifest is the parsed plugin.json describing a Lua plugin. It is decoded
// strictly (unknown fields are rejected) so typos surface at load time rather
// than being silently ignored.
type Manifest struct {
	// Name is the plugin's bare identifier. It must be dispatch-safe (no path
	// separators, not flag- or agent-protocol-shaped) so it can key the
	// allow-list, the data dir, and any contributed command.
	Name string `json:"name"`

	// Version is an informational semantic version string. It is not enforced.
	Version string `json:"version,omitempty"`

	// Description is a short human-readable summary shown by `entire plugin list`.
	Description string `json:"description,omitempty"`

	// Entry is the Lua script executed once at load to register hooks/commands.
	// Defaults to DefaultEntry ("main.lua").
	Entry string `json:"entry,omitempty"`

	// Hooks declares the lifecycle/git hooks the plugin intends to subscribe
	// to. It is validated against the known hook set; actual subscription
	// happens at runtime via entire.on. Declaring hooks lets tooling surface a
	// plugin's surface area without executing it.
	Hooks []string `json:"hooks,omitempty"`

	// Commands declares the CLI subcommands the plugin contributes. Actual
	// registration happens at runtime via entire.command.
	Commands []CommandManifest `json:"commands,omitempty"`

	// Capabilities lists the privileged capabilities the plugin requests. A
	// capability only takes effect when ALSO granted in the plugin's allow-list
	// entry; the manifest field documents intent and is validated against the
	// known capability set.
	Capabilities []string `json:"capabilities,omitempty"`
}

// CommandManifest declares a single CLI subcommand contributed by a plugin.
type CommandManifest struct {
	// Name is the subcommand name, invoked as `entire <name>`.
	Name string `json:"name"`
	// Short is the one-line description shown in help output.
	Short string `json:"short,omitempty"`
}

// EntryFile returns the manifest's entry script name, defaulting to DefaultEntry.
func (m *Manifest) EntryFile() string {
	if m.Entry == "" {
		return DefaultEntry
	}
	return m.Entry
}

// ParseManifest decodes and validates a plugin.json payload. Decoding is strict
// (DisallowUnknownFields) and mirrors the settings package's contract so an
// unknown key is a hard error.
func ParseManifest(data []byte) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parsing plugin manifest: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// validate enforces the manifest's structural invariants: a dispatch-safe name,
// known hook names, non-empty command names, and known capability names.
func (m *Manifest) validate() error {
	if err := ValidatePluginName(m.Name); err != nil {
		return fmt.Errorf("plugin manifest name: %w", err)
	}
	if strings.TrimSpace(m.Entry) != m.Entry {
		return fmt.Errorf("plugin %q: entry %q must not have surrounding whitespace", m.Name, m.Entry)
	}
	if strings.ContainsAny(m.EntryFile(), `/\`) {
		return fmt.Errorf("plugin %q: entry %q must be a bare file name in the plugin dir", m.Name, m.EntryFile())
	}
	for _, h := range m.Hooks {
		if !IsKnownHook(h) {
			return fmt.Errorf("plugin %q: unknown hook %q (see docs/architecture/plugins-lua.md)", m.Name, h)
		}
	}
	seen := make(map[string]struct{}, len(m.Commands))
	for _, c := range m.Commands {
		if err := ValidatePluginName(c.Name); err != nil {
			return fmt.Errorf("plugin %q: command name: %w", m.Name, err)
		}
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("plugin %q: duplicate command %q", m.Name, c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	for _, capName := range m.Capabilities {
		if !settings.IsKnownPluginCapabilityName(capName) {
			return fmt.Errorf("plugin %q: unknown capability %q (allowed: %s)",
				m.Name, capName, strings.Join(settings.KnownPluginCapabilities(), ", "))
		}
	}
	return nil
}

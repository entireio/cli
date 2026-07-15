package plugins

import (
	"errors"
	"fmt"
	"strings"
)

// ValidatePluginName enforces the same dispatch-safety rules the kubectl-style
// binary dispatcher applies to `entire-<name>` (see validatePluginName in
// cmd/entire/cli/plugin_store.go). It is duplicated here rather than imported
// because the cli package imports plugins, so plugins cannot import cli.
//
// The rules must stay in lockstep with the binary dispatcher: a Lua plugin
// contributes commands resolved as `entire <name>`, and its data dir is keyed
// by name, so a name the binary dispatcher would reject must be rejected here
// too.
func ValidatePluginName(name string) error {
	if name == "" {
		return errors.New("plugin name is empty")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("plugin name %q must not start with '-'", name)
	}
	if strings.HasPrefix(name, "agent-") {
		return fmt.Errorf("plugin name %q is reserved for the external agent protocol", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("plugin name %q must not contain path separators", name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("plugin name %q is not a valid identifier", name)
	}
	return nil
}

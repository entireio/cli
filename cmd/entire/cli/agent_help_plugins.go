package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// agentHelpGuidedPluginsHeading heads the plugins with agent guidance near the
// top of `entire agent-help`, so an agent that reads only the start of the
// output still sees them. It ties the guidance to the agent's task, not to
// using the plugin: a plugin's guidance can be about the agent's own work.
const agentHelpGuidedPluginsHeading = "Plugins with agent guidance — run the ones that may bear on your task:"

// agentHelpPluginNameRE is the shape a name must have to be put in a command
// for agents to run: validatePluginName accepts more (spaces, shell
// metacharacters), which must not reach a shell.
var agentHelpPluginNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// agentHelpGuidedPlugins is the installed plugins whose release declared
// agent_help: from the managed tree only (bin entries and install
// manifests), never running a plugin or touching the network. The manifest
// describes the release binary, so it counts only while the bin entry is
// that binary: a local install over a release may not answer agent-help.
// A name a built-in shadows is left out (the built-in answers it).
func agentHelpGuidedPlugins(rootCmd *cobra.Command) []string {
	installed, err := ListInstalledPlugins()
	if err != nil {
		return nil
	}
	var names []string
	for _, p := range installed {
		if !agentHelpPluginNameRE.MatchString(p.Name) || validatePluginName(p.Name) != nil || slices.Contains(names, p.Name) {
			continue
		}
		if c, _, err := rootCmd.Find([]string{p.Name}); err == nil && c != rootCmd {
			continue
		}
		if m, err := LoadPluginManifest(p.Name); err != nil || m == nil || !m.AgentHelp || !agentHelpEntryIsRelease(p) {
			continue
		}
		names = append(names, p.Name)
	}
	return names
}

// agentHelpEntryIsRelease reports whether p's bin entry is the release binary
// its manifest describes, pkg/<name>/entire-<name>.
func agentHelpEntryIsRelease(p *InstalledPlugin) bool {
	dir, err := PluginPkgDir(p.Name)
	if err != nil {
		return false
	}
	release, err := os.Stat(filepath.Join(dir, pluginBinaryName(p.Name)))
	if err != nil {
		return false
	}
	entry, err := os.Stat(p.Path)
	return err == nil && os.SameFile(entry, release)
}

// renderAgentHelpGuidedPlugins is the block naming them, "" when there are
// none.
func renderAgentHelpGuidedPlugins(names []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n" + agentHelpGuidedPluginsHeading + "\n")
	for _, n := range names {
		b.WriteString("  entire " + agentHelpCmdName + " " + n + "\n")
	}
	return b.String()
}

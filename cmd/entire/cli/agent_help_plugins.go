package cli

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
)

// agentHelpPluginDescriptionMax bounds a plugin's description in the listing:
// it is the plugin author's text, shown to agents, and one line is all the
// listing has room for.
const agentHelpPluginDescriptionMax = 100

// agentHelpPlugin is one installed plugin as the top-level agent-help lists
// it. A plugin documents itself (what it is for, which of its commands are
// safe to run) through `entire agent-help <name>`; the listing only lets an
// agent find it.
type agentHelpPlugin struct {
	Name string `json:"name"`
	// Description is the one-line summary recorded at install, from the
	// plugin's entire-plugin.yml. Empty when there is none: a local install
	// has no manifest, and an older manifest predates the field.
	Description string `json:"description,omitempty"`
	// Help is the command that prints the plugin's own agent-help.
	Help string `json:"help"`
}

// agentHelpInstalledPlugins lists the plugins in the managed plugin dir that
// agent-help can delegate to, sorted by name. It reads only the managed tree
// (the bin entries and each install manifest): it never runs a plugin, reads
// the plugin index, or touches the network, since agents run agent-help
// unprompted. A name a built-in command shadows is left out (built-ins win,
// as in the dispatcher). Any failure to read the tree yields no list rather
// than an error: the listing is a pointer, never a reason for agent-help to
// fail.
func agentHelpInstalledPlugins(rootCmd *cobra.Command) []agentHelpPlugin {
	installed, err := ListInstalledPlugins()
	if err != nil {
		return nil
	}
	var out []agentHelpPlugin
	for _, p := range installed {
		if validatePluginName(p.Name) != nil || agentHelpFindChild(rootCmd, p.Name) != nil {
			continue
		}
		entry := agentHelpPlugin{Name: p.Name, Help: "entire " + agentHelpCmdName + " " + p.Name}
		if m, err := LoadPluginManifest(p.Name); err == nil && m != nil {
			entry.Description = agentHelpPluginDescription(m.Description)
		}
		out = append(out, entry)
	}
	return out
}

// agentHelpPluginDescription makes an author's description safe for the
// listing: one trimmed line, at most agentHelpPluginDescriptionMax runes, and
// nothing at all when it carries control characters (newlines, ANSI escapes),
// which would let a plugin write its own lines into the agent's context.
func agentHelpPluginDescription(s string) string {
	s = strings.TrimSpace(s)
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return ""
	}
	if r := []rune(s); len(r) > agentHelpPluginDescriptionMax {
		s = strings.TrimSpace(string(r[:agentHelpPluginDescriptionMax-1])) + "…"
	}
	return s
}

// renderAgentHelpPlugins renders the installed-plugins section of the
// top-level text, or "" when there are none.
func renderAgentHelpPlugins(plugins []agentHelpPlugin) string {
	if len(plugins) == 0 {
		return ""
	}
	width := 10
	for _, p := range plugins {
		width = max(width, len(p.Name))
	}
	var b strings.Builder
	b.WriteString("\nInstalled plugins — each says what it is for and which of its commands are safe to run:\n")
	for _, p := range plugins {
		fmt.Fprintf(&b, "  %s\n", strings.TrimRight(fmt.Sprintf("%-*s  %s", width, p.Name, p.Description), " "))
	}
	b.WriteString("  Read one before using it:  entire agent-help <plugin>\n")
	return b.String()
}

package cli

import (
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
)

const (
	// agentHelpPluginDescriptionMax bounds a plugin's description in the
	// listing: it is the plugin author's text, shown to agents, and one line
	// is all the listing has room for.
	agentHelpPluginDescriptionMax = 100
	// agentHelpPluginNameMax bounds a listed plugin's name, in bytes (names
	// are ASCII, see agentHelpPluginNameRE).
	agentHelpPluginNameMax = 64
	// agentHelpPluginsMax bounds how many plugins the top level lists; the
	// rest are counted, not named.
	agentHelpPluginsMax = 50
	// agentHelpPluginColumnMax caps the name column, so one long name doesn't
	// push every description to the right.
	agentHelpPluginColumnMax = 24
)

// agentHelpPluginNameRE is the conservative shape a plugin name must have to
// be listed. validatePluginName accepts more (spaces, shell metacharacters,
// format characters); the listing hands each name to agents inside a command
// to run (`entire agent-help <name>`), so it only names plugins whose name is
// safe to paste into a shell.
var agentHelpPluginNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// agentHelpPlugin is one installed plugin as the top-level agent-help lists
// it. A plugin documents itself (what it is for, which of its commands are
// safe to run) through `entire agent-help <name>`; the listing only lets an
// agent find it.
type agentHelpPlugin struct {
	Name string `json:"name"`
	// Description is the one-line summary recorded at install, from the
	// plugin's entire-plugin.yml. Empty when there is none: a local install
	// usually has no manifest, and an older manifest predates the field.
	Description string `json:"description,omitempty"`
	// Help is the command that prints the plugin's own agent-help.
	Help string `json:"help"`
}

// agentHelpPluginListing is the installed plugins the top level lists: at
// most agentHelpPluginsMax of them, and how many there were in all.
type agentHelpPluginListing struct {
	Plugins []agentHelpPlugin
	Total   int
}

// agentHelpInstalledPlugins lists the plugins in the managed plugin dir that
// agent-help can delegate to. It reads only the managed tree (the bin entries,
// what they point at, and each install manifest): it never runs a plugin,
// reads the plugin index, or touches the network, since agents run agent-help
// unprompted. Any failure to read the tree yields no list rather than an
// error: the listing is a pointer, never a reason for agent-help to fail.
func agentHelpInstalledPlugins(rootCmd *cobra.Command) agentHelpPluginListing {
	installed, err := ListInstalledPlugins()
	if err != nil {
		return agentHelpPluginListing{}
	}
	return agentHelpSelectPlugins(rootCmd, installed, agentHelpPluginRunnable, agentHelpManifestDescription)
}

// agentHelpSelectPlugins picks the listed plugins from installed (sorted by
// name, as ListInstalledPlugins returns them): those with a listable name
// (agentHelpPluginName), not shadowed by a built-in (built-ins win, as in the
// dispatcher), and that runnable says can be run, each name once (on Windows
// entire-foo.exe and entire-foo.cmd are both "foo"). It keeps the first
// agentHelpPluginsMax and counts the rest.
func agentHelpSelectPlugins(rootCmd *cobra.Command, installed []*InstalledPlugin, runnable func(path string) bool, description func(name string) string) agentHelpPluginListing {
	var listing agentHelpPluginListing
	seen := map[string]bool{}
	for _, p := range installed {
		if seen[p.Name] || !agentHelpPluginName(p.Name) || agentHelpFindChild(rootCmd, p.Name) != nil || !runnable(p.Path) {
			continue
		}
		seen[p.Name] = true
		listing.Total++
		if len(listing.Plugins) == agentHelpPluginsMax {
			continue
		}
		listing.Plugins = append(listing.Plugins, agentHelpPlugin{
			Name:        p.Name,
			Description: description(p.Name),
			Help:        "entire " + agentHelpCmdName + " " + p.Name,
		})
	}
	return listing
}

// agentHelpPluginName reports whether name may be listed: a valid plugin name
// (validatePluginName, which also refuses the reserved agent- prefix) of the
// conservative shape agentHelpPluginNameRE, at most agentHelpPluginNameMax
// bytes.
func agentHelpPluginName(name string) bool {
	return len(name) <= agentHelpPluginNameMax && agentHelpPluginNameRE.MatchString(name) && validatePluginName(name) == nil
}

// agentHelpPluginRunnable reports whether the managed bin entry at path is
// something the dispatcher would run: following symlinks, a regular, non-empty
// file, executable on Unix. A dangling symlink, a directory, a FIFO or a file
// without an executable bit is left out of the listing; `entire plugin doctor`
// reports those.
func agentHelpPluginRunnable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return false
	}
	return runtime.GOOS == windowsGOOS || info.Mode().Perm()&0o111 != 0
}

// agentHelpManifestDescription is name's description from its install
// manifest, made safe for the listing, or "" when there is none.
func agentHelpManifestDescription(name string) string {
	m, err := LoadPluginManifest(name)
	if err != nil || m == nil {
		return ""
	}
	return agentHelpPluginDescription(m.Description)
}

// agentHelpPluginDescription makes an author's description safe for the
// listing: one trimmed line, at most agentHelpPluginDescriptionMax runes, and
// nothing at all unless every character is a letter, number, punctuation
// mark, symbol or a plain space. That leaves out control, bidi and other
// format characters, line and paragraph separators, combining marks
// (variation selectors among them), private-use and unassigned code points,
// and other spaces, any of which could shape what the agent or user sees
// beyond the text itself. The Hangul fillers, letters that render as blank
// space, are refused by name.
func agentHelpPluginDescription(s string) string {
	s = strings.TrimSpace(s)
	if !strings.ContainsFunc(s, agentHelpDescriptionRuneRefused) {
		if r := []rune(s); len(r) > agentHelpPluginDescriptionMax {
			s = strings.TrimSpace(string(r[:agentHelpPluginDescriptionMax-1])) + "…"
		}
		return s
	}
	return ""
}

// agentHelpDescriptionRuneRefused reports whether r may not appear in a
// listed description (see agentHelpPluginDescription).
func agentHelpDescriptionRuneRefused(r rune) bool {
	switch r {
	case ' ':
		return false
	case 'ᅟ', 'ᅠ', 'ㅤ', 'ﾠ': // Hangul fillers
		return true
	}
	return !unicode.In(r, unicode.L, unicode.N, unicode.P, unicode.S)
}

// renderAgentHelpPlugins renders the installed-plugins section of the
// top-level text, or "" when there are none.
func renderAgentHelpPlugins(listing agentHelpPluginListing) string {
	if len(listing.Plugins) == 0 {
		return ""
	}
	// Names are ASCII (agentHelpPluginNameRE), so their length is their width.
	width := 10
	for _, p := range listing.Plugins {
		width = max(width, len(p.Name))
	}
	width = min(width, agentHelpPluginColumnMax)
	var b strings.Builder
	b.WriteString("\nInstalled plugins — each says what it is for and which of its commands are safe to run:\n")
	for _, p := range listing.Plugins {
		fmt.Fprintf(&b, "  %s\n", strings.TrimRight(fmt.Sprintf("%-*s  %s", width, p.Name, p.Description), " "))
	}
	if more := listing.Total - len(listing.Plugins); more > 0 {
		fmt.Fprintf(&b, "  …and %d more\n", more)
	}
	b.WriteString("  Read one before using it:  entire agent-help <plugin>\n")
	return b.String()
}

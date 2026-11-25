package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// agentHelpGuidedPluginsHeading heads the plugins that declared agent_help.
// It asks the agent to read the guidance that may bear on its task rather than
// "before using" the plugin: a live run showed an agent skip stream's check
// before an edit because it wasn't using the plugin.
const agentHelpGuidedPluginsHeading = "Installed plugins with agent guidance — read the ones that may bear on your task: entire agent-help <plugin>"

// agentHelpPluginNameRE is the conservative shape a plugin name must have to
// be listed. validatePluginName accepts more (spaces, shell metacharacters,
// format characters); the listing hands each name to agents inside a command
// to run (`entire agent-help <name>`), so it only names plugins whose name is
// safe to paste into a shell.
var agentHelpPluginNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// agentHelpPlugin is one installed plugin as the top-level agent-help lists
// it. A plugin whose entire-plugin.yml declares agent_help documents itself
// (what it is for, which of its commands are safe to run) through
// `entire agent-help <name>`; for the others the listing only names them,
// since that command runs the plugin and implementing it is opt-in.
type agentHelpPlugin struct {
	Name string `json:"name"`
	// Description is the one-line summary recorded at install, from the
	// plugin's entire-plugin.yml. Empty when there is none: a local install
	// has no manifest, and an older manifest predates the field.
	Description string `json:"description,omitempty"`
	// Help is the command that prints the plugin's own agent-help, set only
	// when the plugin declared agent_help.
	Help string `json:"help,omitempty"`
}

// agentHelpPluginInfo is what the listing takes from a plugin's install
// manifest.
type agentHelpPluginInfo struct {
	Description string
	AgentHelp   bool
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
	if dir, err := PluginBinDir(); err == nil {
		installed = agentHelpDispatched(installed, dir, exec.LookPath)
	}
	return agentHelpSelectPlugins(rootCmd, installed, agentHelpPluginRunnable, agentHelpManifestInfo)
}

// agentHelpDispatched keeps, of each name's entries in installed, the one
// the dispatcher runs, so the listing names, and trusts the manifest of,
// the entry `entire <name>` execs. The dispatcher finds a plugin with
// exec.LookPath, and the managed bin dir is first on PATH (main.go), so
// lookPath on dir/entire-<name> gives the same file: on Windows the first
// PATHEXT extension that exists, on Unix the one entry if executable. A name
// lookPath finds nothing for is left out. Order is kept.
func agentHelpDispatched(installed []*InstalledPlugin, dir string, lookPath func(string) (string, error)) []*InstalledPlugin {
	var out []*InstalledPlugin
	picked := map[string]string{}
	for _, p := range installed {
		run, done := picked[p.Name]
		if !done {
			found, err := lookPath(filepath.Join(dir, pluginBinaryPrefix+p.Name))
			if err != nil {
				found = ""
			}
			run, picked[p.Name] = found, found
		}
		// LookPath spells the extension as PATHEXT does; Windows names
		// ignore case.
		if run != "" && strings.EqualFold(filepath.Clean(run), filepath.Clean(p.Path)) {
			out = append(out, p)
		}
	}
	return out
}

// agentHelpSelectPlugins picks the listed plugins from installed (sorted by
// name, as ListInstalledPlugins returns them): those with a listable name
// (agentHelpPluginName), not shadowed by a built-in (built-ins win, as in the
// dispatcher), and that runnable says can be run, each name once (on Windows
// entire-foo.exe and entire-foo.cmd are both "foo": the first is the one
// the dispatcher runs, see agentHelpDispatched, and when it can't be run
// the name is left out rather than another entry named). It keeps the first
// agentHelpPluginsMax and counts the rest. info supplies each one's
// description and whether it declared agent_help.
func agentHelpSelectPlugins(rootCmd *cobra.Command, installed []*InstalledPlugin, runnable func(path string) bool, info func(*InstalledPlugin) agentHelpPluginInfo) agentHelpPluginListing {
	var listing agentHelpPluginListing
	seen := map[string]bool{}
	for _, p := range installed {
		if seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		if !agentHelpPluginName(p.Name) || agentHelpFindChild(rootCmd, p.Name) != nil || !runnable(p.Path) {
			continue
		}
		listing.Total++
		if len(listing.Plugins) == agentHelpPluginsMax {
			continue
		}
		in := info(p)
		entry := agentHelpPlugin{Name: p.Name, Description: in.Description}
		if in.AgentHelp {
			entry.Help = "entire " + agentHelpCmdName + " " + p.Name
		}
		listing.Plugins = append(listing.Plugins, entry)
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

// agentHelpPluginRunnable reports whether the managed bin entry at path is one
// the listing may name. It starts from checkManagedPluginRunnable, the rule
// `plugin doctor` and on-demand repair share (no dangling or unfollowable
// symlink, directory or empty file), and adds what that rule leaves to the
// exec, since the listing tells agents to run the entry before any exec has
// tried it: a regular file (not, say, a FIFO), executable on Unix.
func agentHelpPluginRunnable(path string) bool {
	if _, err := checkManagedPluginRunnable(path); err != nil {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == windowsGOOS || info.Mode().Perm()&0o111 != 0
}

// agentHelpManifestInfo is p's description (made safe for the listing) and
// agent_help declaration from its install manifest, zero when it has none.
// The manifest describes the release binary in pkg/<name>/, so it is trusted
// only while the bin entry is that binary (a symlink or hardlink to it, which
// os.SameFile settles without reading it). A local `plugin install <path>
// --force` over a release keeps the release's manifest (`plugin doctor` notes
// the replaced entry), and the local build may not answer agent-help.
// `plugin doctor` also accepts a byte-identical copy; the listing doesn't
// hash binaries, so a copy is listed without them.
func agentHelpManifestInfo(p *InstalledPlugin) agentHelpPluginInfo {
	m, err := LoadPluginManifest(p.Name)
	if err != nil || m == nil || !agentHelpEntryIsRelease(p) {
		return agentHelpPluginInfo{}
	}
	return agentHelpPluginInfo{Description: agentHelpPluginDescription(m.Description), AgentHelp: m.AgentHelp}
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

// agentHelpPluginsFirstHeading heads the plugins with agent guidance near the
// top of agent-help, before the commands. A live run showed Codex run
// `entire agent-help`, read only the start of its long output, and take it to
// be about entire's own commands: the listing at the end, the only mention of
// stream's guidance, went unread, and it edited a file a teammate's live
// session was editing. Claude and OpenCode followed the listing at the end.
const agentHelpPluginsFirstHeading = "Before you start — plugins with guidance that may bear on your task; run each first and follow it:"

// renderAgentHelpPluginsFirst renders, near the top of agent-help, a command
// line for each plugin that declared agent_help (at most agentHelpPluginsMax,
// as the listing at the end). It is empty when none did: plugins without
// guidance are listed only at the end, as the user's to run.
func renderAgentHelpPluginsFirst(listing agentHelpPluginListing) string {
	var guided []agentHelpPlugin
	width := 0
	for _, p := range listing.Plugins {
		if p.Help != "" {
			guided = append(guided, p)
			width = max(width, len(p.Help))
		}
	}
	if len(guided) == 0 {
		return ""
	}
	// Names are ASCII (agentHelpPluginNameRE), so the command's length is its
	// width; capped as the listing's name column is.
	width = min(width, len("entire agent-help ")+agentHelpPluginColumnMax)
	var b strings.Builder
	b.WriteString("\n" + agentHelpPluginsFirstHeading + "\n")
	for _, p := range guided {
		fmt.Fprintf(&b, "  %s\n", strings.TrimRight(fmt.Sprintf("%-*s  %s", width, p.Help, p.Description), " "))
	}
	return b.String()
}

// renderAgentHelpPlugins renders the installed-plugins section of the
// top-level text, or "" when there are none: first the plugins that declared
// agent_help, whose guidance an agent should read when it may bear on its task
// (a plugin's guidance can be about the agent's own work, such as stream's
// check before editing a file, not only about using the plugin), then the
// others, which carry no agent guidance and so are the user's to run.
func renderAgentHelpPlugins(listing agentHelpPluginListing) string {
	if len(listing.Plugins) == 0 {
		return ""
	}
	// Names are ASCII (agentHelpPluginNameRE), so their length is their width.
	width := 10
	var guided, other []agentHelpPlugin
	for _, p := range listing.Plugins {
		width = max(width, len(p.Name))
		if p.Help != "" {
			guided = append(guided, p)
		} else {
			other = append(other, p)
		}
	}
	width = min(width, agentHelpPluginColumnMax)
	var b strings.Builder
	group := func(header string, plugins []agentHelpPlugin) {
		if len(plugins) == 0 {
			return
		}
		b.WriteString(header)
		for _, p := range plugins {
			fmt.Fprintf(&b, "  %s\n", strings.TrimRight(fmt.Sprintf("%-*s  %s", width, p.Name, p.Description), " "))
		}
	}
	group("\n"+agentHelpGuidedPluginsHeading+"\n", guided)
	group("\nOther installed plugins — no agent guidance; the user's — suggest, don't run:\n", other)
	if more := listing.Total - len(listing.Plugins); more > 0 {
		fmt.Fprintf(&b, "  …and %d more\n", more)
	}
	return b.String()
}

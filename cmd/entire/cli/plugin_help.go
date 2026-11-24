package cli

import (
	"context"
	"fmt"
	"strings"
	"text/template"
	"unicode"
	"unicode/utf8"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/spf13/cobra"
)

// maxPluginHelpDescriptionLen caps, in runes, a plugin description shown in
// root help. entire-plugin.yml documents it as a one-line summary; anything
// longer is not one, and would wrap the command list it sits in.
const maxPluginHelpDescriptionLen = 160

// pluginHelpAnchor is where the "Plugin Commands:" section is spliced into
// cobra's default usage template: after the command groups, before Flags.
const pluginHelpAnchor = "{{if .HasAvailableLocalFlags}}"

// pluginHelpEntry is one row of the root help's "Plugin Commands:" section.
type pluginHelpEntry struct {
	name        string
	description string
}

// usePluginCommandsInRootHelp makes root help list managed plugins that
// declared a description.
//
// The section is rendered by a usage func rather than placeholder Cobra
// commands: a registered "graph" command would win rootCmd.Find, and dispatch
// would stop reaching entire-graph — breaking "built-ins win" in reverse. Only
// root usage pays for the manifest reads; subcommand help, normal dispatch
// and startup never touch the plugin tree here. Nothing is executed and
// nothing touches the network: the description was recorded in the install
// manifest at install/upgrade time.
//
// Children inherit a root usage func, so every other command is handed back
// to cobra's default, captured before the override.
func usePluginCommandsInRootHelp(root *cobra.Command) {
	cobraDefault := root.UsageFunc()
	base := root.UsageTemplate()
	if !strings.Contains(base, pluginHelpAnchor) {
		// cobra's default template changed shape; a test pins this.
		panic("plugin help: cobra usage template anchor not found")
	}
	tmpl := template.Must(template.New("root-usage").Funcs(template.FuncMap{
		// The two helpers cobra's own default usage template uses.
		"rpad": func(s string, padding int) string {
			return fmt.Sprintf("%-*s", padding, s)
		},
		"trimTrailingWhitespaces": func(s string) string {
			return strings.TrimRightFunc(s, unicode.IsSpace)
		},
		"pluginCommands": func(c *cobra.Command) string {
			return formatPluginHelpSection(c, pluginHelpEntries(c))
		},
	}).Parse(strings.Replace(base, pluginHelpAnchor, "{{pluginCommands .}}"+pluginHelpAnchor, 1)))

	root.SetUsageFunc(func(c *cobra.Command) error {
		if c != root {
			return cobraDefault(c)
		}
		if err := tmpl.Execute(c.OutOrStderr(), c); err != nil {
			c.PrintErrln(err)
			return fmt.Errorf("render root usage: %w", err)
		}
		return nil
	})
}

// pluginHelpEntries returns the managed plugins root help should list, sorted
// by name. A plugin is listed only when every one of these holds:
//
//   - its install manifest records a description (local-dev installs have no
//     manifest; raw-$PATH binaries are never read at all);
//   - it has a bin/ entry, i.e. dispatch can reach it;
//   - its name is not a built-in command or alias, which would always win;
//   - the description is printable and at most maxPluginHelpDescriptionLen
//     runes. It comes from the plugin's author and is printed straight to the
//     terminal, the hazard hasTerminalControlChars exists for.
//
// Any failure leaves the plugin (or the whole section) out of help; help
// itself never fails because of the plugin tree.
func pluginHelpEntries(root *cobra.Command) []pluginHelpEntry {
	ctx := root.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	manifests, err := ListPluginManifests()
	if err != nil {
		logging.Debug(ctx, "root help: listing plugin manifests failed", "error", err.Error())
		return nil
	}
	if len(manifests) == 0 {
		return nil
	}
	installed, err := ListInstalledPlugins()
	if err != nil {
		logging.Debug(ctx, "root help: listing installed plugins failed", "error", err.Error())
		return nil
	}
	linked := make(map[string]bool, len(installed))
	for _, p := range installed {
		linked[p.Name] = true
	}
	var out []pluginHelpEntry
	for _, m := range manifests { // already sorted by name
		desc := strings.TrimSpace(m.Description)
		switch {
		case desc == "",
			!isPluginCandidate(m.Name),
			!linked[m.Name],
			hasTerminalControlChars(desc),
			utf8.RuneCountInString(desc) > maxPluginHelpDescriptionLen,
			shadowsBuiltinCommand(root, m.Name):
			continue
		}
		out = append(out, pluginHelpEntry{name: m.Name, description: desc})
	}
	return out
}

// formatPluginHelpSection renders entries in the layout cobra uses for the
// built-in command groups, aligned with them unless a plugin name is longer.
func formatPluginHelpSection(root *cobra.Command, entries []pluginHelpEntry) string {
	if len(entries) == 0 {
		return ""
	}
	padding := 0
	if cmds := root.Commands(); len(cmds) > 0 {
		padding = cmds[0].NamePadding()
	}
	for _, e := range entries {
		padding = max(padding, len(e.name))
	}
	var b strings.Builder
	b.WriteString("\n\nPlugin Commands:")
	for _, e := range entries {
		fmt.Fprintf(&b, "\n  %-*s %s", padding, e.name, e.description)
	}
	return b.String()
}

// shadowsBuiltinCommand reports whether name resolves to a built-in command or
// alias, in which case dispatch never reaches entire-<name>. It primes help and
// completion the way the dispatcher does, since cobra only adds them inside
// Execute (see resolvePlugin).
func shadowsBuiltinCommand(root *cobra.Command, name string) bool {
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd(name)
	c, _, err := root.Find([]string{name})
	return err == nil && c != root
}

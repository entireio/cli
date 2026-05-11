package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newPluginGroupCmd builds `entire plugin` and its subcommands. The kubectl
// dispatcher in plugin.go is the runtime mechanism — these commands manage a
// per-user managed directory that the dispatcher discovers because main.go
// prepends it to PATH at startup.
//
// Currently only local symlink installs are supported. GitHub-release
// asset and git-clone install paths are deferred until there's demand.
func newPluginGroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Manage Entire plugins (install, list, remove)",
		Long: `Manage Entire plugins.

Plugins are external executables named 'entire-<name>'. The CLI discovers
plugins on $PATH and from a per-user managed directory which is
auto-prepended to PATH at startup. The managed directory is, in order of
precedence:

  $ENTIRE_PLUGIN_DIR/bin (override)
  $XDG_DATA_HOME/entire/plugins/bin (Linux/macOS, when set)
  ~/.local/share/entire/plugins/bin (Linux/macOS default)
  %LOCALAPPDATA%\entire\plugins\bin (Windows, when set)
  ~\AppData\Local\entire\plugins\bin (Windows fallback when LOCALAPPDATA is unset)

Commands:
  install   Install a plugin by linking or copying an existing executable
  list      List plugins installed in the managed directory
  remove    Remove a plugin from the managed directory

Examples:
  entire plugin install ./dist/entire-pgr
  entire plugin list
  entire plugin remove pgr`,
	}

	cmd.AddCommand(newPluginInstallCmd())
	cmd.AddCommand(newPluginListCmd())
	cmd.AddCommand(newPluginRemoveCmd())
	return cmd
}

func newPluginInstallCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "install <path>",
		Short: "Link or copy a plugin executable into the managed directory",
		Long: `Link or copy a plugin executable into the managed directory.

The source must be a file whose basename starts with 'entire-' (the
dispatcher only resolves names of that shape). On Unix the file must be
executable.

The CLI prefers a symlink so rebuilds of the source are reflected
immediately, and falls back to a hardlink, then a copy, if symlinks aren't
available (notably Windows without Developer Mode).

After install, 'entire <name>' invokes the plugin via the kubectl-style
dispatcher — the managed directory is auto-prepended to $PATH.

Examples:
  entire plugin install ./dist/entire-pgr
  entire plugin install /usr/local/bin/entire-pgr --force`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := InstallPluginFromPath(InstallPluginOptions{
				SourcePath: args[0],
				Force:      force,
			})
			if err != nil {
				return fmt.Errorf("install plugin: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Installed plugin %q → %s\n", p.Name, p.Path)
			warnIfShadowsBuiltin(cmd, p.Name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Replace an existing entry with the same name")
	return cmd
}

// warnIfShadowsBuiltin prints a one-line note to stderr when the just-installed
// plugin name matches a built-in command. The dispatcher's resolvePlugin gates
// dispatch on rootCmd.Find, so the built-in always wins at runtime — without
// this hint, a user who installed a shadowed plugin would silently get the
// built-in and have no idea their install was inert. We mirror the dispatcher's
// help/completion priming so names like "help" surface the warning too.
func warnIfShadowsBuiltin(cmd *cobra.Command, name string) {
	root := cmd.Root()
	if root == nil {
		return
	}
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd(name)
	if c, _, err := root.Find([]string{name}); err == nil && c != root {
		fmt.Fprintf(cmd.ErrOrStderr(), "Note: %q shadows the built-in command; the built-in will take precedence at runtime.\n", name)
	}
}

func newPluginListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List plugins installed in the managed directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPluginList(cmd.OutOrStdout())
		},
	}
}

func runPluginList(w io.Writer) error {
	plugins, err := ListInstalledPlugins()
	if err != nil {
		return fmt.Errorf("list plugins: %w", err)
	}
	dir, err := PluginBinDir()
	if err != nil {
		return fmt.Errorf("plugin bin dir: %w", err)
	}
	if len(plugins) == 0 {
		fmt.Fprintf(w, "No plugins installed in %s.\n", dir)
		fmt.Fprintln(w, "Install one with 'entire plugin install <path>', or drop an entire-<name> binary anywhere on $PATH.")
		return nil
	}
	fmt.Fprintf(w, "Managed plugin directory: %s\n\n", dir)
	for _, p := range plugins {
		if p.Symlink {
			fmt.Fprintf(w, "  %-20s → %s\n", p.Name, p.LinkTarget)
		} else {
			fmt.Fprintf(w, "  %-20s %s\n", p.Name, p.Path)
		}
	}
	return nil
}

func newPluginRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a plugin from the managed directory",
		Long: `Remove a plugin from the managed directory.

Only entries in the managed directory are affected. Plugins installed by
dropping a binary elsewhere on $PATH are unmanaged — remove those by hand.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := RemoveInstalledPlugin(args[0]); err != nil {
				return fmt.Errorf("remove plugin: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed plugin %q\n", args[0])
			return nil
		},
	}
}

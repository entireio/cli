package cli

import (
	"fmt"
	"io"
	"os"

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
	cmd.AddCommand(newPluginUpdateCmd())
	cmd.AddCommand(newPluginListCmd())
	cmd.AddCommand(newPluginRemoveCmd())
	return cmd
}

func newPluginInstallCmd() *cobra.Command {
	var force bool
	var ref string
	cmd := &cobra.Command{
		Use:   "install <git-url|path>",
		Short: "Install a plugin from a git URL, a directory, or an executable",
		Long: `Install a plugin. The source may be:

  - a git URL (https://…, git@…, …/repo.git): the repo is cloned into the
    managed Lua plugin directory, keyed by its plugin.json "name". Use --ref to
    pin a tag, branch, or commit.
  - a directory containing a plugin.json: copied into the managed Lua plugin
    directory as a Lua plugin.
  - a file named 'entire-<name>': linked/copied into the managed bin directory
    as a kubectl-style binary plugin.

Installing only places files. A Lua plugin stays inert until you allow-list it
in .entire/settings.json (plugins.<name>.enabled = true) and grant any
capabilities it needs — installing an untrusted URL cannot run its code.

Examples:
  entire plugin install https://github.com/acme/entire-notify.git
  entire plugin install https://github.com/acme/entire-notify.git --ref v1.2.0
  entire plugin install ./my-plugin        # directory with plugin.json
  entire plugin install ./dist/entire-pgr  # binary plugin`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src := args[0]

			if looksLikeGitURL(src) {
				p, err := InstallLuaPluginFromGit(cmd.Context(), src, ref, force)
				if err != nil {
					return fmt.Errorf("install plugin: %w", err)
				}
				printLuaInstalled(cmd, p)
				return nil
			}
			if info, statErr := os.Stat(src); statErr == nil && info.IsDir() {
				p, err := InstallLuaPluginFromPath(cmd.Context(), src, force)
				if err != nil {
					return fmt.Errorf("install plugin: %w", err)
				}
				printLuaInstalled(cmd, p)
				return nil
			}

			if ref != "" {
				return fmt.Errorf("--ref applies only to git installs, not %q", src)
			}
			p, err := InstallPluginFromPath(InstallPluginOptions{
				SourcePath: src,
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
	cmd.Flags().StringVar(&ref, "ref", "", "Pin a git tag, branch, or commit (git installs only)")
	return cmd
}

// printLuaInstalled reports a successful Lua plugin install plus the opt-in hint
// that it will not run until allow-listed.
func printLuaInstalled(cmd *cobra.Command, p *InstalledLuaPlugin) {
	fmt.Fprintf(cmd.OutOrStdout(), "Installed Lua plugin %q → %s\n", p.Name, p.Dir)
	fmt.Fprintf(cmd.ErrOrStderr(),
		"Not enabled yet. To activate, add this to .entire/settings.json:\n"+
			"  \"plugins\": { %q: { \"enabled\": true } }\n", p.Name)
}

func newPluginUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update [name]",
		Short: "Update git-installed Lua plugins",
		Long: `Update Lua plugins that were installed from a git URL.

With a name, updates that plugin; with no name, updates all git-installed Lua
plugins. A plugin pinned with --ref is updated to the latest commit for that
tag/branch (a pinned commit stays put). Plugins installed from a local path or
as binaries cannot be updated this way — reinstall them instead.`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if err := UpdateLuaPlugin(cmd.Context(), args[0]); err != nil {
					return fmt.Errorf("update plugin: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Updated Lua plugin %q\n", args[0])
				return nil
			}
			return runUpdateAllLuaPlugins(cmd)
		},
	}
	return cmd
}

// runUpdateAllLuaPlugins updates every git-installed Lua plugin, reporting
// per-plugin results and skipping non-git installs.
func runUpdateAllLuaPlugins(cmd *cobra.Command) error {
	all, err := ListInstalledLuaPlugins()
	if err != nil {
		return fmt.Errorf("list lua plugins: %w", err)
	}
	updated := 0
	for _, p := range all {
		if p.Type != "git" {
			continue
		}
		if err := UpdateLuaPlugin(cmd.Context(), p.Name); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "  %-20s update failed: %v\n", p.Name, err)
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  %-20s updated\n", p.Name)
		updated++
	}
	if updated == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No git-installed Lua plugins to update.")
	}
	return nil
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
	binPlugins, err := ListInstalledPlugins()
	if err != nil {
		return fmt.Errorf("list plugins: %w", err)
	}
	luaPlugins, err := ListInstalledLuaPlugins()
	if err != nil {
		return fmt.Errorf("list lua plugins: %w", err)
	}
	dir, err := PluginBinDir()
	if err != nil {
		return fmt.Errorf("plugin bin dir: %w", err)
	}

	if len(binPlugins) == 0 && len(luaPlugins) == 0 {
		fmt.Fprintf(w, "No plugins installed in %s.\n", dir)
		fmt.Fprintln(w, "Install one with 'entire plugin install <git-url|path>', or drop an entire-<name> binary anywhere on $PATH.")
		return nil
	}

	if len(luaPlugins) > 0 {
		fmt.Fprintln(w, "Lua plugins:")
		for _, p := range luaPlugins {
			src := p.Source
			if p.Ref != "" {
				src = fmt.Sprintf("%s @ %s", p.Source, p.Ref)
			}
			if src == "" {
				src = p.Dir
			}
			fmt.Fprintf(w, "  %-20s %s\n", p.Name, src)
		}
		fmt.Fprintln(w)
	}

	if len(binPlugins) > 0 {
		fmt.Fprintf(w, "Binary plugins (managed dir %s):\n", dir)
		for _, p := range binPlugins {
			if p.Symlink {
				fmt.Fprintf(w, "  %-20s → %s\n", p.Name, p.LinkTarget)
			} else {
				fmt.Fprintf(w, "  %-20s %s\n", p.Name, p.Path)
			}
		}
	}
	return nil
}

func newPluginRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a plugin from the managed directory",
		Long: `Remove a plugin from the managed directory.

Removes a managed Lua plugin (under the lua/ dir) or a managed binary plugin
(under the bin/ dir). Plugins installed by dropping a binary elsewhere on $PATH
are unmanaged — remove those by hand. Removing does not touch the plugin's
allow-list entry in settings; delete that too to fully deactivate it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			// Prefer the Lua store (the newer surface); fall back to the binary
			// store so both kinds are removable by bare name.
			lua, err := FindInstalledLuaPlugin(name)
			if err != nil {
				return fmt.Errorf("remove plugin: %w", err)
			}
			if lua != nil {
				if err := RemoveLuaPlugin(name); err != nil {
					return fmt.Errorf("remove plugin: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Removed Lua plugin %q\n", name)
				return nil
			}

			if err := RemoveInstalledPlugin(name); err != nil {
				return fmt.Errorf("remove plugin: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed plugin %q\n", name)
			return nil
		},
	}
}

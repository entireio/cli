package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/plugin"
	"github.com/spf13/cobra"
)

// newPluginGroupCmd builds `entire plugin` and its subcommands.
func newPluginGroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Manage Entire plugins (install, list, remove, exec)",
		Long: `Manage Entire plugins.

Plugins are external executables named 'entire-<name>' that the CLI dispatches
to when an unknown subcommand is invoked. Install a local development plugin
with 'entire plugin install <dir>'.

Commands:
  install   Install a plugin (currently: local directories only)
  list      List installed plugins
  remove    Uninstall a plugin
  exec      Run a plugin by name (escape hatch for built-in collisions)

Examples:
  entire plugin install ./entire-foo
  entire plugin list
  entire plugin remove foo
  entire plugin exec foo --help`,
	}

	cmd.AddCommand(newPluginListCmd())
	cmd.AddCommand(newPluginInstallCmd())
	cmd.AddCommand(newPluginRemoveCmd())
	cmd.AddCommand(newPluginExecCmd())
	return cmd
}

func newPluginListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed plugins",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPluginList(cmd.OutOrStdout())
		},
	}
}

func runPluginList(w io.Writer) error {
	mgr, err := plugin.NewManager()
	if err != nil {
		return fmt.Errorf("plugin manager: %w", err)
	}
	plugins, err := mgr.List()
	if err != nil {
		return fmt.Errorf("list plugins: %w", err)
	}
	if len(plugins) == 0 {
		fmt.Fprintln(w, "No plugins installed.")
		fmt.Fprintln(w, "Install one with 'entire plugin install <dir-or-repo>'.")
		return nil
	}
	for _, p := range plugins {
		version := pluginVersionLabel(p)
		fmt.Fprintf(w, "%-20s %-7s %s\n", p.Name, p.Kind, version)
	}
	return nil
}

func pluginVersionLabel(p *plugin.Plugin) string {
	switch p.Kind {
	case plugin.KindBinary:
		if p.Manifest != nil && p.Manifest.Tag != "" {
			if p.PinnedSHA != "" || p.Manifest.IsPinned {
				return p.Manifest.Tag + " (pinned)"
			}
			return p.Manifest.Tag
		}
		return "unknown"
	case plugin.KindLocal:
		return p.ExecPath
	case plugin.KindScript:
		return ""
	default:
		return ""
	}
}

func newPluginInstallCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "install <path>",
		Short: "Install a plugin",
		Long: `Install a plugin.

Currently only local directories are supported. The directory must be named
'entire-<name>' and contain an executable of the same name.

Remote installation (GitHub release assets and git-cloned script plugins) is
planned but not yet implemented.

Examples:
  entire plugin install ./entire-foo
  entire plugin install . --force`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginInstall(cmd, args[0], force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Replace an already-installed plugin")
	return cmd
}

func runPluginInstall(cmd *cobra.Command, src string, force bool) error {
	mgr, err := plugin.NewManager()
	if err != nil {
		return fmt.Errorf("plugin manager: %w", err)
	}
	root := cmd.Root()
	p, err := mgr.InstallLocal(plugin.InstallLocalOptions{
		SourceDir: src,
		Force:     force,
		RootCmd:   root,
	})
	if err != nil {
		return fmt.Errorf("install plugin: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Installed plugin %q (%s) → %s\n", p.Name, p.Kind, p.ExecPath)
	return nil
}

func newPluginRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Uninstall a plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := plugin.NewManager()
			if err != nil {
				return fmt.Errorf("plugin manager: %w", err)
			}
			if err := mgr.Remove(args[0]); err != nil {
				return fmt.Errorf("remove plugin: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed plugin %q\n", args[0])
			return nil
		},
	}
}

func newPluginExecCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "exec <name> [args...]",
		Short:              "Run a plugin by name (bypassing built-in resolution)",
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			rest := args[1:]
			mgr, err := plugin.NewManager()
			if err != nil {
				return fmt.Errorf("plugin manager: %w", err)
			}
			p, err := mgr.Find(name)
			if err != nil {
				return fmt.Errorf("find plugin: %w", err)
			}
			if p == nil {
				return fmt.Errorf("plugin %q is not installed", name)
			}
			if err := plugin.Exec(cmd.Context(), p, rest, mgr.Root); err != nil {
				code := plugin.PropagateExitCode(err)
				if code > 0 {
					// Plugin returned a non-zero exit code. Surface it via a
					// SilentError so main.go preserves the user's intent
					// without printing extra noise.
					return NewSilentError(errors.New(p.FullName() + " exited with non-zero status"))
				}
				return fmt.Errorf("exec plugin: %w", err)
			}
			return nil
		},
	}
	return cmd
}

package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
)

// flagGlobal is `entire enable --global` / `entire disable --global`.
const flagGlobal = "global"

// runEnableGlobal turns Entire on in every repository: it sets
// `global_enable: true` in the user settings file and installs Claude Code's
// user-level hooks now rather than on the next command. It does not set up the
// repository it runs in; the hooks do that the first time an agent runs there.
func runEnableGlobal(cmd *cobra.Command) error {
	if err := rejectFlagsWithGlobal(cmd); err != nil {
		return err
	}
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	path, err := usersettings.SetGlobalEnable(ctx, true)
	if err != nil {
		return err //nolint:wrapcheck // usersettings names the file and the reason
	}
	fmt.Fprintf(w, "Entire is now enabled in every repository you work in (global_enable in %s).\n", path)

	n, hooksPath, err := claudecode.EnsureUserHooks(ctx)
	switch {
	case errors.Is(err, claudecode.ErrClaudeNotInUse):
		fmt.Fprintf(w, "Claude Code is not set up here (%s not found); its hooks will be added the next time you run entire after it is.\n",
			filepath.Dir(hooksPath))
	case err != nil:
		fmt.Fprintf(cmd.ErrOrStderr(), "Could not add Entire's hooks to %s: %v\nThey will be added the next time you run entire.\n", hooksPath, err)
	case n > 0:
		fmt.Fprintf(w, "Added Entire's hooks to Claude Code's user settings: %s\n", hooksPath)
	default:
		fmt.Fprintf(w, "Claude Code's user settings already have Entire's hooks: %s\n", hooksPath)
	}
	return nil
}

// runDisableGlobal sets `global_enable: false`. Repositories enabled with
// `entire enable` stay enabled, and the user-level Claude hooks stay: with the
// flag off they do nothing in a repository that was never enabled.
func runDisableGlobal(cmd *cobra.Command) error {
	if err := rejectFlagsWithGlobal(cmd); err != nil {
		return err
	}
	path, err := usersettings.SetGlobalEnable(cmd.Context(), false)
	if err != nil {
		return err //nolint:wrapcheck // usersettings names the file and the reason
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Entire is no longer enabled in every repository (global_enable in %s).\n", path)
	fmt.Fprintln(w, "Repositories you enabled with `entire enable` stay enabled.")
	return nil
}

// rejectFlagsWithGlobal refuses any other flag of this command beside
// --global: they all configure one repository, and --global covers every one.
// Changed, not value, so a default-true flag such as --telemetry still counts.
func rejectFlagsWithGlobal(cmd *cobra.Command) error {
	var others []string
	inherited := cmd.InheritedFlags()
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if f.Name != flagGlobal && inherited.Lookup(f.Name) == nil {
			others = append(others, "--"+f.Name)
		}
	})
	if len(others) == 0 {
		return nil
	}
	cmd.SilenceUsage = true
	return fmt.Errorf("--global applies to every repository and cannot be combined with %s", strings.Join(others, ", "))
}

// ensureGlobalEnableHooks makes `global_enable` effective in repositories that
// were never enabled. Everything else already follows the flag once Entire is called (settings resolve as enabled, and the
// first hook installs the git hooks lazily), but Claude only calls Entire if
// its hooks are installed, and a never-enabled repository has no project
// hooks. So whenever Entire runs with the flag on, it makes sure its hooks are
// in the user-level Claude settings. Best effort: this never fails a command.
func ensureGlobalEnableHooks(cmd *cobra.Command) {
	ctx := cmd.Context()
	if skipsGlobalEnableHooks(cmd) || !settings.UserTierEnablesEverywhere(ctx) {
		return
	}
	n, path, err := claudecode.EnsureUserHooks(ctx)
	if errors.Is(err, claudecode.ErrClaudeNotInUse) {
		return
	}
	if err != nil {
		logging.Debug(ctx, "global enable: could not install user-level Claude hooks",
			slog.String("error", err.Error()))
		return
	}
	if n > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "Entire is enabled for all your repositories; added its Claude Code hooks to %s\n", path)
	}
}

// hooksCommandName is the `entire hooks` group that agents and git invoke.
const hooksCommandName = "hooks"

// skipsGlobalEnableHooks reports commands that must not (re)install the
// user-level hooks: agent and git hook invocations, which run on every turn
// and only fire once hooks are already installed, and `entire disable`, which
// is removing Entire rather than using it.
func skipsGlobalEnableHooks(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case hooksCommandName, "disable":
			if c.Parent() != nil && !c.Parent().HasParent() {
				return true
			}
		}
	}
	return false
}

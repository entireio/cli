package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
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
func runEnableGlobal(cmd *cobra.Command, yes bool) error {
	if err := rejectFlagsWithGlobal(cmd, "yes"); err != nil {
		return err
	}
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	// Turning it on again adds nothing to agree to.
	if !settings.UserTierEnablesEverywhere(ctx) {
		confirmed, err := confirmGlobalEnable(cmd, yes)
		if err != nil || !confirmed {
			return err
		}
	}
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

// globalEnableDisclosure is what `entire enable --global` asks the developer
// to agree to.
const globalEnableDisclosure = "With global enable on, Entire records your agent sessions in every git repository you work in, " +
	"not just ones you ran `entire enable` in. Session checkpoints are pushed alongside your normal git pushes to that " +
	"repository's remote — including public repositories and forks. Run `entire disable` in a repository to stop it " +
	"there, or `entire disable --global` to turn this off."

// confirmGlobalEnable asks the developer to agree to global enable. --yes
// accepts, and still prints what was accepted; with no terminal to ask in it
// refuses, since the answer must be the developer's.
func confirmGlobalEnable(cmd *cobra.Command, yes bool) (bool, error) {
	if yes {
		fmt.Fprintf(cmd.ErrOrStderr(), "Accepted with --yes: %s\n", globalEnableDisclosure)
		return true, nil
	}
	if !interactive.CanPromptInteractively() {
		cmd.SilenceUsage = true
		return false, errors.New("refusing to enable Entire in every repository without confirmation; run it in a terminal or pass --yes")
	}
	return confirmPrompt(cmd, "Global enable", "Enable Entire in every repository you work in?", "",
		func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "%s\n\n", globalEnableDisclosure)
			return err //nolint:wrapcheck // a write to the prompt's own terminal
		})
}

// claimRepoFromGlobalEnable records, after a successful `entire enable`, that
// the developer enabled this repository themselves when it was on only through
// global enable: once global enable is not the only reason, the committed
// settings it set aside apply and the global-enable notices stop. It writes
// `enabled` to this worktree's settings.local.json, unless that file is
// tracked and so not the developer's own.
func claimRepoFromGlobalEnable(ctx context.Context, w io.Writer) error {
	s, err := LoadEntireSettings(ctx)
	if err != nil || !s.EnabledOnlyByGlobal() {
		return nil //nolint:nilerr // enable already succeeded; the claim is a refinement of it
	}
	if reason := s.LocalLayerRejection(); reason != "" {
		fmt.Fprintf(w, "Entire stays enabled here only through global enable: %s is not written because %s.\n",
			settings.EntireSettingsLocalFile, reason)
		return nil
	}
	if err := setEnabledFlag(ctx, true, false); err != nil {
		return err
	}
	fmt.Fprintf(w, "Entire is now enabled in this repository itself (%s), not only through global enable.\n",
		settings.EntireSettingsLocalFile)
	return nil
}

// rejectFlagsWithGlobal refuses any other flag of this command beside
// --global and allowed: they all configure one repository, and --global covers
// every one. Changed, not value, so a default-true flag such as --telemetry
// still counts.
func rejectFlagsWithGlobal(cmd *cobra.Command, allowed ...string) error {
	var others []string
	inherited := cmd.InheritedFlags()
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if f.Name != flagGlobal && !slices.Contains(allowed, f.Name) && inherited.Lookup(f.Name) == nil {
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

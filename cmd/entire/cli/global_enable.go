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

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// flagGlobal is `entire enable --global` / `entire disable --global`.
const flagGlobal = "global"

// runEnableGlobal turns Entire on in every repository: it sets
// `global_enable: true` in the user settings file and installs every in-use
// agent's user-level hooks now rather than on the next command. It does not
// set up the repository it runs in; the hooks do that the first time an agent
// runs there.
func runEnableGlobal(cmd *cobra.Command, yes bool) error {
	if err := rejectFlagsWithGlobal(cmd, "yes"); err != nil {
		return err
	}
	// Before the already-on short-circuit: an agent re-running it is answering
	// for the user just the same. Without a terminal --yes still works, for a
	// person's scripts.
	if caller := review.DetectAgentSession(); caller != "" {
		cmd.SilenceUsage = true
		return fmt.Errorf("refusing `entire enable --global` from %s: it records sessions and pushes checkpoints in every repository the user works in; ask the user to run it themselves", caller)
	}
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	// Before the disclosure: a file the write would refuse means nothing is
	// being agreed to.
	if err := userSettingsWritable(cmd); err != nil {
		return err
	}
	// Turning it on again adds nothing to agree to.
	alreadyOn := settings.UserTierEnablesEverywhere(ctx)
	if !alreadyOn {
		confirmed, err := confirmGlobalEnable(cmd, yes, globalEnableDisclosure)
		if err != nil || !confirmed {
			return err
		}
	}
	path, err := usersettings.SetGlobalEnable(ctx, true)
	if err != nil {
		return err //nolint:wrapcheck // usersettings names the file and the reason
	}
	if alreadyOn {
		fmt.Fprintf(w, "Entire is already enabled in every repository you work in (global_enable in %s).\n", path)
	} else {
		fmt.Fprintf(w, "Entire is now enabled in every repository you work in (global_enable in %s).\n", path)
	}

	installUserHooks(ctx, func(installer agent.UserHookInstaller, result userHookInstallResult) {
		reportUserHookInstall(cmd, installer, result)
	})
	return nil
}

// userHookInstallResult is one agent's EnsureUserHooks outcome.
type userHookInstallResult struct {
	agent.UserHookInstall

	err error
}

// installUserHooks installs every agent's user-level hooks while global
// enable stays on, handing each result to onResult.
func installUserHooks(ctx context.Context, onResult func(installer agent.UserHookInstaller, result userHookInstallResult)) {
	for _, installer := range userHookInstallers() {
		install, err := installer.EnsureUserHooks(ctx, settings.UserTierEnablesEverywhere)
		onResult(installer, userHookInstallResult{UserHookInstall: install, err: err})
	}
}

// userHookInstallers returns the registered agents whose Entire hooks can
// live in their user-level config, in registry order.
func userHookInstallers() []agent.UserHookInstaller {
	var installers []agent.UserHookInstaller
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		if installer, ok := agent.AsUserHookInstaller(ag); ok {
			installers = append(installers, installer)
		}
	}
	return installers
}

// reportUserHookInstall reports one agent's user-level hook install for `entire
// enable --global` and says what happened.
func reportUserHookInstall(cmd *cobra.Command, installer agent.UserHookInstaller, result userHookInstallResult) {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	name := string(installer.Type())
	switch {
	case errors.Is(result.err, agent.ErrUserHooksNotInUse):
		fmt.Fprintf(w, "%s is not set up here (%s not found); its hooks will be added the next time you run entire after it is.\n",
			name, agent.UserHooksMissingDir(result.err))
		return
	case result.err != nil:
		fmt.Fprintf(cmd.ErrOrStderr(), "Could not add Entire's hooks to %s: %v\nThey will be added the next time you run entire.\n", result.Path, result.err)
		return
	case result.Changed > 0 && result.Updated:
		fmt.Fprintf(w, "Updated Entire's hooks in %s's user settings: %s\n", name, result.Path)
	case result.Changed > 0:
		fmt.Fprintf(w, "Added Entire's hooks to %s's user settings: %s\n", name, result.Path)
	default:
		fmt.Fprintf(w, "%s's user settings already have Entire's hooks: %s\n", name, result.Path)
	}
	for _, note := range installer.InspectUserHooks(ctx).Notes {
		fmt.Fprintf(w, "  %s\n", note)
	}
}

// runEnableGlobalOr is the enable command's RunE: with --global it runs
// runEnableGlobal instead of run, so none of run's defers fire. They report
// the current repository as enabled, and --global does not enable it.
func runEnableGlobalOr(global, yes *bool, run func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if *global {
			return runEnableGlobal(cmd, *yes)
		}
		return run(cmd, args)
	}
}

// runDisableGlobal sets `global_enable: false` and then takes Entire's hooks
// out of every agent's user-level config. In that order: a command that
// read the flag as on re-checks it under the hooks file's lock before
// installing, so once the flag is off the removal cannot be undone.
// Repositories enabled with `entire enable` stay enabled, with their own
// project hooks.
func runDisableGlobal(cmd *cobra.Command) error {
	if err := rejectFlagsWithGlobal(cmd); err != nil {
		return err
	}
	ctx := cmd.Context()
	if err := userSettingsWritable(cmd); err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if settings.UserTierEnablesEverywhere(ctx) {
		path, err := usersettings.SetGlobalEnable(ctx, false)
		if err != nil {
			return err //nolint:wrapcheck // usersettings names the file and the reason
		}
		fmt.Fprintf(w, "Entire is no longer enabled in every repository (global_enable in %s).\n", path)
	} else {
		// Nothing to turn off, so no file to create; hooks an earlier
		// global enable left are still removed below.
		fmt.Fprintf(w, "Entire was not enabled in every repository (global_enable is not set in %s).\n", usersettings.Path())
	}
	for _, installer := range userHookInstallers() {
		removed, hooksPath, err := installer.RemoveUserHooks(ctx)
		switch {
		case err != nil:
			fmt.Fprintf(cmd.ErrOrStderr(), "Could not remove Entire's hooks from %s: %v\nRemove them by hand, or run `entire disable --global` again.\n", hooksPath, err)
		case removed:
			fmt.Fprintf(w, "Removed Entire's hooks from %s's user settings: %s\n", installer.Type(), hooksPath)
		}
	}
	fmt.Fprintln(w, "Repositories you enabled with `entire enable` stay enabled.")
	fmt.Fprintln(w, "Repositories set up only through global enable keep Entire's git hooks, which do nothing now; run `entire disable --uninstall` in one to remove them.")
	if settings.UserReposEnableAny(ctx) {
		fmt.Fprintf(w, "A repository enabled only by a repos entry in %s records agent sessions again once you run `entire enable` in it.\n", usersettings.Path())
	}
	return nil
}

// userSettingsWritable refuses a user settings file that cannot be read,
// which the global_enable write would refuse too.
func userSettingsWritable(cmd *cobra.Command) error {
	if err := settings.UserSettingsReadError(cmd.Context()); err != nil {
		cmd.SilenceUsage = true
		return err //nolint:wrapcheck // usersettings names the file and the reason
	}
	return nil
}

// globalEnableDisclosure is what `entire enable --global` asks the developer
// to agree to.
const globalEnableDisclosure = "With global enable on, Entire records your coding agent sessions (Claude Code, Codex, Pi and OpenCode) " +
	"in every git repository you work in, not just ones you ran `entire enable` in. " +
	"Its session checkpoints are pushed with your git pushes, as in any repository you enabled, and may later leave this machine without a commit as your Entire settings allow. " +
	"Run `entire disable` in a repository to stop it there, or `entire disable --global` to turn this off."

// confirmGlobalEnable asks the developer to agree to disclosure. --yes
// accepts, and still prints what was accepted; with no terminal to ask in it
// refuses, since the answer must be the developer's.
func confirmGlobalEnable(cmd *cobra.Command, yes bool, disclosure string) (bool, error) {
	if yes {
		fmt.Fprintf(cmd.ErrOrStderr(), "Accepted with --yes: %s\n", disclosure)
		return true, nil
	}
	if !interactive.CanPromptInteractively() {
		cmd.SilenceUsage = true
		return false, errors.New("refusing to enable Entire in every repository without confirmation; run it in a terminal or pass --yes")
	}
	return confirmPrompt(cmd, "Global enable", "Enable Entire in every repository you work in?", "",
		func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "%s\n\n", disclosure)
			return err //nolint:wrapcheck // a write to the prompt's own terminal
		})
}

// claimRepoFromGlobalEnable records, after a successful `entire enable`, that
// the developer enabled this repository themselves when it was on only through
// global enable (see enableInUserSettings). Best effort: enable already
// succeeded, so a failure here is a warning.
func claimRepoFromGlobalEnable(ctx context.Context, w, errW io.Writer) {
	if _, err := enableInUserSettings(ctx, w, false); err != nil {
		fmt.Fprintf(errW, "Warning: Entire stays enabled here only through global enable: %v\n", err)
	}
}

// enableInUserSettings is `entire enable` in a repository while global enable
// is on: when the repository is on only through global enable, or (with
// includeDisabled) off, it writes `enabled: true` to the repository's `repos`
// entry in the user settings file. That claims every clone and worktree, lets
// the committed settings global enable set aside apply, and never edits the
// team's committed file. It reports whether it handled the enable. An agent
// cannot enable a repository for the developer; for a repository that is off,
// it is told so and nothing changes.
func enableInUserSettings(ctx context.Context, w io.Writer, includeDisabled bool) (bool, error) {
	if !settings.UserTierEnablesEverywhere(ctx) {
		return false, nil
	}
	s, err := LoadEntireSettings(ctx)
	if err != nil {
		return false, nil //nolint:nilerr // the regular enable path reports a settings error
	}
	onlyByGlobal := s.EnabledOnlyByGlobal()
	// A local file that turned this worktree off outranks a repos entry; the
	// regular path rewrites it.
	off := includeDisabled && !s.Enabled && !scopeExplicitlyDisabled(ctx, false)
	if !onlyByGlobal && !off {
		return false, nil
	}
	if caller := review.DetectAgentSession(); caller != "" {
		if onlyByGlobal && includeDisabled {
			return false, nil // the regular path runs, and the claim after it says so
		}
		state := "enabled here only through global enable"
		if off {
			state = "disabled here"
		}
		fmt.Fprintf(w, "Entire stays %s: %s cannot enable a repository for the user; ask the user to run `entire enable` here themselves.\n",
			state, caller)
		return true, nil
	}
	key, err := setRepoEnabledHere(ctx, true)
	if err != nil {
		return false, err
	}
	fmt.Fprintf(w, "Entire is now enabled in %s (repos in %s).\n", describeRepoKey(key), usersettings.Path())
	return true, nil
}

// setRepoEnabledHere writes `enabled` to this repository's `repos` entry in
// the user settings file and returns the entry's key.
func setRepoEnabledHere(ctx context.Context, enabled bool) (string, error) {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("resolving the repository: %w", err)
	}
	key, err := settings.SetRepoEnabled(ctx, root, enabled)
	if err != nil {
		return "", err //nolint:wrapcheck // settings names the file and the reason
	}
	return key, nil
}

// describeRepoKey names what a `repos` key covers: every clone of an origin,
// or every worktree of the clone at a path.
func describeRepoKey(key string) string {
	if filepath.IsAbs(filepath.FromSlash(key)) {
		return "every worktree of " + tuiutil.SanitizeDisplayText(key)
	}
	return "every clone of " + tuiutil.SanitizeDisplayText(key)
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
// were never enabled. Everything else already follows the flag once Entire is
// called (settings resolve as enabled, and the first hook installs the git
// hooks lazily), but an agent only calls Entire if its hooks are installed,
// and a never-enabled repository has no project hooks. So whenever Entire runs
// with the flag on, it makes sure its hooks are in every in-use agent's
// user-level config (Claude Code, Codex, Pi, OpenCode). Best effort: this
// never fails a command.
func ensureGlobalEnableHooks(cmd *cobra.Command) {
	ctx := cmd.Context()
	if skipsGlobalEnableHooks(cmd) || !settings.UserTierEnablesEverywhere(ctx) {
		return
	}
	installUserHooks(ctx, func(installer agent.UserHookInstaller, result userHookInstallResult) {
		switch {
		case errors.Is(result.err, agent.ErrUserHooksNotInUse):
		case result.err != nil:
			logging.Debug(ctx, "global enable: could not install user-level hooks",
				slog.String("agent", string(installer.Name())), slog.String("error", result.err.Error()))
		case result.Changed > 0 && result.Updated:
			fmt.Fprintf(cmd.ErrOrStderr(), "Entire is enabled for all your repositories; updated its %s hooks in %s\n", installer.Type(), result.Path)
		case result.Changed > 0:
			fmt.Fprintf(cmd.ErrOrStderr(), "Entire is enabled for all your repositories; added its %s hooks to %s\n", installer.Type(), result.Path)
		}
	})
}

// skipGlobalEnableHooksAnnotation marks a command, and everything under it,
// that must not (re)install the user-level hooks: `entire enable`, which
// installs hooks itself, and `entire disable`, which is removing Entire rather
// than using it.
const (
	skipGlobalEnableHooksAnnotation = "entire_skip_global_enable_hooks"
	skipGlobalEnableHooksEnabled    = "true"
)

// skipsGlobalEnableHooks reports commands that must not (re)install the
// user-level hooks: hidden commands, including the agent and git hook
// invocations that run on every turn, and annotated ones.
func skipsGlobalEnableHooks(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Hidden || c.Annotations[skipGlobalEnableHooksAnnotation] == skipGlobalEnableHooksEnabled {
			return true
		}
	}
	return false
}

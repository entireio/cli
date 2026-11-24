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

// flagGlobal is `entire enable --global` / `entire disable --global`, and
// flagOwner is `entire enable --global --owner`.
const (
	flagGlobal = "global"
	flagOwner  = "owner"
)

// runEnableGlobal turns Entire on in every repository: it sets
// `global_enable: true` in the user settings file, adds any --owner to the
// owner list, and installs every in-use agent's user-level hooks now rather
// than on the next command. It does not set up the repository it runs in; the hooks
// do that the first time an agent runs there.
func runEnableGlobal(cmd *cobra.Command, yes bool, ownerFlags []string) error {
	if err := rejectFlagsWithGlobal(cmd, "yes", flagOwner); err != nil {
		return err
	}
	// Before the already-on short-circuit: an agent re-running it, or adding
	// an owner, is answering for the user just the same. Without a terminal
	// --yes still works, for a person's scripts.
	if caller := review.DetectAgentSession(); caller != "" {
		cmd.SilenceUsage = true
		return fmt.Errorf("refusing `entire enable --global` from %s: it records sessions and pushes checkpoints in every repository the user works in; ask the user to run it themselves", caller)
	}
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	owners, err := validateOwners(cmd, ownerFlags)
	if err != nil {
		return err
	}
	listed := settings.UserGlobalEnableOwners(ctx)
	var newOwners []string
	for _, owner := range owners {
		if !slices.Contains(listed, owner) && !slices.Contains(newOwners, owner) {
			newOwners = append(newOwners, owner)
		}
	}
	// Turning it on again, with no owner it did not already have, adds
	// nothing to agree to.
	if !settings.UserTierEnablesEverywhere(ctx) || len(newOwners) > 0 {
		confirmed, err := confirmGlobalEnable(cmd, yes, globalEnableDisclosureFor(newOwners))
		if err != nil || !confirmed {
			return err
		}
	}
	path, err := usersettings.SetGlobalEnable(ctx, true)
	if err != nil {
		return err //nolint:wrapcheck // usersettings names the file and the reason
	}
	fmt.Fprintf(w, "Entire is now enabled in every repository you work in (global_enable in %s).\n", path)
	if len(newOwners) > 0 {
		if listed, err = usersettings.AddGlobalEnableOwners(ctx, newOwners); err != nil {
			return err //nolint:wrapcheck // usersettings names the file and the reason
		}
	}
	reportGlobalEnableOwners(w, listed)

	for _, installer := range userHookInstallers() {
		reportUserHookInstall(cmd, installer)
	}
	return nil
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

// reportUserHookInstall installs one agent's user-level hooks for `entire
// enable --global` and says what happened.
func reportUserHookInstall(cmd *cobra.Command, installer agent.UserHookInstaller) {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	name := string(installer.Type())
	n, hooksPath, err := installer.EnsureUserHooks(ctx, settings.UserTierEnablesEverywhere)
	switch {
	case errors.Is(err, agent.ErrUserHooksNotInUse):
		fmt.Fprintf(w, "%s is not set up here (%s not found); its hooks will be added the next time you run entire after it is.\n",
			name, filepath.Dir(hooksPath))
		return
	case err != nil:
		fmt.Fprintf(cmd.ErrOrStderr(), "Could not add Entire's hooks to %s: %v\nThey will be added the next time you run entire.\n", hooksPath, err)
		return
	case n > 0:
		fmt.Fprintf(w, "Added Entire's hooks to %s's user settings: %s\n", name, hooksPath)
	default:
		fmt.Fprintf(w, "%s's user settings already have Entire's hooks: %s\n", name, hooksPath)
	}
	for _, note := range installer.InspectUserHooks(ctx).Notes {
		fmt.Fprintf(w, "  %s\n", note)
	}
}

// validateOwners normalizes every --owner, refusing the command on the first
// that is not one.
func validateOwners(cmd *cobra.Command, ownerFlags []string) ([]string, error) {
	owners := make([]string, 0, len(ownerFlags))
	for _, flag := range ownerFlags {
		owner, err := usersettings.ValidateOwner(flag)
		if err != nil {
			cmd.SilenceUsage = true
			return nil, err //nolint:wrapcheck // names the flag value and the expected shape
		}
		owners = append(owners, owner)
	}
	return owners, nil
}

// reportGlobalEnableOwners says which repositories push checkpoints without
// asking, and how every other one gets to push.
func reportGlobalEnableOwners(w io.Writer, owners []string) {
	if len(owners) == 0 {
		fmt.Fprintln(w, "Checkpoints from a repository you have not enabled are pushed only after you agree, on a git push from a terminal. Add owners whose repositories should push without asking with --owner.")
		return
	}
	fmt.Fprintf(w, "Checkpoints are pushed without asking for repositories owned by: %s. Remove an owner by editing %s in %s.\n",
		strings.Join(owners, ", "), usersettings.GlobalEnableOwnersKey, usersettings.Path())
}

// runEnableGlobalOr is the enable command's RunE: with --global it runs
// runEnableGlobal instead of run, so none of run's defers fire. They report
// the current repository as enabled, and --global does not enable it.
func runEnableGlobalOr(global, yes *bool, owners *[]string, run func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if *global {
			return runEnableGlobal(cmd, *yes, *owners)
		}
		if len(*owners) > 0 {
			cmd.SilenceUsage = true
			return errors.New("--owner applies only with --global")
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
	path, err := usersettings.SetGlobalEnable(ctx, false)
	if err != nil {
		return err //nolint:wrapcheck // usersettings names the file and the reason
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Entire is no longer enabled in every repository (global_enable in %s).\n", path)
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
	if settings.UserReposEnableAny(ctx) {
		fmt.Fprintf(w, "A repository enabled only by a repos entry in %s records agent sessions again once you run `entire enable` in it.\n", path)
	}
	return nil
}

// globalEnableDisclosure is what `entire enable --global` asks the developer
// to agree to.
const globalEnableDisclosure = "With global enable on, Entire records your coding agent sessions (Claude Code, Codex, Pi and OpenCode) " +
	"in every git repository you work in, not just ones you ran `entire enable` in. " +
	"Recorded sessions and checkpoints may leave this machine as your Entire settings allow, today with your git pushes and later possibly without a commit. " +
	"In a repository you did not enable, checkpoints are not pushed to its remote until you agree, on a git push from a terminal. " +
	"Run `entire disable` in a repository to stop it there, or `entire disable --global` to turn this off."

// globalEnableDisclosureFor is the disclosure, plus what adding owners to the
// owner list means.
func globalEnableDisclosureFor(newOwners []string) string {
	if len(newOwners) == 0 {
		return globalEnableDisclosure
	}
	return globalEnableDisclosure + " Session checkpoints from repositories owned by " + strings.Join(newOwners, ", ") +
		" are pushed with your git pushes without asking, including public repositories and forks they own."
}

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
// global enable: once global enable is not the only reason, the committed
// settings it set aside apply and the global-enable notices stop. It writes
// `enabled` to the repository's `repos` entry in the user settings file, so
// the claim covers every clone and worktree of it. An agent cannot make that
// claim for the developer. Best effort: enable already succeeded, so a
// failure here is a warning.
func claimRepoFromGlobalEnable(ctx context.Context, w, errW io.Writer) {
	if !settings.UserTierEnablesEverywhere(ctx) {
		return
	}
	s, err := LoadEntireSettings(ctx)
	if err != nil || !s.EnabledOnlyByGlobal() {
		return
	}
	if caller := review.DetectAgentSession(); caller != "" {
		fmt.Fprintf(w, "Entire stays enabled here only through global enable: %s cannot enable a repository for the user; ask the user to run `entire enable` here themselves.\n",
			caller)
		return
	}
	key, err := setRepoEnabledHere(ctx, true)
	if err != nil {
		fmt.Fprintf(errW, "Warning: Entire stays enabled here only through global enable: %v\n", err)
		return
	}
	fmt.Fprintf(w, "Entire is now enabled in %s itself, not only through global enable (repos in %s).\n",
		describeRepoKey(key), usersettings.Path())
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
// hooks lazily), but Claude only calls Entire if its hooks are installed, and a
// never-enabled repository has no project hooks. So whenever Entire runs with
// the flag on, it makes sure its hooks are in the user-level Claude settings.
// Best effort: this never fails a command.
func ensureGlobalEnableHooks(cmd *cobra.Command) {
	ctx := cmd.Context()
	if skipsGlobalEnableHooks(cmd) || !settings.UserTierEnablesEverywhere(ctx) {
		return
	}
	for _, installer := range userHookInstallers() {
		n, path, err := installer.EnsureUserHooks(ctx, settings.UserTierEnablesEverywhere)
		if errors.Is(err, agent.ErrUserHooksNotInUse) {
			continue
		}
		if err != nil {
			logging.Debug(ctx, "global enable: could not install user-level hooks",
				slog.String("agent", string(installer.Name())), slog.String("error", err.Error()))
			continue
		}
		if n > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "Entire is enabled for all your repositories; added its %s hooks to %s\n", installer.Type(), path)
		}
	}
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

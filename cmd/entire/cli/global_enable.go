package cli

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// ensureGlobalEnableHooks makes the user-tier `preferences.enabled` flag
// effective in repositories that were never enabled. Everything else already
// follows the flag once Entire is called (settings resolve as enabled, and the
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

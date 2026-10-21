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
	if !settings.UserTierEnablesEverywhere(ctx) {
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

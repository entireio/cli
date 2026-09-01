package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/spf13/cobra"
)

func ShouldCheckCheckpointPolicyWarning(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	for c := cmd; c != nil; c = c.Parent() {
		if isCheckpointPolicyWarningExcludedCommand(c.Name()) {
			return false
		}
	}
	return true
}

func isCheckpointPolicyWarningExcludedCommand(name string) bool {
	switch name {
	case "hooks", "__send_analytics", "__refresh_trail_enablement", "curl-bash-post-install", "__sweep_sessions":
		return true
	default:
		return false
	}
}

func WarnCheckpointPolicyIfNeeded(ctx context.Context, w io.Writer, currentVersion string) {
	// OpenCurrentOrCwd, not OpenCurrent: this runs from main.go after cobra has
	// finished, so it is the one repository open with no pre-run guard ahead of
	// it, and it fires on every command — including outside a repository, where
	// resolving a worktree root legitimately fails. It reads a policy ref to
	// decide whether to print one advisory line, and discards every error, so
	// the cwd fallback costs at most a skipped warning. Nothing that writes may
	// use it.
	repo, err := gitrepo.OpenCurrentOrCwd(ctx)
	if err != nil {
		return
	}
	defer repo.Close()

	state, err := checkpointpolicy.ReadLocal(ctx, repo)
	if err != nil {
		return
	}
	if checkpointpolicy.CanSatisfyPolicy(state.Policy) {
		return
	}

	fmt.Fprint(w, checkpointpolicy.UnsupportedPolicyMessage(
		state.Policy,
		versioncheck.UpdateCommandForCurrentBinary(currentVersion),
	))
}

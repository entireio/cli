package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// resolveProjectTrailCreateBranch also returns the branch checked out now
// (before publishing; "" when detached or with --repo), so create can tell
// whether the linked branch differs from the user's checkout.
func resolveProjectTrailCreateBranch(cmd *cobra.Command, title, branch, base string) (resolved, resolvedBase, current string, err error) {
	if trailRepoFlag(cmd) != "" {
		if err := ensureTrailRepoHasTarget(cmd, strings.TrimSpace(branch) != "", "pass --branch"); err != nil {
			return "", "", "", err
		}
		return branch, base, "", ValidateBranchName(cmd.Context(), branch)
	}
	repo, err := strategy.OpenRepository(cmd.Context())
	if err != nil {
		return "", "", "", fmt.Errorf("open local repository (or use --no-branch for intent only): %w", err)
	}
	defer repo.Close()
	base, namingBase := resolveTrailCreateBase(cmd.Context(), repo, base)
	current = strategy.GetCurrentBranchName(repo)
	branch = resolveCreateBranch(branch, current, namingBase, title, true)
	return branch, base, current, ValidateBranchName(cmd.Context(), branch)
}

// Publish the local tip before asking the server to link it. A new feature
// commit is not required, and uncommitted changes are never committed for the
// user. All push hooks run. Neither push nor API failures retract local/remote
// refs: a failed response may still represent a successful server operation.
func publishProjectTrailBranch(cmd *cobra.Command, branch string) error {
	ctx := cmd.Context()
	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("open repository for trail branch publication: %w", err)
	}
	defer repo.Close()
	remote, err := resolveTrailPushRemote(ctx, branch)
	if err != nil {
		return err
	}
	if branchNeedsCreation(repo, branch) {
		exists, err := remoteHasBranch(ctx, remote, branch)
		if err != nil {
			return fmt.Errorf("check branch %q on %q: %w", branch, remote, err)
		}
		if err := ensureTrailCreateBranchExists(ctx, cmd.ErrOrStderr(), repo, remote, branch, exists); err != nil {
			return err
		}
	}
	// Keep progress and hook output off JSON stdout. Do not bypass hooks or
	// force-push; a rejection must stop before the trail-creation POST.
	if err := pushBranchToRemote(ctx, cmd.ErrOrStderr(), cmd.ErrOrStderr(), remote, branch); err != nil {
		return fmt.Errorf("publish branch %q to %q: %w; trail creation was not sent (see push/hook output above)", branch, remote, err)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Published branch %s to %s\n", tuiutil.SanitizeTerminalText(branch), tuiutil.SanitizeTerminalText(remote))
	return nil
}

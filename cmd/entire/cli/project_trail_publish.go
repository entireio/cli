package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func resolveProjectTrailCreateBranch(cmd *cobra.Command, title, branch, base string) (string, string, error) {
	if trailRepoFlag(cmd) != "" {
		if err := requireTrailWorkingTarget(trailRepoFlag(cmd), "", branch); err != nil {
			return "", "", err
		}
		return branch, base, ValidateBranchName(cmd.Context(), branch)
	}
	repo, err := strategy.OpenRepository(cmd.Context())
	if err != nil {
		return "", "", fmt.Errorf("open local repository (or use --no-branch for intent only): %w", err)
	}
	defer repo.Close()
	base = resolveTrailCreateBase(repo, base)
	current := strategy.GetCurrentBranchName(repo)
	branch = resolveCreateBranch(branch, current, base, title, true)
	return branch, base, ValidateBranchName(cmd.Context(), branch)
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
	state := trailCreateBranchState{NeedsCreation: branchNeedsCreation(repo, branch)}
	if state.NeedsCreation {
		exists, err := remoteHasBranch(ctx, remote, branch)
		if err != nil {
			return fmt.Errorf("check branch %q on %q: %w", branch, remote, err)
		}
		if err := ensureTrailCreateBranchExists(ctx, cmd.ErrOrStderr(), repo, remote, branch, strategy.GetCurrentBranchName(repo), exists, &state); err != nil {
			return err
		}
	}
	// Keep progress and hook output off JSON stdout. Do not bypass hooks or
	// force-push; a rejection must stop before the trail-creation POST.
	if err := pushBranchToRemote(ctx, cmd.ErrOrStderr(), cmd.ErrOrStderr(), remote, branch); err != nil {
		return fmt.Errorf("publish branch %q to %q: %w; trail creation was not sent (see push/hook output above)", branch, remote, err)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Published branch %s to %s\n", branch, remote)
	return nil
}

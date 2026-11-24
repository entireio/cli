package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

func newRepoDefaultBranchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "default-branch <repo> <branch>",
		Short: "Set a repository's default branch",
		Long: "Set a repository's default branch (HEAD).\n\n" +
			"<branch> is a branch name such as main, or a full refs/heads/ ref. " +
			"The branch must already exist. A GitHub mirror follows GitHub's default branch. " +
			"Requires manage permission on the repo.",
		Example: "  entire repo default-branch /et/my-project/my-repo master",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			branch := args[1]
			if strings.TrimSpace(branch) == "" {
				cmd.SilenceUsage = true
				return errors.New("branch must not be empty")
			}
			return runCoreMutation(cmd, func(ctx context.Context, c *coreapi.Client) (string, any, error) {
				resolved, err := resolveRepoPathResolved(ctx, c, args[0])
				if err != nil {
					return "", nil, err
				}
				body := &coreapi.SetDefaultBranchInputBody{Branch: branch}
				out, err := c.SetDefaultBranch(ctx, body, coreapi.SetDefaultBranchParams{RepoId: resolved.ID})
				if err != nil {
					return "", nil, err
				}
				return defaultBranchMessage(out), out, nil
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func defaultBranchMessage(out *coreapi.SetDefaultBranchOutputBody) string {
	if !out.Changed {
		return "Default branch already " + out.Branch
	}
	return fmt.Sprintf("✓ Default branch: %s (was %s)", out.Branch, out.Previous)
}

package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/spf13/cobra"
)

func newTrailListCmd() *cobra.Command {
	var opts trailListOptions
	cmd := &cobra.Command{
		Use: cmdList, Short: "List recent trails", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.InsecureHTTP = trailInsecureHTTP(cmd)
			opts.Repo = trailRepoFlag(cmd)
			return runTrailListAll(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.Author, "author", "", "Filter by author login (case-insensitive); use '"+trailListAuthorMe+"' for yourself (requires gh CLI); omit for any author")
	cmd.Flags().StringVar(&opts.Status, "status", defaultTrailListStatus, "Filter by comma-separated status(es): "+formatValidStatuses()+"; use '"+trailListStatusAny+"' for all statuses")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output as JSON (respects --author, --status, and --limit)")
	cmd.Flags().IntVarP(&opts.Limit, "limit", "n", defaultTrailListLimit, "Maximum number of trails to show")
	return cmd
}

// resolveLegacyTrailContext is legacyTrailMode.workingContext: selectors are
// repository-local numbers, IDs, or branches.
func resolveLegacyTrailContext(cmd *cobra.Command, selector, branch string, localOnly bool) (*trailWorkingContext, error) {
	if selector != "" && strings.TrimSpace(branch) != "" && !localOnly {
		return nil, errors.New("pass a trail selector or --branch, not both")
	}
	repoOverride := trailRepoFlag(cmd)
	if localOnly {
		repoOverride = ""
	}
	if err := ensureTrailRepoHasTarget(cmd, localOnly || selector != "" || strings.TrimSpace(branch) != "", "pass a trail selector or --branch"); err != nil {
		return nil, err
	}
	var selected *trailWorkingContext
	err := runAuthenticatedTrailAPI(cmd.Context(), cmd.ErrOrStderr(), trailInsecureHTTP(cmd), repoOverride, func(ctx context.Context, client *api.Client, repoID string) error {
		forge, owner, repo, err := resolveTrailRepoOrRemote(ctx, repoOverride)
		if err != nil {
			return err
		}
		base, err := trailRepoBasePath(forge, owner, repo, repoID)
		if err != nil {
			return err
		}
		// Number-keyed subresources (approvals) need a numbered trail; the
		// local checkout/resume paths never did.
		resolve := resolveNumberedTrailAtPath
		if localOnly {
			resolve = resolveTrailBySelectorAtPath
		}
		found, err := resolve(ctx, client, base, forge, owner, repo, selector, branch)
		if err != nil {
			return err
		}
		found.Parent = nil
		client.SetTrailRoute(found.ID, trailNumberPathForBase(base, found.Number))
		selected = &trailWorkingContext{Client: client, BasePath: base, Host: forge, Owner: owner, Repo: repo, Work: *found}
		return nil
	})
	return selected, err
}

// authenticatedLegacyTrailReviewTarget is legacyTrailMode.reviewTarget.
// Legacy selectors cannot name another repository, and finding apply already
// rejects --repo, so localOnly needs no extra check here.
func authenticatedLegacyTrailReviewTarget(cmd *cobra.Command, selector string, _ bool) (*api.Client, trailReviewTarget, error) {
	repo, branch := trailRepoFlag(cmd), trailBranchFlag(cmd)
	if selector != "" && branch != "" {
		return nil, trailReviewTarget{}, errors.New("pass a trail selector or --branch, not both")
	}
	if err := ensureTrailRepoHasTarget(cmd, selector != "" || branch != "", "pass a trail selector or --branch"); err != nil {
		return nil, trailReviewTarget{}, err
	}
	var client *api.Client
	var target trailReviewTarget
	err := runAuthenticatedTrailAPI(cmd.Context(), cmd.ErrOrStderr(), trailInsecureHTTP(cmd), repo, func(ctx context.Context, c *api.Client, repoID string) error {
		var err error
		client = c
		target, err = resolveTrailReviewTarget(ctx, c, repoID, selector, repo, branch)
		target.Trail.Parent = nil
		return err
	})
	return client, target, err
}

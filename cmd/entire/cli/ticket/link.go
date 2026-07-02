package ticket

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/gitexec"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// newLinkCmd builds `entire ticket link`.
func newLinkCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "link <ticket-id>",
		Short: "Link a ticket to the current branch",
		Long: `Link a ticket to the current git branch so agents and reviews carry it as
grounding context. Run it from the branch you are working on.

The link is stored per branch (in the git common dir) and persists across
sessions, so you can link a ticket before, during, or after doing the work —
Entire captures the session either way.

Examples:
  entire ticket link ENG-142`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) == 1 {
				id = args[0]
			}
			return runLink(cmd.Context(), cmd.OutOrStdout(), id, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing link on this branch")
	return cmd
}

// newUnlinkCmd builds `entire ticket unlink`.
func newUnlinkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlink",
		Short: "Remove the ticket link from the current branch",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUnlink(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

func runLink(ctx context.Context, out io.Writer, id string, force bool) error {
	link, branch, err := linkCurrentBranch(ctx, id, force)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ Linked %s to branch %q.\n", link.ID, branch)
	return nil
}

// linkCurrentBranch validates configuration, resolves the current branch, and
// records the ticket link (honoring force). Shared by `ticket link` and
// `ticket start`. Returns the stored link and the branch name.
func linkCurrentBranch(ctx context.Context, id string, force bool) (Link, string, error) {
	s, err := settings.Load(ctx)
	if err != nil {
		return Link{}, "", fmt.Errorf("load settings: %w", err)
	}
	tc := s.TicketConfig()
	if tc.IsZero() {
		return Link{}, "", errors.New("no ticket platform configured — run `entire ticket setup` first")
	}

	branch, err := currentBranch(ctx)
	if err != nil {
		return Link{}, "", err
	}

	id = strings.TrimSpace(id)
	if id == "" {
		// Infer the ticket from the branch name via the active provider.
		prov, perr := activeProvider(ctx)
		if perr != nil {
			return Link{}, "", fmt.Errorf("specify a ticket ID (auto-detect unavailable: %w)", perr)
		}
		detected, ok := prov.ResolveFromBranch(branch)
		if !ok {
			return Link{}, "", fmt.Errorf("could not infer a ticket ID from branch %q; specify one, e.g. `entire ticket link ENG-142`", branch)
		}
		id = detected
	}

	link := Link{Platform: tc.Platform, ID: id}
	if err := storeLink(ctx, branch, link, force); err != nil {
		return Link{}, "", err
	}
	return link, branch, nil
}

// storeLink records link on branch, honoring force when a different ticket is
// already linked. Shared by `ticket link` and `ticket start`.
func storeLink(ctx context.Context, branch string, link Link, force bool) error {
	store, err := newLinkStore(ctx)
	if err != nil {
		return err
	}
	existing, ok, err := store.get(branch)
	if err != nil {
		return err
	}
	if ok && !force && (existing.ID != link.ID || existing.Platform != link.Platform) {
		return fmt.Errorf("branch %q is already linked to %s (%s); pass --force to overwrite", branch, existing.ID, existing.Platform)
	}
	logging.Debug(ctx, "ticket linked",
		slog.String("platform", link.Platform),
		slog.String("ticket", link.ID),
		slog.String("branch", branch))
	return store.set(branch, link)
}

func runUnlink(ctx context.Context, out io.Writer) error {
	branch, err := currentBranch(ctx)
	if err != nil {
		return err
	}
	store, err := newLinkStore(ctx)
	if err != nil {
		return err
	}
	removed, err := store.del(branch)
	if err != nil {
		return err
	}
	if !removed {
		fmt.Fprintf(out, "Branch %q has no ticket link.\n", branch)
		return nil
	}
	fmt.Fprintf(out, "✓ Unlinked ticket from branch %q.\n", branch)
	return nil
}

// CurrentLink returns the ticket linked to the current branch, if any. It is
// used by `ticket start` and, later, the review integration.
func CurrentLink(ctx context.Context) (Link, bool, error) {
	branch, err := currentBranch(ctx)
	if err != nil {
		return Link{}, false, err
	}
	store, err := newLinkStore(ctx)
	if err != nil {
		return Link{}, false, err
	}
	return store.get(branch)
}

// currentBranch resolves the current git branch name, erroring on a detached
// HEAD.
func currentBranch(ctx context.Context) (string, error) {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve repo root: %w", err)
	}
	out, err := gitexec.Run(ctx, root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve current branch: %w", err)
	}
	branch := strings.TrimSpace(out)
	if branch == "" || branch == "HEAD" {
		return "", errors.New("not on a branch (detached HEAD)")
	}
	return branch, nil
}

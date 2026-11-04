package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// guardProjectTrailNumberUpdate refuses `trail update <N>` when the checked-out
// branch's work belongs to a different project trail than #N.
//
// Project trail numbers, change numbers, and pull request numbers look alike,
// so `trail update 1744` meant for change cli/1744 or PR #1744 reaches
// unrelated project trail #1744 and replaces fields its owner wrote. The
// guard costs one lookup and runs only for a bare number from a clone of a
// repository in the trail's project, without --branch or --repo; a ULID names
// the trail unambiguously and is the way to update #N anyway. A branch with
// no trail, or one whose work is on #N, passes. A failed lookup warns and
// passes: the guard must not block updates it cannot judge.
func guardProjectTrailNumberUpdate(cmd *cobra.Command, selector string, target *projectTrailTarget, current api.ProjectTrail) error {
	number, ok := parseTrailNumberSelector(selector)
	if !ok || trailRepoFlag(cmd) != "" || projectTrailBranchFlag(cmd) != "" {
		return nil
	}
	ctx := cmd.Context()
	// Outside a clone of a repository in the trail's project, or on a detached
	// HEAD, there is no branch work to confuse with the trail.
	forge, owner, repo, err := resolveTrailRepoOrRemote(ctx, "")
	if err != nil || !strings.EqualFold(forge, target.Host) || !strings.EqualFold(owner, target.Project) {
		return nil //nolint:nilerr // no local work to compare against; the update proceeds as before
	}
	branch, err := GetCurrentBranch(ctx)
	if err != nil || branch == "" {
		return nil //nolint:nilerr // detached HEAD: no branch work to compare against
	}
	work, err := lookupBranchTrailWork(cmd, forge, owner, repo, branch)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not check which trail branch %q belongs to: %v\n", tuiutil.SanitizeTerminalLabel(branch), err)
		return nil
	}
	if work == nil || (work.Parent != nil && work.Parent.ID == current.ID) {
		return nil
	}
	return errors.New(projectTrailNumberMismatch(number, current, target, branch, repo, work))
}

// readProjectTrailForUpdate reads the trail and its ETag, then applies the
// bare-number guard before any write.
func readProjectTrailForUpdate(cmd *cobra.Command, selector string, target *projectTrailTarget) (api.ProjectTrail, string, error) {
	current, etag, err := target.read(cmd.Context())
	if err != nil {
		return api.ProjectTrail{}, "", err
	}
	if etag == "" {
		return api.ProjectTrail{}, "", errors.New("project trail read returned no ETag; refusing an unprotected update")
	}
	if err := guardProjectTrailNumberUpdate(cmd, selector, target, current); err != nil {
		return api.ProjectTrail{}, "", err
	}
	return current, etag, nil
}

func lookupBranchTrailWork(cmd *cobra.Command, forge, owner, repo, branch string) (*api.TrailResource, error) {
	ctx := cmd.Context()
	client, repoID, err := newTrailAPIClient(ctx, trailInsecureHTTP(cmd), forge, owner, repo)
	if err != nil {
		return nil, err
	}
	base, err := trailRepoBasePath(forge, owner, repo, repoID)
	if err != nil {
		return nil, err
	}
	return findTrailByBranchAtPath(ctx, client, base, branch)
}

// projectTrailNumberMismatch explains the refusal and names both safe
// commands. When N is the branch's own change number, the likely intent was
// the change, so it says so.
func projectTrailNumberMismatch(number int, current api.ProjectTrail, target *projectTrailTarget, branch, repo string, work *api.TrailResource) string {
	own := "a repository trail with no project trail"
	if work.Parent != nil && work.Parent.Number > 0 {
		own = fmt.Sprintf("trail #%d", work.Parent.Number)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to update trail #%d %q: branch %q is work on %s, not on #%d",
		number, tuiutil.SanitizeTerminalLabel(current.Title), tuiutil.SanitizeTerminalLabel(branch), own, number)
	if work.Number == number {
		fmt.Fprintf(&b, "; %d is this branch's change number (%s/%d), and a bare number names a project trail",
			number, tuiutil.SanitizeTerminalLabel(repo), number)
	}
	b.WriteString(".\nTo update this branch's trail: entire trail update (no selector).")
	fmt.Fprintf(&b, "\nTo update #%d anyway: entire trail update %s --project %s/%s",
		number, current.ID, tuiutil.SanitizeTerminalLabel(target.Host), tuiutil.SanitizeTerminalLabel(target.Project))
	return b.String()
}

func projectTrailBranchFlag(cmd *cobra.Command) string {
	flag := cmd.Flags().Lookup("branch")
	if flag == nil {
		return ""
	}
	return strings.TrimSpace(flag.Value.String())
}

package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// projectTrailUpdatePath adds the checked-out branch to `trail update <N>`
// so the trail's cell can refuse it, writing nothing, when that branch's work
// belongs to another trail. Project trail, change, and pull request numbers
// look alike, so `trail update 1744` meant for change cli/1744 would otherwise
// replace fields of unrelated trail #1744. The server's refusal explains what
// happened; the CLI shows it sanitized, followed by its own reruns.
//
// Only a bare number without --branch, from a clone of a repository in the
// trail's project (with --repo, only when it names that clone's repository),
// carries the branch. A detached HEAD has no branch to send, so it refuses
// locally. A repository not onboarded to Entire has no trail work.
func projectTrailUpdatePath(cmd *cobra.Command, selector string, target *projectTrailTarget, current api.ProjectTrail) (string, error) {
	path := target.path()
	number, ok := parseTrailNumberSelector(selector)
	if !ok || trailBranchFlag(cmd) != "" {
		return path, nil
	}
	ctx := cmd.Context()
	forge, owner, repo, local, err := guardedLocalRepo(ctx, cmd, target)
	if err != nil || !local {
		return path, err
	}
	branch, err := GetCurrentBranch(ctx)
	if err != nil || branch == "" {
		reason := "no branch is checked out"
		if err != nil {
			reason = singleLineTerminalText(err.Error())
		}
		return "", fmt.Errorf("refusing to update trail #%d %q: the checked-out branch is unknown (%s), so the server cannot check that this work belongs to #%d; rerun with --branch <branch> added, or with %s --project %s/%s in place of %d",
			number, tuiutil.SanitizeTerminalLabel(current.Title), reason, number, tuiutil.SanitizeTerminalLabel(current.ID),
			tuiutil.SanitizeTerminalLabel(target.Host), tuiutil.SanitizeTerminalLabel(target.Project), number)
	}
	placement, err := resolveTrailUpdatePlacement(ctx, forge, owner, repo)
	if errors.Is(err, errRepoNotOnboarded) {
		return path, nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve %s for the trail's branch check: %w", repo, err)
	}
	query := url.Values{"from_repo_id": {placement.RepoID}, "from_branch": {branch}, "branch_check": {"required"}}
	return path + "?" + query.Encode(), nil
}

// trailBranchRefusal shows the server's branch-check refusal on its own,
// sanitized to one line (it echoes the branch, and the server's text is not
// trusted for the terminal), then the CLI's reruns. Every rerun changes only
// the selector, so the user's other flags carry over. Other errors pass
// through unchanged.
func trailBranchRefusal(err error, selector string, target *projectTrailTarget, current api.ProjectTrail) error {
	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) || !strings.HasPrefix(httpErr.Code, "trail_branch_") {
		return err
	}
	return fmt.Errorf("%s\nTo update this branch's trail, rerun without %s. To update #%d anyway, rerun with %s --project %s/%s in place of %s",
		singleLineTerminalText(httpErr.Message), selector, current.Number, tuiutil.SanitizeTerminalLabel(current.ID),
		tuiutil.SanitizeTerminalLabel(target.Host), tuiutil.SanitizeTerminalLabel(target.Project), selector)
}

// singleLineTerminalText sanitizes untrusted text for a one-line terminal
// message.
func singleLineTerminalText(s string) string {
	return strings.Join(strings.Fields(tuiutil.SanitizeTerminalText(s)), " ")
}

// resolveTrailUpdatePlacement resolves the clone's repository to the id the
// server knows it by; swapped in tests.
var resolveTrailUpdatePlacement = resolveForgeRepoCellPlacement

// guardedLocalRepo returns the clone's origin repository when the branch
// applies to the update: origin is in the trail's project, and --repo, if
// given, names that same repository. Outside such a clone, or with --repo
// naming another repository, there is no local branch work to confuse with
// the trail.
func guardedLocalRepo(ctx context.Context, cmd *cobra.Command, target *projectTrailTarget) (forge, owner, repo string, local bool, err error) {
	forge, owner, repo, err = resolveTrailRemote(ctx)
	if err != nil || !strings.EqualFold(forge, target.Host) || !strings.EqualFold(owner, target.Project) {
		return "", "", "", false, nil //nolint:nilerr // no local work to compare against; the update proceeds as before
	}
	if flag := trailRepoFlag(cmd); flag != "" {
		flagForge, flagOwner, flagRepo, err := parseTrailRepoArg(flag)
		if err != nil {
			return "", "", "", false, err
		}
		if !strings.EqualFold(flagForge, forge) || !strings.EqualFold(flagOwner, owner) || !strings.EqualFold(flagRepo, repo) {
			return "", "", "", false, nil
		}
	}
	return forge, owner, repo, true, nil
}

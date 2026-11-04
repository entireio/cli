package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// projectTrailUpdateGuardTimeout bounds the branch-work lookup behind the
// bare-number guard. A lookup that does not finish in time cannot clear the
// update, so it refuses like any other indeterminate lookup.
var projectTrailUpdateGuardTimeout = 10 * time.Second

// guardProjectTrailNumberUpdate refuses `trail update <N>` when the checked-out
// branch's work belongs to a different project trail than #N, or when it
// cannot establish which trail that work belongs to.
//
// Project trail numbers, change numbers, and pull request numbers look alike,
// so `trail update 1744` meant for change cli/1744 or PR #1744 reaches
// unrelated project trail #1744 and replaces fields its owner wrote. The guard
// runs only for a bare number, without --branch, from a clone of a repository
// in the trail's project (with --repo, only when it names that clone's
// repository). A ULID names the trail unambiguously and is the way to update
// #N anyway. A branch with no trail, or one whose work is on #N, passes.
//
// The guard fails closed: overwriting another owner's trail is the worst
// outcome, so a detached HEAD, a failed or timed-out lookup, a scan stopped
// before the last page, or work whose project parent the API did not return
// all refuse, naming the ULID rerun.
func guardProjectTrailNumberUpdate(cmd *cobra.Command, selector string, target *projectTrailTarget, current api.ProjectTrail) error {
	number, ok := parseTrailNumberSelector(selector)
	if !ok || trailBranchFlag(cmd) != "" {
		return nil
	}
	ctx := cmd.Context()
	forge, owner, repo, local, err := guardedLocalRepo(ctx, cmd, target)
	if err != nil || !local {
		return err
	}
	g := projectTrailNumberGuard{number: number, current: current, target: target, repo: repo}
	branch, err := GetCurrentBranch(ctx)
	if err != nil || branch == "" {
		return errors.New(g.detachedHead())
	}
	g.branch = branch
	work, err := lookupBranchTrailWork(cmd, forge, owner, repo, branch)
	switch {
	case err != nil:
		return errors.New(g.indeterminate("could not check which trail it belongs to (" + singleLineTerminalText(err.Error()) + ")"))
	case work == nil:
		return nil
	case work.Parent == nil:
		return errors.New(g.indeterminate("has trail work whose project trail the API did not return"))
	case work.Parent.ID == current.ID:
		return nil
	}
	return errors.New(g.mismatch(work))
}

// guardedLocalRepo returns the clone's origin repository when the guard
// applies to it: origin is in the trail's project, and --repo, if given, names
// that same repository. Outside such a clone, or with --repo naming another
// repository, there is no local branch work to confuse with the trail.
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

// lookupBranchTrailWork finds the branch's work within the guard's time
// budget. A scan that reaches its page budget without finding the branch
// counts as "no work", with a warning: the list is ordered by recent
// activity, so a branch being worked on shows up early, and refusing here
// would block every bare-number update from a branch with no trail (main)
// once a repository passes the budget.
func lookupBranchTrailWork(cmd *cobra.Command, forge, owner, repo, branch string) (*api.TrailResource, error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), projectTrailUpdateGuardTimeout)
	defer cancel()
	client, repoID, err := newTrailAPIClient(ctx, trailInsecureHTTP(cmd), forge, owner, repo)
	if err != nil {
		return nil, err
	}
	base, err := trailRepoBasePath(forge, owner, repo, repoID)
	if err != nil {
		return nil, err
	}
	work, complete, err := scanTrailPages(ctx, client, base, func(t api.TrailResource) bool { return t.Branch == branch })
	if err != nil {
		return nil, err
	}
	if work == nil && !complete {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: branch %q is not among the %d most recently active trails in %s/%s; treating it as having no trail\n",
			tuiutil.SanitizeTerminalLabel(branch), trailFindMaxPages*trailListServerMaxLimit, tuiutil.SanitizeTerminalLabel(owner), tuiutil.SanitizeTerminalLabel(repo))
	}
	return work, nil
}

// singleLineTerminalText sanitizes server- or transport-supplied text for a
// one-line terminal message.
func singleLineTerminalText(s string) string {
	return strings.Join(strings.Fields(tuiutil.SanitizeTerminalText(s)), " ")
}

// projectTrailNumberGuard words the guard's refusals. Every suggested rerun
// changes only the selector, so the user's update flags carry over.
type projectTrailNumberGuard struct {
	number  int
	current api.ProjectTrail
	target  *projectTrailTarget
	repo    string
	branch  string
}

func (g projectTrailNumberGuard) refusal() string {
	return fmt.Sprintf("refusing to update trail #%d %q", g.number, tuiutil.SanitizeTerminalLabel(g.current.Title))
}

// anyway names the ULID rerun that updates #N regardless of the branch.
func (g projectTrailNumberGuard) anyway() string {
	return fmt.Sprintf("\nTo update #%d anyway, rerun with %s --project %s/%s in place of %d, keeping your other flags.",
		g.number, tuiutil.SanitizeTerminalLabel(g.current.ID), tuiutil.SanitizeTerminalLabel(g.target.Host),
		tuiutil.SanitizeTerminalLabel(g.target.Project), g.number)
}

func (g projectTrailNumberGuard) detachedHead() string {
	return g.refusal() + ": HEAD is not on a branch, so the guard cannot check whether this work belongs to another trail, and a bare number names a project trail." +
		fmt.Sprintf("\nTo update #%d as a branch's trail, rerun with --branch <branch> added; that branch's work must be on #%d.", g.number, g.number) +
		g.anyway()
}

// indeterminate refuses when the branch's trail cannot be established. It
// does not suggest the no-selector rerun, which depends on that same unknown.
func (g projectTrailNumberGuard) indeterminate(reason string) string {
	return fmt.Sprintf("%s: branch %q %s, and a bare number can name another trail.", g.refusal(), tuiutil.SanitizeTerminalLabel(g.branch), reason) +
		g.anyway()
}

// mismatch explains the refusal and names both safe reruns. When N is the
// branch's own change number, the likely intent was the change, so it says so.
func (g projectTrailNumberGuard) mismatch(work *api.TrailResource) string {
	own := "another trail (" + tuiutil.SanitizeTerminalLabel(work.Parent.ID) + ")"
	if work.Parent.Number > 0 {
		own = fmt.Sprintf("trail #%d", work.Parent.Number)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: branch %q is work on %s, not on #%d", g.refusal(), tuiutil.SanitizeTerminalLabel(g.branch), own, g.number)
	if work.Number == g.number {
		fmt.Fprintf(&b, "; %d is this branch's change number, and a bare number names a project trail. For the change, rerun with %s/%d in place of %d",
			g.number, tuiutil.SanitizeTerminalLabel(g.repo), g.number, g.number)
	}
	fmt.Fprintf(&b, ".\nTo update this branch's trail, rerun without %d, keeping your other flags.", g.number)
	b.WriteString(g.anyway())
	return b.String()
}

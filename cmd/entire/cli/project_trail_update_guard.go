package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// guardProjectTrailNumberUpdate prepares `trail update <N>` so the server can
// refuse it when the checked-out branch's work belongs to a different project
// trail than #N.
//
// Project trail numbers, change numbers, and pull request numbers look alike,
// so `trail update 1744` meant for change cli/1744 or PR #1744 reaches
// unrelated project trail #1744 and replaces fields its owner wrote. The guard
// applies only to a bare number, without --branch, from a clone of a repository
// in the trail's project (with --repo, only when it names that clone's
// repository). A ULID names the trail unambiguously and is the way to update
// #N anyway.
//
// The decision is the server's: the update carries the branch and repository
// with branch_check=required, and the trail's cell refuses atomically when the
// branch's work is on another trail, or when it cannot compare. The CLI
// refuses locally only where nothing could be sent: a detached HEAD, a
// repository that does not resolve, a repository processed in another cell
// than the trail, or values the server would reject. A repository not
// onboarded to Entire has no trail work, so the update proceeds unguarded.
func guardProjectTrailNumberUpdate(cmd *cobra.Command, selector string, target *projectTrailTarget, current api.ProjectTrail) (*projectTrailNumberGuard, error) {
	number, ok := parseTrailNumberSelector(selector)
	if !ok || trailBranchFlag(cmd) != "" {
		return nil, nil //nolint:nilnil // a nil guard sends nothing; its methods are nil-safe
	}
	ctx := cmd.Context()
	forge, owner, repo, local, err := guardedLocalRepo(ctx, cmd, target)
	if err != nil || !local {
		return nil, err
	}
	g := &projectTrailNumberGuard{number: number, current: current, target: target, repo: repo}
	branch, err := GetCurrentBranch(ctx)
	if err != nil || branch == "" {
		return nil, errors.New(g.detachedHead())
	}
	g.branch = branch
	placement, err := resolveTrailUpdatePlacement(ctx, forge, owner, repo)
	switch {
	case errors.Is(err, errRepoNotOnboarded):
		return nil, nil //nolint:nilnil // no trail work can exist; the update proceeds unguarded
	case err != nil:
		return nil, errors.New(g.indeterminate("could not be checked: its repository did not resolve (" + singleLineTerminalText(err.Error()) + ")"))
	case placement.Target == nil || !sameAPIOrigin(placement.Target.BaseURL, target.Client.BaseURL()):
		return nil, errors.New(g.indeterminate("could not be checked: its repository is processed in another cell than the trail"))
	case !branchCheckSendable(placement.RepoID, branch):
		return nil, errors.New(g.indeterminate("could not be checked: the server cannot accept this branch or repository for checking"))
	}
	g.repoID = placement.RepoID
	return g, nil
}

// resolveTrailUpdatePlacement resolves the clone's repository to the id and
// cell the server knows it by; swapped in tests.
var resolveTrailUpdatePlacement = resolveForgeRepoCellPlacement

// maxTrailBranchCheckBytes is the server's from_branch limit. Longer names are
// refused locally: sending them would fail the whole update validation.
const maxTrailBranchCheckBytes = 1024

// branchCheckSendable reports whether the server's parameter validation
// accepts the pair; a rejected pair would fail the whole update.
func branchCheckSendable(repoID, branch string) bool {
	return looksLikeULID(repoID) && branch != "" && len(branch) <= maxTrailBranchCheckBytes
}

func sameAPIOrigin(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(b, "/"))
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

// readProjectTrailForUpdate reads the trail and its ETag, then prepares the
// bare-number guard before any write.
func readProjectTrailForUpdate(cmd *cobra.Command, selector string, target *projectTrailTarget) (api.ProjectTrail, string, *projectTrailNumberGuard, error) {
	current, etag, err := target.read(cmd.Context())
	if err != nil {
		return api.ProjectTrail{}, "", nil, err
	}
	if etag == "" {
		return api.ProjectTrail{}, "", nil, errors.New("project trail read returned no ETag; refusing an unprotected update")
	}
	guard, err := guardProjectTrailNumberUpdate(cmd, selector, target, current)
	if err != nil {
		return api.ProjectTrail{}, "", nil, err
	}
	return current, etag, guard, nil
}

// updatePath adds the branch the update comes from and requires the server to
// compare it. A nil guard leaves the path unchanged.
func (g *projectTrailNumberGuard) updatePath(path string) string {
	if g == nil {
		return path
	}
	query := url.Values{"from_repo_id": {g.repoID}, "from_branch": {g.branch}, "branch_check": {"required"}}
	return path + "?" + query.Encode()
}

// serverRefusal words the server's branch-check refusals; other errors pass
// through unchanged. Neither refusal names the other trail: the server does
// not return it.
func (g *projectTrailNumberGuard) serverRefusal(err error) error {
	var httpErr *api.HTTPError
	if g == nil || !errors.As(err, &httpErr) {
		return err
	}
	switch httpErr.Code {
	case "trail_branch_mismatch":
		return errors.New(g.mismatch(httpErr.ChangeNumber))
	case "trail_branch_unchecked":
		return errors.New(g.indeterminate("could not be checked by the server; nothing was updated"))
	}
	return err
}

// checkedNote warns when a guarded update reached a server that did not
// confirm the check (an older server, or a rollback), so the user can verify
// what changed. It never touches stdout.
func (g *projectTrailNumberGuard) checkedNote(cmd *cobra.Command, headers http.Header) {
	if g == nil || headers.Get("Entire-Branch-Check") == "checked" {
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Note: the server did not confirm that branch %q has no work on another trail; check #%d (%s) to make sure this update was meant for it.\n",
		tuiutil.SanitizeTerminalLabel(g.branch), g.number, tuiutil.SanitizeTerminalLabel(g.current.ID))
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
	repoID  string
}

func (g *projectTrailNumberGuard) refusal() string {
	return fmt.Sprintf("refusing to update trail #%d %q", g.number, tuiutil.SanitizeTerminalLabel(g.current.Title))
}

// anyway names the ULID rerun that updates #N regardless of the branch.
func (g *projectTrailNumberGuard) anyway() string {
	return fmt.Sprintf("\nTo update #%d anyway, rerun with %s --project %s/%s in place of %d, keeping your other flags.",
		g.number, tuiutil.SanitizeTerminalLabel(g.current.ID), tuiutil.SanitizeTerminalLabel(g.target.Host),
		tuiutil.SanitizeTerminalLabel(g.target.Project), g.number)
}

func (g *projectTrailNumberGuard) detachedHead() string {
	return g.refusal() + ": HEAD is not on a branch, so the guard cannot check whether this work belongs to another trail, and a bare number names a project trail." +
		fmt.Sprintf("\nTo update #%d as a branch's trail, rerun with --branch <branch> added; that branch's work must be on #%d.", g.number, g.number) +
		g.anyway()
}

// indeterminate refuses when the branch's trail cannot be established. It
// does not suggest the no-selector rerun, which depends on that same unknown.
func (g *projectTrailNumberGuard) indeterminate(reason string) string {
	return fmt.Sprintf("%s: branch %q %s, and a bare number can name another trail.", g.refusal(), tuiutil.SanitizeTerminalLabel(g.branch), reason) +
		g.anyway()
}

// mismatch explains the server's refusal and names both safe reruns. When N is
// the branch's own change number, the likely intent was the change, so it says
// so.
func (g *projectTrailNumberGuard) mismatch(changeNumber int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: branch %q is work on another trail, not on #%d; nothing was updated", g.refusal(), tuiutil.SanitizeTerminalLabel(g.branch), g.number)
	if changeNumber == g.number {
		fmt.Fprintf(&b, ". %d is this branch's change number, and a bare number names a project trail. For the change, rerun with %s/%d in place of %d",
			g.number, tuiutil.SanitizeTerminalLabel(g.repo), g.number, g.number)
	}
	fmt.Fprintf(&b, ".\nTo update this branch's trail, rerun without %d, keeping your other flags.", g.number)
	b.WriteString(g.anyway())
	return b.String()
}

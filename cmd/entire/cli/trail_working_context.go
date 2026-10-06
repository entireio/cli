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

// trailWorkingContext keeps branch identity internal. User selectors always name
// project intent; only repository subresource requests use Work.ID/Number.
type trailWorkingContext struct {
	Target            *projectTrailTarget
	Parent            api.ProjectTrail
	ETag              string
	Client            *api.Client
	BasePath          string
	Host, Owner, Repo string
	Work              api.TrailResource
}

func (t *trailWorkingContext) description() string {
	if t.Target == nil {
		return fmt.Sprintf("trail #%d", t.Work.Number)
	}
	return fmt.Sprintf("trail #%d (%s/%s/%s / %s)", t.Parent.Number, t.Host, t.Owner, t.Repo, tuiutil.SanitizeTerminalLabel(changeBranchName(t.Work)))
}

// resolveProjectTrailWorkingContext is projectTrailMode.workingContext.
// localOnly is used by checkout/resume: --repo may assert a local repository,
// but must never cause these commands to check out a foreign branch here.
func resolveProjectTrailWorkingContext(cmd *cobra.Command, selector, branch string, localOnly bool) (*trailWorkingContext, error) {
	if change, ok := parseProjectTrailChangeSelector(selector); ok {
		if strings.TrimSpace(branch) != "" {
			return nil, errChangeSelectorWithBranch
		}
		return resolveProjectTrailChangeContext(cmd, change, localOnly)
	}
	ctx := cmd.Context()
	if selector != "" {
		if err := validateProjectTrailSelector(selector); err != nil {
			return nil, err
		}
	}
	repoOverride := trailRepoFlag(cmd)
	if localOnly {
		repoOverride = ""
	} else if err := ensureTrailRepoHasTarget(cmd, selector != "" || strings.TrimSpace(branch) != "", "pass a trail selector or --branch"); err != nil {
		return nil, err
	}
	host, owner, repo, err := resolveTrailRepoOrRemote(ctx, repoOverride)
	if err != nil {
		return nil, err
	}
	client, repoID, err := newTrailAPIClient(ctx, trailInsecureHTTP(cmd), host, owner, repo)
	if err != nil {
		return nil, err
	}
	base, err := trailRepoBasePath(host, owner, repo, repoID)
	if err != nil {
		return nil, err
	}

	var target *projectTrailTarget
	if selector == "" {
		branch, err = resolveTrailBranch(ctx, branch)
		if err != nil {
			return nil, err
		}
		work, err := findTrailByBranchAtPath(ctx, client, base, branch)
		if err != nil {
			return nil, err
		}
		target, err = openTrailParentTarget(cmd, work, fmt.Sprintf("branch %q", branch))
		if err != nil {
			return nil, err
		}
	} else {
		forge, project, err := trailProjectReferenceOr(cmd, host, owner)
		if err != nil {
			return nil, err
		}
		target, err = resolveProjectTrailCollectionFor(ctx, forge, project, trailInsecureHTTP(cmd))
		if err != nil {
			return nil, err
		}
		target, err = target.resolveSelector(ctx, selector)
		if err != nil {
			return nil, err
		}
	}
	parent, etag, err := target.read(ctx)
	if err != nil {
		return nil, err
	}
	preferred := ""
	if repoOverride == "" {
		preferred, _ = GetCurrentBranch(ctx) //nolint:errcheck // no preference outside a checkout or on detached HEAD
	}
	selected, err := selectTrailWorkingBranch(parent.Changes, repoID, branch, preferred)
	if err != nil {
		return nil, err
	}
	return finishTrailWorkingContext(ctx, target, parent, etag, selected, client, base, host, owner, repo, repoID)
}

// resolveProjectTrailChangeContext selects branch work by its <repo>/<number>
// and reaches its project trail through the change's parent, so it needs no
// collection lookup and works for merged work whose branch is gone.
func resolveProjectTrailChangeContext(cmd *cobra.Command, sel projectTrailChangeSelector, localOnly bool) (*trailWorkingContext, error) {
	ctx := cmd.Context()
	change, err := resolveProjectTrailChange(cmd, sel, localOnly)
	if err != nil {
		return nil, err
	}
	target, err := openTrailParentTarget(cmd, change.Work, "change "+sel.String())
	if err != nil {
		return nil, err
	}
	parent, etag, err := target.read(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range parent.Changes {
		if item.ID == change.Work.ID && item.RepositoryID == change.RepoID {
			return finishTrailWorkingContext(ctx, target, parent, etag, item, change.Client, change.BasePath, change.Host, change.Owner, change.Repo, change.RepoID)
		}
	}
	return nil, fmt.Errorf("trail #%d does not list %s (it may be hidden by access filtering)", parent.Number, sel)
}

// finishTrailWorkingContext rereads the selected work through the owning
// project route to recheck containment, not a repo-local number that could
// name unrelated work. The repo client is retained for reviews.
func finishTrailWorkingContext(ctx context.Context, target *projectTrailTarget, parent api.ProjectTrail, etag string, selected api.ChangeSummary, client *api.Client, base, host, owner, repo, repoID string) (*trailWorkingContext, error) {
	if !looksLikeULID(selected.ID) {
		return nil, errors.New("selected branch has no valid backing identity")
	}
	var out api.ChangeResource
	_, err := target.Client.ProjectTrailRequest(ctx, http.MethodGet, target.path()+"/changes/"+url.PathEscape(selected.ID), nil, nil, &out)
	if err != nil {
		return nil, fmt.Errorf("read trail branch: %w", err)
	}
	if out.ID != selected.ID || out.TrailID != parent.ID || out.RepositoryID != repoID || out.Number <= 0 ||
		// An unlinked change may list no branch on its trail; the identity
		// checks above prove containment then.
		(selected.Branch != "" && changeBranchName(out.TrailResource) != selected.Branch) {
		return nil, errors.New("trail branch response does not match the selected repository/branch")
	}
	out.Parent = &api.TrailParentReference{ID: parent.ID, Number: parent.Number, ProjectID: parent.ProjectID,
		Host: target.Host, Project: target.Project, Path: target.path()}
	client.SetTrailRoute(out.ID, trailNumberPathForBase(base, out.Number))
	return &trailWorkingContext{Target: target, Parent: parent, ETag: etag, Client: client, BasePath: base,
		Host: host, Owner: owner, Repo: repo, Work: out.TrailResource}, nil
}

// changeBranchName is the branch a change summary lists: a merged change's
// read returns branch null and keeps the name in original_branch.
func changeBranchName(work api.TrailResource) string {
	if work.Branch != "" {
		return work.Branch
	}
	return work.OriginalBranch
}

func selectTrailWorkingBranch(changes []api.ChangeSummary, repoID, explicit, preferred string) (api.ChangeSummary, error) {
	var candidates []api.ChangeSummary
	for _, item := range changes {
		if item.RepositoryID == repoID && item.Branch != "" {
			candidates = append(candidates, item)
		}
	}
	for _, item := range candidates {
		if explicit != "" && item.Branch == explicit {
			return item, nil
		}
	}
	if explicit != "" {
		return api.ChangeSummary{}, fmt.Errorf("trail has no visible branch %q in the selected repository", explicit)
	}
	for _, item := range candidates {
		if preferred != "" && item.Branch == preferred {
			return item, nil
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	if len(candidates) == 0 {
		return api.ChangeSummary{}, errors.New("trail has no visible branches in the selected repository; choose --repo")
	}
	branches := make([]string, len(candidates))
	for i, item := range candidates {
		branches[i] = tuiutil.SanitizeTerminalLabel(item.Branch)
	}
	return api.ChangeSummary{}, fmt.Errorf("trail has multiple branches in this repository; select --branch: %s", strings.Join(branches, ", "))
}

func trailForDisplay(work api.TrailResource) api.TrailResource {
	if work.Parent != nil {
		work.ID, work.Number = work.Parent.ID, work.Parent.Number
	}
	return work
}

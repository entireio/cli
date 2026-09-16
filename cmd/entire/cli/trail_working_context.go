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
	return fmt.Sprintf("trail #%d (%s/%s/%s / %s)", t.Parent.Number, t.Host, t.Owner, t.Repo, tuiutil.SanitizeTerminalLabel(t.Work.Branch))
}

// localOnly is used by checkout/resume: --repo may assert a local repository,
// but must never cause these commands to check out a foreign branch here.
func resolveTrailWorkingContext(cmd *cobra.Command, selector, branch string, localOnly bool) (*trailWorkingContext, error) {
	ctx := cmd.Context()
	if selector != "" && !looksLikeULID(selector) {
		if _, ok := parseTrailNumberSelector(selector); !ok {
			return nil, errors.New("use a project trail ID or number; select a branch with --branch")
		}
	}
	repoOverride := trailRepoFlag(cmd)
	if localOnly {
		repoOverride = ""
	}
	if err := requireTrailWorkingTarget(repoOverride, selector, branch); err != nil {
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
		if work == nil || work.Parent == nil || !looksLikeULID(work.Parent.ID) {
			return nil, fmt.Errorf("branch %q has no discoverable trail; pass a project trail ID with --project", branch)
		}
		if ref := projectTrailProjectFlag(cmd); ref != "" {
			forge, project, err := parseTrailProjectRef(ref)
			if err != nil {
				return nil, err
			}
			if forge != work.Parent.Host || !strings.EqualFold(project, work.Parent.Project) {
				return nil, errors.New("branch's trail does not belong to --project")
			}
		}
		core, err := newProjectTrailCoreClient()
		if err != nil {
			return nil, fmt.Errorf("project control plane: %w", err)
		}
		routingCtx, cancel := context.WithTimeout(ctx, requiredCellResolveTimeout)
		defer cancel()
		target, err = openProjectTrailTarget(routingCtx, core, *work.Parent, trailInsecureHTTP(cmd))
		if err != nil {
			return nil, err
		}
	} else {
		forge, project := host, owner
		if ref := projectTrailProjectFlag(cmd); ref != "" {
			forge, project, err = parseTrailProjectRef(ref)
			if err != nil {
				return nil, err
			}
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
	if !looksLikeULID(selected.ID) {
		return nil, errors.New("selected branch has no valid backing identity")
	}
	// Read via the owned route to recheck containment, not a repo-local number
	// that could name unrelated work. The repo client is retained for reviews.
	var out struct {
		api.TrailResource

		TrailID      string `json:"trailId"`
		RepositoryID string `json:"repositoryId"`
	}
	_, err = target.Client.ProjectTrailRequest(ctx, http.MethodGet, target.path()+"/changes/"+url.PathEscape(selected.ID), nil, nil, &out)
	if err != nil {
		return nil, fmt.Errorf("read trail branch: %w", err)
	}
	if out.ID != selected.ID || out.TrailID != parent.ID || out.RepositoryID != repoID || out.Branch != selected.Branch || out.Number <= 0 {
		return nil, errors.New("trail branch response does not match the selected repository/branch")
	}
	out.Parent = &api.TrailParentReference{ID: parent.ID, Number: parent.Number, ProjectID: parent.ProjectID,
		Host: target.Host, Project: target.Project, Path: target.path()}
	client.SetTrailRoute(out.ID, trailNumberPathForBase(base, out.Number))
	return &trailWorkingContext{Target: target, Parent: parent, ETag: etag, Client: client, BasePath: base,
		Host: host, Owner: owner, Repo: repo, Work: out.TrailResource}, nil
}

func requireTrailWorkingTarget(repo, selector, branch string) error {
	if repo != "" && selector == "" && strings.TrimSpace(branch) == "" {
		return errors.New("--repo requires an explicit target: pass a trail selector or --branch")
	}
	return nil
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

func trailDisplayNumber(work *api.TrailResource) int {
	if work.Parent != nil {
		return work.Parent.Number
	}
	return work.Number
}

func trailForDisplay(work api.TrailResource) api.TrailResource {
	if work.Parent != nil {
		work.ID, work.Number = work.Parent.ID, work.Parent.Number
	}
	return work
}

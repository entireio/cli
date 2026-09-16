package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
)

type projectTrailCoreClient interface {
	ResolveProject(ctx context.Context, host, project string) (*coreapi.ProjectResolution, error)
	ListClusters(ctx context.Context) (*coreapi.ListClustersOutputBody, error)
}

var newProjectTrailCoreClient = func() (projectTrailCoreClient, error) { return coreapi.New() }
var newProjectTrailCellClient = auth.NewEntireAPICellClient

type projectTrailTarget struct {
	Client    *api.Client
	ProjectID string
	Host      string
	Project   string
	BasePath  string
	TrailID   string
}

func projectTrailBasePath(host, project string) string {
	return "/api/v1/" + url.PathEscape(host) + "/" + url.PathEscape(strings.ToLower(project)) + "/trails"
}

func projectTrailProjectFlag(cmd *cobra.Command) string {
	v, _ := cmd.Flags().GetString("project") //nolint:errcheck // registered on trail root
	return strings.TrimSpace(v)
}

// Project references always declare their namespace. In particular gh/acme
// and et/acme must not resolve through the same by-name project lookup.
func parseTrailProjectRef(ref string) (string, string, error) {
	host, project, ok := strings.Cut(ref, "/")
	if !ok || (host != mirrorCloneForge && host != nativeCloneForge) || project == "" || strings.ContainsAny(project, "/\\?#%") || project == "." || project == ".." {
		return "", "", fmt.Errorf("invalid project %q: use gh/<owner> or et/<project>", ref)
	}
	for _, c := range project {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
			return "", "", fmt.Errorf("invalid project %q: project names contain only letters, digits, and hyphens", ref)
		}
	}
	return host, strings.ToLower(project), nil
}

// projectTrailCellTarget never falls back to a repo or jurisdiction-default
// cell. A project may be assigned somewhere else, or not assigned at all.
func projectTrailCellTarget(clusters []coreapi.Cluster, cell, jurisdiction string) (*auth.CellTarget, error) {
	cell, jurisdiction = strings.TrimSpace(cell), strings.TrimSpace(jurisdiction)
	if cell == "" || jurisdiction == "" {
		return nil, errors.New("project has no processing cell or jurisdiction assignment")
	}
	cluster, ok := matchClusterBySlug(clusters, cell)
	if !ok {
		cluster, ok = matchClusterByCellInURL(clusters, cell)
	}
	if !ok {
		return nil, fmt.Errorf("project processing cell %q is absent from Core's cluster catalog", cell)
	}
	if !strings.EqualFold(cluster.Jurisdiction, jurisdiction) {
		return nil, fmt.Errorf("project processing cell %q has jurisdiction %q, expected %q", cell, cluster.Jurisdiction, jurisdiction)
	}
	return cellTargetFromCluster(cluster)
}

func openProjectTrailTarget(ctx context.Context, core projectTrailCoreClient, ref api.TrailParentReference, insecure bool) (*projectTrailTarget, error) {
	host, project, err := parseTrailProjectRef(ref.Host + "/" + ref.Project)
	if err != nil {
		return nil, err
	}
	base := projectTrailBasePath(host, project)
	if !looksLikeULID(ref.ProjectID) {
		return nil, errors.New("project reference has no valid project ID")
	}
	if ref.ID != "" && (!looksLikeULID(ref.ID) || ref.Path != base+"/"+ref.ID) {
		return nil, errors.New("parent reference has an invalid trail ID or canonical path")
	}
	clusters, err := core.ListClusters(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve project cell catalog: %w", err)
	}
	cell, err := projectTrailCellTarget(clusters.Clusters, ref.PrimaryProcessingCell, ref.Jurisdiction)
	if err != nil {
		return nil, err
	}
	client, err := newProjectTrailCellClient(ctx, insecure, cell)
	if err != nil {
		return nil, fmt.Errorf("open project trail cell: %w", err)
	}
	return &projectTrailTarget{Client: client, ProjectID: ref.ProjectID, Host: host, Project: project, BasePath: base, TrailID: ref.ID}, nil
}

func resolveProjectTrailCollection(cmd *cobra.Command) (*projectTrailTarget, error) {
	host, project, err := resolveTrailProjectReference(cmd)
	if err != nil {
		return nil, err
	}
	return resolveProjectTrailCollectionFor(cmd.Context(), host, project, trailInsecureHTTP(cmd))
}

func resolveProjectTrailCollectionFor(ctx context.Context, host, project string, insecure bool) (*projectTrailTarget, error) {
	ctx, cancel := context.WithTimeout(ctx, requiredCellResolveTimeout)
	defer cancel()
	core, err := newProjectTrailCoreClient()
	if err != nil {
		return nil, fmt.Errorf("project control plane: %w", err)
	}
	resolved, err := core.ResolveProject(ctx, host, project)
	if err != nil {
		return nil, fmt.Errorf("resolve project: %w", err)
	}
	if resolved.Project == nil || resolved.Reference.Host != host || !strings.EqualFold(resolved.Reference.Project, project) {
		return nil, errors.New("core returned a different project reference")
	}
	return openProjectTrailTarget(ctx, core, api.TrailParentReference{
		ProjectID: resolved.Project.ID, Host: resolved.Reference.Host, Project: resolved.Reference.Project,
		Jurisdiction: resolved.Project.Region, PrimaryProcessingCell: resolved.Project.PrimaryProcessingCell,
	}, insecure)
}

func resolveTrailProjectReference(cmd *cobra.Command) (string, string, error) {
	if ref := projectTrailProjectFlag(cmd); ref != "" {
		return parseTrailProjectRef(ref)
	}
	host, owner, _, err := resolveTrailRepoOrRemote(cmd.Context(), trailRepoFlag(cmd))
	return host, owner, err
}

// With no selector, discover the project parent through the branch's Change.
// This does NOT call ResolveProject: repo-only readers must retain access to an
// addressed parent even when they cannot enumerate the project's collection.
func resolveProjectTrail(cmd *cobra.Command, selector string) (*projectTrailTarget, error) {
	branch := trailBranchFlag(cmd)
	if selector != "" && branch != "" {
		selected, err := resolveTrailWorkingContext(cmd, selector, branch, false)
		if err != nil {
			return nil, err
		}
		return selected.Target, nil
	}
	return resolveProjectTrailWithBranch(cmd, selector, branch)
}

func resolveProjectTrailWithBranch(cmd *cobra.Command, selector, branch string) (*projectTrailTarget, error) {
	if selector != "" && branch != "" {
		return nil, errors.New("pass a project trail selector or --branch, not both")
	}
	if selector == "" {
		return resolveBranchProjectTrail(cmd, branch)
	}
	if !looksLikeULID(selector) {
		if _, ok := parseTrailNumberSelector(selector); !ok {
			return nil, errors.New("use a project trail ID or number; select branch work with --branch")
		}
	}
	target, err := resolveProjectTrailCollection(cmd)
	if err != nil {
		return nil, err
	}
	return target.resolveSelector(cmd.Context(), selector)
}

func (t *projectTrailTarget) resolveSelector(ctx context.Context, selector string) (*projectTrailTarget, error) {
	if looksLikeULID(selector) {
		t.TrailID = selector
		return t, nil
	}
	number, ok := parseTrailNumberSelector(selector)
	if !ok {
		return nil, errors.New("use a project trail ID or number; select a branch with --branch")
	}
	pageToken := ""
	seen := map[string]bool{}
	for range trailFindMaxPages {
		page, err := t.list(ctx, trailListServerMaxLimit, pageToken)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if item.Number == number {
				t.TrailID = item.ID
				return t, nil
			}
		}
		if page.NextPageToken == nil || *page.NextPageToken == "" {
			return nil, fmt.Errorf("project trail #%d not found", number)
		}
		pageToken = *page.NextPageToken
		if seen[pageToken] {
			return nil, errors.New("project trail pagination repeated a cursor")
		}
		seen[pageToken] = true
	}
	return nil, errors.New("project trail number lookup exceeded its page budget; use the trail ID")
}

func resolveBranchProjectTrail(cmd *cobra.Command, branch string) (*projectTrailTarget, error) {
	if err := ensureTrailRepoHasTarget(cmd, branch != "", "pass --branch or a project trail ID"); err != nil {
		return nil, err
	}
	ctx := cmd.Context()
	forge, owner, repo, err := resolveTrailRepoOrRemote(ctx, trailRepoFlag(cmd))
	if err != nil {
		return nil, err
	}
	branch, err = resolveTrailBranch(ctx, branch)
	if err != nil {
		return nil, err
	}
	client, repoID, err := newTrailAPIClient(ctx, trailInsecureHTTP(cmd), forge, owner, repo)
	if err != nil {
		return nil, err
	}
	base, err := trailRepoBasePath(forge, owner, repo, repoID)
	if err != nil {
		return nil, err
	}
	change, err := findTrailByBranchAtPath(ctx, client, base, branch)
	if err != nil {
		return nil, err
	}
	if change == nil || change.Parent == nil {
		return nil, fmt.Errorf("branch %q has no discoverable project trail; pass a project trail ID with --project (a missing parent may be inaccessible or unresolved)", branch)
	}
	parent := *change.Parent
	if !looksLikeULID(parent.ID) {
		return nil, errors.New("branch parent reference is missing a valid project trail ID")
	}
	if ref := projectTrailProjectFlag(cmd); ref != "" {
		host, project, err := parseTrailProjectRef(ref)
		if err != nil {
			return nil, err
		}
		if parent.Host != host || !strings.EqualFold(parent.Project, project) {
			return nil, errors.New("branch's parent does not belong to --project")
		}
	}
	core, err := newProjectTrailCoreClient()
	if err != nil {
		return nil, fmt.Errorf("project control plane: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, requiredCellResolveTimeout)
	defer cancel()
	return openProjectTrailTarget(ctx, core, parent, trailInsecureHTTP(cmd))
}

func (t *projectTrailTarget) path() string { return t.BasePath + "/" + url.PathEscape(t.TrailID) }

func (t *projectTrailTarget) read(ctx context.Context) (api.ProjectTrail, string, error) {
	var out api.ProjectTrail
	etag, err := t.Client.ProjectTrailRequest(ctx, http.MethodGet, t.path(), nil, nil, &out)
	if err != nil {
		return out, "", fmt.Errorf("read project trail: %w", err)
	}
	if err := t.validateResponse(out); err != nil {
		return out, "", err
	}
	return out, etag, nil
}

func (t *projectTrailTarget) validateResponse(out api.ProjectTrail) error {
	if !looksLikeULID(out.ID) || out.ProjectID != t.ProjectID || (t.TrailID != "" && out.ID != t.TrailID) {
		return errors.New("project trail response identity does not match the request")
	}
	return nil
}

func (t *projectTrailTarget) list(ctx context.Context, size int, cursor string) (api.ProjectTrailListResponse, error) {
	var out api.ProjectTrailListResponse
	q := url.Values{"pageSize": {strconv.Itoa(size)}}
	if cursor != "" {
		q.Set("pageToken", cursor)
	}
	_, err := t.Client.ProjectTrailRequest(ctx, http.MethodGet, t.BasePath+"?"+q.Encode(), nil, nil, &out)
	if err != nil {
		return out, fmt.Errorf("list project trails: %w", err)
	}
	for _, item := range out.Items {
		if item.ProjectID != t.ProjectID || !looksLikeULID(item.ID) {
			return out, errors.New("project trail list returned an invalid identity")
		}
	}
	return out, nil
}

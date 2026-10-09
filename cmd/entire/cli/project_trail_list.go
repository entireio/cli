package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// Listing requires an explicitly selected project, including inside a clone.
// A repository only narrows that project's results; its cell never routes the
// collection read. Failed project resolution must not trigger fleet discovery.
func listProjectTrails(cmd *cobra.Command, status string, size int, cursor string) (api.ProjectTrailListResponse, error) {
	var out api.ProjectTrailListResponse
	ref := projectTrailProjectFlag(cmd)
	if ref == "" {
		return out, errors.New("trail list requires --project gh/<owner> or et/<project>; --repo is only a filter within that project")
	}
	host, project, err := parseTrailProjectRef(ref)
	if err != nil {
		return out, err
	}
	target, err := resolveProjectTrailCollectionFor(cmd.Context(), host, project, trailInsecureHTTP(cmd))
	if err != nil {
		return out, renderDataAPIAuthError(cmd.Context(), cmd.ErrOrStderr(), "", err)
	}
	repoID := ""
	if repo := trailRepoFlag(cmd); repo != "" {
		if repoID, err = resolveTrailRepoID(cmd.Context(), repo); err != nil {
			return out, err
		}
	}
	return target.list(cmd.Context(), size, cursor, status, repoID)
}

// resolveTrailRepoID turns a --repo value (or the origin remote when empty)
// into the control plane's repository ID.
func resolveTrailRepoID(ctx context.Context, repoFlag string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, requiredCellResolveTimeout)
	defer cancel()
	forge, owner, repo, err := resolveTrailRepoOrRemote(ctx, repoFlag)
	if err != nil {
		return "", err
	}
	placement, err := resolveForgeRepoCellPlacement(ctx, forge, owner, repo)
	if err != nil {
		return "", err
	}
	return placement.RepoID, nil
}

// projectTrailListTimeout bounds one collection page, including each page of a
// numeric selector lookup.
const projectTrailListTimeout = 30 * time.Second

// All collection reads, including numeric selector lookup, use the one
// project-addressed endpoint. The server owns cursor binding and ordering;
// forward its opaque token unchanged, never wrap or merge it client-side.
func (t *projectTrailTarget) list(ctx context.Context, size int, cursor, status, repoID string) (api.ProjectTrailListResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, projectTrailListTimeout)
	defer cancel()
	var out api.ProjectTrailListResponse
	q := url.Values{
		"projectId": {t.ProjectID}, "pageSize": {strconv.Itoa(size)},
		"sort": {"updated"}, "groupBy": {"none"},
	}
	if cursor != "" {
		q.Set("pageToken", cursor)
	}
	if status != "" {
		q.Set("status", status)
	}
	if repoID != "" {
		q.Set("repoId", repoID)
	}
	_, err := t.Client.ProjectTrailRequest(ctx, http.MethodGet, "/api/v1/trails?"+q.Encode(), nil, nil, &out)
	if err != nil {
		return out, fmt.Errorf("list project trails: %w", err)
	}
	if len(out.Items) > size {
		return out, errors.New("project trail response exceeds the requested page size")
	}
	if out.NextPageToken != nil && *out.NextPageToken != "" && *out.NextPageToken == cursor {
		return out, errors.New("project trail pagination did not advance")
	}
	for _, item := range out.Items {
		if err := t.validateResponse(item); err != nil {
			return out, err
		}
		if (status != "" && item.Status != status) || (repoID != "" && !slices.Contains(item.RepositoryIDs, repoID)) {
			return out, errors.New("project trail response does not match the requested filters")
		}
	}
	if out.Items == nil {
		out.Items = []api.ProjectTrail{}
	}
	return out, nil
}

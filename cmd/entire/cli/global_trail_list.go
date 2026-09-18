package cli

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// A cursor contains only continuation positions, never routing authority. Every
// invocation resolves destinations afresh and requires exactly the same set.
// Unconsumed rows are fetched again; advancing a cell to its page-end cursor
// would silently skip them when another cell fills the merged page first.
type globalTrailCursor struct {
	Version int                            `json:"version"`
	Binding string                         `json:"binding"`
	Cells   map[string]globalTrailPosition `json:"cells"`
}

type globalTrailPosition struct {
	Token string `json:"token,omitempty"`
	Done  bool   `json:"done,omitempty"`
}

func globalTrailCell(target *auth.CellTarget) cellGroup {
	return cellGroup{cell: target.Jurisdiction + "|" + target.BaseURL, baseURL: target.BaseURL, jurisdiction: target.Jurisdiction}
}

// Unfiltered listing does not inspect the checkout. Explicit project routing
// remains authoritative, including hidden cells. Repo-only readers can filter
// via their processing placement without requiring project enumeration access.
func resolveGlobalTrailList(cmd *cobra.Command, status string) ([]cellGroup, url.Values, error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), requiredCellResolveTimeout)
	defer cancel()
	query := url.Values{"sort": {"updated"}, "groupBy": {"none"}}
	if status != "" {
		query.Set("status", status)
	}
	var repoTarget *auth.CellTarget
	if repo := trailRepoFlag(cmd); repo != "" {
		forge, owner, name, err := resolveTrailRepoOrRemote(ctx, repo)
		if err != nil {
			return nil, nil, err
		}
		placement, err := resolveForgeRepoCellPlacement(ctx, forge, owner, name)
		if err != nil {
			return nil, nil, err
		}
		query.Set("repoId", placement.RepoID)
		repoTarget = placement.Target
	}
	ref := projectTrailProjectFlag(cmd)
	if ref == "" && repoTarget != nil {
		return []cellGroup{globalTrailCell(repoTarget)}, query, nil
	}
	core, err := newProjectTrailCoreClient()
	if err != nil {
		return nil, nil, fmt.Errorf("global trails control plane: %w", err)
	}
	if ref != "" {
		host, project, err := parseTrailProjectRef(ref)
		if err != nil {
			return nil, nil, err
		}
		target, cell, err := resolveProjectTrailRoute(ctx, core, host, project)
		if err != nil {
			return nil, nil, err
		}
		query.Set("projectId", target.ProjectID)
		return []cellGroup{globalTrailCell(cell)}, query, nil
	}
	catalog, err := core.ListClusters(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("global trails cell catalog: %w", err)
	}
	cells := []cellGroup{}
	seen := map[string]bool{}
	for _, cluster := range catalog.Clusters {
		cell, err := projectTrailResolvedCellTarget(cluster.ApiUrl.Or(""), cluster.Slug, cluster.Jurisdiction)
		if err != nil {
			return nil, nil, fmt.Errorf("global trails cluster %q: %w", cluster.Slug, err)
		}
		group := globalTrailCell(cell)
		if !seen[group.cell] {
			cells = append(cells, group)
			seen[group.cell] = true
		}
	}
	slices.SortFunc(cells, func(a, b cellGroup) int { return strings.Compare(a.cell, b.cell) })
	if len(cells) == 0 {
		return nil, nil, errors.New("global trails: Core returned no available cells")
	}
	return cells, query, nil
}

func decodeGlobalTrailCursor(raw string, query url.Values, cells []cellGroup) (globalTrailCursor, error) {
	binding := fmt.Sprintf("%x", sha256.Sum256([]byte(query.Encode())))
	cursor := globalTrailCursor{Version: 1, Binding: binding, Cells: make(map[string]globalTrailPosition, len(cells))}
	for _, cell := range cells {
		cursor.Cells[cell.cell] = globalTrailPosition{}
	}
	if raw == "" {
		return cursor, nil
	}
	const maxCursorBytes = 256 * 1024
	if len(raw) > maxCursorBytes {
		return cursor, errors.New("global trail page token is too large")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	var decoded globalTrailCursor
	if err != nil || json.Unmarshal(data, &decoded) != nil || decoded.Version != 1 {
		return cursor, errors.New("invalid global trail page token; restart pagination")
	}
	if decoded.Binding != binding || len(decoded.Cells) != len(cursor.Cells) {
		return cursor, errors.New("global trail filters or cells changed; restart pagination")
	}
	for key := range cursor.Cells {
		if _, ok := decoded.Cells[key]; !ok {
			return cursor, errors.New("global trail cells changed; restart pagination")
		}
	}
	return decoded, nil
}

func compareGlobalTrails(a, b api.ProjectTrail) int {
	x, y := a.Order, b.Order
	return cmp.Or(cmp.Compare(x.GroupRank, y.GroupRank), strings.Compare(x.GroupKey, y.GroupKey),
		cmp.Compare(x.SortRank, y.SortRank), strings.Compare(x.SortValue, y.SortValue),
		strings.Compare(y.UpdatedAt, x.UpdatedAt), strings.Compare(y.TrailID, x.TrailID))
}

func validateGlobalTrailPage(page api.GlobalTrailListResponse, cell cellGroup, query url.Values, size int, token string) error {
	if page.Jurisdiction != cell.jurisdiction || len(page.Items) > size {
		return errors.New("global trail response has an unexpected jurisdiction or page size")
	}
	if page.NextPageToken != nil && *page.NextPageToken != "" && (len(page.Items) == 0 || *page.NextPageToken == token) {
		return errors.New("global trail pagination did not advance")
	}
	for i, item := range page.Items {
		if !looksLikeULID(item.ID) || !looksLikeULID(item.ProjectID) || item.Number <= 0 || item.Project == nil ||
			item.Project.ID != item.ProjectID || item.Project.Jurisdiction != cell.jurisdiction {
			return errors.New("global trail response has an invalid project identity")
		}
		if _, _, err := parseTrailProjectRef(item.Project.Reference.Forge + "/" + item.Project.Reference.Project); err != nil {
			return fmt.Errorf("global trail project reference: %w", err)
		}
		if (query.Get("projectId") != "" && item.ProjectID != query.Get("projectId")) ||
			(query.Get("repoId") != "" && !slices.Contains(item.RepositoryIDs, query.Get("repoId"))) ||
			(query.Get("status") != "" && item.Status != query.Get("status")) {
			return errors.New("global trail response does not match the requested filters")
		}
		if item.Order == nil || item.Order.TrailID != item.ID || item.ContinuationToken == "" || item.ContinuationToken == token {
			return errors.New("global trail response is missing ordering or continuation metadata")
		}
		if _, err := time.Parse("2006-01-02T15:04:05.000000000Z", item.Order.UpdatedAt); err != nil {
			return fmt.Errorf("invalid global trail ordering timestamp: %w", err)
		}
		if i > 0 && compareGlobalTrails(page.Items[i-1], item) >= 0 {
			return errors.New("global trail response is not strictly ordered")
		}
	}
	return nil
}

func listGlobalTrails(cmd *cobra.Command, status string, size int, rawCursor string) (api.ProjectTrailListResponse, error) {
	out := api.ProjectTrailListResponse{Items: []api.ProjectTrail{}}
	cells, query, err := resolveGlobalTrailList(cmd, status)
	if err != nil {
		return out, err
	}
	cursor, err := decodeGlobalTrailCursor(rawCursor, query, cells)
	if err != nil {
		return out, err
	}
	active := []cellGroup{}
	for _, cell := range cells {
		if !cursor.Cells[cell.cell].Done {
			active = append(active, cell)
		}
	}
	results, err := fanOutCells(cmd.Context(), trailInsecureHTTP(cmd), 30*time.Second, active,
		func(ctx context.Context, cell cellGroup, client *api.Client) (api.GlobalTrailListResponse, error) {
			q := url.Values{}
			for key, values := range query {
				q[key] = slices.Clone(values)
			}
			q.Set("pageSize", strconv.Itoa(size))
			token := cursor.Cells[cell.cell].Token
			if token != "" {
				q.Set("pageToken", token)
			}
			var page api.GlobalTrailListResponse
			_, err := client.ProjectTrailRequest(ctx, http.MethodGet, "/api/v1/trails?"+q.Encode(), nil, nil, &page)
			if err == nil {
				err = validateGlobalTrailPage(page, cell, query, size, token)
			}
			return page, err
		})
	if err != nil {
		return out, err
	}
	type candidate struct {
		item api.ProjectTrail
		cell string
	}
	candidates := []candidate{}
	seen := map[string]bool{}
	for _, result := range results {
		// No partial-success page or cursor: otherwise an unavailable cell's
		// newest rows could be skipped or returned out of global order.
		if result.err != nil {
			return out, fmt.Errorf("list global trails in %s: %w", result.group.cell, result.err)
		}
		for _, item := range result.value.Items {
			if seen[item.ID] {
				return out, fmt.Errorf("global trail %s was returned by multiple cells", item.ID)
			}
			seen[item.ID] = true
			candidates = append(candidates, candidate{item: item, cell: result.group.cell})
		}
	}
	slices.SortFunc(candidates, func(a, b candidate) int { return compareGlobalTrails(a.item, b.item) })
	consumed := map[string]int{}
	for _, row := range candidates[:min(size, len(candidates))] {
		out.Items = append(out.Items, row.item)
		consumed[row.cell]++
		cursor.Cells[row.cell] = globalTrailPosition{Token: row.item.ContinuationToken}
	}
	for _, result := range results {
		if consumed[result.group.cell] == len(result.value.Items) && (result.value.NextPageToken == nil || *result.value.NextPageToken == "") {
			cursor.Cells[result.group.cell] = globalTrailPosition{Done: true}
		}
	}
	for _, pos := range cursor.Cells {
		if !pos.Done {
			data, err := json.Marshal(cursor)
			if err != nil {
				return out, fmt.Errorf("encode global trail cursor: %w", err)
			}
			token := base64.RawURLEncoding.EncodeToString(data)
			out.NextPageToken = &token
			break
		}
	}
	return out, nil
}

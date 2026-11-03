package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
	"github.com/entireio/cli/internal/coreapi"
)

// myProjectTrailsCellTimeout bounds each region's /me/project-trails read.
const myProjectTrailsCellTimeout = 15 * time.Second

// A project trail lives in its project's cell, so a cell's /me/project-trails
// only knows the caller's trails homed there. Unlike other /me reads, which the
// home cell answers alone, "my trails" asks every jurisdiction's default cell
// and merges, as the web app's sidebar does. The merged list is one bounded
// page: per-region cursors cannot continue a merged order. Unlike the sidebar,
// which fails on any regional outage, a region that fails is reported as
// partial and the others are still listed.

// myProjectTrailCellItem is a /me/project-trails item as a cell serves it:
// snake_case, with the project inline because the list spans projects.
type myProjectTrailCellItem struct {
	ID                 string         `json:"id"`
	Number             int            `json:"number"`
	URL                string         `json:"url"`
	Title              string         `json:"title"`
	Body               string         `json:"body"`
	Status             string         `json:"status"`
	Phase              string         `json:"phase"`
	Type               string         `json:"type"`
	Priority           string         `json:"priority"`
	Metadata           map[string]any `json:"metadata"`
	Assignees          []string       `json:"assignees"`
	AssigneeAccountIDs []*string      `json:"assignee_account_ids"`
	RepositoryIDs      []string       `json:"repository_ids"`
	AuthorAccountID    string         `json:"author_account_id"`
	CreatedAt          time.Time      `json:"created_at"`
	ClosedAt           *time.Time     `json:"closed_at"`
	LastActivityAt     time.Time      `json:"last_activity_at"`
	// MetadataUpdatedAt is the trail's own last update; it breaks ties
	// between copies of one trail with the same activity.
	MetadataUpdatedAt time.Time `json:"metadata_updated_at"`
	IsPossiblyPartial bool      `json:"is_possibly_partial"`
	Project           *struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Provider     string `json:"provider"`
		Jurisdiction string `json:"jurisdiction"`
		Reference    struct {
			Forge   string `json:"forge"`
			Project string `json:"project"`
		} `json:"reference"`
		CanManageTrails bool `json:"can_manage_trails"`
	} `json:"project"`
	Changes []struct {
		ID             string    `json:"id"`
		RepositoryID   string    `json:"repository_id"`
		Repository     string    `json:"repository"`
		Branch         string    `json:"branch"`
		Number         int       `json:"number"`
		Title          string    `json:"title"`
		Status         string    `json:"status"`
		HasCodeChanges bool      `json:"has_code_changes"`
		CreatedAt      time.Time `json:"created_at"`
		UpdatedAt      time.Time `json:"updated_at"`
	} `json:"changes"`
}

// myProjectTrail is one `trail list --mine` item: the same shape as a plain
// `trail list --json` item, so scripts read one shape. metadataUpdatedAt only
// orders duplicate copies and is not printed.
type myProjectTrail struct {
	api.ProjectTrail

	metadataUpdatedAt time.Time
}

// toMyProjectTrail maps a cell item onto the plain list's shape.
func (c myProjectTrailCellItem) toMyProjectTrail() myProjectTrail {
	t := myProjectTrail{
		ProjectTrail: api.ProjectTrail{
			ID: c.ID, Number: c.Number, URL: c.URL, Title: c.Title, Body: c.Body, Status: c.Status,
			Metadata: c.Metadata, Type: c.Type, Priority: c.Priority, Assignees: c.Assignees,
			AssigneeAccountIDs: c.AssigneeAccountIDs, RepositoryIDs: c.RepositoryIDs, AuthorAccountID: c.AuthorAccountID,
			CreatedAt: c.CreatedAt, ClosedAt: c.ClosedAt, IsPossiblyPartial: c.IsPossiblyPartial,
			Phase: c.Phase, LastActivityAt: c.LastActivityAt,
		},
		metadataUpdatedAt: c.MetadataUpdatedAt,
	}
	if p := c.Project; p != nil {
		project := &api.TrailProject{ID: p.ID, Name: p.Name, Provider: p.Provider, Jurisdiction: p.Jurisdiction}
		project.Reference.Forge, project.Reference.Project = p.Reference.Forge, p.Reference.Project
		project.Capabilities.CanManageTrails = p.CanManageTrails
		t.Project, t.ProjectID = project, p.ID
	}
	for _, ch := range c.Changes {
		t.Changes = append(t.Changes, api.ChangeSummary{ID: ch.ID, RepositoryID: ch.RepositoryID, Repository: ch.Repository,
			Branch: ch.Branch, Number: ch.Number, Title: ch.Title, Status: ch.Status, HasCodeChanges: ch.HasCodeChanges,
			CreatedAt: ch.CreatedAt, UpdatedAt: ch.UpdatedAt})
	}
	return t
}

// myProjectTrailsRegion is what one region contributed: its readable items,
// and why its list may be incomplete.
type myProjectTrailsRegion struct {
	items   []myProjectTrail
	partial []string
}

// myProjectTrailsJSON is `trail list --mine --json`. Partial names the
// regions, or the cells' own partial-result reasons, that the list is missing.
type myProjectTrailsJSON struct {
	Items   []myProjectTrail `json:"items"`
	Partial []string         `json:"partial"`
}

// myProjectTrailRegions builds one fan-out group per jurisdiction, routed to
// that jurisdiction's default cluster among those with an API URL.
func myProjectTrailRegions(ctx context.Context) ([]cellGroup, error) {
	ctx, cancel := context.WithTimeout(ctx, cellResolveTimeout)
	defer cancel()
	c, err := newCellCoreClient()
	if err != nil {
		return nil, fmt.Errorf("list regions: %w", err)
	}
	clusters, err := c.ListClusters(ctx)
	if err != nil {
		return nil, fmt.Errorf("list regions: %w", err)
	}
	apiURL := func(cl coreapi.Cluster) string { return strings.TrimRight(strings.TrimSpace(cl.ApiUrl.Or("")), "/") }
	byJurisdiction := defaultClusterByJurisdiction(clusters.Clusters, func(cl coreapi.Cluster) bool { return apiURL(cl) != "" })
	if len(byJurisdiction) == 0 {
		return nil, errors.New("list regions: the cluster catalog names no region with an API URL")
	}
	regions := make([]cellGroup, 0, len(byJurisdiction))
	for j, cl := range byJurisdiction {
		regions = append(regions, cellGroup{jurisdiction: j, baseURL: apiURL(cl)})
	}
	slices.SortFunc(regions, func(a, b cellGroup) int { return strings.Compare(a.jurisdiction, b.jurisdiction) })
	return regions, nil
}

func listMyProjectTrails(cmd *cobra.Command, status string, limit int) (myProjectTrailsJSON, error) {
	ctx := cmd.Context()
	regions, err := myProjectTrailRegions(ctx)
	if err != nil {
		return myProjectTrailsJSON{}, err
	}
	query := url.Values{"per_page": {strconv.Itoa(limit)}, "sort": {"updated"}}
	if status != "" {
		query.Set("status", status)
	}
	results, err := fanOutCells(ctx, trailInsecureHTTP(cmd), myProjectTrailsCellTimeout, regions, func(ctx context.Context, g cellGroup, client *api.Client) (myProjectTrailsRegion, error) {
		return readMyProjectTrailsRegion(ctx, g, client, "/api/v1/me/project-trails?"+query.Encode(), status)
	})
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return myProjectTrailsJSON{}, loginHintErr(err)
		}
		return myProjectTrailsJSON{}, fmt.Errorf("list your trails: %w", err)
	}
	return mergeMyProjectTrails(ctx, results, limit)
}

// readMyProjectTrailsRegion reads one region's page. An item it cannot read,
// one without an ID or activity time, or one outside the requested status
// (which the plain list also refuses) is left out and reported as partial
// rather than failing the region or the list.
func readMyProjectTrailsRegion(ctx context.Context, g cellGroup, client *api.Client, path, status string) (myProjectTrailsRegion, error) {
	var region myProjectTrailsRegion
	resp, err := client.Get(ctx, path)
	if err != nil {
		return region, err //nolint:wrapcheck // the region label is added when reported
	}
	defer resp.Body.Close()
	if err := api.CheckResponse(resp); err != nil {
		return region, err //nolint:wrapcheck // the region label is added when reported
	}
	var page struct {
		Items []json.RawMessage `json:"items"`
		Meta  struct {
			Partial []string `json:"partial"`
		} `json:"meta"`
	}
	if err := api.DecodeJSON(resp, &page); err != nil {
		return region, fmt.Errorf("decode: %w", err)
	}
	region.partial = append(region.partial, page.Meta.Partial...)
	unreadable, mismatched := 0, 0
	for _, raw := range page.Items {
		var item myProjectTrailCellItem
		var reason string
		switch err := json.Unmarshal(raw, &item); {
		case err != nil:
			reason = "decode: " + err.Error()
		case item.ID == "":
			reason = "missing id"
		case item.LastActivityAt.IsZero():
			reason = "missing last_activity_at"
		case status != "" && item.Status != status:
			mismatched++
			logging.Debug(ctx, "trail list --mine: item outside the requested status", "region", g.label(), "requested", status, "status", item.Status)
			continue
		}
		if reason != "" {
			unreadable++
			logging.Debug(ctx, "trail list --mine: unreadable item", "region", g.label(), "reason", reason)
			continue
		}
		region.items = append(region.items, item.toMyProjectTrail())
	}
	if unreadable > 0 {
		region.partial = append(region.partial, fmt.Sprintf("%d trail(s) could not be read", unreadable))
	}
	if mismatched > 0 {
		region.partial = append(region.partial, fmt.Sprintf("%d trail(s) did not match status %s", mismatched, status))
	}
	return region, nil
}

// myProjectTrailsRegionFailure describes a failed region by status, code, and
// request ID: an error body can be an HTML error page, which belongs in the
// debug log, not in the list's output.
func myProjectTrailsRegionFailure(err error) string {
	var he *api.HTTPError
	switch {
	case errors.As(err, &he):
		parts := []string{"status " + strconv.Itoa(he.StatusCode)}
		if he.Code != "" {
			parts = append(parts, "code "+tuiutil.SanitizeTerminalLabel(he.Code))
		}
		if he.RequestID != "" {
			parts = append(parts, "request "+tuiutil.SanitizeTerminalLabel(he.RequestID))
		}
		return strings.Join(parts, ", ")
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	}
	return "could not be reached"
}

// isAuthFailure is a region rejecting the caller's login, or the login itself
// failing for that region's client.
func isAuthFailure(err error) bool {
	return api.IsHTTPErrorStatus(err, http.StatusUnauthorized) || api.IsHTTPErrorStatus(err, http.StatusForbidden) ||
		errors.Is(err, auth.ErrNotLoggedIn) || errors.Is(err, auth.ErrReauthRequired)
}

// myProjectTrailsLoginError is a region rejecting the caller's login. Its
// message names only the region and the failure's status, code, and request
// ID; the response body, which can be an HTML error page, is reachable through
// Unwrap for errors.Is and errors.As but never printed.
type myProjectTrailsLoginError struct {
	region string
	err    error
}

func (e *myProjectTrailsLoginError) Error() string {
	return fmt.Sprintf("list your trails: %s rejected your login (%s)", e.region, myProjectTrailsRegionFailure(e.err))
}

func (e *myProjectTrailsLoginError) Unwrap() error { return e.err }

// mergeMyProjectTrails keeps the newest copy of each trail, orders them by
// latest activity, and caps the page. A failed region is reported in Partial;
// a rejected login (401/403) from any region fails the list, since every
// region is sent the same credentials, and so does every region failing.
func mergeMyProjectTrails(ctx context.Context, results []cellCallResult[myProjectTrailsRegion], limit int) (myProjectTrailsJSON, error) {
	out := myProjectTrailsJSON{Items: []myProjectTrail{}, Partial: []string{}}
	byID := map[string]myProjectTrail{}
	failed := 0
	for _, r := range results {
		label := "region " + r.group.label()
		if r.err != nil {
			logging.Debug(ctx, "trail list --mine: region failed", "region", r.group.label(), "failure", myProjectTrailsRegionFailure(r.err))
			if isAuthFailure(r.err) {
				if errors.Is(r.err, auth.ErrNotLoggedIn) {
					return myProjectTrailsJSON{}, loginHintErr(r.err)
				}
				return myProjectTrailsJSON{}, &myProjectTrailsLoginError{region: label, err: r.err}
			}
			failed++
			out.Partial = append(out.Partial, label+": "+myProjectTrailsRegionFailure(r.err))
			continue
		}
		for _, p := range r.value.partial {
			out.Partial = append(out.Partial, label+": "+p)
		}
		for _, item := range r.value.items {
			if prev, ok := byID[item.ID]; !ok || newerMyProjectTrail(item, prev) {
				byID[item.ID] = item
			}
		}
	}
	if failed > 0 && failed == len(results) {
		return myProjectTrailsJSON{}, fmt.Errorf("list your trails: every region failed (%s)", tuiutil.SanitizeTerminalText(strings.Join(out.Partial, "; ")))
	}
	for _, item := range byID {
		out.Items = append(out.Items, item)
	}
	// Latest activity first, ties by ID descending, as the web sidebar orders.
	slices.SortFunc(out.Items, func(a, b myProjectTrail) int {
		return cmp.Or(b.LastActivityAt.Compare(a.LastActivityAt), strings.Compare(b.ID, a.ID))
	})
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
	}
	return out, nil
}

// newerMyProjectTrail reports whether a is a newer copy of a trail than b: later
// activity, then a later metadata update.
func newerMyProjectTrail(a, b myProjectTrail) bool {
	if c := a.LastActivityAt.Compare(b.LastActivityAt); c != 0 {
		return c > 0
	}
	return a.metadataUpdatedAt.After(b.metadataUpdatedAt)
}

// printMyProjectTrailsWarnings reports an incomplete list on stderr, with or
// without --json.
func printMyProjectTrailsWarnings(errW io.Writer, out myProjectTrailsJSON) {
	for _, p := range out.Partial {
		fmt.Fprintf(errW, "Warning: list may be incomplete: %s\n", tuiutil.SanitizeTerminalText(p))
	}
}

func printMyProjectTrails(w io.Writer, out myProjectTrailsJSON) error {
	if len(out.Items) == 0 {
		fmt.Fprintln(w, "No trails authored by or assigned to you.")
		return nil
	}
	return printTable(w, []string{colHeaderProject, "NUMBER", colHeaderStatus, "PHASE", "ACTIVITY", colHeaderTitle}, out.Items, func(t myProjectTrail) []string {
		project := t.ProjectID
		if t.Project != nil {
			project = t.Project.Reference.Forge + "/" + t.Project.Reference.Project
		}
		return []string{tuiutil.SanitizeTerminalLabel(project), strconv.Itoa(t.Number), tuiutil.SanitizeTerminalLabel(t.Status),
			tuiutil.SanitizeTerminalLabel(t.Phase), t.LastActivityAt.Format(time.RFC3339), tuiutil.SanitizeTerminalLabel(t.Title)}
	})
}

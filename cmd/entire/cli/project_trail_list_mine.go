package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
)

// myProjectTrailsCellTimeout bounds each region's /me/project-trails read.
const myProjectTrailsCellTimeout = 15 * time.Second

// A project trail lives in its project's cell, so a cell's /me/project-trails
// only knows the caller's trails homed there. Unlike other /me reads, which the
// home cell answers alone, "my trails" asks every jurisdiction's default cell
// and merges, the same way the web app's sidebar does. The merged list is one
// bounded page: per-region cursors cannot continue a merged order.

// myProjectTrail is the part of a /me/project-trails item the merge and table
// read; Raw is the item as served, which --json prints.
type myProjectTrail struct {
	ID             string    `json:"id"`
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	Status         string    `json:"status"`
	Phase          string    `json:"phase"`
	LastActivityAt time.Time `json:"last_activity_at"`
	Project        struct {
		Reference struct {
			Forge   string `json:"forge"`
			Project string `json:"project"`
		} `json:"reference"`
	} `json:"project"`
	Raw json.RawMessage `json:"-"`
}

type myProjectTrailsPage struct {
	Items []json.RawMessage `json:"items"`
	Meta  struct {
		Partial []string `json:"partial"`
	} `json:"meta"`
}

// myProjectTrailsJSON is `trail list --mine --json`. Partial names the
// regions, or the cells' own partial-result reasons, that the list is missing.
type myProjectTrailsJSON struct {
	Items   []json.RawMessage `json:"items"`
	Partial []string          `json:"partial"`
}

// myProjectTrailRegions builds one fan-out group per jurisdiction, routed to
// that jurisdiction's default cluster.
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
	byJurisdiction := map[string]cellGroup{}
	defaults := map[string]bool{}
	for _, cl := range clusters.Clusters {
		j := strings.ToLower(strings.TrimSpace(cl.Jurisdiction))
		base := strings.TrimRight(strings.TrimSpace(cl.ApiUrl.Or("")), "/")
		if j == "" || base == "" {
			continue
		}
		if _, seen := byJurisdiction[j]; seen && (defaults[j] || !cl.IsDefault) {
			continue
		}
		byJurisdiction[j] = cellGroup{jurisdiction: j, baseURL: base}
		defaults[j] = cl.IsDefault
	}
	if len(byJurisdiction) == 0 {
		return nil, errors.New("list regions: the cluster catalog names no region with an API URL")
	}
	regions := make([]cellGroup, 0, len(byJurisdiction))
	for _, g := range byJurisdiction {
		regions = append(regions, g)
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
	results, err := fanOutCells(ctx, trailInsecureHTTP(cmd), myProjectTrailsCellTimeout, regions, func(ctx context.Context, _ cellGroup, client *api.Client) (myProjectTrailsPage, error) {
		var page myProjectTrailsPage
		resp, err := client.Get(ctx, "/api/v1/me/project-trails?"+query.Encode())
		if err != nil {
			return page, err //nolint:wrapcheck // the region label is added when reported
		}
		defer resp.Body.Close()
		if err := api.CheckResponse(resp); err != nil {
			return page, err //nolint:wrapcheck // the region label is added when reported
		}
		if err := api.DecodeJSON(resp, &page); err != nil {
			return page, fmt.Errorf("decode: %w", err)
		}
		return page, nil
	})
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return myProjectTrailsJSON{}, loginHintErr(err)
		}
		return myProjectTrailsJSON{}, fmt.Errorf("list your trails: %w", err)
	}
	return mergeMyProjectTrails(ctx, results, limit)
}

// mergeMyProjectTrails orders every region's items by latest activity, drops
// duplicates, and caps the page. A failed region is reported in Partial; only
// every region failing is an error.
func mergeMyProjectTrails(ctx context.Context, results []cellCallResult[myProjectTrailsPage], limit int) (myProjectTrailsJSON, error) {
	out := myProjectTrailsJSON{Items: []json.RawMessage{}, Partial: []string{}}
	var items []myProjectTrail
	seen := map[string]bool{}
	var firstErr error
	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			if firstErr == nil {
				firstErr = r.err
			}
			logging.Debug(ctx, "trail list --mine: region failed", "region", r.group.label(), "error", r.err.Error())
			out.Partial = append(out.Partial, "region "+r.group.label()+": "+r.err.Error())
			continue
		}
		for _, p := range r.value.Meta.Partial {
			out.Partial = append(out.Partial, "region "+r.group.label()+": "+p)
		}
		for _, raw := range r.value.Items {
			var item myProjectTrail
			if err := json.Unmarshal(raw, &item); err != nil {
				return myProjectTrailsJSON{}, fmt.Errorf("decode trail from region %s: %w", r.group.label(), err)
			}
			if seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			item.Raw = raw
			items = append(items, item)
		}
	}
	if failed > 0 && failed == len(results) {
		return myProjectTrailsJSON{}, fmt.Errorf("list your trails: every region failed: %w", firstErr)
	}
	slices.SortStableFunc(items, func(a, b myProjectTrail) int { return b.LastActivityAt.Compare(a.LastActivityAt) })
	if len(items) > limit {
		items = items[:limit]
	}
	for _, item := range items {
		out.Items = append(out.Items, item.Raw)
	}
	return out, nil
}

func printMyProjectTrails(w, errW io.Writer, out myProjectTrailsJSON) error {
	for _, p := range out.Partial {
		fmt.Fprintf(errW, "Warning: list may be incomplete: %s\n", tuiutil.SanitizeTerminalText(p))
	}
	items := make([]myProjectTrail, len(out.Items))
	for i, raw := range out.Items {
		if err := json.Unmarshal(raw, &items[i]); err != nil {
			return fmt.Errorf("decode trail: %w", err)
		}
	}
	if len(items) == 0 {
		fmt.Fprintln(w, "No trails authored by or assigned to you.")
		return nil
	}
	return printTable(w, []string{colHeaderProject, "NUMBER", colHeaderStatus, "PHASE", "ACTIVITY", colHeaderTitle}, items, func(t myProjectTrail) []string {
		project := t.Project.Reference.Forge + "/" + t.Project.Reference.Project
		return []string{tuiutil.SanitizeTerminalLabel(project), strconv.Itoa(t.Number), tuiutil.SanitizeTerminalLabel(t.Status),
			tuiutil.SanitizeTerminalLabel(t.Phase), t.LastActivityAt.Format(time.RFC3339), tuiutil.SanitizeTerminalLabel(t.Title)}
	})
}

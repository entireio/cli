package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/coreapi"
)

const checkpointRepoIndexBudget = 5000

// listCheckpointRepoIndex is the shared, recent-first catalogue for discovery.
// Bound both time and entries so interactive callers cannot walk indefinitely.
func listCheckpointRepoIndex(ctx context.Context) ([]coreapi.RepoIndexEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, err := newCellCoreClient()
	if err != nil {
		return nil, fmt.Errorf("control plane unavailable: %w", err)
	}
	truncated := false
	entries, partial, err := fetchPagesBounded(ctx, checkpointRepoIndexBudget, func(ctx context.Context, cursor string) ([]coreapi.RepoIndexEntry, string, error) {
		params := coreapi.ListReposParams{
			Sort:           coreapi.NewOptString("last_activity_at"),
			Order:          coreapi.NewOptListReposOrder(coreapi.ListReposOrderDesc),
			HasCheckpoints: coreapi.NewOptListReposHasCheckpoints(coreapi.ListReposHasCheckpointsTrue),
		}
		if cursor != "" {
			params.PageToken = coreapi.NewOptString(cursor)
		}
		out, err := client.ListRepos(ctx, params)
		if err != nil {
			return nil, "", fmt.Errorf("list checkpoint repos: %w", err)
		}
		next := out.NextPageToken.Or("")
		if (out.Truncated && next == "") || out.CandidatesIncomplete.Or(false) {
			truncated = true
		}
		return out.Repos, next, nil
	})
	if err != nil {
		return nil, err
	}
	if partial || truncated {
		logging.Warn(ctx, "repo index truncated; repository suggestions may be incomplete")
	}
	return entries, nil
}

// checkpointRepoSlugs preserves server order and excludes empty repositories.
func checkpointRepoSlugs(entries []coreapi.RepoIndexEntry) []string {
	slugs := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.CheckpointCount.Or(0) <= 0 {
			continue
		}
		if slug := checkpointRepoSlug(entry); slug != "" {
			key := strings.ToLower(slug)
			if !seen[key] {
				slugs = append(slugs, slug)
				seen[key] = true
			}
		}
	}
	return slugs
}

// listCompletionRepoIndex makes at most one request. Completion is invoked on
// every TAB press, so it must not share the wizard's multi-page/time budget.
func listCompletionRepoIndex(ctx context.Context, prefix string) ([]coreapi.RepoIndexEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	client, err := newCellCoreClient()
	if err != nil {
		return nil, fmt.Errorf("control plane unavailable: %w", err)
	}
	params := coreapi.ListReposParams{
		Sort:           coreapi.NewOptString("last_activity_at"),
		Order:          coreapi.NewOptListReposOrder(coreapi.ListReposOrderDesc),
		PageSize:       coreapi.NewOptInt32(100),
		HasCheckpoints: coreapi.NewOptListReposHasCheckpoints(coreapi.ListReposHasCheckpointsTrue),
	}
	// Q searches owner/repo, not the forge. Partial forge prefixes must
	// remain local filters or they would exclude unrelated repo names.
	query := strings.ToLower(prefix)
	if (strings.HasPrefix(query, "gh/") || strings.HasPrefix(query, "et/")) && len(query) > 3 {
		params.Q = coreapi.NewOptString(query[3:])
	}
	out, err := client.ListRepos(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("list completion repos: %w", err)
	}
	// More pages are deliberately ignored; typing a narrower prefix can
	// surface less-recent repos without making completion walk the index.
	return out.Repos, nil
}

func checkpointRepoSlug(entry coreapi.RepoIndexEntry) string {
	forge, _ := forgeOfEntry(entry)
	name := strings.Trim(strings.TrimSpace(entry.FullName), "/")
	if forge == "" || name == "" {
		return ""
	}
	// A two-component name can have an owner/project named after its forge.
	// Only a three-component name can already be forge-qualified.
	if strings.Count(name, "/") == 2 && strings.HasPrefix(name, forge+"/") {
		return name
	}
	return forge + "/" + name
}

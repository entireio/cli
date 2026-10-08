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
		params := coreapi.ListReposParams{Sort: coreapi.NewOptString("last_activity_at"), Order: coreapi.NewOptListReposOrder(coreapi.ListReposOrderDesc)}
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
	for _, entry := range entries {
		if entry.CheckpointCount.Or(0) <= 0 {
			continue
		}
		if slug := checkpointRepoSlug(entry); slug != "" {
			slugs = append(slugs, slug)
		}
	}
	return slugs
}

func checkpointRepoSlug(entry coreapi.RepoIndexEntry) string {
	forge, _ := forgeOfEntry(entry)
	name := strings.Trim(strings.TrimSpace(entry.FullName), "/")
	if forge == "" || name == "" {
		return ""
	}
	// Native entries can already carry their forge in full_name.
	return forge + "/" + strings.TrimPrefix(name, forge+"/")
}

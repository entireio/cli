package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// runSearchLocal is the --local entry point invoked from the search command.
// It resolves the local owner/repo best-effort (for result enrichment),
// performs the local search, and writes JSON or a static table.
//
// The local path intentionally skips the TUI: the remote TUI re-issues
// queries against the search service as the user types, which would
// require either special-casing the dispatcher or duplicating the TUI
// model. A static table is good enough as a fallback surface.
func runSearchLocal(ctx context.Context, w io.Writer, query, branch string, jsonOutput, isTerminal bool, limit, page int) error {
	owner, repoName := localOriginIdentity(ctx)
	resp, err := runLocalSearch(ctx, localSearchInput{
		Query:  query,
		Branch: branch,
		Owner:  owner,
		Repo:   repoName,
	})
	if err != nil {
		return fmt.Errorf("local search failed: %w", err)
	}

	if jsonOutput || !isTerminal {
		return writeSearchJSON(w, resp, limit, page)
	}

	styles := newStatusStyles(w)
	if len(resp.Results) == 0 {
		fmt.Fprintln(w, "No local checkpoints matched.")
		return nil
	}
	renderSearchStatic(w, resp.Results, query, resp.Total, styles)
	return nil
}

// localOriginIdentity resolves the GitHub owner/repo from the local
// origin remote on a best-effort basis. Failures are silently swallowed
// because local search must work even when there's no GitHub origin
// (e.g. on a clean repro repo, or one that points at a non-GitHub host).
func localOriginIdentity(ctx context.Context) (string, string) {
	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		return "", ""
	}
	remote, err := repo.Remote("origin")
	if err != nil {
		return "", ""
	}
	urls := remote.Config().URLs
	if len(urls) == 0 {
		return "", ""
	}
	owner, repoName, err := search.ParseGitHubRemote(urls[0])
	if err != nil {
		return "", ""
	}
	return owner, repoName
}

// localSearchInput holds the inputs for a local checkpoint search.
type localSearchInput struct {
	Query  string // case-insensitive substring; empty matches all
	Branch string // exact-match filter on checkpoint branch
	Owner  string // origin owner for result enrichment (best-effort)
	Repo   string // origin repo for result enrichment (best-effort)
}

// runLocalSearch walks entire/checkpoints/v1 in the current repo and returns
// checkpoints whose text content contains the query (case-insensitive). The
// search covers branch, file paths, prompts, and transcript text. An empty
// query matches every checkpoint that passes the filters.
//
// This is a CLI-only fallback for the remote search service. Use it when
// the remote index is lagging, unavailable, or scoped to a different
// GitHub repo than the local working copy (e.g. when checkpoints are
// pushed to a dedicated `<repo>-checkpoints-private` mirror).
func runLocalSearch(ctx context.Context, in localSearchInput) (*search.Response, error) {
	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("open repository: %w", err)
	}
	return localSearchWithStore(ctx, checkpoint.NewGitStore(repo), in)
}

// localSearchWithStore is the pure matching logic, exposed for tests so they
// can build a synthetic store without going through OpenRepository (which
// resolves the worktree via CWD and is incompatible with t.Parallel).
func localSearchWithStore(ctx context.Context, store *checkpoint.GitStore, in localSearchInput) (*search.Response, error) {
	infos, err := store.ListCommitted(ctx)
	if err != nil {
		return nil, fmt.Errorf("list local checkpoints: %w", err)
	}

	needle := strings.ToLower(strings.TrimSpace(in.Query))
	branchFilter := strings.TrimSpace(in.Branch)

	matches := make([]search.Result, 0, len(infos))
	for _, info := range infos {
		summary, err := store.ReadCommitted(ctx, info.CheckpointID)
		if err != nil || summary == nil {
			continue
		}
		if branchFilter != "" && summary.Branch != branchFilter {
			continue
		}

		prompt, transcript := readLatestSessionText(ctx, store, info.CheckpointID, summary)

		matched := needle == ""
		snippet := ""
		if !matched {
			for _, candidate := range []string{
				prompt,
				string(transcript),
				strings.Join(summary.FilesTouched, " "),
				summary.Branch,
			} {
				if candidate == "" {
					continue
				}
				lower := strings.ToLower(candidate)
				idx := strings.Index(lower, needle)
				if idx < 0 {
					continue
				}
				matched = true
				snippet = makeLocalSnippet(candidate, idx, len(needle))
				break
			}
		}
		if !matched {
			continue
		}

		matches = append(matches, search.Result{
			Type: "checkpoint",
			Data: search.CheckpointResult{
				ID:           info.CheckpointID.String(),
				Prompt:       strings.TrimSpace(firstLine(strings.TrimSpace(prompt))),
				Branch:       summary.Branch,
				Org:          in.Owner,
				Repo:         in.Repo,
				CreatedAt:    info.CreatedAt.UTC().Format(time.RFC3339),
				FilesTouched: summary.FilesTouched,
			},
			Meta: search.Meta{
				MatchType: "local",
				Score:     1.0,
				Snippet:   snippet,
			},
		})
	}

	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].Data.CreatedAt > matches[j].Data.CreatedAt
	})

	return &search.Response{Results: matches, Total: len(matches), Page: 1}, nil
}

// readLatestSessionText reads the latest session's prompts and transcript.
// Returns empty strings on any error so callers treat missing content as
// non-matching rather than failing the whole search — checkpoints written
// by other agents or older CLI versions are still listable, just not
// fully grep-able.
func readLatestSessionText(ctx context.Context, store *checkpoint.GitStore, cpID id.CheckpointID, summary *checkpoint.CheckpointSummary) (string, []byte) {
	if summary == nil || len(summary.Sessions) == 0 {
		return "", nil
	}
	latest := len(summary.Sessions) - 1
	content, err := store.ReadSessionContent(ctx, cpID, latest)
	if err != nil || content == nil {
		return "", nil
	}
	return content.Prompts, content.Transcript
}

// makeLocalSnippet returns a short windowed slice around idx for display.
// Newlines are folded to spaces so the snippet renders on a single line in
// the table view.
func makeLocalSnippet(text string, idx, qlen int) string {
	const window = 40
	if idx < 0 || idx >= len(text) {
		return ""
	}
	start := idx - window
	if start < 0 {
		start = 0
	}
	end := idx + qlen + window
	if end > len(text) {
		end = len(text)
	}
	s := text[start:end]
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

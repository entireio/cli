package codesearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

const maxResponseBytes = 8 << 20 // 8 MiB — code search results with context lines can be large

// SearchRequest is the request body for peregrine's /search/api/search endpoint.
type SearchRequest struct {
	Query         string   `json:"query"`
	Repos         []string `json:"repos,omitempty"`
	MaxResults    int      `json:"max_results,omitempty"`
	MaxFiles      int      `json:"max_files,omitempty"`
	CaseSensitive bool     `json:"case_sensitive,omitempty"`
}

// Stats holds aggregate search statistics.
type Stats struct {
	TotalMatches  int     `json:"total_matches"`
	TotalFiles    int     `json:"total_files"`
	DurationMs    float64 `json:"duration_ms"`
	ReposSearched int     `json:"repos_searched"`
}

// RepoStats holds per-repo match statistics.
type RepoStats struct {
	Repo       string `json:"repo"`
	MatchCount int    `json:"match_count"`
	FileCount  int    `json:"file_count"`
}

// Result is a single code search match from peregrine.
type Result struct {
	Repo          string   `json:"repo"`
	Path          string   `json:"path"`
	Line          int      `json:"line"`
	Column        int      `json:"column"`
	ContextBefore []string `json:"context_before"`
	ContextLine   string   `json:"context_line"`
	ContextAfter  []string `json:"context_after"`
	Score         float64  `json:"score"`
}

// SearchResponse is peregrine's code search response.
type SearchResponse struct {
	Query     string      `json:"query"`
	Stats     Stats       `json:"stats"`
	RepoStats []RepoStats `json:"repo_stats"`
	Results   []Result    `json:"results"`
}

// Search calls peregrine's code search endpoint through entire-api.
// The client must already be authenticated against the cell.
// The caller controls the context deadline/timeout.
func Search(ctx context.Context, client *api.Client, req SearchRequest) (*SearchResponse, error) {
	resp, err := client.Post(ctx, "/search/api/search", req)
	if err != nil {
		return nil, fmt.Errorf("code search request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading code search response: %w", err)
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, fmt.Errorf("code search response exceeds %d bytes", maxResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &api.HTTPError{StatusCode: resp.StatusCode}
		var parsed api.ErrorResponse
		if json.Unmarshal(body, &parsed) == nil {
			if msg := parsed.Message(); msg != "" {
				apiErr.Message = msg
			}
		}
		if apiErr.Message == "" && len(body) > 0 {
			apiErr.Message = string(body)
		}
		return nil, fmt.Errorf("code search: %w", apiErr)
	}

	var result SearchResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decoding code search response: %w", err)
	}

	return &result, nil
}

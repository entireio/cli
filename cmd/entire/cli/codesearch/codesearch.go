package codesearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

const maxResponseBytes = 8 << 20 // 8 MiB — code search results with context lines can be large

// SearchRequest holds the parameters for a code search call to peregrine
// via the cell's entire-api gateway at GET /api/v1/search/api/search.
type SearchRequest struct {
	Query         string
	Repos         []string
	MaxResults    int
	CaseSensitive bool
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

	// FailedJurisdictions is set by the CLI's merge layer (not by peregrine)
	// when one or more cells failed during multi-region fan-out.
	FailedJurisdictions []string `json:"failed_jurisdictions,omitempty"`
}

// Search calls peregrine's code search endpoint through the cell's entire-api
// gateway: GET /api/v1/search/api/search?q=...&max_results=...&repo=...
// The client must already be authenticated against the cell.
func Search(ctx context.Context, client *api.Client, req SearchRequest) (*SearchResponse, error) {
	params := url.Values{}
	params.Set("q", req.Query)
	if req.MaxResults > 0 {
		params.Set("max_results", strconv.Itoa(req.MaxResults))
	}
	if req.CaseSensitive {
		// ponytail: peregrine's proto does not yet define case_sensitive;
		// the param is sent optimistically so it takes effect once
		// peregrine adds support without a CLI release.
		params.Set("case_sensitive", "true")
	}
	for _, r := range req.Repos {
		params.Add("repo", r)
	}
	searchPath := "/api/v1/search/api/search?" + params.Encode()

	resp, err := client.Get(ctx, searchPath)
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
			apiErr.Message = strings.TrimSpace(string(body))
		}
		return nil, fmt.Errorf("code search: %w", apiErr)
	}

	var result SearchResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decoding code search response: %w", err)
	}

	return &result, nil
}

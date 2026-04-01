package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const apiTimeout = 30 * time.Second

// DefaultServiceURL is the production search service URL.
const DefaultServiceURL = "https://entire.io"

// WildcardQuery is the query string used when only filters are provided (no search terms).
const WildcardQuery = "*"

// AllReposFilter is the inline repo filter value that disables repo scoping.
const AllReposFilter = "*"

// MaxLimit is the maximum number of results the search API will return per request.
const MaxLimit = 200

// Meta contains search ranking metadata for a result.
type Meta struct {
	MatchType string  `json:"matchType"`
	Score     float64 `json:"score"`
	Snippet   string  `json:"snippet,omitempty"`
}

// CheckpointResult represents a checkpoint returned by the search service.
type CheckpointResult struct {
	ID             string   `json:"id"`
	Prompt         string   `json:"prompt"`
	CommitMessage  *string  `json:"commitMessage"`
	CommitSHA      *string  `json:"commitSha"`
	Branch         string   `json:"branch"`
	Org            string   `json:"org"`
	Repo           string   `json:"repo"`
	Author         string   `json:"author"`
	AuthorUsername *string  `json:"authorUsername"`
	CreatedAt      string   `json:"createdAt"`
	FilesTouched   []string `json:"filesTouched"`
}

// Result wraps a search result with its type and ranking metadata.
type Result struct {
	Type string           `json:"type"`
	Data CheckpointResult `json:"data"`
	Meta Meta             `json:"searchMeta"`
}

// Response is the search service response.
type Response struct {
	Results []Result `json:"results"`
	Total   int      `json:"total"`
	Page    int      `json:"page"`
	Error   string   `json:"error,omitempty"`
}

// Config holds the configuration for a search request.
type Config struct {
	ServiceURL  string // Base URL of the search service
	GitHubToken string
	Owner       string
	Repo        string
	Repos       []string
	Query       string
	Limit       int
	Author      string // Filter by author name
	Date        string // Filter by time period: "week" or "month"
	Branch      string // Filter by branch name
	Page        int    // 1-based page number (0 means omit, API defaults to 1)
}

// HasFilters reports whether any filter fields are set on the config.
func (c Config) HasFilters() bool {
	return c.Author != "" || c.Date != "" || c.Branch != "" || len(c.Repos) > 0
}

// ParsedInput holds the parsed query and optional filters extracted from search input.
type ParsedInput struct {
	Query  string
	Author string
	Date   string
	Branch string
	Repos  []string
}

// ParseSearchInput extracts filter prefixes from raw input.
// Supports quoted values for single-value filters, for example: author:"alice smith".
// Remaining tokens become the query.
func ParseSearchInput(raw string) ParsedInput {
	var p ParsedInput
	var queryParts []string

	tokens := tokenizeInput(raw)
	for _, tok := range tokens {
		switch {
		case strings.HasPrefix(tok, "author:"):
			p.Author = strings.Trim(tok[len("author:"):], "\"")
		case strings.HasPrefix(tok, "date:"):
			p.Date = strings.Trim(tok[len("date:"):], "\"")
		case strings.HasPrefix(tok, "branch:"):
			p.Branch = strings.Trim(tok[len("branch:"):], "\"")
		case strings.HasPrefix(tok, "repo:"):
			p.Repos = appendUnique(p.Repos, parseListFilter(strings.TrimPrefix(tok, "repo:"))...)
		default:
			queryParts = append(queryParts, tok)
		}
	}

	p.Query = strings.Join(queryParts, " ")
	return p
}

// tokenizeInput splits input on whitespace but respects quoted values after filter prefixes.
// Example: `author:"alice smith" fix bug` → ["author:\"alice smith\"", "fix", "bug"]
func tokenizeInput(s string) []string {
	var tokens []string
	i := 0
	s = strings.TrimSpace(s)
	for i < len(s) {
		// Skip whitespace
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}

		start := i

		// Look ahead: is this a prefix:"quoted" token?
		if colonIdx := strings.Index(s[i:], ":\""); colonIdx >= 0 && !strings.Contains(s[i:i+colonIdx], " ") {
			// Found prefix:" — scan to closing quote
			quoteStart := i + colonIdx + 2
			endQuote := strings.IndexByte(s[quoteStart:], '"')
			if endQuote >= 0 {
				i = quoteStart + endQuote + 1
				tokens = append(tokens, s[start:i])
				continue
			}
		}

		// Regular token: advance to next space
		for i < len(s) && s[i] != ' ' {
			i++
		}
		tokens = append(tokens, s[start:i])
	}
	return tokens
}

func parseListFilter(raw string) []string {
	if raw == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.Trim(strings.TrimSpace(part), "\"")
		if value == "" {
			continue
		}
		values = append(values, value)
	}

	return values
}

// ValidateRepoFilters ensures repo filters match backend semantics.
func ValidateRepoFilters(repos []string) error {
	if len(repos) > 1 {
		return errors.New("only one explicit repo filter is currently supported")
	}
	if len(repos) == 1 && !isValidRepoFilter(repos[0]) {
		return fmt.Errorf(
			"invalid repo filter %q: expected owner/name or *; if you meant all repos, quote the asterisk: --repo '*'",
			repos[0],
		)
	}
	return nil
}

func isValidRepoFilter(repo string) bool {
	if repo == AllReposFilter {
		return true
	}
	if strings.Contains(repo, " ") {
		return false
	}
	parts := strings.Split(repo, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

func appendUnique(existing []string, values ...string) []string {
	if len(values) == 0 {
		return existing
	}

	seen := make(map[string]struct{}, len(existing)+len(values))
	for _, value := range existing {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		existing = append(existing, value)
	}

	return existing
}

var httpClient = &http.Client{}

// Search calls the search service to perform a hybrid search.
func Search(ctx context.Context, cfg Config) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	serviceURL := cfg.ServiceURL
	if serviceURL == "" {
		serviceURL = DefaultServiceURL
	}

	u, err := url.Parse(serviceURL)
	if err != nil {
		return nil, fmt.Errorf("parsing service URL: %w", err)
	}
	u.Path = "/search/v1/search"

	q := u.Query()
	q.Set("q", cfg.Query)
	if err := ValidateRepoFilters(cfg.Repos); err != nil {
		return nil, err
	}
	allRepos := len(cfg.Repos) == 1 && cfg.Repos[0] == AllReposFilter
	if len(cfg.Repos) > 0 && !allRepos {
		for _, repo := range cfg.Repos {
			q.Add("repo", repo)
		}
	} else if len(cfg.Repos) == 0 && cfg.Owner != "" && cfg.Repo != "" {
		q.Set("repo", cfg.Owner+"/"+cfg.Repo)
	}
	q.Set("types", "checkpoints")
	if cfg.Limit > 0 {
		q.Set("limit", strconv.Itoa(cfg.Limit))
	}
	if cfg.Author != "" {
		q.Set("author", cfg.Author)
	}
	if cfg.Date != "" {
		q.Set("date", cfg.Date)
	}
	if cfg.Branch != "" {
		q.Set("branch", cfg.Branch)
	}
	if cfg.Page > 0 {
		q.Set("page", strconv.Itoa(cfg.Page))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
	req.Header.Set("User-Agent", "entire-cli")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling search service: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			return nil, fmt.Errorf("search service error (%d): %s", resp.StatusCode, errResp.Error)
		}
		return nil, fmt.Errorf("search service returned %d: %s", resp.StatusCode, string(body))
	}

	var result Response
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unexpected response from search service: %s", string(body))
	}

	if result.Error != "" {
		return nil, fmt.Errorf("search service error: %s", result.Error)
	}

	return &result, nil
}

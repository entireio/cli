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

	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

const apiTimeout = 30 * time.Second

// DefaultServiceURL is the production search service URL.
const DefaultServiceURL = "https://entire.io"

// WildcardQuery is the query string used when only filters are provided (no search terms).
const WildcardQuery = "*"

// AllReposFilter is the inline repo filter value that disables repo scoping.
const AllReposFilter = "*"

// Result type constants.
const (
	TypeCheckpoint = "checkpoint"
	TypeCommit     = "commit"
	TypeSession    = "session"
)

// MaxLimit is the maximum number of results the search API will return per request.
const MaxLimit = 200

// DefaultLimit is the default number of results to fetch per request, matching the UI.
const DefaultLimit = 100

// Meta contains search ranking metadata for a result.
type Meta struct {
	MatchType string   `json:"matchType"`
	Score     float64  `json:"score"`
	Tier      *int     `json:"tier,omitempty"`
	Snippet   string   `json:"snippet,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	BM25Score *float64 `json:"bm25Score,omitempty"`
	ANNScore  *float64 `json:"annScore,omitempty"`
}

// CheckpointResult represents a checkpoint returned by the search service.
type CheckpointResult struct {
	ID             string   `json:"id"`
	Prompt         string   `json:"prompt"`
	CommitMessage  *string  `json:"commitMessage"`
	CommitSubject  *string  `json:"commitSubject"`
	CommitSHA      *string  `json:"commitSha"`
	Branch         string   `json:"branch"`
	Org            string   `json:"org"`
	Repo           string   `json:"repo"`
	Author         string   `json:"author"`
	AuthorUsername *string  `json:"authorUsername"`
	CreatedAt      string   `json:"createdAt"`
	FilesTouched   []string `json:"filesTouched"`
}

// CommitResult represents a commit returned by the search service.
type CommitResult struct {
	ID             string  `json:"id"`
	CommitSHA      string  `json:"commitSha"`
	CommitMessage  string  `json:"commitMessage"`
	CommitSubject  string  `json:"commitSubject"`
	Branch         string  `json:"branch"`
	Org            string  `json:"org"`
	Repo           string  `json:"repo"`
	Author         string  `json:"author"`
	AuthorUsername *string `json:"authorUsername"`
	CreatedAt      string  `json:"createdAt"`
	Additions      int     `json:"additions"`
	Deletions      int     `json:"deletions"`
	FilesChanged   int     `json:"filesChanged"`
	HTMLUrl        *string `json:"htmlUrl"`
}

// SessionResult represents a session returned by the search service.
type SessionResult struct {
	SessionID      string  `json:"sessionId"`
	DisplayName    string  `json:"displayName"`
	Prompt         *string `json:"prompt"`
	Agent          *string `json:"agent"`
	Model          *string `json:"model"`
	StepCount      int     `json:"stepCount"`
	Org            string  `json:"org"`
	Repo           string  `json:"repo"`
	Branch         *string `json:"branch"`
	AuthorUsername *string `json:"authorUsername"`
	CreatedAt      string  `json:"createdAt"`
}

// Result wraps a search result with its type and ranking metadata.
// Exactly one of Checkpoint, Commit, or Session is non-nil based on Type.
type Result struct {
	Type       string            `json:"-"`
	Meta       Meta              `json:"-"`
	Checkpoint *CheckpointResult `json:"-"`
	Commit     *CommitResult     `json:"-"`
	Session    *SessionResult    `json:"-"`

	// rawData preserves the original JSON for unknown types (repo, pr)
	rawData json.RawMessage
}

// resultJSON is the wire format for JSON marshaling/unmarshaling.
type resultJSON struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
	Meta Meta            `json:"searchMeta"`
}

// MarshalJSON implements custom JSON marshaling to produce the API wire format.
func (r *Result) MarshalJSON() ([]byte, error) {
	var data any
	switch r.Type {
	case TypeCheckpoint:
		data = r.Checkpoint
	case TypeCommit:
		data = r.Commit
	case TypeSession:
		data = r.Session
	default:
		data = r.rawData
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshaling result data: %w", err)
	}
	out, err := json.Marshal(resultJSON{
		Type: r.Type,
		Data: raw,
		Meta: r.Meta,
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling result: %w", err)
	}
	return out, nil
}

// UnmarshalJSON implements custom JSON unmarshaling to parse typed data.
func (r *Result) UnmarshalJSON(b []byte) error {
	var raw resultJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("unmarshaling result: %w", err)
	}
	r.Type = raw.Type
	r.Meta = raw.Meta
	r.rawData = raw.Data
	// Clear any previously-decoded payloads so a reused Result keeps the
	// "exactly one typed pointer is non-nil" invariant.
	r.Checkpoint, r.Commit, r.Session = nil, nil, nil

	switch raw.Type {
	case TypeCheckpoint:
		var d CheckpointResult
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return fmt.Errorf("unmarshaling checkpoint data: %w", err)
		}
		r.Checkpoint = &d
	case TypeCommit:
		var d CommitResult
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return fmt.Errorf("unmarshaling commit data: %w", err)
		}
		r.Commit = &d
	case TypeSession:
		var d SessionResult
		if err := json.Unmarshal(raw.Data, &d); err != nil {
			return fmt.Errorf("unmarshaling session data: %w", err)
		}
		r.Session = &d
	}
	return nil
}

// resultField dispatches to the accessor matching the result's type, guarding
// against a nil payload (returns "" for nil or unknown types like repo/pr).
func resultField(r *Result, fromCheckpoint func(*CheckpointResult) string, fromCommit func(*CommitResult) string, fromSession func(*SessionResult) string) string {
	switch r.Type {
	case TypeCheckpoint:
		if r.Checkpoint != nil {
			return fromCheckpoint(r.Checkpoint)
		}
	case TypeCommit:
		if r.Commit != nil {
			return fromCommit(r.Commit)
		}
	case TypeSession:
		if r.Session != nil {
			return fromSession(r.Session)
		}
	}
	return ""
}

// ResultOrg returns the org for any result type.
func (r *Result) ResultOrg() string {
	return resultField(r,
		func(c *CheckpointResult) string { return c.Org },
		func(c *CommitResult) string { return c.Org },
		func(s *SessionResult) string { return s.Org })
}

// ResultRepo returns the repo for any result type.
func (r *Result) ResultRepo() string {
	return resultField(r,
		func(c *CheckpointResult) string { return c.Repo },
		func(c *CommitResult) string { return c.Repo },
		func(s *SessionResult) string { return s.Repo })
}

// ResultBranch returns the branch for any result type.
func (r *Result) ResultBranch() string {
	return resultField(r,
		func(c *CheckpointResult) string { return c.Branch },
		func(c *CommitResult) string { return c.Branch },
		func(s *SessionResult) string {
			if s.Branch != nil {
				return *s.Branch
			}
			return ""
		})
}

// ResultCreatedAt returns the createdAt for any result type.
func (r *Result) ResultCreatedAt() string {
	return resultField(r,
		func(c *CheckpointResult) string { return c.CreatedAt },
		func(c *CommitResult) string { return c.CreatedAt },
		func(s *SessionResult) string { return s.CreatedAt })
}

// ResultAuthor returns the display author for any result type.
func (r *Result) ResultAuthor() string {
	return resultField(r,
		func(c *CheckpointResult) string {
			if c.AuthorUsername != nil && *c.AuthorUsername != "" {
				return *c.AuthorUsername
			}
			return c.Author
		},
		func(c *CommitResult) string {
			if c.AuthorUsername != nil && *c.AuthorUsername != "" {
				return *c.AuthorUsername
			}
			return c.Author
		},
		func(s *SessionResult) string {
			if s.AuthorUsername != nil {
				return *s.AuthorUsername
			}
			return ""
		})
}

// ResultID returns the primary ID for any result type.
func (r *Result) ResultID() string {
	return resultField(r,
		func(c *CheckpointResult) string { return c.ID },
		func(c *CommitResult) string { return c.CommitSHA },
		func(s *SessionResult) string { return s.SessionID })
}

// ResultTitle returns the primary display text for any result type.
func (r *Result) ResultTitle() string {
	return resultField(r,
		func(c *CheckpointResult) string {
			// Prefer the commit title over the prompt; fall back to the prompt
			// for uncommitted checkpoints. The full prompt remains in the detail view.
			if c.CommitSubject != nil && *c.CommitSubject != "" {
				return *c.CommitSubject
			}
			if c.CommitMessage != nil && *c.CommitMessage != "" {
				return *c.CommitMessage
			}
			return c.Prompt
		},
		func(c *CommitResult) string {
			if c.CommitSubject != "" {
				return c.CommitSubject
			}
			return c.CommitMessage
		},
		func(s *SessionResult) string { return s.DisplayName })
}

// TypeCounts holds per-type result counts.
type TypeCounts struct {
	Repos       int `json:"repos"`
	Checkpoints int `json:"checkpoints"`
	Commits     int `json:"commits"`
	PRs         int `json:"prs"`
	Sessions    int `json:"sessions"`
}

// Timing holds search performance timing data.
type Timing struct {
	TotalMs            *float64 `json:"total_ms"`
	KeywordMs          *float64 `json:"keyword_ms"`
	EmbeddingMs        *float64 `json:"embedding_ms"`
	VectorMs           *float64 `json:"vector_ms"`
	RerankMs           *float64 `json:"rerank_ms"`
	FanoutMs           *float64 `json:"fanout_ms"`
	SessionHydrationMs *float64 `json:"session_hydration_ms"`
}

// Response is the search service response.
type Response struct {
	Results  []Result    `json:"results"`
	Total    int         `json:"total"`
	Page     int         `json:"page"`
	Error    string      `json:"error,omitempty"`
	Timing   *Timing     `json:"timing,omitempty"`
	Reranked *bool       `json:"reranked,omitempty"`
	Counts   *TypeCounts `json:"counts,omitempty"`
}

// Config holds the configuration for a search request.
type Config struct {
	ServiceURL  string // Base URL of the search service
	GitHubToken string
	Owner       string
	Repo        string
	Repos       []string
	AllRepos    bool // When true, search all accessible repos (no repo scoping)
	Query       string
	Limit       int
	Author      string // Filter by author name
	Date        string // Filter by time period: "week" or "month"
	Branch      string // Filter by branch name
	Page        int    // 1-based page number (0 means omit, API defaults to 1)
}

// HasFilters reports whether any filter fields are set on the config.
func (c Config) HasFilters() bool {
	return c.Author != "" || c.Date != "" || c.Branch != "" || len(c.Repos) > 0 || c.AllRepos
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
	allRepos := cfg.AllRepos || (len(cfg.Repos) == 1 && cfg.Repos[0] == AllReposFilter)
	hasExplicitRepo := false
	for _, repo := range cfg.Repos {
		if repo != AllReposFilter {
			hasExplicitRepo = true
			break
		}
	}
	switch {
	case hasExplicitRepo:
		// An explicit owner/name filter always scopes the search, even when
		// --all-repos is also set (the more specific filter wins).
		for _, repo := range cfg.Repos {
			if repo != AllReposFilter {
				q.Add("repo", repo)
			}
		}
	case allRepos:
		// No repo scoping — search every accessible repo.
	case cfg.Owner != "" && cfg.Repo != "":
		q.Set("repo", cfg.Owner+"/"+cfg.Repo)
	}
	// Don't set types — let the API return all types (checkpoints, commits, sessions, etc.)
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
	req.Header.Set("User-Agent", versioninfo.UserAgent())

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

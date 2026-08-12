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

	ulid "github.com/oklog/ulid/v2"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

const apiTimeout = 30 * time.Second

// v4ServicePath is the per-repo v4 query-serve route exposed by the entire-api
// cell gateway. It takes repo=<ULID>. The BFF (entire.io /api/v1/search)
// forwards to this same path; the CLI dials the cell directly with a
// jurisdictional identity token, skipping the BFF hop.
const v4ServicePath = "/api/v1/semantic-search/search/v1/search"

// ErrCellUnavailable reports that a cell's gateway does not expose the
// semantic-search route at all (HTTP 404 at the route level) — query-serve is
// not deployed in that cell yet. Callers fanning out across cells match it
// with errors.Is and skip the cell quietly instead of warning the user about
// a "failed" region.
var ErrCellUnavailable = errors.New("semantic search is not available in this cell")

// ErrRepoFilterUnmatched reports that query-serve answered (the route exists)
// but the explicit repo filter matched nothing the caller can search — the
// repo isn't indexed yet, or its owner org isn't enabled on the
// semantic-search feature flag (entire-search fails closed with a JSON 404,
// existence not disclosed). A typo'd repo can't produce this from the CLI:
// the slug was already resolved against the control-plane index before any
// cell was contacted. Distinct from ErrCellUnavailable so fan-out callers
// don't misreport a repo-level miss as a region without query-serve.
var ErrRepoFilterUnmatched = errors.New("no requested repo was found in this cell")

// WildcardQuery is the query string used when only filters are provided (no search terms).
const WildcardQuery = "*"

// AllReposFilter is the inline repo filter value that disables repo scoping.
const AllReposFilter = "*"

// Result type constants.
const (
	TypeCheckpoint = "checkpoint"
	TypeCommit     = "commit"
	TypeSession    = "session"
	// TypeRepo and TypePR are returned by the backend but have no typed struct
	// (decoded via rawData). They're named so the cross-cell v4 merge can bucket
	// and tally them without string literals.
	TypeRepo = "repo"
	TypePR   = "pr"
)

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

	// rawData preserves the original JSON for unknown types (repo, pr).
	rawData json.RawMessage
	// rawFields is rawData decoded once at unmarshal time, and only for types
	// without a typed struct (repo, pr) — accessors never read raw fields for
	// typed rows, so a backend field addition cannot change what they return.
	rawFields map[string]json.RawMessage
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
	r.rawFields = nil

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
	default:
		// Unknown types (repo, pr): decode the payload once so accessors can
		// read identifying fields without re-parsing per call (the TUI calls
		// them per row per render).
		_ = json.Unmarshal(raw.Data, &r.rawFields) //nolint:errcheck // best-effort; accessors return "" when nil
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

// rawString returns the first non-empty string value among the given keys in
// the raw payload of a result without a typed struct (repo, pr). Returns ""
// for typed results — rawFields is only populated for unknown types — or when
// no key matches.
func (r *Result) rawString(keys ...string) string {
	for _, k := range keys {
		var s string
		if err := json.Unmarshal(r.rawFields[k], &s); err == nil && s != "" {
			return s
		}
	}
	return ""
}

// ResultOrg returns the org for any result type. Repo/PR raw payloads may
// only carry an owner-qualified "fullName"; its owner segment is the org.
func (r *Result) ResultOrg() string {
	if v := resultField(r,
		func(c *CheckpointResult) string { return c.Org },
		func(c *CommitResult) string { return c.Org },
		func(s *SessionResult) string { return s.Org }); v != "" {
		return v
	}
	if v := r.rawString("org"); v != "" {
		return v
	}
	owner, _ := splitFullName(r.rawString("fullName"))
	return owner
}

// ResultRepo returns the bare repo name (no owner) for any result type, so
// callers can join it with ResultOrg without doubling the owner. Repo/PR raw
// payloads carry it under "repo" or "name", or qualified inside "fullName".
func (r *Result) ResultRepo() string {
	if v := resultField(r,
		func(c *CheckpointResult) string { return c.Repo },
		func(c *CommitResult) string { return c.Repo },
		func(s *SessionResult) string { return s.Repo }); v != "" {
		return v
	}
	if v := r.rawString("repo", "name"); v != "" {
		return v
	}
	_, name := splitFullName(r.rawString("fullName"))
	return name
}

// splitFullName splits an "owner/repo" full name; without a slash the whole
// value is the repo name.
func splitFullName(fullName string) (owner, name string) {
	if i := strings.IndexByte(fullName, '/'); i >= 0 {
		return fullName[:i], fullName[i+1:]
	}
	return "", fullName
}

// ResultBranch returns the branch for any result type. PR raw payloads carry
// the head branch under "headBranch" (searcher.PRResult).
func (r *Result) ResultBranch() string {
	if v := resultField(r,
		func(c *CheckpointResult) string { return c.Branch },
		func(c *CommitResult) string { return c.Branch },
		func(s *SessionResult) string {
			if s.Branch != nil {
				return *s.Branch
			}
			return ""
		}); v != "" {
		return v
	}
	return r.rawString("headBranch")
}

// ResultCreatedAt returns the createdAt for any result type.
func (r *Result) ResultCreatedAt() string {
	if v := resultField(r,
		func(c *CheckpointResult) string { return c.CreatedAt },
		func(c *CommitResult) string { return c.CreatedAt },
		func(s *SessionResult) string { return s.CreatedAt }); v != "" {
		return v
	}
	return r.rawString("createdAt")
}

// ResultAuthor returns the display author for any result type. PR raw payloads
// carry the author login under "userLogin" (searcher.PRResult).
func (r *Result) ResultAuthor() string {
	if v := resultField(r,
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
		}); v != "" {
		return v
	}
	return r.rawString("userLogin")
}

// ResultID returns the primary ID for any result type. Types without a typed
// struct (repo, pr) fall back to the "id" field of the raw payload, so a
// cross-cell merge can still identify the same logical result returned by two
// cells (e.g. a repo mirrored in both).
func (r *Result) ResultID() string {
	if id := resultField(r,
		func(c *CheckpointResult) string { return c.ID },
		func(c *CommitResult) string { return c.CommitSHA },
		func(s *SessionResult) string { return s.SessionID }); id != "" {
		return id
	}
	return r.rawString("id")
}

// ResultTitle returns the primary display text for any result type. Repo/PR
// raw payloads identify themselves via "title", "name", or "fullName".
func (r *Result) ResultTitle() string {
	if v := resultField(r,
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
		func(s *SessionResult) string { return s.DisplayName }); v != "" {
		return v
	}
	return r.rawString("title", "name", "fullName")
}

// ResultDescription returns the repo description for raw-payload rows
// (searcher.RepoResult). Typed rows return "" — rawFields is never populated
// for them.
func (r *Result) ResultDescription() string {
	return r.rawString("description")
}

// ResultCheckpointCount returns the indexed checkpoint count for raw-payload
// repo rows (searcher.RepoResult), 0 elsewhere.
func (r *Result) ResultCheckpointCount() int {
	var n int
	if err := json.Unmarshal(r.rawFields["checkpointCount"], &n); err != nil {
		return 0
	}
	return n
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

	// Warnings are client-side completeness notes (e.g. a truncated repo
	// index or a failed region in a cross-cell fan-out) surfaced to the user
	// on stderr. Never part of the wire format.
	Warnings []string `json:"-"`
}

// Config holds the configuration for a search request.
type Config struct {
	Owner    string
	Repo     string
	Repos    []string
	AllRepos bool // When true, search all accessible repos (no repo scoping)
	Query    string
	Limit    int
	Author   string // Filter by author name
	Date     string // Filter by time period: "week" or "month"
	Branch   string // Filter by branch name
	Page     int    // 1-based page number (0 means omit, API defaults to 1)
}

// ScopeSlugs resolves the repo scope of a search: the explicit repo filters
// (an explicit owner/name filter always scopes the search, even when
// --all-repos is also set — the more specific filter wins), else allRepos for
// an unfiltered repo:* / --all-repos search, else the current-repo default.
// slugs empty with allRepos false means no scope could be determined.
func (c Config) ScopeSlugs() (slugs []string, allRepos bool) {
	for _, repo := range c.Repos {
		if repo != AllReposFilter {
			slugs = append(slugs, repo)
		}
	}
	if len(slugs) > 0 {
		return slugs, false
	}
	if c.AllRepos || (len(c.Repos) == 1 && c.Repos[0] == AllReposFilter) {
		return nil, true
	}
	if c.Owner != "" && c.Repo != "" {
		return []string{c.Owner + "/" + c.Repo}, false
	}
	return nil, false
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
			p.Repos = AppendUnique(p.Repos, parseListFilter(strings.TrimPrefix(tok, "repo:"))...)
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

// ValidateRepoFilters ensures each repo filter matches backend semantics.
// Multiple explicit repo filters are accepted: the v4 query-serve path resolves
// each and fans out across the cells hosting them, mirroring code search.
func ValidateRepoFilters(repos []string) error {
	for _, repo := range repos {
		if !isValidRepoFilter(repo) {
			return fmt.Errorf(
				"invalid repo filter %q: expected owner/name, gh/owner/repo, a repo ULID, or *; if you meant all repos, quote the asterisk: --repo '*'",
				repo,
			)
		}
	}
	return nil
}

// isValidRepoFilter reports whether repo is a filter shape the search backends
// can resolve. It accepts every form the CLI help advertises and that the
// resolvers handle downstream — a bare owner/name slug, a prefixed path
// (gh/owner/repo, et/proj/repo, git/owner/repo), a raw repo ULID, or the
// all-repos wildcard — so validation never rejects a filter the semantic v4
// lookup (lookupFilter) or code-search resolver (resolveRepoFilters) would
// otherwise resolve. It still rejects obvious mistakes like a bare filename.
func isValidRepoFilter(repo string) bool {
	if repo == AllReposFilter {
		return true
	}
	if repo == "" || strings.Contains(repo, " ") {
		return false
	}
	// Raw repo ULID: the v4 route keys on ULIDs and lookupFilter matches a
	// prefix-less token against repo IDs.
	if _, err := ulid.Parse(repo); err == nil {
		return true
	}
	// A slug or prefixed path: owner/name or <prefix>/owner/repo. Every
	// path segment must be non-empty.
	parts := strings.Split(repo, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
	}
	return true
}

// AppendUnique appends values to existing, skipping any already present, and
// returns the result. Order is preserved (first occurrence wins).
func AppendUnique(existing []string, values ...string) []string {
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

// CellV4 performs a v4 query-serve search against a single entire-api
// cell, via the pre-authenticated client (bearer = jurisdictional identity
// token; host = the cell). repoIDs are repo ULIDs to scope to (the v4 route is
// per-repo and keys on ULIDs, not owner/name slugs); an empty repoIDs means
// "every repo the caller can access in this cell" — query-serve fans out across
// those namespaces itself. The cross-cell fan-out and merge live in the cli
// layer (mirroring code search), so this is the single-cell primitive it calls.
func CellV4(ctx context.Context, client *api.Client, cfg Config, repoIDs []string) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	q := url.Values{}
	q.Set("q", cfg.Query)
	filtered := false
	for _, id := range repoIDs {
		if id != "" {
			q.Add("repo", id)
			filtered = true
		}
	}
	addCommonSearchParams(q, cfg)

	resp, err := client.Get(ctx, v4ServicePath+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("calling search service: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		// Two distinct 404s share this status. A JSON error body is
		// query-serve answering through the gateway: the route exists but the
		// repo filter matched nothing the caller may search (not indexed, or
		// the owner org isn't flag-enabled — entire-search fails closed,
		// existence not disclosed). A plain "404 page not found" is the
		// gateway itself: no semantic-search route, query-serve not deployed
		// in this cell. Deployed cells answer unfiltered searches of unknown
		// repos with an empty 200, so the split is unambiguous. The
		// repo-filter-miss reading only holds when a filter was actually sent
		// — an unfiltered call named no repo to blame, so its JSON 404
		// (whatever produced it) degrades to the ErrCellUnavailable fail-safe.
		var errResp struct {
			Error string `json:"error"`
		}
		if filtered && json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			// Wrap rather than return the bare sentinel so the server's own
			// message survives into debug logs; errors.Is still matches.
			return nil, fmt.Errorf("%w: %s", ErrRepoFilterUnmatched, errResp.Error)
		}
		return nil, ErrCellUnavailable
	}
	return parseSearchResponse(resp.StatusCode, body)
}

// addCommonSearchParams sets the query params other than the repo scoping
// (repo IDs are added by CellV4's caller). types is deliberately never sent —
// the backend returns all types.
func addCommonSearchParams(q url.Values, cfg Config) {
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
}

// parseSearchResponse decodes a search response body, preserving the
// long-standing error wording so callers (and error-message assertions) are
// unchanged.
func parseSearchResponse(statusCode int, body []byte) (*Response, error) {
	if statusCode != http.StatusOK {
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			return nil, fmt.Errorf("search service error (%d): %s", statusCode, errResp.Error)
		}
		return nil, fmt.Errorf("search service returned %d: %s", statusCode, string(body))
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

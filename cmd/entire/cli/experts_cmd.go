package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/palette"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/spf13/cobra"
)

type expertsAPIClient interface {
	Get(ctx context.Context, path string) (*http.Response, error)
	Post(ctx context.Context, path string, body any) (*http.Response, error)
}

// newExpertsAPIClient builds the entire-api cell client. fullName (owner/repo)
// and/or ulid identify the repo so the client can route to the cell that hosts
// it (see NewAuthenticatedEntireAPICellClient).
var newExpertsAPIClient = func(ctx context.Context, insecureHTTP bool, fullName, ulid string) (expertsAPIClient, error) {
	return NewAuthenticatedEntireAPICellClient(ctx, insecureHTTP, fullName, ulid)
}

func setExpertsClientFactoryForTest(
	t interface{ Helper() },
	fn func(context.Context, bool, string, string) (expertsAPIClient, error),
) func() {
	t.Helper()
	prev := newExpertsAPIClient
	newExpertsAPIClient = fn
	return func() { newExpertsAPIClient = prev }
}

type expertsFlags struct {
	repo         string
	branch       string
	limit        int
	json         bool
	staged       bool
	tui          bool
	insecureHTTP bool
}

const (
	expertsDefaultLimit  = 8
	expertsMaxLimit      = 20
	expertsReposListPath = "/api/v1/repos"
)

// expertLocalScopeResult is the outcome of interpreting a scope argument as a
// local filesystem path (vs a natural-language query).
type expertLocalScopeResult struct {
	scope        string
	isLocal      bool
	validateRepo bool // when true, cross-check git origin against --repo
}

type expertsRequest struct {
	Scopes        []string `json:"scopes,omitempty"`
	Query         *string  `json:"query,omitempty"`
	Branch        string   `json:"branch,omitempty"`
	Limit         int      `json:"limit,omitempty"`
	EvidenceLimit int      `json:"evidence_limit,omitempty"`
}

type expertsResponse struct {
	RepoFullName string           `json:"repo_full_name"`
	Scopes       []string         `json:"scopes"`
	Query        *string          `json:"query"`
	Branch       string           `json:"branch"`
	Source       string           `json:"source"`
	Profiles     []expertsProfile `json:"profiles"`
}

type expertsFacetCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type expertsProfile struct {
	AgentID                   string                `json:"agent_id"`
	AgentLabel                string                `json:"agent_label"`
	RawAgents                 []string              `json:"raw_agents"`
	Models                    []string              `json:"models"`
	Labels                    []expertsFacetCount   `json:"labels"`
	Skills                    []expertsFacetCount   `json:"skills"`
	ToolMix                   []expertsFacetCount   `json:"tool_mix"`
	MCPServers                []expertsFacetCount   `json:"mcp_servers"`
	TranscriptTokens          int                   `json:"transcript_tokens"`
	FilesChanged              int                   `json:"files_changed"`
	LastActivityAt            string                `json:"last_activity_at"`
	SessionCount              int                   `json:"session_count"`
	CheckpointCount           int                   `json:"checkpoint_count"`
	StepCount                 int                   `json:"step_count"`
	AttributionAgentLines     *int                  `json:"attribution_agent_lines"`
	AttributionTotalCommitted *int                  `json:"attribution_total_committed"`
	MatchedFiles              []string              `json:"matched_files"`
	ExactFileMatches          int                   `json:"exact_file_matches"`
	PrefixFileMatches         int                   `json:"prefix_file_matches"`
	Sessions                  []expertsEvidenceItem `json:"sessions"`
}

type expertsEvidenceItem struct {
	SessionID                 string   `json:"session_id"`
	DisplayName               string   `json:"display_name"`
	Agent                     *string  `json:"agent"`
	Model                     *string  `json:"model"`
	LastActivityAt            string   `json:"last_activity_at"`
	CheckpointCount           int      `json:"checkpoint_count"`
	StepCount                 int      `json:"step_count"`
	AttributionAgentLines     *int     `json:"attribution_agent_lines,omitempty"`
	AttributionTotalCommitted *int     `json:"attribution_total_committed,omitempty"`
	MatchedFiles              []string `json:"matched_files"`
	ExactFileMatches          int      `json:"exact_file_matches"`
	PrefixFileMatches         int      `json:"prefix_file_matches"`
	CheckpointIDs             []string `json:"checkpoint_ids"`
}

type expertsStyles struct {
	colorEnabled bool

	title  lipgloss.Style
	agent  lipgloss.Style
	label  lipgloss.Style
	facet  lipgloss.Style
	muted  lipgloss.Style
	file   lipgloss.Style
	bullet lipgloss.Style
}

func newExpertsStyles(w io.Writer) expertsStyles {
	return expertsStylesForColor(shouldUseColor(w))
}

func expertsStylesForColor(useColor bool) expertsStyles {
	styles := expertsStyles{colorEnabled: useColor}
	if !useColor {
		return styles
	}

	styles.title = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Accent)).Bold(true)
	styles.agent = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Accent)).Bold(true)
	styles.label = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Info))
	styles.facet = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Blue))
	styles.muted = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted))
	styles.file = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Info))
	styles.bullet = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Accent))
	return styles
}

func (s expertsStyles) render(style lipgloss.Style, text string) string {
	if !s.colorEnabled {
		return text
	}
	return style.Render(text)
}

// link renders text in the given style with an OSC 8 terminal hyperlink to url
// attached. Links are only emitted when styling is enabled (a capable, non-piped
// terminal); otherwise it falls back to plain styled text so scripts and dumb
// terminals are unaffected.
func (s expertsStyles) link(style lipgloss.Style, url, text string) string {
	if !s.colorEnabled || strings.TrimSpace(url) == "" {
		return s.render(style, text)
	}
	return style.Hyperlink(url).Render(text)
}

func newExpertsCmd() *cobra.Command {
	f := &expertsFlags{limit: 8}
	cmd := &cobra.Command{
		Use:    "experts [scope-or-query]",
		Short:  "Rank agent provenance for code scopes",
		Hidden: true,
		Args:   cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExperts(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), args, f)
		},
	}
	cmd.Flags().StringVar(&f.repo, "repo", "", "Repository as owner/repo")
	cmd.Flags().StringVar(&f.branch, "branch", "", "Branch to inspect")
	cmd.Flags().IntVar(&f.limit, "limit", expertsDefaultLimit, "Maximum profiles to return (1–20; values above 20 are clamped)")
	cmd.Flags().BoolVar(&f.json, "json", false, "Print JSON")
	cmd.Flags().BoolVar(&f.staged, "staged", false, "Use staged file paths as scopes")
	cmd.Flags().BoolVar(&f.tui, "tui", false, "Browse provenance in an interactive viewer (TTY only)")
	cmd.Flags().BoolVar(&f.insecureHTTP, "insecure-http-auth", false, "Allow plain-HTTP auth (local dev only)")
	if err := cmd.Flags().MarkHidden("insecure-http-auth"); err != nil {
		panic(fmt.Sprintf("hide experts insecure auth flag: %v", err))
	}
	return cmd
}

func runExperts(ctx context.Context, out, errOut io.Writer, args []string, f *expertsFlags) error {
	if f.staged && strings.TrimSpace(f.repo) != "" {
		return errors.New("--staged cannot be used with --repo")
	}
	if f.limit <= 0 {
		f.limit = expertsDefaultLimit
	}
	if f.limit > expertsMaxLimit {
		f.limit = expertsMaxLimit
	}

	// The data API (entire-api) is repo-ULID keyed. --repo may be a ULID (used
	// directly) or an owner/repo, which we resolve to its ULID after the client
	// exists (via the caller's accessible-repo list). With no --repo we derive
	// owner/repo from the git origin.
	repoOverride := strings.TrimSpace(f.repo)
	repoIsULID := looksLikeULID(repoOverride)
	var repoFullName string
	if !repoIsULID {
		var err error
		repoFullName, err = resolveExpertsRepo(ctx, f.repo)
		if err != nil {
			return err
		}
	}

	req := expertsRequest{Limit: f.limit, EvidenceLimit: 3}
	if strings.TrimSpace(f.branch) != "" {
		req.Branch = strings.TrimSpace(f.branch)
	}

	if f.staged {
		scopes, err := stagedExpertScopes(ctx)
		if err != nil {
			return err
		}
		if len(scopes) == 0 {
			fmt.Fprintln(errOut, "No staged files found.")
			return NewSilentError(errors.New("no staged files"))
		}
		req.Scopes = scopes
	} else {
		input := strings.TrimSpace(strings.Join(args, " "))
		if input == "" {
			return errors.New("scope or query required unless --staged is set")
		}
		if strings.TrimSpace(f.repo) != "" && looksLikeExpertPath(input) {
			req.Scopes = []string{normalizeExpertScope(input)}
		} else {
			local, err := localExpertScope(ctx, input)
			if err != nil {
				return err
			}
			if local.isLocal {
				if !repoIsULID && repoOverride != "" && local.validateRepo {
					currentRepo, err := resolveExpertsRepo(ctx, "")
					if err == nil && !strings.EqualFold(currentRepo, repoFullName) {
						return fmt.Errorf("local path belongs to %s, not --repo %s", currentRepo, repoFullName)
					}
				}
				req.Scopes = []string{local.scope}
			} else {
				query := input
				req.Query = &query
			}
		}
	}

	// Identify the repo for cell routing: a ULID goes on the ulid arg, an
	// owner/repo on the fullName arg. The client uses whichever is set to reach
	// the cell that hosts the repo (falling back to home-jurisdiction routing).
	cellFullName, cellULID := "", ""
	if repoIsULID {
		cellULID = repoOverride
	} else {
		cellFullName = repoFullName
	}
	client, err := newExpertsAPIClient(ctx, f.insecureHTTP, cellFullName, cellULID)
	if err != nil {
		return fmt.Errorf("create experts API client: %w", err)
	}

	repoID := repoOverride
	if !repoIsULID {
		repoID, err = resolveExpertsRepoID(ctx, client, repoFullName)
		if err != nil {
			return err
		}
	}

	resp, err := client.Post(ctx, expertsAPIPath(repoID), req)
	if err != nil {
		return fmt.Errorf("post experts request: %w", err)
	}
	defer resp.Body.Close()

	if err := api.CheckResponse(resp); err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) {
			// A natural-language query needs code search on the cell. When it
			// isn't available the cell returns 503 — sometimes with only a bare
			// "Service Unavailable" body — so treat any 503 on a query as the
			// code-search-unavailable case. Path scopes don't need code search
			// and fall through to the generic error below.
			if httpErr.StatusCode == http.StatusServiceUnavailable && req.Query != nil {
				fmt.Fprintln(errOut, "Code search is not available for natural-language experts queries on this backend.")
				return NewSilentError(err)
			}
			// entire-api returns 404 "repo not in this region" when the repo is
			// homed in a different cell than the one reached. Surface that as an
			// actionable region hint instead of the raw service-to-service text.
			if httpErr.StatusCode == http.StatusNotFound &&
				strings.Contains(strings.ToLower(httpErr.Message), "region") {
				fmt.Fprintln(errOut, "This repo appears to be homed in a different Entire region than the cell this command reached; cross-region experts routing may be incomplete.")
				return NewSilentError(err)
			}
		}
		return fmt.Errorf("fetch experts: %w", err)
	}

	var decoded expertsResponse
	if err := api.DecodeJSON(resp, &decoded); err != nil {
		return fmt.Errorf("decode experts response: %w", err)
	}

	if f.json {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(decoded); err != nil {
			return fmt.Errorf("write experts JSON: %w", err)
		}
		return nil
	}

	// The interactive viewer is opt-in and only runs on a real terminal with
	// results to show. Piped/accessible output and empty results always fall
	// through to the deterministic plain renderer so agents and scripts get
	// stable output.
	if f.tui && len(decoded.Profiles) > 0 && interactive.IsTerminalWriter(out) && !IsAccessibleMode() {
		return runExpertsTUI(decoded, shouldUseColor(out))
	}

	renderExperts(out, decoded)
	return nil
}

func resolveExpertsRepo(ctx context.Context, override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return parseExpertsRepo(override)
	}
	_, owner, repo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil {
		return "", fmt.Errorf("resolve repo from origin: %w", err)
	}
	if owner == "" || repo == "" {
		return "", errors.New("could not resolve owner/repo from origin")
	}
	return owner + "/" + repo, nil
}

func parseExpertsRepo(value string) (string, error) {
	trimmed := strings.Trim(strings.TrimSpace(value), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 3 && parts[0] == "gh" {
		parts = parts[1:]
	}
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("invalid --repo %q (use owner/repo)", value)
	}
	return parts[0] + "/" + strings.TrimSuffix(parts[1], ".git"), nil
}

func expertsAPIPath(repoID string) string {
	return expertsReposListPath + "/" + url.PathEscape(repoID) + "/experts"
}

// resolveExpertsRepoID maps an owner/repo to its repo ULID for the entire-api
// data plane, which is ULID-keyed. It reads the caller's accessible-repo list
// (GET /api/v1/repos) and matches on full name — an authz-safe resolution (the
// list only contains repos the caller can read, so it never reveals a repo they
// can't see). A ULID is passed straight through by the caller, so this is only
// hit for the owner/repo form.
func resolveExpertsRepoID(ctx context.Context, client expertsAPIClient, fullName string) (string, error) {
	repos, err := listExpertsAccessibleRepos(ctx, client)
	if err != nil {
		return "", err
	}
	want := strings.ToLower(fullName)
	for _, r := range repos {
		if r.ID != "" && strings.ToLower(r.FullName) == want {
			return r.ID, nil
		}
	}
	return "", fmt.Errorf("repo %q was not found on the entire-api cell this command reached. It may be homed in another Entire region (cross-region experts routing may be incomplete), not onboarded to Entire, or outside your access", fullName)
}

type expertsRepoListItem struct {
	ID       string `json:"id"`
	FullName string `json:"full_name"`
}

// listExpertsAccessibleRepos returns every repo the caller can read on this data
// API. entire-api's GET /repos currently returns the full SpiceDB-filtered set in
// one response (no page_token), but the loop is forward-compatible if pagination
// is added — same pattern as fetchAllPages in core list commands.
func listExpertsAccessibleRepos(ctx context.Context, client expertsAPIClient) ([]expertsRepoListItem, error) {
	return fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]expertsRepoListItem, string, error) {
		path := expertsReposListPath
		if cursor != "" {
			path += "?" + url.Values{"page_token": {cursor}}.Encode()
		}
		resp, err := client.Get(ctx, path)
		if err != nil {
			return nil, "", fmt.Errorf("list repos: %w", err)
		}
		defer resp.Body.Close()
		if err := api.CheckResponse(resp); err != nil {
			return nil, "", fmt.Errorf("list repos: %w", err)
		}
		var body struct {
			Repos         []expertsRepoListItem `json:"repos"`
			NextPageToken string                `json:"next_page_token,omitempty"`
		}
		if err := api.DecodeJSON(resp, &body); err != nil {
			return nil, "", fmt.Errorf("decode repos: %w", err)
		}
		return body.Repos, body.NextPageToken, nil
	})
}

// expertsWebBaseURL is the origin used to build user-facing session links.
//
// Session links must point at the real Entire web app, not at whatever data API
// the CLI happens to be talking to. So:
//   - ENTIRE_WEB_BASE_URL wins when set (e.g. http://localhost:5173 for a local
//     frontend during dev).
//   - otherwise, if the API base is itself an entire.io host (prod/staging), use
//     it (frontend and API share that origin).
//   - otherwise (local dev API like 127.0.0.1) fall back to the canonical
//     https://entire.io so links still resolve to the proper site.
func expertsWebBaseURL() string {
	if raw := strings.TrimSpace(os.Getenv("ENTIRE_WEB_BASE_URL")); raw != "" {
		return strings.TrimRight(raw, "/")
	}
	if base := strings.TrimRight(api.BaseURL(), "/"); isEntireWebHost(base) {
		return base
	}
	return strings.TrimRight(api.DefaultBaseURL, "/")
}

func isEntireWebHost(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "entire.io" || strings.HasSuffix(host, ".entire.io")
}

// expertsSessionURL builds the entire.io web URL for a session, matching the
// frontend route /gh/:org/:repo/session/:sessionId. Returns "" when the inputs
// can't form a valid link.
func expertsSessionURL(repoFullName, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	owner, repo, ok := strings.Cut(repoFullName, "/")
	if !ok || owner == "" || repo == "" || sessionID == "" {
		return ""
	}
	return fmt.Sprintf("%s/gh/%s/%s/session/%s",
		expertsWebBaseURL(), url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sessionID))
}

func localExpertScope(ctx context.Context, input string) (expertLocalScopeResult, error) {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		if looksLikeExpertPath(input) {
			return expertLocalScopeResult{
				scope: normalizeExpertScope(input), isLocal: true,
			}, nil
		}
		return expertLocalScopeResult{}, nil
	}

	candidates := make([]string, 0, 2)
	if filepath.IsAbs(input) {
		candidates = append(candidates, input)
	} else {
		cwdAbs, err := filepath.Abs(input)
		if err != nil {
			return expertLocalScopeResult{}, fmt.Errorf("resolve cwd-relative path: %w", err)
		}
		candidates = append(candidates, cwdAbs, filepath.Join(root, input))
	}
	candidates = uniqueStrings(candidates)

	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return expertLocalScopeResult{}, fmt.Errorf("stat local scope: %w", err)
		}
		scope, err := localPathScope(root, candidate, input)
		if err != nil {
			return expertLocalScopeResult{}, err
		}
		if info.IsDir() && !strings.HasSuffix(scope, "/") {
			scope += "/"
		}
		return expertLocalScopeResult{scope: scope, isLocal: true, validateRepo: true}, nil
	}

	if looksLikeExpertPath(input) {
		missingCandidates := make([]string, 0, 3)
		if filepath.IsAbs(input) {
			missingCandidates = append(missingCandidates, input)
		} else {
			cwdAbs, err := filepath.Abs(input)
			if err != nil {
				return expertLocalScopeResult{}, fmt.Errorf("resolve cwd-relative path: %w", err)
			}
			missingCandidates = append(missingCandidates, cwdAbs, filepath.Join(root, input))
		}
		for _, candidate := range uniqueStrings(missingCandidates) {
			scope, err := localPathScope(root, candidate, input)
			if err != nil {
				continue
			}
			return expertLocalScopeResult{scope: scope, isLocal: true}, nil
		}
		return expertLocalScopeResult{
			scope: normalizeExpertScope(input), isLocal: true,
		}, nil
	}
	return expertLocalScopeResult{}, nil
}

func localPathScope(root, candidate, original string) (string, error) {
	rel, err := filepath.Rel(canonicalPathForRel(root), canonicalPathForRel(candidate))
	if err != nil {
		return "", fmt.Errorf("relativize local scope: %w", err)
	}
	if rel == "." {
		return "./", nil
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", fmt.Errorf("path %q is outside the git worktree", original)
	}
	return filepath.ToSlash(rel), nil
}

func canonicalPathForRel(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved
	}

	var missing []string
	current := path
	for {
		parent := filepath.Dir(current)
		base := filepath.Base(current)
		if parent == current {
			return path
		}
		missing = append([]string{base}, missing...)
		resolvedParent, err := filepath.EvalSymlinks(parent)
		if err == nil {
			return filepath.Join(append([]string{resolvedParent}, missing...)...)
		}
		current = parent
	}
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func looksLikeExpertPath(input string) bool {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || strings.ContainsAny(trimmed, " \t\n\r") {
		return false
	}
	return strings.ContainsAny(trimmed, `/\`) ||
		strings.HasPrefix(trimmed, ".") ||
		strings.HasSuffix(trimmed, "/") ||
		filepath.Ext(trimmed) != ""
}

func normalizeExpertScope(input string) string {
	scope := strings.TrimSpace(filepath.ToSlash(input))
	scope = strings.TrimPrefix(scope, "./")
	scope = strings.TrimLeft(scope, "/")
	return scope
}

func stagedExpertScopes(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--name-only", "--diff-filter=ACMRD")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read staged files: %w", err)
	}
	return parseGitStagedScopeLines(string(output)), nil
}

// parseGitStagedScopeLines normalizes git name-only output into repo-relative
// path scopes. Windows git may emit CRLF line endings; strip them before split.
func parseGitStagedScopeLines(raw string) []string {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.ReplaceAll(trimmed, "\r\n", "\n")
	lines := strings.Split(trimmed, "\n")
	scopes := make([]string, 0, len(lines))
	seen := make(map[string]bool, len(lines))
	for _, line := range lines {
		scope := strings.TrimSpace(filepath.ToSlash(line))
		if scope == "" || seen[scope] {
			continue
		}
		seen[scope] = true
		scopes = append(scopes, scope)
	}
	return scopes
}

func renderExperts(w io.Writer, resp expertsResponse) {
	renderExpertsWithStyles(w, resp, newExpertsStyles(w))
}

func renderExpertsWithStyles(w io.Writer, resp expertsResponse, styles expertsStyles) {
	scopeLabel := strings.Join(resp.Scopes, ", ")
	if resp.Query != nil && strings.TrimSpace(*resp.Query) != "" {
		scopeLabel = *resp.Query
	}
	if scopeLabel == "" {
		scopeLabel = resp.RepoFullName
	}
	if len(resp.Profiles) == 0 {
		fmt.Fprintf(w, "No agent provenance found for %s.\n", styles.render(styles.file, scopeLabel))
		return
	}

	fmt.Fprintf(w, "%s for %s", styles.render(styles.title, "Agent provenance"), styles.render(styles.file, resp.RepoFullName))
	if resp.Branch != "" {
		fmt.Fprintf(w, " %s", styles.render(styles.muted, "("+resp.Branch+")"))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w)

	for i, profile := range resp.Profiles {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s\n", styles.render(styles.agent, profile.AgentLabel))
		fmt.Fprintf(w, "  %s: %d sessions, %d matching checkpoints, %d steps", styles.render(styles.label, "evidence"), profile.SessionCount, profile.CheckpointCount, profile.StepCount)
		if profile.AttributionAgentLines != nil {
			fmt.Fprintf(w, ", %d agent-attributed lines", *profile.AttributionAgentLines)
		}
		fmt.Fprintln(w)
		writeFacetLineWithStyles(w, "skills", profile.Skills, styles)
		writeFacetLineWithStyles(w, "tools", profile.ToolMix, styles)
		writeFacetLineWithStyles(w, "mcp", profile.MCPServers, styles)
		if len(profile.MatchedFiles) > 0 {
			fmt.Fprintf(w, "  %s: %s\n", styles.render(styles.label, "files"), strings.Join(renderExpertFiles(profile.MatchedFiles, styles), ", "))
		}
		for _, session := range profile.Sessions {
			sessionURL := expertsSessionURL(resp.RepoFullName, session.SessionID)
			fmt.Fprintf(w, "  %s %s", styles.render(styles.bullet, "-"), styles.link(styles.facet, sessionURL, session.DisplayName))
			if session.CheckpointCount > 0 || session.StepCount > 0 {
				fmt.Fprintf(w, " %s %s", styles.render(styles.muted, "-"), styles.render(styles.muted, fmt.Sprintf("%d checkpoints, %d steps", session.CheckpointCount, session.StepCount)))
			}
			fmt.Fprintln(w)
		}
	}
}

func writeFacetLineWithStyles(w io.Writer, label string, facets []expertsFacetCount, styles expertsStyles) {
	if len(facets) == 0 {
		return
	}
	trimmedLabel := strings.TrimSpace(label)
	indent := label[:len(label)-len(strings.TrimLeft(label, " \t"))]
	if indent == "" {
		indent = "  "
	}
	parts := make([]string, 0, len(facets))
	for _, facet := range facets {
		if styles.colorEnabled {
			parts = append(parts, styles.render(styles.facet, facet.Name)+styles.render(styles.muted, fmt.Sprintf(" (%d)", facet.Count)))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (%d)", facet.Name, facet.Count))
	}
	fmt.Fprintf(w, "%s%s: %s\n", indent, styles.render(styles.label, trimmedLabel), strings.Join(parts, ", "))
}

func renderExpertFiles(files []string, styles expertsStyles) []string {
	if !styles.colorEnabled {
		return files
	}
	rendered := make([]string, 0, len(files))
	for _, file := range files {
		rendered = append(rendered, styles.render(styles.file, file))
	}
	return rendered
}

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/spf13/cobra"
)

const (
	defaultExpertsLimit         = 10
	defaultExpertsEvidenceLimit = 3
)

type expertsRequest struct {
	Owner         string
	Repo          string
	Scopes        []string
	Query         string
	Branch        string
	Limit         int
	EvidenceLimit int
}

type expertsAPIRequest struct {
	Scopes        []string `json:"scopes,omitempty"`
	Query         string   `json:"query,omitempty"`
	Branch        string   `json:"branch,omitempty"`
	Limit         int      `json:"limit,omitempty"`
	EvidenceLimit int      `json:"evidence_limit,omitempty"`
}

type expertEvidence struct {
	SessionID             string   `json:"sessionId"`
	DisplayName           string   `json:"displayName"`
	Agent                 *string  `json:"agent"`
	Prompt                *string  `json:"prompt"`
	LastActivityAt        string   `json:"lastActivityAt"`
	CheckpointCount       int      `json:"checkpointCount"`
	StepCount             int      `json:"stepCount"`
	AttributionAgentLines *int     `json:"attributionAgentLines"`
	MatchedFiles          []string `json:"matchedFiles"`
}

type expertEntry struct {
	Login                 string           `json:"login"`
	Score                 int              `json:"score"`
	MembershipStatus      string           `json:"membershipStatus"`
	LastActivityAt        string           `json:"lastActivityAt"`
	SessionCount          int              `json:"sessionCount"`
	CheckpointCount       int              `json:"checkpointCount"`
	StepCount             int              `json:"stepCount"`
	AttributionAgentLines *int             `json:"attributionAgentLines"`
	MatchedFiles          []string         `json:"matchedFiles"`
	Evidence              []expertEvidence `json:"evidence"`
}

type expertsResponse struct {
	Experts      []expertEntry `json:"experts"`
	Scopes       []string      `json:"scopes"`
	Query        *string       `json:"query"`
	RepoFullName string        `json:"repo_full_name"`
	Source       string        `json:"source"`
}

func newExpertsCmd() *cobra.Command {
	var opts expertsCommandOptions

	cmd := &cobra.Command{
		Use:    "experts [scope-or-query]",
		Short:  "Find people with prior session evidence for code",
		Hidden: true,
		Args:   cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Subject = strings.TrimSpace(strings.Join(args, " "))
			return runExpertsCommand(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
		},
	}

	cmd.Flags().StringVar(&opts.RepoOverride, "repo", "", "Target repository as owner/repo, gh/owner/repo, or a clone URL; defaults to origin")
	cmd.Flags().StringVar(&opts.Branch, "branch", "", "Restrict evidence to a branch; defaults to the repository default branch")
	cmd.Flags().IntVar(&opts.Limit, "limit", defaultExpertsLimit, "Maximum experts to show")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Emit JSON")
	cmd.Flags().BoolVar(&opts.Staged, "staged", false, "Use staged file paths as scopes")
	cmd.Flags().BoolVar(&opts.InsecureHTTPAuth, "insecure-http-auth", false, "Allow API calls over plain HTTP (insecure, for local development only)")
	if err := cmd.Flags().MarkHidden("insecure-http-auth"); err != nil {
		panic(fmt.Sprintf("hide insecure-http-auth flag: %v", err))
	}
	return cmd
}

type expertsCommandOptions struct {
	Subject          string
	RepoOverride     string
	Branch           string
	Limit            int
	JSON             bool
	Staged           bool
	InsecureHTTPAuth bool
}

func runExpertsCommand(ctx context.Context, w, errW io.Writer, opts expertsCommandOptions) error {
	owner, repo, err := resolveExpertsRepo(ctx, opts.RepoOverride)
	if err != nil {
		return err
	}
	req, label, err := buildExpertsRequest(ctx, owner, repo, opts)
	if err != nil {
		return err
	}

	return runAuthenticatedDataAPI(ctx, errW, opts.InsecureHTTPAuth, func(ctx context.Context, client *api.Client) error {
		resp, err := fetchExperts(ctx, client, req)
		if err != nil {
			return err
		}
		if opts.JSON {
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			return enc.Encode(resp)
		}
		if len(resp.Experts) == 0 {
			fmt.Fprintf(w, "No expert evidence found for %s.\n", label)
			return nil
		}
		renderExpertsText(w, *resp)
		return nil
	})
}

func buildExpertsRequest(
	ctx context.Context,
	owner, repo string,
	opts expertsCommandOptions,
) (expertsRequest, string, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultExpertsLimit
	}

	req := expertsRequest{
		Owner:         owner,
		Repo:          repo,
		Branch:        strings.TrimSpace(opts.Branch),
		Limit:         limit,
		EvidenceLimit: defaultExpertsEvidenceLimit,
	}

	if opts.Staged {
		scopes, err := stagedExpertScopes(ctx)
		if err != nil {
			return req, "", err
		}
		if len(scopes) == 0 {
			return req, "", errors.New("no staged files found")
		}
		req.Scopes = scopes
		return req, strings.Join(scopes, ", "), nil
	}

	if opts.Subject == "" {
		return req, "", errors.New("scope or query required")
	}

	if scope, ok, err := localExpertScope(ctx, opts.Subject); ok || err != nil {
		if err != nil {
			return req, "", err
		}
		req.Scopes = []string{scope}
		return req, scope, nil
	}

	req.Query = opts.Subject
	return req, opts.Subject, nil
}

func fetchExperts(ctx context.Context, client *api.Client, req expertsRequest) (*expertsResponse, error) {
	body := expertsAPIRequest{
		Scopes:        req.Scopes,
		Query:         req.Query,
		Branch:        req.Branch,
		Limit:         req.Limit,
		EvidenceLimit: req.EvidenceLimit,
	}
	path := fmt.Sprintf(
		"/api/v1/cache/%s/%s/experts",
		url.PathEscape(req.Owner),
		url.PathEscape(req.Repo),
	)
	resp, err := client.Post(ctx, path, body)
	if err != nil {
		return nil, fmt.Errorf("POST experts: %w", err)
	}
	defer resp.Body.Close()
	if err := api.CheckResponse(resp); err != nil {
		return nil, fmt.Errorf("experts response: %w", err)
	}

	var result expertsResponse
	if err := api.DecodeJSON(resp, &result); err != nil {
		return nil, fmt.Errorf("decode experts: %w", err)
	}
	return &result, nil
}

func resolveExpertsRepo(ctx context.Context, override string) (string, string, error) {
	override = strings.TrimSpace(override)
	if override == "" {
		_, owner, repo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
		if err != nil {
			return "", "", err
		}
		return owner, repo, nil
	}

	if strings.Contains(override, "://") || strings.Contains(override, ":") {
		info, err := gitremote.ParseURL(override)
		if err != nil {
			return "", "", err
		}
		return info.Owner, info.Repo, nil
	}

	parts := strings.Split(override, "/")
	switch len(parts) {
	case 2:
		return parts[0], parts[1], nil
	case 3:
		if !gitremote.IsSupportedForge(parts[0]) {
			return "", "", fmt.Errorf("unsupported forge %q", parts[0])
		}
		return parts[1], parts[2], nil
	default:
		return "", "", fmt.Errorf("invalid repository %q", override)
	}
}

func localExpertScope(ctx context.Context, subject string) (string, bool, error) {
	info, err := os.Stat(subject)
	if err != nil {
		return "", false, nil
	}

	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return "", true, fmt.Errorf("resolve git worktree root for %q: %w", subject, err)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", true, fmt.Errorf("resolve git worktree root for %q: %w", subject, err)
	}
	if resolved, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = resolved
	}
	subjectAbs, err := filepath.Abs(subject)
	if err != nil {
		return "", true, fmt.Errorf("resolve path %q: %w", subject, err)
	}
	if resolved, err := filepath.EvalSymlinks(subjectAbs); err == nil {
		subjectAbs = resolved
	}
	rel, err := filepath.Rel(rootAbs, subjectAbs)
	if err != nil {
		return "", true, fmt.Errorf("resolve path %q relative to git worktree: %w", subject, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", true, fmt.Errorf("path %q is outside git worktree %q", subject, root)
	}

	scope := filepath.ToSlash(filepath.Clean(rel))
	scope = strings.TrimPrefix(scope, "./")
	if info.IsDir() && !strings.HasSuffix(scope, "/") {
		scope += "/"
	}
	return scope, true, nil
}

func stagedExpertScopes(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--name-only", "--diff-filter=ACMR")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read staged files: %w", err)
	}
	seen := map[string]struct{}{}
	var scopes []string
	for _, line := range strings.Split(string(out), "\n") {
		scope := strings.TrimSpace(filepath.ToSlash(line))
		if scope == "" {
			continue
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	return scopes, nil
}

func renderExpertsText(w io.Writer, resp expertsResponse) {
	fmt.Fprintf(w, "EXPERTS for %s\n\n", resp.RepoFullName)
	if resp.Query != nil && strings.TrimSpace(*resp.Query) != "" {
		fmt.Fprintf(w, "Query: %s\n", *resp.Query)
	}
	if len(resp.Scopes) > 0 {
		fmt.Fprintf(w, "Scopes: %s\n", strings.Join(resp.Scopes, ", "))
	}
	fmt.Fprintln(w)

	for _, expert := range resp.Experts {
		fmt.Fprintf(w, "@%s", expert.Login)
		if expert.MembershipStatus != "" && expert.MembershipStatus != "unknown" {
			fmt.Fprintf(w, " (%s)", expert.MembershipStatus)
		}
		fmt.Fprintf(w, "  %d sessions, %d checkpoints, %d steps", expert.SessionCount, expert.CheckpointCount, expert.StepCount)
		if expert.AttributionAgentLines != nil {
			fmt.Fprintf(w, ", %d agent lines", *expert.AttributionAgentLines)
		}
		fmt.Fprintln(w)
		if len(expert.MatchedFiles) > 0 {
			fmt.Fprintf(w, "  files: %s\n", strings.Join(expert.MatchedFiles, ", "))
		}
		for _, ev := range expert.Evidence {
			when := shortExpertTime(ev.LastActivityAt)
			agent := ptrStringValue(ev.Agent)
			if agent == "" {
				agent = "unknown"
			}
			fmt.Fprintf(w, "  - %s", ev.DisplayName)
			if when != "" {
				fmt.Fprintf(w, " (%s, %s)", agent, when)
			} else {
				fmt.Fprintf(w, " (%s)", agent)
			}
			fmt.Fprintf(w, " — %d checkpoints", ev.CheckpointCount)
			if ev.AttributionAgentLines != nil {
				fmt.Fprintf(w, ", %d agent lines", *ev.AttributionAgentLines)
			}
			fmt.Fprintln(w)
			if len(ev.MatchedFiles) > 0 {
				fmt.Fprintf(w, "    %s\n", strings.Join(ev.MatchedFiles, ", "))
			}
		}
		fmt.Fprintln(w)
	}
}

func ptrStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func shortExpertTime(value string) string {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return ""
	}
	return t.Format("2006-01-02")
}

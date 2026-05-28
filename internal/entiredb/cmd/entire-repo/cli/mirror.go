package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/entireio/cli/internal/entiredb/client"
	"github.com/entireio/cli/internal/entiredb/client/clilogin"
	"github.com/entireio/cli/internal/entiredb/core/api"
	"github.com/entireio/cli/internal/entiredb/internal/cliauth"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/spf13/cobra"
)

const originalURLConfigKey = "entiredb-original-url"

func NewMirrorCmd(cfg *cliauth.Config) *cobra.Command {
	var disable bool
	var remote string

	cmd := &cobra.Command{
		Use:   "mirror",
		Short: "Manage EntireDB GitHub-mirror placements and point local clones at them",
		Long: `Mirror lifecycle and local-clone integration for GitHub repos.

Server-side placement (no local git config touched):
  entire-repo mirror create <github-url> <cluster-host> [--no-wait]
  entire-repo mirror remove <github-url> <cluster-host>

Local clone integration (rewrites this checkout's git remote):
  entire-repo mirror use <cluster-host> [--remote origin]
  entire-repo mirror use --disable [--remote origin]
  entire-repo mirror --disable [--remote origin]   (shortcut for the above)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --disable on the parent is a shortcut for `mirror use --disable`
			// — same code path, same flags. Without --disable the parent has
			// nothing to do, so fall back to showing help (cobra's default
			// when RunE is unset).
			if disable {
				return runMirrorDisable(cmd, cfg, remote)
			}
			return cmd.Help()
		},
	}

	cmd.Flags().BoolVar(&disable, "disable", false, "Restore the original remote URL (shortcut for `mirror use --disable`)")
	cmd.Flags().StringVar(&remote, "remote", "origin", "Git remote to modify (only honored with --disable)")

	cmd.AddCommand(newMirrorCreateCmd(cfg))
	cmd.AddCommand(newMirrorRemoveCmd(cfg))
	cmd.AddCommand(newMirrorUseCmd(cfg))

	return cmd
}

// newMirrorUseCmd is the per-checkout side of the mirror surface:
// rewrites the local git remote to entire://<cluster-host>/gh/<owner>/<repo>,
// or restores the original GitHub URL with --disable. It does not touch
// server state; the mirror placement must already exist (use `mirror
// create` first when starting from scratch).
func newMirrorUseCmd(cfg *cliauth.Config) *cobra.Command {
	var disable bool
	var remote string

	cmd := &cobra.Command{
		Use:   "use <cluster-host>",
		Short: "Point this checkout's git remote at an existing mirror (or restore with --disable)",
		Long: `Rewrite the current git repository's remote URL to use an
existing EntireDB mirror. The remote must currently point to a
GitHub repository that has been mirrored on the named cluster
(see ` + "`entire-repo mirror create`" + `). The original URL is saved in
git config so --disable can restore it later.

This command only touches the local .git/config; it does not
create or delete mirror placements on the server.

Example:
  entire-repo mirror use royalcanin.partial.to
  entire-repo mirror use --disable`,
		Args: func(cmd *cobra.Command, args []string) error {
			// --disable doesn't need the cluster host (it reads the
			// saved-original-URL out of git config); enable requires it.
			if disable {
				return cobra.NoArgs(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if disable {
				return runMirrorDisable(cmd, cfg, remote)
			}
			return runMirrorEnable(cmd, cfg, args[0], remote)
		},
	}

	cmd.Flags().BoolVar(&disable, "disable", false, "Restore the original remote URL")
	cmd.Flags().StringVar(&remote, "remote", "origin", "Git remote to modify")

	return cmd
}

// newMirrorCreateCmd registers a new GitHub mirror placement on the
// supplied cluster. The bare `entire-repo mirror <cluster-host>` flow
// just rewrites a local remote and presumes the mirror already
// exists; `create` is the explicit constructor for operators
// onboarding a fresh upstream.
func newMirrorCreateCmd(cfg *cliauth.Config) *cobra.Command {
	var noWait bool
	var waitTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "create <github-url> <cluster-host>",
		Short: "Register a new GitHub mirror on the named cluster",
		Long: `Create a mirror placement for a GitHub repo on the named cluster.

<github-url> accepts the same shapes the parent command parses:
  https://github.com/<owner>/<repo>(.git)
  git@github.com:<owner>/<repo>(.git)
  <owner>/<repo>

The caller must hold GitHub *read* on the upstream (the same
permission GitHub requires to clone it). Idempotent on
(upstream, cluster): re-running with the same args succeeds and
returns the existing mirror id.

By default the command polls the data plane until the initial
clone is visible (up to --wait-timeout, default 30m). Pass
--no-wait to return as soon as the placement is registered.

Example:
  entire-repo mirror create github.com/octocat/hello-world royalcanin.partial.to`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMirrorCreate(cmd, cfg, args[0], args[1], !noWait, waitTimeout)
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Return as soon as the placement is registered, without waiting for the initial clone")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 30*time.Minute, "Timeout for waiting on the initial clone")
	return cmd
}

// newMirrorRemoveCmd un-publishes a mirror placement on the supplied
// cluster. Removes only this cluster's placement; other clusters'
// placements of the same upstream are unaffected.
func newMirrorRemoveCmd(cfg *cliauth.Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <github-url> <cluster-host>",
		Short: "Un-register a GitHub mirror from the named cluster",
		Long: `Remove a mirror placement for a GitHub repo from the named cluster.

The caller must hold GitHub *write* on the upstream — read-only
collaborators can create mirrors but not remove them. The storage
on entire-server is deleted before the metadata, so a transient
failure of the storage delete leaves the metadata intact and the
operator can retry.

Example:
  entire-repo mirror remove github.com/octocat/hello-world royalcanin.partial.to`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMirrorRemove(cmd, cfg, args[0], args[1])
		},
	}
	return cmd
}

func runMirrorCreate(cmd *cobra.Command, cfg *cliauth.Config, githubURL, clusterHost string, wait bool, waitTimeout time.Duration) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	owner, repo, err := parseGitHubURL(githubURL)
	if err != nil {
		// parseGitHubURL only accepts the URL shapes the parent
		// command also accepts; surface the same error envelope so
		// operators don't have to memorise two parsers.
		return fmt.Errorf("invalid <github-url>: %w", err)
	}

	httpClient := cliauth.NewHTTPClient(cfg.SkipTLSVerify)
	c, err := cliauth.ResolveClusterContext(ctx, *cfg, clusterHost, httpClient)
	if err != nil {
		return err
	}

	token, err := client.GetTokenWithRefresh(ctx, client.AuthRefreshConfig{
		KeyringService: c.KeychainService,
		BaseURL:        "https://" + clusterHost,
		Username:       c.Handle,
		CoreBaseURL:    c.CoreURL,
		HTTPClient:     httpClient,
		ClientID:       clilogin.DefaultClientID,
	})
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	resp, err := postMirrorAPI(ctx, cfg, c.CoreURL, token, mirrorAPIBody{
		Provider:    "github",
		Owner:       owner,
		Repo:        repo,
		ClusterHost: clusterHost,
	})
	if err != nil {
		return err
	}

	repoSlug := "/gh/" + owner + "/" + repo
	mirrorURL := fmt.Sprintf("entire://%s%s", clusterHost, repoSlug)
	fmt.Fprintf(out, "Resolving %s...\n", mirrorURL)
	if resp.Created {
		fmt.Fprintf(out, "  registered new mirror (id %s)\n", resp.MirrorRepoID)
	} else {
		fmt.Fprintln(out, "  found existing mirror")
	}

	gitToken, err := mirrorPreflightTokenForSlug(ctx, cfg, c.CoreURL, token, clusterHost, repoSlug)
	if err != nil {
		return err
	}
	checkURL := fmt.Sprintf("https://%s%s/info/refs?service=git-upload-pack", clusterHost, repoSlug)
	probeClient := cliauth.NewHTTPClient(cfg.SkipTLSVerify)
	ready, reason := mirrorAdvertisesRealHead(ctx, probeClient, checkURL, gitToken)

	switch {
	case ready:
		fmt.Fprintln(out, "  ready to use")
	case wait:
		fmt.Fprint(out, "  cloning...")
		elapsed, err := pollUntilReady(ctx, out, probeClient, checkURL, gitToken, waitTimeout)
		if err != nil {
			fmt.Fprintf(out, "\n  failed: %v\n", err)
			printMirrorSummary(out, owner, repo, clusterHost, resp.MirrorRepoID, resp.Created)
			return err
		}
		fmt.Fprintf(out, " ready (%s)\n", formatPollDuration(elapsed))
	default:
		fmt.Fprintf(out, "  initial clone in progress (%s) — drop --no-wait to block\n", reason)
	}

	printMirrorSummary(out, owner, repo, clusterHost, resp.MirrorRepoID, resp.Created)
	fmt.Fprintf(out, "To point a local clone at this mirror, cd into the checkout and run:\n  entire-repo mirror use %s\n", clusterHost)
	return nil
}

// printMirrorSummary prints the trailing two-row table that follows
// the trace lines. Mirror URL is omitted because it appears in the
// "Resolving ..." headline above; cluster + ID stay even when one of
// them was just named in a trace line so the summary stands on its own
// for copy/paste.
func printMirrorSummary(out io.Writer, owner, repo, clusterHost, mirrorID string, created bool) {
	rows := [][2]string{
		{"Upstream", fmt.Sprintf("github.com/%s/%s", owner, repo)},
		{"Cluster", clusterHost},
	}
	if !created {
		// On the new-mirror branch the ID was just announced in the
		// "registered new mirror (id …)" line; on the found-existing
		// branch it never appeared inline, so add it here.
		rows = append(rows, [2]string{"Mirror ID", mirrorID})
	}
	printTable(out, rows)
}

// formatPollDuration rounds the elapsed time to the nearest second so
// a clone-took-just-now message reads "ready (47s)" instead of
// "ready (47.83247s)". Sub-second clones get rounded up to "1s" so we
// never show "ready (0s)".
func formatPollDuration(d time.Duration) string {
	return max(d.Round(time.Second), time.Second).String()
}

func runMirrorRemove(cmd *cobra.Command, cfg *cliauth.Config, githubURL, clusterHost string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	owner, repo, err := parseGitHubURL(githubURL)
	if err != nil {
		return fmt.Errorf("invalid <github-url>: %w", err)
	}

	httpClient := cliauth.NewHTTPClient(cfg.SkipTLSVerify)
	c, err := cliauth.ResolveClusterContext(ctx, *cfg, clusterHost, httpClient)
	if err != nil {
		return err
	}

	token, err := client.GetTokenWithRefresh(ctx, client.AuthRefreshConfig{
		KeyringService: c.KeychainService,
		BaseURL:        "https://" + clusterHost,
		Username:       c.Handle,
		CoreBaseURL:    c.CoreURL,
		HTTPClient:     httpClient,
		ClientID:       clilogin.DefaultClientID,
	})
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	mirrorID, err := lookupMirrorID(ctx, cfg, c.CoreURL, token, owner, repo, clusterHost)
	if err != nil {
		return err
	}
	if err := deleteMirrorByID(ctx, cfg, c.CoreURL, token, mirrorID); err != nil {
		return err
	}

	fmt.Fprintf(out, "Mirror removed.\n")
	printTable(out, [][2]string{
		{"Upstream", fmt.Sprintf("github.com/%s/%s", owner, repo)},
		{"Cluster", clusterHost},
	})
	return nil
}

type mirrorAPIBody struct {
	Provider    string `json:"provider"`
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	ClusterHost string `json:"clusterHost"`
}

type mirrorAPIResponse struct {
	MirrorRepoID string `json:"mirrorId"`
	MirrorURL    string `json:"mirrorUrl"`
	PublicURL    string `json:"publicUrl"`
	// Created is true when the API actually inserted a new mirror_repos
	// row; false when the (github_repo_id, cluster_slug) pair was
	// already mirrored and the response describes the existing
	// placement. Drives the create-vs-found wording in the CLI's
	// trace-style output.
	Created bool `json:"created"`
}

// mirrorAPIPath is the create-side endpoint. DELETE moves to a path-
// param shape (/api/v1/mirrors/{mirrorId}) and is built per call in
// deleteMirrorByCoords.
const mirrorAPIPath = "/api/v1/mirrors"

// lookupMirrorID resolves a mirror's ULID from its external coords by
// calling GET /api/v1/repos/by-mirror/{provider}/{owner}/{repo}. The
// returned repoId is the same value as the mirror's mirrorId — mirror
// repos identify themselves by the same ULID at both the storage and
// metadata layers. Path + query segments are URL-escaped so unusual
// owner/repo/host characters don't corrupt routing.
func lookupMirrorID(ctx context.Context, cfg *cliauth.Config, coreURL, loginJWT, owner, repo, clusterHost string) (string, error) {
	coreURL = strings.TrimRight(coreURL, "/")
	path := "/api/v1/repos/by-mirror/github/" +
		neturl.PathEscape(owner) + "/" + neturl.PathEscape(repo) +
		"?clusterHost=" + neturl.QueryEscape(clusterHost)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, coreURL+path, nil)
	if err != nil {
		return "", fmt.Errorf("build mirror lookup: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+loginJWT)
	resp, err := cliauth.NewHTTPClient(cfg.SkipTLSVerify).Do(req)
	if err != nil {
		return "", fmt.Errorf("call mirror lookup: %w", err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body) //nolint:errcheck // best-effort
	if resp.StatusCode == http.StatusUnauthorized {
		return "", errors.New("authentication failed, run 'entire-core auth login' to refresh your session")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mirror lookup failed (HTTP %d): %s", resp.StatusCode, extractAPIError(rawBody, strings.TrimSpace(string(rawBody))))
	}
	var out struct {
		RepoID string `json:"repoId"`
	}
	if err := json.Unmarshal(rawBody, &out); err != nil {
		return "", fmt.Errorf("decode mirror lookup: %w", err)
	}
	if out.RepoID == "" {
		return "", fmt.Errorf("mirror lookup returned empty repoId for github.com/%s/%s on %s", owner, repo, clusterHost)
	}
	return out.RepoID, nil
}

// deleteMirrorByID drives DELETE /api/v1/mirrors/{mirrorId} via the same
// cross-juris-aware HTTP client postMirrorAPI uses for create. 404 is
// treated as idempotent success: when two operators race to remove the
// same mirror, only the first DELETE writes; the loser sees 404 but the
// desired end-state (mirror gone) is reached, so the CLI exits 0
// instead of confusing the user with a 'not found' for something they
// were trying to remove anyway.
func deleteMirrorByID(ctx context.Context, cfg *cliauth.Config, coreURL, loginJWT, mirrorID string) error {
	coreURL = strings.TrimRight(coreURL, "/")
	endpoint := coreURL + "/api/v1/mirrors/" + neturl.PathEscape(mirrorID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build DELETE %s: %w", endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+loginJWT)
	resp, err := cliauth.NewHTTPClient(cfg.SkipTLSVerify).Do(req)
	if err != nil {
		return fmt.Errorf("call DELETE %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body) //nolint:errcheck // best-effort
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	case http.StatusUnauthorized:
		return errors.New("authentication failed, run 'entire-core auth login' to refresh your session")
	}
	return fmt.Errorf("DELETE /api/v1/mirrors/%s failed (HTTP %d): %s", mirrorID, resp.StatusCode, extractAPIError(rawBody, strings.TrimSpace(string(rawBody))))
}

// postMirrorAPI is the transport for POST /api/v1/mirrors. Returns the
// decoded response body on 2xx and a pre-formatted error on non-2xx
// (the API surfaces structured JSON errors but the user only sees the
// message string here). DELETE moved to a {mirrorId} path shape and
// goes through deleteMirrorByID directly.
//
// 421 follow + cross-juris token exchange are handled transparently by
// cliauth.NewHTTPClient's crossJurisRoundTripper — by the time we see a 401
// here, it's a genuine auth failure that the transport's retry budget
// could not recover from, not a recoverable cross-juris hop.
func postMirrorAPI(ctx context.Context, cfg *cliauth.Config, coreURL, loginJWT string, body mirrorAPIBody) (*mirrorAPIResponse, error) {
	coreURL = strings.TrimRight(coreURL, "/")
	if coreURL == "" {
		return nil, errors.New("no entire-core URL configured; log in with `entire-core auth login` or set ENTIRE_CORE_AUTH_BASE_URL")
	}
	endpoint := coreURL + mirrorAPIPath
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("build POST %s: %w", mirrorAPIPath, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+loginJWT)
	cliauth.Debugf("mirror api POST %s body=%s", endpoint, string(payload))

	resp, err := cliauth.NewHTTPClient(cfg.SkipTLSVerify).Do(req)
	if err != nil {
		return nil, fmt.Errorf("call POST %s: %w", mirrorAPIPath, err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	logBody := strings.TrimSpace(string(rawBody))
	if len(logBody) > 1024 {
		logBody = logBody[:1024] + "...(truncated)"
	}
	cliauth.Debugf("mirror api response: status=%d body=%s", resp.StatusCode, logBody)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		out := &mirrorAPIResponse{}
		if len(rawBody) == 0 {
			return out, nil
		}
		if err := json.Unmarshal(rawBody, out); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		return out, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errors.New("authentication failed, run 'entire-core auth login' to refresh your session")
	}
	return nil, fmt.Errorf("POST %s failed (HTTP %d): %s", mirrorAPIPath, resp.StatusCode, extractAPIError(rawBody, logBody))
}

// extractAPIError pulls the human-readable message out of the API's
// error envelope so the CLI prints
//
//	POST /api/v1/mirrors failed (HTTP 409): mirror URL already in use
//
// rather than echoing the raw JSON body. Handles both the legacy
// {"error":"..."} shape and the RFC 7807 problem+json shape
// {"type":"...","title":"...","detail":"...","status":...} the new
// /api/v1 surface emits. Falls back to the already-trimmed/log-bounded
// body when the response isn't shaped like either envelope (proxy 502
// HTML, an empty body, etc.).
func extractAPIError(rawBody []byte, fallback string) string {
	if msg := api.ExtractAPIErrorMessage(rawBody); msg != "" {
		return msg
	}
	return fallback
}

// pollUntilReady polls info/refs every 2s until HEAD resolves or the
// deadline expires. Prints a "." per tick as a heartbeat, and a
// "\n  [<reason>] " line whenever the probe outcome changes — so an
// operator watching a long clone sees progress and stuck-state
// transitions instead of an opaque hang. Returns the elapsed time so
// the caller can render "ready (47s)".
func pollUntilReady(ctx context.Context, out io.Writer, httpClient *http.Client, checkURL, token string, timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	if _, ok := ctx.Deadline(); !ok && timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	deadline, _ := ctx.Deadline()
	var lastReason string
	for {
		select {
		case <-ctx.Done():
			return time.Since(start), fmt.Errorf("wait cancelled: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
		ready, reason := mirrorAdvertisesRealHead(ctx, httpClient, checkURL, token)
		if ready {
			return time.Since(start), nil
		}
		if reason != lastReason {
			fmt.Fprintf(out, "\n  [%s] ", reason)
			lastReason = reason
		} else {
			fmt.Fprint(out, ".")
		}
		if time.Now().After(deadline) {
			return time.Since(start), fmt.Errorf("deadline exceeded (last probe: %s)", lastReason)
		}
	}
}

// mirrorAdvertisesRealHead fetches info/refs and reports whether HEAD's
// symbolic target points at a real commit. The second return is a short,
// human-readable summary of the probe outcome — printed by the poll
// loop when it changes so an operator watching the wait can see what
// the data plane is actually returning instead of an opaque hang.
func mirrorAdvertisesRealHead(ctx context.Context, httpClient *http.Client, checkURL, token string) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		return false, fmt.Sprintf("build request: %v", err)
	}
	req.SetBasicAuth("entiredb-cli", token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, fmt.Sprintf("transport: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	// Smart-HTTP wraps the advertisement in a "# service=..." pkt-line
	// header followed by a flush. AdvRefs.Decode expects to start at
	// the first hash line, so strip the wrapper first.
	var sr packp.SmartReply
	if err := sr.Decode(resp.Body); err != nil {
		return false, fmt.Sprintf("decode smart reply: %v", err)
	}
	var adv packp.AdvRefs
	if err := adv.Decode(resp.Body); err != nil {
		return false, fmt.Sprintf("decode advrefs: %v", err)
	}
	if _, err := adv.ResolvedHead(); err != nil {
		return false, "HEAD not yet resolvable (empty mirror)"
	}
	return true, ""
}

func runMirrorEnable(cmd *cobra.Command, cfg *cliauth.Config, clusterHost, remote string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	// Check git-remote-entire is in PATH
	if _, err := exec.LookPath("git-remote-entire"); err != nil {
		return errors.New("git-remote-entire not found in PATH. Install it with: mise run dev:publish")
	}

	// Verify we're in a git repo
	if err := execGit(ctx, "rev-parse", "--git-dir"); err != nil {
		return errors.New("not a git repository (run this from inside a git repo)")
	}

	// Get current remote URL
	currentURL, err := execGitOutput(ctx, "remote", "get-url", remote)
	if err != nil {
		return fmt.Errorf("remote %q not found (use --remote to specify a different remote)", remote)
	}

	// Check if already using entire://
	if strings.HasPrefix(currentURL, "entire://") {
		fmt.Fprintf(out, "Remote %q is already using EntireDB mirror (%s)\n", remote, currentURL)
		return nil
	}

	// Parse owner/repo from GitHub URL
	owner, repo, err := parseGitHubURL(currentURL)
	if err != nil {
		return fmt.Errorf("remote %q is not a GitHub URL: %s\nonly GitHub repositories can be mirrored", remote, currentURL)
	}

	httpClient := cliauth.NewHTTPClient(cfg.SkipTLSVerify)
	c, err := cliauth.ResolveClusterContext(ctx, *cfg, clusterHost, httpClient)
	if err != nil {
		return err
	}

	token, err := client.GetTokenWithRefresh(ctx, client.AuthRefreshConfig{
		KeyringService: c.KeychainService,
		BaseURL:        "https://" + clusterHost,
		Username:       c.Handle,
		CoreBaseURL:    c.CoreURL,
		HTTPClient:     httpClient,
		ClientID:       clilogin.DefaultClientID,
	})
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	// PR 6 cutover: the canonical EntireDB URL for a GitHub mirror is
	// synthesised from upstream coords as /gh/<owner>/<repo>. We no
	// longer call /api/repos/by-mirror — that endpoint targeted the
	// legacy mirror_registry and is being retired. The STS exchange
	// below is the existence check: validateMirrorRepoExchange returns
	// invalid_target when no github_mirror_repo_urls row matches the
	// (cluster_slug, slug) tuple, so an unknown mirror surfaces as an
	// auth failure rather than a successful clone of nothing.
	repoSlug := "/gh/" + owner + "/" + repo

	// The data-plane git endpoint is gated by AcceptGitAuth, which only
	// accepts repo-scoped JWTs — a raw login JWT is rejected with HTTP
	// 403. Exchange the login JWT via core for a repo-scoped token first.
	gitToken, err := mirrorPreflightTokenForSlug(ctx, cfg, c.CoreURL, token, clusterHost, repoSlug)
	if err != nil {
		return err
	}

	clusterBaseURL := "https://" + clusterHost
	if err := checkMirrorStatusBySlug(ctx, out, clusterBaseURL, currentURL, repoSlug, gitToken, cfg.SkipTLSVerify); err != nil {
		return err
	}

	// Save original URL
	if err := execGit(ctx, "config", gitConfigKey(remote), currentURL); err != nil {
		return fmt.Errorf("failed to save original URL: %w", err)
	}

	// Set remote URL to entire://. The host is the cluster network target
	// (git-remote-entire dials it); credential lookup is by issuer and
	// happens inside git-remote-entire. /gh/ is the PR-6 mirror prefix.
	entireURL := fmt.Sprintf("entire://%s%s", clusterHost, repoSlug)
	if err := execGit(ctx, "remote", "set-url", remote, entireURL); err != nil {
		return fmt.Errorf("failed to update remote URL: %w", err)
	}

	fmt.Fprintf(out, "Switched %q from GitHub to EntireDB mirror.\n", remote)
	printTable(out, [][2]string{
		{"Previous URL", currentURL},
		{"EntireDB URL", entireURL},
	})
	fmt.Fprintf(out, "Run 'entire-repo mirror use --disable' to restore the original URL.\n")
	return nil
}

func runMirrorDisable(cmd *cobra.Command, _ *cliauth.Config, remote string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	// Get current remote URL
	currentURL, err := execGitOutput(ctx, "remote", "get-url", remote)
	if err != nil {
		return fmt.Errorf("remote %q not found", remote)
	}

	// Verify it's an entire:// URL
	if !strings.HasPrefix(currentURL, "entire://") {
		return fmt.Errorf("remote %q is not using EntireDB (%s), nothing to disable", remote, currentURL)
	}

	// Restore from saved config
	originalURL, err := execGitOutput(ctx, "config", "--get", gitConfigKey(remote))
	if err != nil || originalURL == "" {
		return fmt.Errorf("no saved original URL found for remote %q, cannot restore", remote)
	}

	// Restore original URL
	if err := execGit(ctx, "remote", "set-url", remote, originalURL); err != nil {
		return fmt.Errorf("failed to restore remote URL: %w", err)
	}

	// Clean up saved config key (best-effort)
	_ = execGit(ctx, "config", "--unset", gitConfigKey(remote)) //nolint:errcheck // non-critical cleanup

	fmt.Fprintf(out, "Restored %q to original URL.\n", remote)
	printTable(out, [][2]string{
		{"EntireDB URL", currentURL},
		{"Restored URL", originalURL},
	})
	return nil
}

func gitConfigKey(remote string) string {
	return fmt.Sprintf("remote.%s.%s", remote, originalURLConfigKey)
}

// gitHubHTTPSRe matches https://github.com/owner/repo or https://github.com/owner/repo.git
var gitHubHTTPSRe = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+?)(?:\.git)?$`)

// gitHubSSHRe matches git@github.com:owner/repo.git or git@github.com:owner/repo
var gitHubSSHRe = regexp.MustCompile(`^git@github\.com:([^/]+)/([^/]+?)(?:\.git)?$`)

// gitHubBareRe matches `github.com/owner/repo` and the even barer
// `owner/repo` — both shapes the `mirror create` help text documents
// and the project plan promises. Anchored on owner/repo with no
// trailing slash so accidental tail content surfaces as a parse error
// rather than silently truncating.
var gitHubBareRe = regexp.MustCompile(`^(?:github\.com/)?([^/\s]+)/([^/\s]+?)(?:\.git)?$`)

func parseGitHubURL(rawURL string) (owner, repo string, err error) {
	// Lower-case the coords here so every downstream slug
	// synthesis (/gh/<owner>/<repo> for remote rewrite, mirror
	// preflight, etc.) matches the lowercased URL the server
	// persists. The server lowercases too, but doing it here also
	// keeps the `mirror use` flow — which never hits the API —
	// consistent.
	if m := gitHubHTTPSRe.FindStringSubmatch(rawURL); m != nil {
		return strings.ToLower(m[1]), strings.ToLower(m[2]), nil
	}
	if m := gitHubSSHRe.FindStringSubmatch(rawURL); m != nil {
		return strings.ToLower(m[1]), strings.ToLower(m[2]), nil
	}
	// Bare forms are last so the SSH / HTTPS patterns win on inputs
	// that match both (a fully-qualified URL would otherwise greedy-
	// match the bare regex's owner as "https:" or "git@github.com:").
	if m := gitHubBareRe.FindStringSubmatch(rawURL); m != nil {
		return strings.ToLower(m[1]), strings.ToLower(m[2]), nil
	}
	return "", "", fmt.Errorf("not a recognized GitHub URL: %s", rawURL)
}

// mirrorPreflightTokenForSlug is the PR-6 /gh/ counterpart to
// mirrorPreflightToken: the audience-slug is the full /gh/<owner>/<repo>
// URL the data plane sees, and STS routes the exchange through
// validateMirrorRepoExchange instead of the native repo path.
func mirrorPreflightTokenForSlug(ctx context.Context, cfg *cliauth.Config, coreURL, token, clusterHost, repoSlug string) (string, error) {
	scoped, err := cliauth.ExchangeRepoScopedToken(ctx, coreURL, token, repoSlug, "pull", "https://"+clusterHost, cliauth.NewHTTPClient(cfg.SkipTLSVerify))
	if err != nil {
		return "", fmt.Errorf("exchange repo-scoped token: %w", err)
	}
	return scoped, nil
}

// checkMirrorStatus has been replaced by checkMirrorStatusBySlug; the
// legacy /git/<path> probe was deleted with the rest of the
// /api/repos/by-mirror flow in PR 6.

// checkMirrorStatusBySlug is the PR-6 /gh/ counterpart to checkMirrorStatus.
// repoSlug already carries its leading /gh/ prefix; the helper just probes
// info/refs at the slug and maps response codes to user-facing messages.
func checkMirrorStatusBySlug(ctx context.Context, out io.Writer, baseURL, upstreamURL, repoSlug, token string, skipTLSVerify bool) error {
	checkURL := fmt.Sprintf("%s%s/info/refs?service=git-upload-pack", baseURL, repoSlug)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.SetBasicAuth("entiredb-cli", token)

	resp, err := cliauth.NewHTTPClient(skipTLSVerify).Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", baseURL, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		// On /gh/, a 404 means STS minted a JWT for a slug the data
		// plane's local repo_urls doesn't know — i.e. the mirror was
		// created but the data-plane provisioning step hasn't landed
		// yet. Surface as a 503-style "still initializing" rather
		// than an outright "no mirror".
		fmt.Fprintf(out, "Warning: mirror for %s is registered but not yet served by this cluster's data plane; retry shortly.\n", upstreamURL)
		return nil
	case http.StatusServiceUnavailable:
		fmt.Fprintf(out, "Warning: mirror for %s is still initializing. Git operations may fail until the initial sync completes.\n", upstreamURL)
		return nil
	case http.StatusUnauthorized:
		return errors.New("authentication failed, run 'entire-core auth login' to refresh your session")
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512)) //nolint:errcheck // best-effort body read
		return fmt.Errorf("unexpected response from server (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// printTable prints rows of [label, value] pairs, left-aligning labels to the widest one.
func printTable(w io.Writer, rows [][2]string) {
	var width int
	for _, r := range rows {
		if len(r[0]) > width {
			width = len(r[0])
		}
	}
	for _, r := range rows {
		fmt.Fprintf(w, "  %-*s  %s\n", width, r[0], r[1])
	}
}

func execGit(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}

func execGitOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

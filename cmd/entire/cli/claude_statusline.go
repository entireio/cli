package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// Claude Code polls the status line several times per second, so the serve path
// must never block on git or network. It serves a cached result immediately and
// spawns a detached background refresh (throttled) that performs the slow
// authenticated trail lookup and rewrites the cache for the next poll.
const (
	// statuslineFreshDuration is how long a cached result is served without
	// triggering a background refresh.
	statuslineFreshDuration = 60 * time.Second
	// statuslineRefreshLock throttles how often a background refresh may start.
	statuslineRefreshLock = 10 * time.Second
	// statuslineRefreshTimeout caps the background lookup.
	statuslineRefreshTimeout = 15 * time.Second
)

// ANSI / OSC-8 escape codes for the rendered status line.
const (
	ansiReset = "\x1b[0m"
	ansiCyan  = "\x1b[36m"
	ansiDim   = "\x1b[2m"
)

// statuslineInfiniteAge is returned as the cache age when there is no usable
// cache, so the caller always treats it as stale.
const statuslineInfiniteAge = time.Duration(math.MaxInt64)

// statuslineWebBase is the web origin used to build trail links, matching the
// canonical entire.io trail path used by the Pi trails extension.
const statuslineWebBase = "https://entire.io"

// statuslineInput is the subset of Claude Code's status-line stdin JSON we need.
type statuslineInput struct {
	CWD       string `json:"cwd"`
	Workspace struct {
		CurrentDir string `json:"current_dir"`
	} `json:"workspace"`
}

// statuslineResult is the resolved trail state cached for fast rendering.
type statuslineResult struct {
	Status      string `json:"status"` // "found", "no-trail", "auth", "error"
	Number      int    `json:"number,omitempty"`
	TrailStatus string `json:"trail_status,omitempty"`
	URL         string `json:"url,omitempty"`
	Message     string `json:"message,omitempty"`
}

// statuslineCachePayload is what we persist between polls.
type statuslineCachePayload struct {
	TS     int64            `json:"ts"` // unix milliseconds
	Result statuslineResult `json:"result"`
}

// newClaudeStatuslineCmd builds the `entire hooks claude-code statusline` verb.
// It bypasses the generic hook dispatcher (executeAgentHook) and its
// per-invocation logging because the status line is polled very frequently and
// must stay fast. The no-op PersistentPreRunE/PostRunE shadow the parent
// claude-code hooks command's logging setup.
func newClaudeStatuslineCmd() *cobra.Command {
	var refresh bool
	var refreshCwd, refreshCacheFile string

	cmd := &cobra.Command{
		Use:               claudecode.HookNameStatusLine,
		Short:             "Render the Entire trail status line for Claude Code",
		Hidden:            true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error { return nil },
		RunE: func(cmd *cobra.Command, _ []string) error {
			if refresh {
				return runClaudeStatuslineRefresh(cmd.Context(), refreshCwd, refreshCacheFile)
			}
			return runClaudeStatuslineServe(cmd)
		},
	}
	cmd.Flags().BoolVar(&refresh, "refresh", false, "Perform the slow trail lookup and rewrite the cache (internal)")
	cmd.Flags().StringVar(&refreshCwd, "cwd", "", "Working directory for the refresh lookup (internal)")
	cmd.Flags().StringVar(&refreshCacheFile, "cache-file", "", "Cache file to rewrite (internal)")
	return cmd
}

// runClaudeStatuslineServe is the fast path run on every poll: it does only
// local, in-process work (branch/remote resolution + cache read) and serves the
// cached result, spawning a detached refresh when the cache is stale.
func runClaudeStatuslineServe(cmd *cobra.Command) error {
	ctx := cmd.Context()

	cwd := readStatuslineCWD(cmd.InOrStdin())
	if cwd != "" {
		_ = os.Chdir(cwd) //nolint:errcheck // best-effort; fall back to process CWD
	}

	forge, owner, repo, branch, ok := resolveStatuslineRepo(ctx)
	if !ok {
		return nil // not a repo, detached HEAD, or unsupported remote → render nothing
	}

	effectiveCwd := cwd
	if effectiveCwd == "" {
		if wd, err := os.Getwd(); err == nil { //nolint:forbidigo // need the real CWD to hand to the detached refresh
			effectiveCwd = wd
		}
	}

	cacheFile := statuslineCacheFile(forge, owner, repo, branch)
	cached, age := readStatuslineCache(cacheFile)
	if age > statuslineFreshDuration {
		spawnStatuslineRefresh(effectiveCwd, cacheFile)
	}
	if cached != nil {
		if out := renderStatusline(*cached); out != "" {
			fmt.Fprint(cmd.OutOrStdout(), out)
		}
	}
	return nil
}

// runClaudeStatuslineRefresh performs the slow authenticated trail lookup and
// rewrites the cache. It prints nothing and is invoked detached.
func runClaudeStatuslineRefresh(ctx context.Context, cwd, cacheFile string) error {
	if cacheFile == "" {
		return nil
	}
	if cwd != "" {
		_ = os.Chdir(cwd) //nolint:errcheck // best-effort
	}

	forge, owner, repo, branch, ok := resolveStatuslineRepo(ctx)
	if !ok {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, statuslineRefreshTimeout)
	defer cancel()

	writeStatuslineCache(cacheFile, resolveStatuslineTrail(ctx, forge, owner, repo, branch))
	return nil
}

// resolveStatuslineRepo resolves the current branch and forge/owner/repo from
// the process CWD using in-process go-git calls. Returns ok=false when there is
// nothing to show (not a repo, detached HEAD, or a remote on an unsupported
// forge).
func resolveStatuslineRepo(ctx context.Context) (forge, owner, repo, branch string, ok bool) {
	branch, err := GetCurrentBranch(ctx)
	if err != nil || branch == "" {
		return "", "", "", "", false
	}
	forge, owner, repo, err = gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil || forge == "" {
		return "", "", "", "", false
	}
	return forge, owner, repo, branch, true
}

// resolveStatuslineTrail performs the authenticated lookup for the branch's
// trail and maps the outcome to a cacheable result.
func resolveStatuslineTrail(ctx context.Context, forge, owner, repo, branch string) statuslineResult {
	client, err := NewAuthenticatedAPIClient(ctx, false)
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return statuslineResult{Status: "auth"}
		}
		return statuslineResult{Status: "error", Message: shortStatuslineError(err.Error())}
	}

	trail, err := findTrailByBranch(ctx, client, forge, owner, repo, branch)
	if err != nil {
		if isStatuslineAuthError(err.Error()) {
			return statuslineResult{Status: "auth"}
		}
		return statuslineResult{Status: "error", Message: shortStatuslineError(err.Error())}
	}
	if trail == nil {
		return statuslineResult{Status: "no-trail"}
	}
	return statuslineResult{
		Status:      "found",
		Number:      trail.Number,
		TrailStatus: trail.Status,
		URL:         statuslineTrailURL(forge, owner, repo, trail, branch),
	}
}

// renderStatusline produces the terminal string for a resolved result.
// "no-trail" and unknown states render empty (segment is simply absent).
func renderStatusline(r statuslineResult) string {
	switch r.Status {
	case "found":
		label := fmt.Sprintf("Trail #%d", r.Number)
		if r.TrailStatus != "" {
			label += " " + r.TrailStatus
		}
		if r.URL != "" {
			label = osc8Hyperlink(r.URL, label)
		}
		return ansiCyan + label + ansiReset
	case "auth":
		return ansiDim + "Trail: run `entire login`" + ansiReset
	case "error":
		if r.Message != "" {
			return ansiDim + "Trail: " + r.Message + ansiReset
		}
		return ""
	default:
		return ""
	}
}

// osc8Hyperlink wraps label in an OSC-8 terminal hyperlink pointing at url.
func osc8Hyperlink(url, label string) string {
	return "\x1b]8;;" + url + "\x07" + label + "\x1b]8;;\x07"
}

// statuslineTrailURL builds the web URL for a trail, mirroring the Pi trails
// extension scheme: https://entire.io/<forge>/<owner>/<repo>/trails/<n>[/<slug>].
func statuslineTrailURL(forge, owner, repo string, trail *api.TrailResource, branch string) string {
	if trail == nil || trail.Number == 0 {
		return ""
	}
	titleOrBranch := trail.Title
	if titleOrBranch == "" {
		titleOrBranch = branch
	}
	u := fmt.Sprintf("%s/%s/%s/%s/trails/%d", statuslineWebBase, forge, owner, repo, trail.Number)
	if slug := slugifyTitle(titleOrBranch); slug != "" {
		u += "/" + slug
	}
	return u
}

// readStatuslineCWD extracts the working directory from Claude Code's status
// line stdin JSON, preferring workspace.current_dir.
func readStatuslineCWD(r io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil || len(data) == 0 {
		return ""
	}
	var in statuslineInput
	if json.Unmarshal(data, &in) != nil {
		return ""
	}
	if in.Workspace.CurrentDir != "" {
		return in.Workspace.CurrentDir
	}
	return in.CWD
}

// statuslineCacheFile returns the cache path for a forge/owner/repo + branch.
func statuslineCacheFile(forge, owner, repo, branch string) string {
	sum := sha256.Sum256([]byte(forge + "/" + owner + "/" + repo + "|" + branch))
	key := hex.EncodeToString(sum[:])[:16]
	return filepath.Join(userdirs.Cache(), "statusline", key+".json")
}

// readStatuslineCache reads a cached result, returning (nil, infinite age) when
// the cache is missing or unusable.
func readStatuslineCache(cacheFile string) (*statuslineResult, time.Duration) {
	data, err := os.ReadFile(cacheFile) //nolint:gosec // path derived from cache dir + hashed key
	if err != nil {
		return nil, statuslineInfiniteAge
	}
	var payload statuslineCachePayload
	if json.Unmarshal(data, &payload) != nil || payload.TS == 0 {
		return nil, statuslineInfiniteAge
	}
	return &payload.Result, time.Since(time.UnixMilli(payload.TS))
}

// writeStatuslineCache atomically writes a result to the cache file.
func writeStatuslineCache(cacheFile string, result statuslineResult) {
	payload := statuslineCachePayload{TS: time.Now().UnixMilli(), Result: result}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o700); err != nil {
		return
	}
	tmp := cacheFile + ".tmp"
	if os.WriteFile(tmp, data, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, cacheFile) //nolint:errcheck // a failed swap just means a re-resolve next poll
}

// spawnStatuslineRefresh launches the detached background refresh. It is a
// package var so tests can stub it instead of spawning real subprocesses.
var spawnStatuslineRefresh = maybeSpawnStatuslineRefresh

// maybeSpawnStatuslineRefresh starts a detached background refresh unless one
// started recently (throttled by a lock file's mtime).
func maybeSpawnStatuslineRefresh(cwd, cacheFile string) {
	lock := cacheFile + ".lock"
	if info, err := os.Stat(lock); err == nil && time.Since(info.ModTime()) < statuslineRefreshLock {
		return // a refresh is already in flight
	}
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err == nil {
		_ = os.WriteFile(lock, []byte(time.Now().Format(time.RFC3339Nano)), 0o600) //nolint:errcheck // best-effort lock
	}

	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "entire"
	}
	// Detach from the parent (Setsid) and from this turn's context so the
	// refresh survives after the fast poll returns.
	child := execx.NonInteractive(context.Background(), exe,
		"hooks", "claude-code", claudecode.HookNameStatusLine,
		"--refresh", "--cwd", cwd, "--cache-file", cacheFile)
	_ = child.Start() //nolint:errcheck // best-effort; the link just appears a poll later
}

// isStatuslineAuthError reports whether an error message indicates the user is
// not authenticated, so the status line can show the login hint.
func isStatuslineAuthError(msg string) bool {
	m := strings.ToLower(msg)
	for _, needle := range []string{"not logged in", "unauthorized", "authentication required", "run 'entire login'", "please log in", "401"} {
		if strings.Contains(m, needle) {
			return true
		}
	}
	return false
}

// shortStatuslineError trims an error to a single short line that fits the
// status bar.
func shortStatuslineError(msg string) string {
	line := strings.TrimSpace(msg)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if len(line) > 60 {
		line = line[:60]
	}
	if line == "" {
		return "lookup failed"
	}
	return line
}

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/session"

	"github.com/spf13/cobra"
)

// The trail status command is the polling engine behind agent status-line
// integrations (Claude Code's statusLine, the Codex/other-agent session-start
// banner). Agents invoke `entire trail status` frequently, so it must be fast
// and never block: results are cached on disk per repo+branch with a short TTL
// and a stale-while-revalidate fallback, and every non-trail state degrades to
// empty output rather than an error so a status line stays clean.

const (
	// trailStatusFreshTTL is how long a cached snapshot is served without
	// hitting the API. Agent status lines re-run this command on every
	// assistant message; a 30s window keeps the vast majority of those reads
	// purely local while still surfacing new findings promptly.
	trailStatusFreshTTL = 30 * time.Second
	// trailStatusDefaultTimeout bounds the foreground refresh on a cache miss
	// so a slow network never stalls the status line for long. Claude Code
	// cancels an in-flight status-line command when a newer update arrives, so
	// a short bound here keeps the line responsive.
	trailStatusDefaultTimeout = 2500 * time.Millisecond
	trailStatusCacheFileName  = "trail-status.json"
	// trailStatusEnableWarmTimeout bounds the best-effort cache warm performed
	// during `entire enable` so warming never noticeably slows setup.
	trailStatusEnableWarmTimeout = 3 * time.Second
	// trailStatusCacheMaxEntries caps the per-repo cache file so long-lived
	// clones that touch many branches don't grow it without bound.
	trailStatusCacheMaxEntries = 64
	trailStatusMaxTitleRunes   = 48
	// trailStatusFindingsScanLimit is the single-page size used to count open
	// findings. One page keeps the refresh cheap; more than this many open
	// findings on a single trail renders as "N+".
	trailStatusFindingsScanLimit = 100
)

// trail status states. These are stable JSON values consumed by the agent
// banner path and any external reader of `--format json`.
const (
	trailStatusStateTrail   = "trail"
	trailStatusStateNoTrail = "no_trail"
	// trailStatusStateDisabled means trails are not enabled for this repo.
	trailStatusStateDisabled = "disabled"
	trailStatusStateUnauth   = "unauthenticated"
	// trailStatusStateNoRepo means the working dir is not a clone of a
	// forge-backed repo Entire trails support.
	trailStatusStateNoRepo = "no_repo"
	trailStatusStateError  = "error"
)

const (
	trailStatusFormatStatusline = "statusline"
	trailStatusFormatPlain      = "plain"
	trailStatusFormatJSON       = "json"
)

// trailStatusSnapshot is the cached, rendered-from result of resolving the
// current branch's trail. It is both the on-disk cache entry and the
// `--format json` payload.
type trailStatusSnapshot struct {
	State         string    `json:"state"`
	Forge         string    `json:"forge,omitempty"`
	Owner         string    `json:"owner,omitempty"`
	Repo          string    `json:"repo,omitempty"`
	Branch        string    `json:"branch,omitempty"`
	Number        int       `json:"number,omitempty"`
	Title         string    `json:"title,omitempty"`
	TrailStatus   string    `json:"trail_status,omitempty"`
	Phase         string    `json:"phase,omitempty"`
	URL           string    `json:"url,omitempty"`
	OpenFindings  int       `json:"open_findings"`
	HighFindings  int       `json:"high_findings"`
	FindingsKnown bool      `json:"findings_known"`
	Message       string    `json:"message,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`
}

// trailStatusCacheFile is the on-disk cache shape: one entry per repo+branch.
type trailStatusCacheFile struct {
	Entries map[string]trailStatusSnapshot `json:"entries"`
}

type trailStatusOptions struct {
	Format       string
	Dir          string
	Timeout      time.Duration
	NoCache      bool
	Refresh      bool
	InsecureHTTP bool
}

// trailStatusStdinPayload is the subset of an agent status-line/hook stdin
// payload the command reads to locate the workspace. Claude Code's statusLine
// payload carries both `cwd` and `workspace.current_dir`; Codex hook payloads
// carry `cwd`. Everything else is ignored.
type trailStatusStdinPayload struct {
	CWD       string `json:"cwd"`
	Workspace struct {
		CurrentDir string `json:"current_dir"`
	} `json:"workspace"`
}

func newTrailStatusCmd() *cobra.Command {
	opts := trailStatusOptions{Format: trailStatusFormatStatusline, Timeout: trailStatusDefaultTimeout}

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print a one-line summary of the current branch's trail",
		Long: `Print a compact summary of the trail connected to the current branch.

Designed for agent status lines (Claude Code's statusLine, the Codex
session-start banner): results are cached per repo+branch with a short TTL so
repeated calls are instant, and any non-trail state (no trail, trails disabled,
not logged in, not a supported repo) prints nothing rather than an error so the
status line stays clean.

When given a status-line/hook JSON payload on stdin, the working directory is
read from it (cwd / workspace.current_dir) so the trail is resolved against the
agent's workspace regardless of where the command was launched.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.InsecureHTTP = trailInsecureHTTP(cmd)
			return runTrailStatus(cmd, opts)
		},
	}

	cmd.Flags().StringVar(&opts.Format, "format", opts.Format, "Output format: statusline, plain, or json")
	cmd.Flags().StringVar(&opts.Dir, "dir", "", "Resolve the trail for this directory instead of the current one")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", opts.Timeout, "Max time to spend refreshing before falling back to cached data")
	cmd.Flags().BoolVar(&opts.NoCache, "no-cache", false, "Bypass the cache and always refresh")
	cmd.Flags().BoolVar(&opts.Refresh, "refresh", false, "Force a refresh and update the cache (used to warm the cache)")

	return cmd
}

func runTrailStatus(cmd *cobra.Command, opts trailStatusOptions) error {
	if err := validateTrailStatusFormat(opts.Format); err != nil {
		return err
	}
	ctx := cmd.Context()

	// Resolve relative to the agent's workspace when a dir was supplied (flag
	// or stdin payload). The command is a short-lived, single-purpose process,
	// so changing its working directory here is safe and lets every CWD-based
	// git helper resolve the right repo.
	if dir := trailStatusTargetDir(opts, cmd.InOrStdin()); dir != "" {
		if err := os.Chdir(dir); err != nil {
			logging.Debug(ctx, "trail status: chdir to workspace failed", "dir", dir, "error", err.Error())
		}
	}

	snap := resolveTrailStatusForRender(ctx, opts)
	return writeTrailStatus(cmd.OutOrStdout(), snap, opts.Format)
}

func validateTrailStatusFormat(format string) error {
	switch format {
	case trailStatusFormatStatusline, trailStatusFormatPlain, trailStatusFormatJSON:
		return nil
	default:
		return fmt.Errorf("invalid --format %q: valid values are %s, %s, %s",
			format, trailStatusFormatStatusline, trailStatusFormatPlain, trailStatusFormatJSON)
	}
}

// resolveTrailStatusForRender returns the snapshot to render: a fresh cache hit
// when available, otherwise a bounded refresh, falling back to a stale cache
// entry (so the line never blanks on a transient failure) and finally to a
// state derived from the error.
func resolveTrailStatusForRender(ctx context.Context, opts trailStatusOptions) trailStatusSnapshot {
	key, keyErr := trailStatusCacheKey(ctx)
	useCache := keyErr == nil && !opts.NoCache

	if useCache && !opts.Refresh {
		if snap, ok := loadTrailStatusCache(ctx, key); ok && trailStatusFresh(snap, time.Now()) {
			return snap
		}
	}

	refreshCtx := ctx
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		refreshCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	snap, err := resolveTrailStatusSnapshot(refreshCtx, opts.InsecureHTTP)
	if err == nil {
		if useCache {
			saveTrailStatusCacheBestEffort(ctx, key, snap)
		}
		return snap
	}

	// Refresh failed: prefer stale cached data over showing nothing.
	if useCache {
		if stale, ok := loadTrailStatusCache(ctx, key); ok {
			return stale
		}
	}
	return trailStatusSnapshotFromError(err)
}

// resolveTrailStatusSnapshot performs the live resolve: scope → branch → auth →
// trail → open finding counts. Expected "nothing to show" outcomes (no repo,
// no branch, not logged in, trails disabled, no trail) return a typed snapshot
// with a nil error; only genuine failures (network, 5xx, unexpected) return an
// error for the caller to fall back on.
func resolveTrailStatusSnapshot(ctx context.Context, insecureHTTP bool) (trailStatusSnapshot, error) {
	now := time.Now()

	scope, err := currentTrailEnablementScope(ctx)
	if err != nil || !scope.Supported {
		// A resolve failure degrades to a typed "nothing to show" snapshot, not a command error.
		noRepo := trailStatusSnapshot{State: trailStatusStateNoRepo, CheckedAt: now}
		return noRepo, nil //nolint:nilerr // intentional: degrade, don't surface the error
	}

	branch, err := GetCurrentBranch(ctx)
	if err != nil || strings.TrimSpace(branch) == "" {
		// A detached HEAD / unknown branch is "no trail", not a command error.
		noTrail := trailStatusSnapshot{
			State: trailStatusStateNoTrail,
			Forge: scope.Forge, Owner: scope.Owner, Repo: scope.Repo,
			CheckedAt: now,
		}
		return noTrail, nil //nolint:nilerr // intentional: degrade, don't surface the error
	}

	client, err := NewAuthenticatedAPIClient(ctx, insecureHTTP)
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return trailStatusSnapshot{State: trailStatusStateUnauth, Branch: branch, CheckedAt: now}, nil
		}
		return trailStatusSnapshot{}, err
	}

	found, err := findTrailByBranch(ctx, client, scope.Forge, scope.Owner, scope.Repo, branch)
	if err != nil {
		if trailStatusErrIsUnauth(err) {
			return trailStatusSnapshot{State: trailStatusStateUnauth, Branch: branch, CheckedAt: now}, nil
		}
		if trailStatusErrIsDisabled(err) {
			saveTrailsEnabledForRemoteBestEffort(ctx, scope.Forge, scope.Owner, scope.Repo, false)
			return trailStatusSnapshot{State: trailStatusStateDisabled, Branch: branch, CheckedAt: now}, nil
		}
		return trailStatusSnapshot{}, err
	}
	saveTrailsEnabledForRemoteBestEffort(ctx, scope.Forge, scope.Owner, scope.Repo, true)

	if found == nil {
		return trailStatusSnapshot{
			State: trailStatusStateNoTrail,
			Forge: scope.Forge, Owner: scope.Owner, Repo: scope.Repo,
			Branch:    branch,
			CheckedAt: now,
		}, nil
	}

	m := found.ToMetadata()
	snap := trailStatusSnapshot{
		State:       trailStatusStateTrail,
		Forge:       scope.Forge,
		Owner:       scope.Owner,
		Repo:        scope.Repo,
		Branch:      branch,
		Number:      m.Number,
		Title:       strings.TrimSpace(m.Title),
		TrailStatus: string(m.Status),
		Phase:       m.Phase,
		URL:         trailDisplayURL(*found, scope.Forge, scope.Owner, scope.Repo),
		CheckedAt:   now,
	}
	if found.ID != "" {
		if open, high, ok := fetchTrailStatusFindingCounts(ctx, client, found.ID); ok {
			snap.OpenFindings = open
			snap.HighFindings = high
			snap.FindingsKnown = true
		}
	}
	return snap, nil
}

// fetchTrailStatusFindingCounts returns the open / open-high finding counts for
// a trail from a single page. It is best-effort: a failure leaves the counts
// unknown rather than failing the whole status resolve.
func fetchTrailStatusFindingCounts(ctx context.Context, client *api.Client, trailID string) (open, high int, ok bool) {
	comments, _, err := fetchTrailReviewComments(ctx, client, trailID, trailReviewListOptions{
		Status:    trailReviewStatusOpen,
		Freshness: trailReviewFreshnessCurrent,
		Limit:     trailStatusFindingsScanLimit,
	})
	if err != nil {
		return 0, 0, false
	}
	counts := countTrailReviewComments(comments)
	return counts.Open, counts.OpenHigh, true
}

func trailStatusErrIsDisabled(err error) bool {
	return api.IsHTTPErrorStatus(err, http.StatusForbidden) ||
		api.IsHTTPErrorStatus(err, http.StatusNotFound) ||
		api.IsHTTPErrorStatus(err, http.StatusGone)
}

func trailStatusErrIsUnauth(err error) bool {
	return api.IsHTTPErrorStatus(err, http.StatusUnauthorized)
}

func trailStatusSnapshotFromError(err error) trailStatusSnapshot {
	now := time.Now()
	if errors.Is(err, auth.ErrNotLoggedIn) || trailStatusErrIsUnauth(err) {
		return trailStatusSnapshot{State: trailStatusStateUnauth, CheckedAt: now}
	}
	return trailStatusSnapshot{State: trailStatusStateError, Message: oneLineError(err), CheckedAt: now}
}

// oneLineError collapses an error to its first line for compact display.
func oneLineError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if idx := strings.IndexByte(msg, '\n'); idx >= 0 {
		return strings.TrimSpace(msg[:idx])
	}
	return msg
}

// --- working directory resolution ---

func trailStatusTargetDir(opts trailStatusOptions, in io.Reader) string {
	if d := strings.TrimSpace(opts.Dir); d != "" {
		return d
	}
	return trailStatusDirFromStdin(in)
}

// trailStatusDirFromStdin reads a status-line/hook JSON payload from stdin and
// returns its workspace dir. It only reads when stdin is piped (not a TTY) so a
// manual `entire trail status` in a terminal never blocks waiting on input.
func trailStatusDirFromStdin(in io.Reader) string {
	if f, ok := in.(*os.File); ok {
		info, err := f.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice != 0 {
			return ""
		}
	}
	data, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil || len(data) == 0 {
		return ""
	}
	var p trailStatusStdinPayload
	if json.Unmarshal(data, &p) != nil {
		return ""
	}
	if d := strings.TrimSpace(p.Workspace.CurrentDir); d != "" {
		return d
	}
	return strings.TrimSpace(p.CWD)
}

// --- cache ---

func trailStatusFresh(snap trailStatusSnapshot, now time.Time) bool {
	if snap.CheckedAt.IsZero() || now.Before(snap.CheckedAt) {
		return false
	}
	return now.Sub(snap.CheckedAt) <= trailStatusFreshTTL
}

func trailStatusCacheKey(ctx context.Context) (string, error) {
	scope, err := currentTrailEnablementScope(ctx)
	if err != nil {
		return "", err
	}
	branch, err := GetCurrentBranch(ctx)
	if err != nil || strings.TrimSpace(branch) == "" {
		return "", errors.New("trail status: current branch is unknown")
	}
	return scope.RepoKey + "@" + branch, nil
}

func trailStatusCachePath(ctx context.Context) (string, error) {
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}
	return filepath.Join(commonDir, session.SessionStateDirName, trailStatusCacheFileName), nil
}

func loadTrailStatusCache(ctx context.Context, key string) (trailStatusSnapshot, bool) {
	path, err := trailStatusCachePath(ctx)
	if err != nil {
		return trailStatusSnapshot{}, false
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is derived from the git common dir
	if err != nil {
		return trailStatusSnapshot{}, false
	}
	var file trailStatusCacheFile
	if json.Unmarshal(data, &file) != nil {
		return trailStatusSnapshot{}, false
	}
	snap, ok := file.Entries[key]
	return snap, ok
}

func saveTrailStatusCacheBestEffort(ctx context.Context, key string, snap trailStatusSnapshot) {
	if err := saveTrailStatusCache(ctx, key, snap); err != nil {
		logging.Debug(ctx, "failed to cache trail status", "error", err.Error())
	}
}

func saveTrailStatusCache(ctx context.Context, key string, snap trailStatusSnapshot) error {
	path, err := trailStatusCachePath(ctx)
	if err != nil {
		return err
	}
	file := trailStatusCacheFile{Entries: map[string]trailStatusSnapshot{}}
	if data, rerr := os.ReadFile(path); rerr == nil { //nolint:gosec // path derived from git common dir
		if json.Unmarshal(data, &file) != nil || file.Entries == nil {
			file.Entries = map[string]trailStatusSnapshot{}
		}
	}
	file.Entries[key] = snap
	file.Entries = pruneTrailStatusCache(file.Entries)

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create session state dir: %w", err)
	}
	data, err := jsonutil.MarshalIndentWithNewline(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal trail status cache: %w", err)
	}
	if err := jsonutil.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write trail status cache: %w", err)
	}
	return nil
}

// pruneTrailStatusCache caps the cache to the most-recently-checked entries.
func pruneTrailStatusCache(entries map[string]trailStatusSnapshot) map[string]trailStatusSnapshot {
	if len(entries) <= trailStatusCacheMaxEntries {
		return entries
	}
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return entries[keys[i]].CheckedAt.After(entries[keys[j]].CheckedAt)
	})
	pruned := make(map[string]trailStatusSnapshot, trailStatusCacheMaxEntries)
	for _, k := range keys[:trailStatusCacheMaxEntries] {
		pruned[k] = entries[k]
	}
	return pruned
}

// warmTrailStatusCache refreshes and caches the current branch's trail status.
// It is best-effort and bounded; callers (e.g. `entire enable`) use it so the
// first agent status-line poll — and the Codex/banner cache-only read — render
// from a warm cache instead of cold. It no-ops when the cache is already fresh,
// so calling it once per configured agent triggers at most one network refresh.
func warmTrailStatusCache(ctx context.Context, insecureHTTP bool) {
	key, err := trailStatusCacheKey(ctx)
	if err != nil {
		return
	}
	if snap, ok := loadTrailStatusCache(ctx, key); ok && trailStatusFresh(snap, time.Now()) {
		return
	}
	snap, err := resolveTrailStatusSnapshot(ctx, insecureHTTP)
	if err != nil {
		logging.Debug(ctx, "trail status cache warm skipped", "error", err.Error())
		return
	}
	saveTrailStatusCacheBestEffort(ctx, key, snap)
}

// warmTrailStatusCacheBestEffort warms the cache within a short bound so it
// never noticeably slows `entire enable`.
func warmTrailStatusCacheBestEffort(ctx context.Context, insecureHTTP bool) {
	warmCtx, cancel := context.WithTimeout(ctx, trailStatusEnableWarmTimeout)
	defer cancel()
	warmTrailStatusCache(warmCtx, insecureHTTP)
}

// cachedTrailStatusSnapshot returns a cached snapshot for the current branch
// without any network access. The session-start banner uses this so it never
// slows session startup.
func cachedTrailStatusSnapshot(ctx context.Context) (trailStatusSnapshot, bool) {
	key, err := trailStatusCacheKey(ctx)
	if err != nil {
		return trailStatusSnapshot{}, false
	}
	return loadTrailStatusCache(ctx, key)
}

// sessionStartTrailBanner returns a one-line trail summary for the session-start
// banner, or "" when there's nothing to show. It is cache-only (no network) so
// it never slows session startup, and it stays silent for agents that already
// render Entire's status line — those surface the trail there instead.
func sessionStartTrailBanner(ctx context.Context, ag agent.Agent) string {
	if sl, ok := agent.AsStatusLineSupport(ag); ok && sl.IsStatusLineInstalled(ctx) {
		return ""
	}
	if !trailsEnabledForRepo(ctx) {
		return ""
	}
	snap, ok := cachedTrailStatusSnapshot(ctx)
	if !ok {
		return ""
	}
	return trailBannerLine(snap)
}

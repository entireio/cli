package strategy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/settings"

	"github.com/go-git/go-git/v6/plumbing"
)

// checkpointRemoteFetchTimeout bounds checkpoint-remote fetches made from the
// push hot path, where the user's own `git push` is blocked for the duration.
const checkpointRemoteFetchTimeout = 30 * time.Second

// checkpointFetchMode selects how a checkpoint-remote fetch is bounded.
type checkpointFetchMode int

const (
	// hotPathFetch bounds the fetch by checkpointRemoteFetchTimeout.
	hotPathFetch checkpointFetchMode = iota
	// foregroundFetch is for user-initiated commands (enable, resume, explain,
	// delete), which fetch the whole transcript archive. No fixed deadline
	// suits that: one the archive outgrows discards the partial transfer, the
	// ref never lands, and every later command restarts the same fetch from
	// zero. So these abort a stalled transfer, run under a per-repository lock
	// so concurrent commands transfer the archive once, and are bounded only by
	// the overall budget (see startForegroundFetch).
	foregroundFetch
)

// checkpointRemoteFetchLockName is the lock file, in the git common dir, that
// serializes foreground checkpoint-remote fetches across processes.
const checkpointRemoteFetchLockName = "entire-checkpoint-remote-fetch.lock"

// checkpointRemoteFetchFailureName is the file, next to the fetch lock, that
// records the last foreground fetch that stalled or ran out of budget.
const checkpointRemoteFetchFailureName = "entire-checkpoint-remote-fetch-failure.json"

// recentSlowFetchFailureWindow is how long after a slow failure foreground
// fetches from the same remote fail fast. A stalled remote costs every fetch a
// full stall window, so without it commands queued on the fetch lock each pay
// that window in turn, and so does every new process started meanwhile.
const recentSlowFetchFailureWindow = 2 * time.Minute

var errRecentSlowFetchFailure = errors.New("checkpoint remote failed recently")

// defaultForegroundFetchBudget bounds a foreground fetch, lock wait included.
// It is well above the ~11 minutes a multi-gigabyte archive took to transfer,
// so a working fetch finishes, while a caller still gets an answer eventually.
const defaultForegroundFetchBudget = 30 * time.Minute

// foregroundFetchBudgetEnvVar overrides defaultForegroundFetchBudget with a Go
// duration such as "45m". It is read from the environment so programs that run
// entire as a subprocess can set it for every command they start.
const foregroundFetchBudgetEnvVar = "ENTIRE_CHECKPOINT_FETCH_TIMEOUT"

var errForegroundFetchBudget = errors.New("checkpoint fetch budget exhausted")

// pushSettings holds the resolved push configuration from a single settings load.
type pushSettings struct {
	// remote is the git remote name to use for pushing (the user's push remote).
	remote string
	// checkpointURL is the derived URL for pushing checkpoint branches.
	// When set, checkpoint/trails branches are pushed directly to this URL
	// instead of the remote name. Empty means use the remote name.
	checkpointURL string
	// pushDisabled is true if push_sessions is explicitly set to false.
	pushDisabled bool
	// checkpointRemoteConfigured records that settings NAME a checkpoint_remote,
	// which checkpointURL alone cannot express: an empty checkpointURL means
	// either "none configured" or "configured and refused", and only the second
	// is worth telling the user about.
	checkpointRemoteConfigured bool
	// primaryIsRefs records whether the git-refs backend is the configured
	// primary, resolved once here so the pre-push path does not re-read the
	// checkpoints config (LoadCheckpointsConfig is uncached: two whole-file
	// reads and JSON parses per call).
	primaryIsRefs bool
}

// pushTarget returns the target to use for git push/fetch commands for checkpoint branches.
// If a checkpoint URL is configured, returns that; otherwise returns the remote name.
func (ps *pushSettings) pushTarget() string {
	if ps.checkpointURL != "" {
		return ps.checkpointURL
	}
	return ps.remote
}

// hasCheckpointURL returns true if a dedicated checkpoint URL is configured.
func (ps *pushSettings) hasCheckpointURL() bool {
	return ps.checkpointURL != ""
}

// resolvePushSettings loads settings once and returns the resolved push config.
// If a structured checkpoint_remote is configured (e.g., {"provider": "github", "repo": "org/repo"}):
//   - Derives the checkpoint URL from the push remote's protocol (SSH vs HTTPS)
//   - Ignores the setting when it looks inherited from an upstream project rather
//     than configured by this developer, so a fork contributor's checkpoints are
//     not pushed into the upstream's checkpoint repo (see
//     remote.checkpointRemoteIsInherited for how ownership is established)
//   - If a checkpoint branch doesn't exist locally, attempts to fetch it from the URL
//
// The push itself handles failures gracefully (doPushRef warns and continues),
// so no reachability check is needed here.
func resolvePushSettings(ctx context.Context, pushRemoteName string) pushSettings {
	s, err := settings.Load(ctx)
	if err != nil {
		return pushSettings{remote: pushRemoteName}
	}

	ps := pushSettings{
		remote:        pushRemoteName,
		pushDisabled:  s.IsPushSessionsDisabled(),
		primaryIsRefs: primaryIsGitRefs(ctx),
	}

	config := s.GetCheckpointRemote()
	if config == nil {
		return ps
	}
	ps.checkpointRemoteConfigured = true
	checkpointURL, enabled, err := remote.PushURL(ctx, pushRemoteName)
	if err != nil {
		logging.Warn(ctx, "checkpoint-remote: could not derive URL from push remote",
			slog.String("remote", pushRemoteName),
			slog.String("repo", config.Repo),
			slog.String("error", err.Error()),
		)
		return ps
	}
	if !enabled || checkpointURL == "" {
		return ps
	}

	ps.checkpointURL = checkpointURL

	// If the v1 checkpoint branch doesn't exist locally, try to fetch it from the URL.
	// This is a one-time operation — once the branch exists locally, subsequent pushes
	// skip the fetch entirely. Only fetch the metadata branch; trails are always pushed
	// to the user's push remote, not the checkpoint remote.
	//
	// Skipped entirely under the git-refs primary backend, where the "one-time"
	// framing does not hold: that backend pushes per-checkpoint refs and never
	// writes the local v1 branch (see prePush), so the branch stays missing
	// forever and every single push would re-pay the fetch. On a large checkpoint
	// remote that is a multi-second stall on each `git push` for a branch this
	// push will not touch.
	if !ps.primaryIsRefs {
		if err := fetchMetadataBranchIfMissing(ctx, checkpointURL); err != nil {
			logging.Warn(ctx, "checkpoint-remote: failed to fetch metadata branch",
				slog.String("error", err.Error()),
			)
		}
	}

	return ps
}

// FetchMetadataBranch fetches the metadata branch from the checkpoint remote URL
// and updates the local branch. Unlike fetchMetadataBranchIfMissing, this always
// fetches regardless of whether the branch exists locally (for resume scenarios
// where the local branch may be stale).
//
// The fetch is unfiltered (NoFilter: true) because resume needs blob content
// (transcripts, metadata JSON) — not just tree objects, and it runs as a
// foregroundFetch for the same reason: it is the call that actually moves the
// transcript archive.
func FetchMetadataBranch(ctx context.Context, remoteURL string) error {
	return fetchMetadataBranchWithin(ctx, remoteURL, foregroundFetch)
}

func fetchMetadataBranchWithin(ctx context.Context, remoteURL string, mode checkpointFetchMode) error {
	refs := checkpoint.ResolveRefs(ctx)
	if !refs.Primary.IsBranch() {
		return fmt.Errorf("primary metadata ref %s is not a branch", refs.Primary)
	}
	branchName := refs.Primary.Short()
	tmpRef := FetchTmpRefPrefix + branchName
	if mode == hotPathFetch {
		// The hot path skips the fetch lock rather than stall a push behind a
		// long foreground fetch, so it must not share that fetch's tmp ref.
		tmpRef = FetchTmpRefPrefix + "push/" + branchName
	}
	srcRef := refs.Primary.String()

	fetchCtx := ctx
	if mode == foregroundFetch {
		var release func()
		var err error
		fetchCtx, release, err = startForegroundFetch(ctx, "")
		if err != nil {
			return err
		}
		defer release()
	}
	if err := fetchURLIntoTmpRef(fetchCtx, "", remoteURL, srcRef, tmpRef, "metadata branch", true, mode); err != nil {
		return err
	}
	if err := PromoteTmpRefSafely(ctx, plumbing.ReferenceName(tmpRef), refs.Primary, branchName); err != nil {
		return err
	}

	return nil
}

// fetchURLIntoTmpRef runs `git fetch <remoteURL> +<srcRef>:<tmpRef>` via the
// checkpoint git wrapper, disabling the terminal prompt so a misconfigured
// credential helper doesn't hang the process. Errors include the redacted URL
// and any captured stderr so operators can diagnose without credentials
// leaking into logs.
//
// When noFilter is true, --filter=blob:none is suppressed even if filtered
// fetches are globally enabled. v1's blobs are the full transcript archive, so
// an unfiltered fetch costs the whole history where a filtered one costs only
// the commit graph — but every caller here passes true, and the reason is worth
// stating because the cheaper option looks safe and is not.
//
// Filtering would be correct only where nothing downstream reads blob content
// from the local branch. No caller meets that bar: each one lands v1 as the
// repo's checkpoint store, and GitStore.List reads each checkpoint's
// metadata.json through a plain tree with no blob fetcher attached. Worse, the
// read path's recovery tier is keyed on the *ref* being missing, so once a
// filtered fetch lands the ref it never fires again — leaving `checkpoint list`
// permanently showing bare IDs with no prompt, date, or counts, recoverable only
// by explaining each checkpoint by ID.
//
// The parameter is kept rather than inlined so the contract stays explicit at
// each call site.
// mode bounds the fetch: hotPathFetch on the push hot path, foregroundFetch from
// user-initiated commands.
func fetchURLIntoTmpRef(ctx context.Context, dir, remoteURL, srcRef, tmpRef, label string, noFilter bool, mode checkpointFetchMode) error { //nolint:unparam // every caller needs blob content today; see the doc above for why the filtered variant is unsafe here
	opts := remote.FetchOptions{
		Remote:   remoteURL,
		RefSpecs: []string{fmt.Sprintf("+%s:%s", srcRef, tmpRef)},
		NoTags:   true,
		NoFilter: noFilter,
		Dir:      dir,
	}
	fetchCtx, cancel := boundFetch(ctx, mode, &opts)
	defer cancel()
	redactedURL := remote.RedactURL(remoteURL)
	if mode == foregroundFetch {
		if err := recentSlowFetchFailure(ctx, dir, remoteURL, srcRef); err != nil {
			return fmt.Errorf("fetch %s from %s skipped: %w", label, redactedURL, err)
		}
		printFetchStatus("Fetching checkpoints from " + remote.RedactURLOrPath(remoteURL))
	}

	_, fetchErr := remote.Fetch(fetchCtx, opts)
	// remote.Fetch already folds git's output into fetchErr, redacted and
	// capped; splicing it again printed the same text twice.
	var err error
	switch cause := context.Cause(fetchCtx); {
	case fetchErr == nil:
	case errors.Is(cause, errForegroundFetchBudget):
		err = fmt.Errorf("fetch %s from %s failed: %w: %w", label, redactedURL, cause, fetchErr)
	default:
		err = fmt.Errorf("fetch %s from %s failed: %w", label, redactedURL, fetchErr)
	}
	if mode == foregroundFetch {
		recordForegroundFetchOutcome(ctx, dir, remoteURL, srcRef, err)
	}
	return err
}

// slowFetchFailure is the record checkpointRemoteFetchFailureName holds.
type slowFetchFailure struct {
	Remote string    `json:"remote"` // redacted
	Ref    string    `json:"ref"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// recentSlowFetchFailure returns an errRecentSlowFetchFailure error when a
// foreground fetch of srcRef from remoteURL stalled or ran out of budget within
// recentSlowFetchFailureWindow. Commands waiting on the fetch lock check it once
// they hold the lock, so they fail together with the fetch they waited for.
func recentSlowFetchFailure(ctx context.Context, dir, remoteURL, srcRef string) error {
	f, ok := readSlowFetchFailure(ctx, dir, remoteURL, srcRef)
	if !ok {
		return nil
	}
	age := time.Since(f.At)
	if age < 0 || age >= recentSlowFetchFailureWindow {
		return nil
	}
	return fmt.Errorf("%w: a fetch %s ago failed (%s); retry after %s",
		errRecentSlowFetchFailure, age.Round(time.Second), f.Reason, (recentSlowFetchFailureWindow - age).Round(time.Second))
}

// recordForegroundFetchOutcome records a fetch that stalled or ran out of
// budget for recentSlowFetchFailure, and clears the record when a fetch of the
// same ref from the same remote succeeds. Other failures are not recorded: they
// end quickly, so a retry costs little, and refusing it would hide a fix such
// as new credentials. Best-effort: a failed write only logs.
func recordForegroundFetchOutcome(ctx context.Context, dir, remoteURL, srcRef string, fetchErr error) {
	var reason string
	switch {
	case fetchErr == nil:
	case errors.Is(fetchErr, remote.ErrFetchStalled):
		reason = remote.ErrFetchStalled.Error()
	case errors.Is(fetchErr, errForegroundFetchBudget):
		reason = errForegroundFetchBudget.Error()
	default:
		return
	}
	root, err := openFetchLockRoot(ctx, dir)
	if err == nil {
		if reason == "" {
			if _, ok := readSlowFetchFailure(ctx, dir, remoteURL, srcRef); ok {
				err = root.Remove(checkpointRemoteFetchFailureName)
			}
		} else {
			err = writeSlowFetchFailure(root, slowFetchFailure{
				Remote: remote.RedactURL(remoteURL),
				Ref:    srcRef,
				Reason: reason,
				At:     time.Now(),
			})
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		logging.Warn(ctx, "checkpoint-remote: could not record fetch outcome",
			slog.String("error", err.Error()),
		)
	}
}

// readSlowFetchFailure returns the recorded failure when it is for srcRef from
// remoteURL. An unreadable record reads as none.
func readSlowFetchFailure(ctx context.Context, dir, remoteURL, srcRef string) (slowFetchFailure, bool) {
	root, err := openFetchLockRoot(ctx, dir)
	if err != nil {
		return slowFetchFailure{}, false
	}
	var f slowFetchFailure
	data, err := osroot.ReadFileNoFollow(root, checkpointRemoteFetchFailureName)
	if err != nil || json.Unmarshal(data, &f) != nil {
		return slowFetchFailure{}, false
	}
	return f, f.Remote == remote.RedactURL(remoteURL) && f.Ref == srcRef
}

func writeSlowFetchFailure(root *os.Root, f slowFetchFailure) error {
	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("marshal fetch failure: %w", err)
	}
	return jsonutil.WriteFileAtomicIn(root, checkpointRemoteFetchFailureName, data, 0o600) //nolint:wrapcheck // already names the file
}

// printFetchStatus tells a terminal user why a foreground command is paused.
// These fetches run under spinners owned by the calling command, so on a
// terminal that renders ANSI the line is cleared first: the message takes the
// spinner's row and the spinner redraws below it.
func printFetchStatus(msg string) {
	switch {
	case interactive.ShouldStyle(os.Stderr):
		fmt.Fprintf(os.Stderr, "\r\033[K[entire] %s\n", msg)
	case interactive.IsTerminalWriter(os.Stderr):
		fmt.Fprintf(os.Stderr, "[entire] %s\n", msg)
	}
}

// boundFetch applies mode to a fetch, returning the context it runs under.
func boundFetch(ctx context.Context, mode checkpointFetchMode, opts *remote.FetchOptions) (context.Context, context.CancelFunc) {
	if mode == foregroundFetch {
		opts.AbortStalled = true
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, checkpointRemoteFetchTimeout)
}

// startForegroundFetch starts a foreground fetch's budget, then waits for any
// other foreground checkpoint-remote fetch in this repository to finish. The
// waiter's own fetch then negotiates against the objects the first one landed
// and transfers only what is new. Run the fetch under the returned context,
// which carries the budget, and call release when done with the fetched ref.
//
// Callers that fetch into the shared metadata tmp ref (FetchTmpRefPrefix plus
// the primary branch) hold the lock until that ref is promoted and removed:
// released after the fetch alone, a finishing caller's cleanup can delete the
// ref out from under the next caller's fetch. The push hot path does not take
// the lock and uses its own tmp ref instead.
//
// The wait says why the command is paused and stops when ctx is cancelled or
// the budget runs out, returning that cause; a waiter never falls back to its
// own transfer, which would move the archive once per waiter. Failing to take
// the lock at all logs and lets the caller fetch unserialized. dir is the
// worktree to resolve the git common dir from; empty means the current
// repository.
func startForegroundFetch(ctx context.Context, dir string) (context.Context, func(), error) {
	budget, parseErr := parseForegroundFetchBudget(os.Getenv(foregroundFetchBudgetEnvVar))
	if parseErr != nil {
		logging.Warn(ctx, "checkpoint-remote: ignoring invalid fetch budget",
			slog.String("env", foregroundFetchBudgetEnvVar),
			slog.String("error", parseErr.Error()),
		)
	}
	fetchCtx, cancel := context.WithTimeoutCause(ctx, budget,
		fmt.Errorf("%w after %s (set %s to allow longer)", errForegroundFetchBudget, budget, foregroundFetchBudgetEnvVar))
	unlock, err := acquireCheckpointRemoteFetchLock(fetchCtx, dir)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return fetchCtx, func() {
		unlock()
		cancel()
	}, nil
}

// parseForegroundFetchBudget returns the budget a foregroundFetchBudgetEnvVar
// value sets: the default when it is empty, and the default with an error when
// it is not a positive duration.
func parseForegroundFetchBudget(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultForegroundFetchBudget, nil
	}
	budget, err := time.ParseDuration(value)
	if err != nil {
		return defaultForegroundFetchBudget, fmt.Errorf("parse %s: %w", foregroundFetchBudgetEnvVar, err)
	}
	if budget <= 0 {
		return defaultForegroundFetchBudget, fmt.Errorf("%s must be positive, got %q", foregroundFetchBudgetEnvVar, value)
	}
	return budget, nil
}

// acquireCheckpointRemoteFetchLock takes the fetch lock for startForegroundFetch,
// whose budget gives ctx the deadline the wait needs. Only ctx's own cause is
// returned as an error.
func acquireCheckpointRemoteFetchLock(ctx context.Context, dir string) (release func(), err error) {
	root, err := openFetchLockRoot(ctx, dir)
	if err == nil {
		release, err = acquireFetchLockIn(ctx, root)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("wait for checkpoint fetch lock: %w", context.Cause(ctx))
		}
		logging.Warn(ctx, "checkpoint-remote: fetch lock unavailable; fetching unserialized",
			slog.String("error", err.Error()),
		)
		return func() {}, nil
	}
	return release, nil
}

// openFetchLockRoot opens the git common dir that holds the fetch lock. dir is
// the worktree to resolve it from; empty means the current repository.
func openFetchLockRoot(ctx context.Context, dir string) (*os.Root, error) {
	if dir == "" {
		return gitdir.Open(ctx) //nolint:wrapcheck // gitdir errors name the path
	}
	commonDir, err := gitdir.CommonDirForWorktree(ctx, dir)
	if err != nil {
		return nil, err //nolint:wrapcheck // gitdir errors name the path
	}
	return gitdir.OpenAt(commonDir) //nolint:wrapcheck // gitdir errors name the path
}

// acquireFetchLockIn takes the fetch lock in root, announcing the wait when
// another process holds it. flock only polls, and so only honors cancellation,
// under a deadline: ctx must carry one, and an already-expired one makes the
// first attempt a non-blocking probe.
func acquireFetchLockIn(ctx context.Context, root *os.Root) (func(), error) {
	probeCtx, cancelProbe := context.WithDeadline(ctx, time.Now())
	release, err := flock.AcquireContextIn(probeCtx, root, checkpointRemoteFetchLockName)
	cancelProbe()
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return release, err //nolint:wrapcheck // flock errors are already wrapped
	}

	printFetchStatus("Waiting for another entire command to finish fetching checkpoints...")
	logging.Info(ctx, "checkpoint-remote: waiting for another fetch to release the fetch lock")
	return flock.AcquireContextIn(ctx, root, checkpointRemoteFetchLockName) //nolint:wrapcheck // flock errors are already wrapped
}

// fetchMetadataBranchIfMissing fetches the primary metadata ref from a URL only if it doesn't exist locally.
// This avoids network calls on every push — once the branch exists locally, this is a no-op.
// It runs on the push hot path, so it uses the short fetch budget.
//
// A fetch failure is not fatal — the push will create the branch on the remote
// when it succeeds — but it is returned rather than swallowed so the caller can
// log it. Returning nil unconditionally meant a checkpoint remote that was
// unreachable, too slow, or refusing auth looked identical to one that was never
// contacted, and resolvePushSettings' warning could never fire.
//
// A remote that simply does not carry the branch yet (the normal state of a
// brand-new checkpoint repo) is not a failure and stays quiet. That case is
// established by probing with ls-remote first — positive evidence, matching
// remote.FetchCheckpointRef's absence-vs-failure contract — rather than by
// treating every fetch error as absence.
//
// The probe and the fetch share one deadline, so the budget the caller sees is
// the constant's value rather than twice it: per-call timeouts do not compose
// when applied at two nesting levels.
func fetchMetadataBranchIfMissing(ctx context.Context, remoteURL string) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}
	defer repo.Close()

	// Check if branch already exists locally - if so, nothing to do. Only a
	// genuinely-absent ref means "missing": any other read failure (corrupt or
	// unreadable ref storage) is surfaced rather than masked as absence, which
	// would send us to the network to fix a local problem.
	refs := checkpoint.ResolveRefs(ctx)
	switch _, err := repo.Reference(refs.Primary, true); {
	case err == nil:
		return nil // Branch exists locally, skip fetch
	case !errors.Is(err, plumbing.ErrReferenceNotFound):
		return fmt.Errorf("read local ref %s: %w", refs.Primary, err)
	}

	ctx, cancel := context.WithTimeout(ctx, checkpointRemoteFetchTimeout)
	defer cancel()

	out, probeErr := remote.LsRemoteInDir(ctx, "", remoteURL, refs.Primary.String())
	if probeErr != nil {
		return fmt.Errorf("probe %s on %s: %w", refs.Primary, remote.RedactURL(remoteURL), probeErr)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		// Remote reachable, branch not there yet. The first push creates it.
		return nil
	}

	if err := fetchMetadataBranchWithin(ctx, remoteURL, hotPathFetch); err != nil {
		return err
	}

	logging.Info(ctx, "checkpoint-remote: fetched metadata branch from URL")
	return nil
}

// resolveCheckpointFetchURL resolves the checkpoint_remote fetch URL for the repo
// rooted at worktreeRoot and verifies it actually targets the configured
// checkpoint repository. It returns ok=false (never an error) when no
// checkpoint_remote is configured or a dedicated checkpoint URL cannot be
// resolved, so callers fall back to keeping the local branch / creating an orphan.
//
// remote.FetchURL silently falls back to the origin remote URL in several paths
// (ENTIRE_CHECKPOINT_TOKEN short-circuit, unparseable origin, non-derivable origin
// protocol). Adopting from origin would contradict the rule that origin is never
// authoritative when a checkpoint_remote is configured (issue #1374), so a resolved
// URL that does not target the configured checkpoint repo is treated as unresolved.
func resolveCheckpointFetchURL(ctx context.Context, worktreeRoot string) (string, bool) {
	s, err := settings.Load(ctx)
	if err != nil {
		logging.Debug(ctx, "checkpoint-remote: could not load settings for metadata fetch URL",
			slog.String("error", err.Error()))
		return "", false
	}
	config := s.GetCheckpointRemote()
	if config == nil {
		return "", false
	}
	url, err := remote.FetchURL(ctx, remote.FetchURLOptions{
		WorktreeRoot: worktreeRoot,
		// The elected remote joins the ownership vote so the fork-shaped
		// topology that only the push destination exposes is refused here too.
		LeadReadRemote: LeadCheckpointReadRemote(ctx),
	})
	if err != nil || strings.TrimSpace(url) == "" {
		logging.Debug(ctx, "checkpoint-remote: could not resolve fetch URL for metadata bootstrap",
			slog.Any("error", err))
		return "", false
	}
	if !urlTargetsCheckpointRepo(url, config) {
		logging.Debug(ctx, "checkpoint-remote: fetch URL did not resolve to the configured checkpoint repo; not adopting from origin",
			slog.String("repo", config.Repo))
		return "", false
	}
	return url, true
}

// urlTargetsCheckpointRepo reports whether url points at the configured checkpoint
// repository (host-agnostic, case-insensitive owner/repo match). It distinguishes a
// derived checkpoint URL from remote.FetchURL's origin fallback, which targets the
// origin repository instead. A same-repo checkpoint_remote (origin == checkpoint
// repo) still matches, which is correct: adopting from that URL is adopting the
// checkpoint repo.
func urlTargetsCheckpointRepo(url string, config *settings.CheckpointRemoteConfig) bool {
	info, err := remote.ParseURL(url)
	if err != nil || info.Owner == "" || info.Repo == "" {
		return false
	}
	return strings.EqualFold(info.Owner+"/"+info.Repo, config.Repo)
}

// CheckpointPushTarget returns where PrePush sends checkpoint metadata for
// pushRemoteName — the checkpoint_remote URL when one is configured, otherwise
// the remote itself — and whether push_sessions turns pushing off. Callers use
// it to confirm delivery after a push, since PrePush is deliberately fail-soft.
func CheckpointPushTarget(ctx context.Context, pushRemoteName string) (target string, disabled bool) {
	ps := resolvePushSettings(ctx, pushRemoteName)
	if ps.checkpointURL != "" {
		return ps.checkpointURL, ps.pushDisabled
	}
	return ps.remote, ps.pushDisabled
}

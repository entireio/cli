package strategy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// pushStderr is the destination for output emitted by doPushBranch itself
// (the dot lines, "Warning:" lines, hints). The standalone hint helpers
// (printCheckpointRemoteHint, printSettingsCommitHint, …) deliberately keep
// writing to os.Stderr because they pre-date this var and have their own
// inline-redirect tests. captureStderr patches both so end-to-end tests of
// doPushBranch still see hint output.
var pushStderr io.Writer = os.Stderr

// pushAttemptTimeout caps a single tryPushSessionsCommon invocation. Two
// minutes is generous for a healthy network and short enough that a hung
// helper does not hold the hook open indefinitely.
var pushAttemptTimeout = 2 * time.Minute

var fetchAttemptTimeout = 2 * time.Minute

// errPushTimedOut signals that tryPushSessionsCommon's inner deadline fired
// before git push completed. doPushBranch uses it to suppress the
// sync-and-retry cascade — fetch would just time out on the same condition.
var errPushTimedOut = errors.New("push timed out")

var errFetchTimedOut = errors.New("fetch timed out")

// Log-field values for the "class" attribute on push-related INFO logs.
// Kept as constants so log consumers and tests share one source of truth and
// the function stays free of bare string literals.
const (
	pushClassTimedOut       = "timed_out"
	pushClassProtectedRef   = "protected_ref"
	pushClassNonFastForward = "non_fast_forward"
	pushClassOther          = "other"
)

// pushBranchIfNeeded pushes a branch to the given target if it has unpushed changes.
// The target can be a remote name (e.g., "origin") or a URL for direct push.
// When pushing to a URL, the "has unpushed" optimization is skipped since there are
// no remote tracking refs — git itself handles the no-op case.
// Does not check any settings — callers are responsible for gating.
func pushBranchIfNeeded(ctx context.Context, target, branchName string) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	// Check if branch exists locally
	branchRef := plumbing.NewBranchReferenceName(branchName)
	localRef, err := repo.Reference(branchRef, true)
	if err != nil {
		// No branch, nothing to push
		return nil //nolint:nilerr // Expected when no sessions exist yet
	}

	// Only check remote tracking refs when target is a remote name (not a URL).
	// URLs don't have tracking refs, so we always attempt the push and let git handle it.
	if !remote.IsURL(target) && !hasUnpushedSessionsCommon(repo, target, localRef.Hash(), branchName) {
		return nil
	}

	return doPushBranch(ctx, target, branchName)
}

// hasUnpushedSessionsCommon checks if the local branch differs from the remote.
// Returns true if there's any difference that needs syncing (local ahead, remote ahead, or diverged).
func hasUnpushedSessionsCommon(repo *git.Repository, remoteName string, localHash plumbing.Hash, branchName string) bool {
	// Check for remote tracking ref: refs/remotes/<remoteName>/<branch>
	remoteRefName := plumbing.NewRemoteReferenceName(remoteName, branchName)
	remoteRef, err := repo.Reference(remoteRefName, true)
	if err != nil {
		// Remote branch doesn't exist yet - we have content to push
		return true
	}

	// If local and remote point to same commit, nothing to sync
	// This is the only case where we skip - any difference needs handling
	return localHash != remoteRef.Hash()
}

func displayPushTarget(target string) string {
	if remote.IsURL(target) {
		return "checkpoint remote"
	}
	return target
}

// doPushBranch pushes the given branch to the target with fetch+merge recovery.
// The target can be a remote name or a URL.
func doPushBranch(ctx context.Context, target, branchName string) error {
	displayTarget := displayPushTarget(target)

	fmt.Fprintf(pushStderr, "[entire] Pushing %s to %s...", branchName, displayTarget)
	stop := startProgressDots(pushStderr)

	pushStart := time.Now()
	logging.Info(ctx, "push attempt start",
		slog.String("branch", branchName),
		slog.String("target", displayTarget),
	)

	result, err := tryPushSessionsCommon(ctx, target, branchName)
	if err == nil {
		logging.Info(ctx, "push attempt completed",
			slog.String("branch", branchName),
			slog.String("target", displayTarget),
			slog.Duration("elapsed", time.Since(pushStart)),
			slog.Bool("up_to_date", result.upToDate),
		)
		finishPush(ctx, stop, result, target)
		return nil
	}

	logging.Info(ctx, "push attempt failed",
		slog.String("branch", branchName),
		slog.String("target", displayTarget),
		slog.Duration("elapsed", time.Since(pushStart)),
		slog.String("class", classifyForLog(err)),
	)

	// Outer context cancelled (user Ctrl-C, hook timeout) — bail quietly
	// rather than cascading into a sync that will also fail. doPushBranch
	// is best-effort: never fail the user's main push because of checkpoint
	// sync, mirroring the existing graceful-degradation pattern below.
	if ctx.Err() != nil {
		stop("")
		return nil //nolint:nilerr // intentional graceful degradation
	}

	// Inner attempt timed out. Sync will also time out, so skip the cascade
	// and give the user a clear next step instead.
	if errors.Is(err, errPushTimedOut) {
		stop(" timed out")
		fmt.Fprintf(pushStderr, "[entire] Warning: push of %s to %s timed out after %s. Network or proxy issue?\n",
			branchName, displayTarget, pushAttemptTimeout)
		fmt.Fprintln(pushStderr, `[entire] Hint: set "log_level": "DEBUG" in .entire/settings.local.json to capture git's underlying output next time.`)
		printCheckpointRemoteHint(target)
		return nil
	}

	stop("")

	// Protected refs cannot be fixed by syncing and retrying.
	var protectedErr *protectedRefError
	if errors.As(err, &protectedErr) {
		printProtectedRefBlock(pushStderr, branchName, target)
		return nil
	}

	fmt.Fprintf(pushStderr, "[entire] Syncing %s with remote...", branchName)
	stop = startProgressDots(pushStderr)
	syncStart := time.Now()

	if syncErr := fetchAndRebaseSessionsCommon(ctx, target, branchName); syncErr != nil {
		reportPushFailure(ctx, reportPushFailureArgs{
			out:     pushStderr,
			stop:    stop,
			err:     syncErr,
			logMsg:  "push sync failed",
			warnMsg: fmt.Sprintf("[entire] Warning: couldn't sync %s: %v\n", branchName, syncErr),
			branch:  branchName,
			target:  target,
			start:   syncStart,
		})
		return nil
	}
	stop(" done")

	fmt.Fprintf(pushStderr, "[entire] Pushing %s to %s...", branchName, displayTarget)
	stop = startProgressDots(pushStderr)
	retryStart := time.Now()

	retryResult, retryErr := tryPushSessionsCommon(ctx, target, branchName)
	if retryErr != nil {
		reportPushFailure(ctx, reportPushFailureArgs{
			out:     pushStderr,
			stop:    stop,
			err:     retryErr,
			logMsg:  "push retry failed",
			warnMsg: fmt.Sprintf("[entire] Warning: failed to push %s after sync: %v\n", branchName, retryErr),
			branch:  branchName,
			target:  target,
			start:   retryStart,
		})
		return nil
	}
	logging.Info(ctx, "push retry completed",
		slog.String("branch", branchName),
		slog.String("target", displayTarget),
		slog.Duration("elapsed", time.Since(retryStart)),
		slog.Bool("up_to_date", retryResult.upToDate),
	)
	finishPush(ctx, stop, retryResult, target)
	return nil
}

// reportPushFailureArgs groups the inputs to reportPushFailure to keep the
// recoverable-failure paths in doPushBranch readable. out is taken as a
// parameter (rather than reaching into the package-global pushStderr) so
// tests can capture without racing on shared state.
type reportPushFailureArgs struct {
	out     io.Writer
	stop    func(string)
	err     error
	logMsg  string
	warnMsg string
	branch  string
	target  string
	start   time.Time
}

// reportPushFailure handles the common tail of a failed sync or retry-push:
// pick the dot-line suffix (`" timed out"` vs blank), log at INFO with a
// classification label, print the warning, and surface the
// checkpoint-remote hint when the target is a URL.
//
// If the outer context is done (user Ctrl-C or hook deadline arrived after
// the first push attempt), this collapses to a silent bail: empty dot suffix,
// no log, no warning. Matches the early bail-out in doPushBranch's
// first-attempt path so that every cancellation point behaves the same.
func reportPushFailure(ctx context.Context, a reportPushFailureArgs) {
	if ctx.Err() != nil {
		a.stop("")
		return
	}
	suffix := ""
	if errors.Is(a.err, errPushTimedOut) || errors.Is(a.err, errFetchTimedOut) {
		suffix = " timed out"
	}
	a.stop(suffix)
	logging.Info(ctx, a.logMsg,
		slog.String("branch", a.branch),
		slog.String("target", displayPushTarget(a.target)),
		slog.Duration("elapsed", time.Since(a.start)),
		slog.String("class", classifyForLog(a.err)),
	)
	fmt.Fprint(a.out, a.warnMsg)
	printCheckpointRemoteHint(a.target)
}

// classifyForLog maps a push/fetch error to a short string suitable for log
// fields. Avoids leaking the raw error (which may contain URLs or tokens
// embedded in git's output) — only the category is logged.
func classifyForLog(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errPushTimedOut) || errors.Is(err, errFetchTimedOut) {
		return pushClassTimedOut
	}
	var protectedErr *protectedRefError
	if errors.As(err, &protectedErr) {
		return pushClassProtectedRef
	}
	if errors.Is(err, errNonFastForward) {
		return pushClassNonFastForward
	}
	return pushClassOther
}

// printCheckpointRemoteHint prints a hint when a push to a checkpoint URL fails.
// Only prints when the target is a URL (not the user's default remote).
func printCheckpointRemoteHint(target string) {
	if !remote.IsURL(target) {
		return
	}
	fmt.Fprintln(os.Stderr, "[entire] A checkpoint remote is configured in Entire settings (.entire/settings.json or .entire/settings.local.json) but could not be reached.")
	fmt.Fprintln(os.Stderr, "[entire] Checkpoints are saved locally but not synced. Ensure you have access to the checkpoint remote.")
}

// settingsHintOnce ensures the settings commit hint prints at most once per process.
var settingsHintOnce sync.Once

// printSettingsCommitHint prints a hint after a successful checkpoint remote push
// when the committed .entire/settings.json does not contain a checkpoint_remote config.
// entire.io discovers the external checkpoint repo by reading the committed project
// settings, so the checkpoint_remote must be present in HEAD:.entire/settings.json
// (not just in settings.local.json or uncommitted local changes).
// Uses sync.Once to avoid duplicates when multiple branches/refs are pushed in a
// single pre-push invocation.
func printSettingsCommitHint(ctx context.Context, target string) {
	if !remote.IsURL(target) {
		return
	}
	settingsHintOnce.Do(func() {
		if isCheckpointRemoteCommitted(ctx) {
			return
		}
		fmt.Fprintln(os.Stderr, "[entire] Note: Checkpoints were pushed to a separate checkpoint remote, but .entire/settings.json does not contain checkpoint_remote in the latest commit. entire.io will not be able to discover these checkpoints until checkpoint_remote is committed and pushed in .entire/settings.json.")
	})
}

// isCheckpointRemoteCommitted returns true if the committed .entire/settings.json
// at HEAD contains a valid checkpoint_remote configuration. This is the true
// discoverability check: entire.io reads from committed project settings, not from
// local overrides or uncommitted changes.
func isCheckpointRemoteCommitted(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "git", "show", "HEAD:.entire/settings.json")
	output, err := cmd.Output()
	if err != nil {
		return false // file doesn't exist at HEAD
	}
	// Parse the committed content and check for checkpoint_remote
	committed, err := settings.LoadFromBytes(output)
	if err != nil {
		return false
	}
	return committed.GetCheckpointRemote() != nil
}

// pushResult describes what happened during a push attempt.
type pushResult struct {
	// upToDate is true when the remote already had all commits (nothing transferred).
	upToDate bool
}

// parsePushResult checks git push --porcelain output for ref status flags.
// In porcelain mode, each ref gets a tab-delimited status line:
//
//	<flag>\t<from>:<to>\t<summary>
//
// where flag '=' means the ref was already up-to-date. This is locale-independent,
// unlike the human-readable "Everything up-to-date" message.
func parsePushResult(output string) pushResult {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "=\t") {
			return pushResult{upToDate: true}
		}
	}
	return pushResult{upToDate: false}
}

// finishPush stops the progress dots and prints "already up-to-date" or "done"
// depending on the push result. Only prints the settings commit hint when new
// content was actually pushed.
func finishPush(ctx context.Context, stop func(string), result pushResult, target string) {
	if result.upToDate {
		stop(" already up-to-date")
	} else {
		stop(" done")
		printSettingsCommitHint(ctx, target)
	}
}

// tryPushSessionsCommon attempts to push the sessions branch.
func tryPushSessionsCommon(ctx context.Context, remoteName, branchName string) (pushResult, error) {
	localCtx, cancel := context.WithTimeout(ctx, pushAttemptTimeout)
	defer cancel()

	result, err := remote.Push(localCtx, remoteName, branchName)
	outputStr := result.Output
	if err != nil {
		// Inner deadline fired: distinct sentinel so doPushBranch can pick
		// the right messaging and skip the sync-retry cascade.
		if errors.Is(localCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			logging.Debug(ctx, "git push timed out",
				slog.String("error", err.Error()),
				slog.Duration("after", pushAttemptTimeout),
			)
			return pushResult{}, errPushTimedOut
		}
		return pushResult{}, classifyPushFailure(localCtx, outputStr, err)
	}

	return parsePushResult(outputStr), nil
}

// protectedRefError means the remote is blocking writes to the ref itself.
type protectedRefError struct {
	output string
}

func (e *protectedRefError) Error() string {
	return "remote rejected push to protected ref"
}

// isProtectedRefRejection detects GitHub ruleset and branch-protection failures.
func isProtectedRefRejection(output string) bool {
	return strings.Contains(output, "GH013") ||
		strings.Contains(output, "Cannot update this protected ref") ||
		strings.Contains(output, "protected branch hook declined")
}

var errNonFastForward = errors.New("non-fast-forward")

func isNonFastForwardRejection(output string) bool {
	if strings.Contains(output, "non-fast-forward") {
		return true
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "[rejected]") && strings.Contains(line, "(fetch first)") {
			return true
		}
	}
	return strings.Contains(output, "Updates were rejected because the tip of your current branch is behind") ||
		strings.Contains(output, "Updates were rejected because the remote contains work that you do not have locally")
}

// classifyPushOutput maps failing push stderr to a typed error.
func classifyPushOutput(output string) error {
	if isProtectedRefRejection(output) {
		return &protectedRefError{output: output}
	}
	if isNonFastForwardRejection(output) {
		return errNonFastForward
	}
	if strings.TrimSpace(output) == "" {
		return errors.New("push failed")
	}
	return fmt.Errorf("push failed: %s", output)
}

func classifyPushFailure(ctx context.Context, output string, pushErr error) error {
	if strings.TrimSpace(output) != "" {
		if pushErr != nil {
			logging.Debug(ctx, "git push failed",
				slog.String("error", pushErr.Error()),
				slog.String("output", output),
			)
		}
		return classifyPushOutput(output)
	}
	if pushErr != nil {
		logging.Debug(ctx, "git push failed without output",
			slog.String("error", pushErr.Error()),
		)
		return fmt.Errorf("push failed: %w", pushErr)
	}
	return errors.New("push failed")
}

// printProtectedRefBlock explains that checkpoint syncing was blocked remotely.
func printProtectedRefBlock(w io.Writer, ref, target string) {
	const banner = "[entire] ============================================================"
	displayTarget := displayPushTarget(target)
	fmt.Fprintln(w, banner)
	fmt.Fprintf(w, "[entire] BLOCKED: remote rejected push to %s\n", ref)
	fmt.Fprintln(w, "[entire] Reason:  GitHub branch protection or repository ruleset (e.g. GH013)")
	fmt.Fprintf(w, "[entire] Target:  %s\n", displayTarget)
	fmt.Fprintln(w, "[entire] Impact:  checkpoints are saved locally but NOT synced to this remote.")
	fmt.Fprintln(w, "[entire] Action:  allow pushes to `entire/*` in your ruleset, or set")
	fmt.Fprintln(w, "[entire]          `checkpoint_remote` in .entire/settings.json to a separate repo.")
	fmt.Fprintln(w, banner)
}

// fetchAndRebaseSessionsCommon fetches remote sessions and rebases local commits
// on top of the remote tip. Since checkpoint shards use unique paths, rebases
// always apply cleanly.
// The target can be a remote name or a URL.
func fetchAndRebaseSessionsCommon(ctx context.Context, target, branchName string) error {
	localCtx, cancel := context.WithTimeout(ctx, fetchAttemptTimeout)
	defer cancel()

	fetchTarget, err := remote.ResolveFetchTarget(localCtx, target)
	if err != nil {
		return fmt.Errorf("resolve fetch target: %w", err)
	}

	// Determine fetch refspec. When the resolved fetch target is a URL, use a
	// temp ref; when it's still a remote name, use the standard remote-tracking
	// ref.
	var fetchedRefName plumbing.ReferenceName
	var refSpec string
	usedTempRef := remote.IsURL(fetchTarget)
	if usedTempRef {
		tmpRef := "refs/entire-fetch-tmp/" + branchName
		refSpec = fmt.Sprintf("+refs/heads/%s:%s", branchName, tmpRef)
		fetchedRefName = plumbing.ReferenceName(tmpRef)
	} else {
		refSpec = fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branchName, target, branchName)
		fetchedRefName = plumbing.NewRemoteReferenceName(target, branchName)
	}

	// Use git CLI for fetch (go-git's fetch can be tricky with auth).
	// Use --filter=blob:none for a partial fetch that downloads only commits
	// and trees, skipping blobs. The merge only needs the tree structure to
	// combine entries; blobs are already local or fetched on demand.
	if output, fetchErr := remote.Fetch(localCtx, remote.FetchOptions{
		Remote:   fetchTarget,
		RefSpecs: []string{refSpec},
		NoTags:   true,
	}); fetchErr != nil {
		// Inner deadline fired (but outer is still alive): distinct sentinel
		// so doPushBranch can render " timed out" instead of an empty trailing
		// colon. When the outer context fired, let the wrapped error propagate
		// so the caller sees the real cancellation cause instead of an inner-
		// timeout mislabel.
		if errors.Is(localCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			logging.Debug(ctx, "git fetch timed out",
				slog.String("error", fetchErr.Error()),
				slog.Duration("after", fetchAttemptTimeout),
			)
			return errFetchTimedOut
		}
		msg := strings.TrimSpace(string(output))
		if msg == "" {
			// When git is SIGKILLed (kill-on-cancel from execx.KillOnCancel) the
			// child writes nothing to stderr; fall back to the wrapper's error
			// text so users don't see a bare "fetch failed:" with no detail.
			msg = fetchErr.Error()
		}
		return fmt.Errorf("fetch failed: %s", msg)
	}

	repo, err := OpenRepository(localCtx)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	// Reconcile disconnected metadata branches before rebasing.
	// The fetch above updated the remote-tracking ref, so reconciliation
	// can compare fresh local vs remote. If disconnected (empty-orphan bug),
	// this cherry-picks local commits onto remote tip, updating the local ref.
	// If reconciliation fails, abort — proceeding to rebase on disconnected
	// branches would silently combine unrelated histories.
	if reconcileErr := ReconcileDisconnectedMetadataBranch(localCtx, repo, fetchedRefName, pushStderr); reconcileErr != nil {
		return fmt.Errorf("metadata reconciliation failed: %w", reconcileErr)
	}

	// Get local branch (re-read after potential reconciliation update)
	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		return fmt.Errorf("failed to get local ref: %w", err)
	}

	// Get fetched ref (remote-tracking or temp ref, updated by the fetch above)
	remoteRef, err := repo.Reference(fetchedRefName, true)
	if err != nil {
		return fmt.Errorf("failed to get remote ref: %w", err)
	}

	// If local is already at or behind remote, fast-forward
	if localRef.Hash() == remoteRef.Hash() {
		return nil
	}

	// Find merge base
	repoPath, err := getRepoPath(repo)
	if err != nil {
		return fmt.Errorf("failed to get repo path: %w", err)
	}
	mergeBase, err := getMergeBase(localCtx, repoPath, localRef.Hash().String(), remoteRef.Hash().String())
	if err != nil {
		return fmt.Errorf("failed to find merge base: %w", err)
	}

	// If local is ancestor of remote (merge base == local), fast-forward to remote
	if mergeBase == localRef.Hash() {
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchName), remoteRef.Hash())
		if err := repo.Storer.SetReference(ref); err != nil {
			return fmt.Errorf("failed to fast-forward branch ref: %w", err)
		}
		if usedTempRef {
			_ = repo.Storer.RemoveReference(fetchedRefName) //nolint:errcheck // cleanup is best-effort
		}
		return nil
	}

	// Collect commits reachable from local but not from remote and cherry-pick
	// them onto the remote tip. This preserves local-only commits even when the
	// local metadata branch already contains old merge commits, while avoiding
	// replaying shared ancestors older than the true merge-base.
	localCommits, err := collectCommitsSince(localCtx, repo, repoPath, localRef.Hash(), remoteRef.Hash())
	if err != nil {
		return fmt.Errorf("failed to collect local commits: %w", err)
	}

	if len(localCommits) == 0 {
		// No local-only commits — just point to remote
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchName), remoteRef.Hash())
		if err := repo.Storer.SetReference(ref); err != nil {
			return fmt.Errorf("failed to update branch ref: %w", err)
		}
		if usedTempRef {
			_ = repo.Storer.RemoveReference(fetchedRefName) //nolint:errcheck // cleanup is best-effort
		}
		return nil
	}

	newTip, err := cherryPickOnto(localCtx, repo, remoteRef.Hash(), localCommits)
	if err != nil {
		return fmt.Errorf("failed to rebase local commits onto remote: %w", err)
	}

	// Update branch ref
	newRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchName), newTip)
	if err := repo.Storer.SetReference(newRef); err != nil {
		return fmt.Errorf("failed to update branch ref: %w", err)
	}

	// Clean up temp ref if we used one (best-effort, not critical if it fails)
	if usedTempRef {
		_ = repo.Storer.RemoveReference(fetchedRefName) //nolint:errcheck // cleanup is best-effort
	}

	return nil
}

// getMergeBase returns the merge base hash of two commits, or an error if they
// have no common ancestor.
func getMergeBase(ctx context.Context, repoPath, hashA, hashB string) (plumbing.Hash, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "merge-base", hashA, hashB)
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("git merge-base failed: %w", err)
	}

	return plumbing.NewHash(strings.TrimSpace(string(output))), nil
}

// collectCommitsSince returns non-merge commits reachable from tip but not from
// exclude, ordered oldest-first in topological order.
func collectCommitsSince(ctx context.Context, repo *git.Repository, repoPath string, tip, exclude plumbing.Hash) ([]*object.Commit, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// cherryPickOnto computes each commit's delta against its first parent, so
	// replaying merge commits would incorrectly re-apply changes that arrived via
	// non-first-parent history. Limit the replay set to non-merge commits.
	cmd := exec.CommandContext(ctx, "git", "rev-list", "--reverse", "--topo-order", "--no-merges", exclude.String()+".."+tip.String())
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-list failed: %w", err)
	}

	lines := strings.Fields(string(output))
	if len(lines) > MaxCommitTraversalDepth {
		return nil, fmt.Errorf("commit chain exceeded %d commits; aborting rebase", MaxCommitTraversalDepth)
	}

	commits := make([]*object.Commit, 0, len(lines))
	for _, line := range lines {
		hash := plumbing.NewHash(line)
		commit, commitErr := repo.CommitObject(hash)
		if commitErr != nil {
			return nil, fmt.Errorf("failed to get commit %s: %w", hash, commitErr)
		}
		if len(commit.ParentHashes) > 1 {
			continue
		}
		commits = append(commits, commit)
	}

	return commits, nil
}

// startProgressDots prints dots to w every second until the returned stop function
// is called. The stop function prints the given suffix and a newline.
func startProgressDots(w io.Writer) func(suffix string) {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Fprint(w, ".")
			}
		}
	}()
	return func(suffix string) {
		close(done)
		<-stopped // Wait for goroutine to finish before writing suffix
		fmt.Fprintln(w, suffix)
	}
}

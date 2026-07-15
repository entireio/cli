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
	"github.com/entireio/cli/perf"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// partitionLocalRefs splits refs into those that exist locally (pushable) and
// those that don't (stale queue entries — e.g. a checkpoint ref deleted by
// cleanup). Stale refs can never push, so callers drop them from the queue
// rather than retrying them forever.
func partitionLocalRefs(repo *git.Repository, refs []plumbing.ReferenceName) (existing, stale []plumbing.ReferenceName) {
	for _, ref := range refs {
		_, err := repo.Reference(ref, false)
		switch {
		case err == nil:
			existing = append(existing, ref)
		case errors.Is(err, plumbing.ErrReferenceNotFound):
			// Genuinely gone (e.g. deleted by cleanup) — never pushable, drop it.
			stale = append(stale, ref)
		default:
			// A transient/IO lookup error: keep the ref as pushable so a real
			// entry isn't dropped from the queue forever over a flaky read.
			existing = append(existing, ref)
		}
	}
	return existing, stale
}

// batchPushRefs pushes all of refs to target in a single git push,
// fast-forward-only (NOT a force push). Batching keeps a backfill of many refs to
// one network round-trip. Per-checkpoint refs normally advance by fast-forward
// (each write parents on the prior tip), so the common case succeeds; a
// non-fast-forward update — genuine divergence, e.g. the same checkpoint written
// differently on another machine — is REJECTED rather than silently overwriting
// the remote. We deliberately do not force: there is no server-side ref
// protection, so a force push would make a buggy or racing client clobber good
// remote history with no signal. On rejection the whole push errors; the caller
// retries the rejected refs individually with fetch+replay recovery
// (pushCheckpointRefWithRecovery).
func batchPushRefs(ctx context.Context, target string, refs []plumbing.ReferenceName) error {
	if len(refs) == 0 {
		return nil
	}
	refSpecs := make([]string, 0, len(refs))
	for _, ref := range refs {
		refSpecs = append(refSpecs, ref.String()+":"+ref.String())
	}
	if _, err := remote.PushWithOptions(ctx, remote.PushOptions{Remote: target, RefSpecs: refSpecs}); err != nil {
		return fmt.Errorf("push %d checkpoint refs: %w", len(refs), err)
	}
	return nil
}

// pushCheckpointRefWithRecovery pushes a single checkpoint ref fast-forward-only;
// on rejection — typically the ref diverged on the remote (the same checkpoint
// re-written elsewhere) — it fetches the remote ref and replays the local-only
// commits on top via fetchAndRebaseRefCommon, then retries. The retry is still
// non-force: after the replay the local ref is a fast-forward over the remote, so
// the remote commit is preserved as an ancestor rather than overwritten. The
// cherry-pick is delta-based, so non-overlapping changes merge; a genuine overlap
// (e.g. both sides rewrote the root metadata.json) surfaces as a rebase error and
// the ref is left for a later pre-push. Returns nil only if the ref reached the
// remote.
func pushCheckpointRefWithRecovery(ctx context.Context, target string, ref plumbing.ReferenceName) error {
	// One shared budget across the initial push, fetch+replay, and retry, matching
	// doPushRef (fetchAndRebaseRefCommon relies on the caller's deadline).
	ctx, cancel := context.WithTimeout(ctx, checkpointPushBudget)
	defer cancel()

	if err := batchPushRefs(ctx, target, []plumbing.ReferenceName{ref}); err == nil {
		return nil
	}
	if err := fetchAndRebaseRefCommon(ctx, target, ref); err != nil {
		return fmt.Errorf("sync diverged checkpoint ref %s: %w", ref, err)
	}
	return batchPushRefs(ctx, target, []plumbing.ReferenceName{ref})
}

// pushRefIfNeeded pushes a ref to the given target if it has unpushed changes.
// The target can be a remote name (e.g., "origin") or a URL for direct push.
// For branch refs, the "has unpushed" optimization consults the remote-tracking
// ref. Non-branch refs and URL targets skip the optimization and let git
// handle the no-op case.
// Does not check any settings — callers are responsible for gating.
func pushRefIfNeeded(ctx context.Context, target string, ref plumbing.ReferenceName) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Debug(ctx, "push skipped: open repository failed",
			slog.String("ref", ref.String()),
			slog.String("error", err.Error()))
		return nil
	}
	defer repo.Close()

	localRef, err := repo.Reference(ref, true)
	if err != nil {
		// Ref doesn't exist locally — nothing to push.
		return nil
	}

	if ref.IsBranch() && !remote.IsURL(target) && !hasUnpushedBranchRef(repo, target, localRef.Hash(), ref.Short()) {
		return nil
	}

	return doPushRef(ctx, target, ref)
}

// hasUnpushedBranchRef checks if the local branch differs from the remote.
// Returns true if there's any difference that needs syncing (local ahead, remote ahead, or diverged).
func hasUnpushedBranchRef(repo *git.Repository, remoteName string, localHash plumbing.Hash, branchName string) bool {
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

// checkpointPushBudget is one shared deadline across the initial push,
// fetch+rebase, and retry — per-attempt timeouts can stack to ~3x. var so tests
// can shrink it.
var checkpointPushBudget = 2 * time.Minute

// doPushRef pushes the given ref to the target with fetch+rebase recovery.
// The target can be a remote name or a URL.
func doPushRef(ctx context.Context, target string, ref plumbing.ReferenceName) error {
	ctx, cancel := context.WithTimeout(ctx, checkpointPushBudget)
	defer cancel()

	displayTarget := displayPushTarget(target)
	refLabel := refDisplayName(ref)

	fmt.Fprintf(os.Stderr, "[entire] Pushing %s to %s...", refLabel, displayTarget)
	stop := startProgressDots(os.Stderr)

	// Try pushing first
	result, err := tryPushRefCommon(ctx, target, ref)
	if err == nil {
		finishPush(ctx, stop, result, target)
		return nil
	}
	stop("")

	// Protected refs cannot be fixed by syncing and retrying.
	var protectedErr *protectedRefError
	if errors.As(err, &protectedErr) {
		printProtectedRefBlock(os.Stderr, refLabel, target)
		return nil
	}

	// Push failed - likely non-fast-forward. Try to fetch and rebase.
	// Spanned (with the network fetch as a child) so the trace distinguishes
	// "the raw push is slow" from "we keep hitting contention and re-syncing".
	fmt.Fprintf(os.Stderr, "[entire] Syncing %s with remote...", refLabel)
	stop = startProgressDots(os.Stderr)

	frCtx, fetchRebaseSpan := perf.Start(ctx, "fetch_and_rebase")
	syncErr := fetchAndRebaseRefCommon(frCtx, target, ref)
	fetchRebaseSpan.RecordError(syncErr)
	fetchRebaseSpan.End()
	if syncErr != nil {
		stop("")
		fmt.Fprintf(os.Stderr, "[entire] Warning: couldn't sync %s: %v\n", refLabel, syncErr)
		printCheckpointRemoteHint(target)
		return nil // Don't fail the main push
	}
	stop(" done")

	// Try pushing again after rebase
	fmt.Fprintf(os.Stderr, "[entire] Pushing %s to %s...", refLabel, displayTarget)
	stop = startProgressDots(os.Stderr)

	if result, err := tryPushRefCommon(ctx, target, ref); err != nil {
		stop("")
		fmt.Fprintf(os.Stderr, "[entire] Warning: failed to push %s after sync: %v\n", refLabel, err)
		printCheckpointRemoteHint(target)
	} else {
		finishPush(ctx, stop, result, target)
	}

	return nil
}

// refDisplayName returns a user-readable name for ref. Branch refs use the
// short name (e.g. "entire/checkpoints/v1"); other refs use the full name.
func refDisplayName(ref plumbing.ReferenceName) string {
	if ref.IsBranch() {
		return ref.Short()
	}
	return ref.String()
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

// tryPushRefCommon attempts to push a ref. No timeout of its own —
// runs under doPushRef's shared budget. Branch refs use a bare branch-name
// refSpec so existing remote-tracking works; non-branch refs use an explicit
// "refs/...:refs/..." refSpec with no tracking shadow. Neither forces: a
// non-fast-forward is rejected so doPushRef's fetch+rebase recovery runs (and a
// genuinely diverged ref is never silently overwritten — there is no
// server-side ref protection). This keeps one consistent non-force policy for
// every checkpoint ref, branch or per-checkpoint.
func tryPushRefCommon(ctx context.Context, remoteName string, ref plumbing.ReferenceName) (pushResult, error) {
	refSpec := ref.Short()
	if !ref.IsBranch() {
		refSpec = ref.String() + ":" + ref.String()
	}

	// Span the actual `git push` subprocess: on a slow remote (e.g. a custom
	// git transport) this is typically where pre-push time is spent. Called once
	// per push attempt, so a retry after fetch+rebase shows up as a second
	// git_push step (git_push~1) in the trace. A rejected first push records an
	// error flag, which signals the recovery path was taken.
	_, pushSpan := perf.Start(ctx, "git_push")
	result, err := remote.Push(ctx, remoteName, refSpec)
	pushSpan.RecordError(err)
	pushSpan.End()

	outputStr := result.Output
	if err != nil {
		return pushResult{}, classifyPushFailure(ctx, outputStr, err)
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

// fetchAndRebaseRefCommon fetches a remote ref and rebases local commits on top
// of the remote tip. Since checkpoint shards use unique paths, rebases always
// apply cleanly.
// The target can be a remote name or a URL.
func fetchAndRebaseRefCommon(ctx context.Context, target string, ref plumbing.ReferenceName) error {
	// No timeout: runs under doPushRef's shared budget.
	fetchTarget, err := remote.ResolveFetchTarget(ctx, target)
	if err != nil {
		return fmt.Errorf("resolve fetch target: %w", err)
	}

	// Determine fetch refspec. When the resolved fetch target is a URL or the
	// ref isn't a branch, fetch into a temp ref (no remote-tracking shadow);
	// otherwise use the standard refs/remotes/<remote>/<branch> destination.
	var fetchedRefName plumbing.ReferenceName
	var refSpec string
	usedTempRef := remote.IsURL(fetchTarget) || !ref.IsBranch()
	if usedTempRef {
		tmpRef := "refs/entire-fetch-tmp/" + strings.TrimPrefix(ref.String(), "refs/")
		refSpec = fmt.Sprintf("+%s:%s", ref.String(), tmpRef)
		fetchedRefName = plumbing.ReferenceName(tmpRef)
	} else {
		refSpec = fmt.Sprintf("+%s:refs/remotes/%s/%s", ref.String(), target, ref.Short())
		fetchedRefName = plumbing.NewRemoteReferenceName(target, ref.Short())
	}

	// Use git CLI for fetch (go-git's fetch can be tricky with auth).
	// Do NOT --unshallow here: on a shallow repo with deep history (e.g. a
	// shared monorepo), --unshallow downloads the whole repository because
	// git treats shallow as a global property of the clone, not per-ref.
	// The downstream reconcile/rebase paths walk only commits visible past
	// .git/shallow (collectCommitChain / collectCommitsSince), so the
	// missing pre-shallow history isn't needed to produce a correct rebase.
	// Span the fetch separately so a slow sync can be attributed to the network
	// fetch versus the local reconcile/rebase that follows it.
	_, fetchSpan := perf.Start(ctx, "git_fetch")
	fetchOutput, fetchErr := remote.Fetch(ctx, remote.FetchOptions{
		Remote:   fetchTarget,
		RefSpecs: []string{refSpec},
		NoTags:   true,
	})
	fetchSpan.RecordError(fetchErr)
	fetchSpan.End()
	if fetchErr != nil {
		return fmt.Errorf("fetch failed: %s", fetchOutput)
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	// Reconcile disconnected metadata branches before rebasing.
	// The fetch above updated the remote-tracking ref, so reconciliation
	// can compare fresh local vs remote. If disconnected (empty-orphan bug),
	// this cherry-picks local commits onto remote tip, updating the local ref.
	// If reconciliation fails, abort — proceeding to rebase on disconnected
	// refs would silently combine unrelated histories.
	if reconcileErr := ReconcileDisconnectedMetadataRef(ctx, repo, ref, fetchedRefName, os.Stderr); reconcileErr != nil {
		return fmt.Errorf("metadata reconciliation failed: %w", reconcileErr)
	}

	// Get local ref (re-read after potential reconciliation update)
	localRef, err := repo.Reference(ref, true)
	if err != nil {
		return fmt.Errorf("failed to get local ref: %w", err)
	}

	// Get fetched ref (remote-tracking or temp ref, updated by the fetch above)
	remoteRef, err := repo.Reference(fetchedRefName, true)
	if err != nil {
		return fmt.Errorf("failed to get remote ref: %w", err)
	}

	advance := func(hash plumbing.Hash) error {
		if err := setRefHash(repo, ref, hash); err != nil {
			return err
		}
		if usedTempRef {
			_ = repo.Storer.RemoveReference(fetchedRefName) //nolint:errcheck // cleanup is best-effort
		}
		return nil
	}

	// If local is already at or behind remote, fast-forward
	if localRef.Hash() == remoteRef.Hash() {
		return advance(remoteRef.Hash())
	}

	// Find merge base
	repoPath, err := getRepoPath(repo)
	if err != nil {
		return fmt.Errorf("failed to get repo path: %w", err)
	}
	mergeBase, err := getMergeBase(ctx, repoPath, localRef.Hash().String(), remoteRef.Hash().String())
	if err != nil {
		return fmt.Errorf("failed to find merge base: %w", err)
	}

	// If local is ancestor of remote (merge base == local), fast-forward to remote
	if mergeBase == localRef.Hash() {
		if err := advance(remoteRef.Hash()); err != nil {
			return fmt.Errorf("failed to fast-forward ref: %w", err)
		}
		return nil
	}

	// Collect commits reachable from local but not from remote and cherry-pick
	// them onto the remote tip. This preserves local-only commits even when the
	// local metadata branch already contains old merge commits, while avoiding
	// replaying shared ancestors older than the true merge-base.
	localCommits, err := collectCommitsSince(ctx, repo, repoPath, localRef.Hash(), remoteRef.Hash())
	if err != nil {
		return fmt.Errorf("failed to collect local commits: %w", err)
	}

	if len(localCommits) == 0 {
		// No local-only commits — just point to remote
		return advance(remoteRef.Hash())
	}

	shallow, err := loadShallowHashes(ctx, repoPath)
	if err != nil {
		return fmt.Errorf("failed to load shallow boundaries: %w", err)
	}

	newTip, err := cherryPickOnto(ctx, repo, remoteRef.Hash(), localCommits, shallow)
	if err != nil {
		return fmt.Errorf("failed to rebase local commits onto remote: %w", err)
	}

	return advance(newTip)
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
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return plumbing.ZeroHash, errNoMergeBase
		}
		return plumbing.ZeroHash, fmt.Errorf("git merge-base failed: %w", err)
	}

	mergeBase := strings.TrimSpace(string(output))
	if mergeBase == "" {
		return plumbing.ZeroHash, errNoMergeBase
	}
	return plumbing.NewHash(mergeBase), nil
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

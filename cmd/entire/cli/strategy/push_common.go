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

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/perf"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// checkpointRefPush binds delivery to one immutable source hash while retaining
// the queue generation token that successful delivery may remove.
type checkpointRefPush struct {
	token checkpoint.PushQueueEntry
	name  plumbing.ReferenceName
	hash  plumbing.Hash
}

var checkpointRefRecoveryCAS = checkpoint.CASPersistentRef //nolint:gochecknoglobals // deterministic recovery-race test seam

// errCheckpointRefSuperseded marks a recovery that lost its compare-and-swap to
// a concurrent checkpoint write. The newer generation stays local and queued, so
// the flush treats the ref as superseded rather than as a failed push.
var errCheckpointRefSuperseded = errors.New("checkpoint ref superseded by a newer local generation")

func (p checkpointRefPush) refSpec() string {
	return p.hash.String() + ":" + p.name.String()
}

// partitionLocalRefs splits refs into those that exist locally (pushable) and
// those that don't (stale queue entries — e.g. a checkpoint ref deleted by
// cleanup). Stale refs can never push, so callers drop them from the queue
// rather than retrying them forever. listed holds the tip of every ref found by
// listing the checkpoint namespace; it is nil when the namespace was not
// listed, and a ref missing from it has no known tip.
func partitionLocalRefs(
	ctx context.Context,
	repo *git.Repository,
	refs []plumbing.ReferenceName,
) (existing, stale []plumbing.ReferenceName, listed map[plumbing.ReferenceName]plumbing.Hash) {
	return partitionLocalRefsUsing(ctx, repo, refs, len(refs) >= checkpointRefListingMinQueue)
}

// checkpointRefListingMinQueue is the queue length from which one listing of
// the checkpoint namespace beats a lookup per queued ref. The listing's cost
// follows every ref it reads, so a short queue is cheaper to look up ref by ref.
const checkpointRefListingMinQueue = 16

// partitionLocalRefsUsing is partitionLocalRefs with the choice of listing made
// by the caller. One listing answers most refs, where a per-ref lookup costs a
// packed-refs scan each. Refs the listing omits still get that lookup, because
// the listings skip broken refs, the go-git one skips symbolic refs, and both
// report the on-disk shard case, while a case-insensitive filesystem still
// resolves the queued name.
func partitionLocalRefsUsing(
	ctx context.Context,
	repo *git.Repository,
	refs []plumbing.ReferenceName,
	useListing bool,
) (existing, stale []plumbing.ReferenceName, listed map[plumbing.ReferenceName]plumbing.Hash) {
	if len(refs) == 0 {
		return nil, nil, nil
	}
	var present map[plumbing.ReferenceName]plumbing.Hash
	if useListing {
		var tipsMatchLookup bool
		var err error
		present, tipsMatchLookup, err = listLocalCheckpointRefs(ctx, repo)
		if err != nil {
			logging.Debug(ctx, "checkpoint refs: listing failed, looking up each queued ref", listErrorAttrs(err)...)
		}
		if tipsMatchLookup {
			listed = present
		}
	}
	for _, ref := range refs {
		if _, ok := present[ref]; ok || localRefMayExist(repo, ref) {
			existing = append(existing, ref)
		} else {
			stale = append(stale, ref)
		}
	}
	return existing, stale, listed
}

// localRefMayExist reports whether ref may still be present locally. Only a
// ref that is genuinely gone (e.g. deleted by cleanup) is stale; a transient/IO
// lookup error keeps the ref pushable so a real entry isn't dropped from the
// queue forever over a flaky read.
func localRefMayExist(repo *git.Repository, ref plumbing.ReferenceName) bool {
	_, err := repo.Reference(ref, false)
	return !errors.Is(err, plumbing.ErrReferenceNotFound)
}

// localTip is ref's commit: the listed tip when partitionLocalRefs listed it,
// otherwise a resolving lookup. listed may be nil.
func localTip(repo *git.Repository, listed map[plumbing.ReferenceName]plumbing.Hash, ref plumbing.ReferenceName) (plumbing.Hash, error) {
	if hash, ok := listed[ref]; ok {
		return hash, nil
	}
	r, err := repo.Reference(ref, true)
	if err != nil {
		return plumbing.ZeroHash, err //nolint:wrapcheck // callers branch on plumbing.ErrReferenceNotFound
	}
	return r.Hash(), nil
}

// listLocalCheckpointRefs maps every ref under the checkpoint namespace to its
// tip. It reads through go-git, which needs no subprocess, and falls back to git
// for-each-ref when go-git's walk fails: one empty or unreadable loose ref, or a
// malformed packed-refs line, anywhere under refs/ aborts it. tipsMatchLookup
// reports whether the tips are the ones repo.Reference returns; git's are not
// for a ref file with trailing data, which go-git reads as the zero hash, so
// those only show which refs exist.
func listLocalCheckpointRefs(
	ctx context.Context,
	repo *git.Repository,
) (listed map[plumbing.ReferenceName]plumbing.Hash, tipsMatchLookup bool, err error) {
	listed, err = listCheckpointRefsWithGoGit(ctx, repo)
	if err == nil {
		return listed, true, nil
	}
	logging.Debug(ctx, "checkpoint refs: go-git listing failed, listing with git for-each-ref",
		slog.String("error", err.Error()))
	listed, err = listCheckpointRefsWithGit(ctx, repo)
	return listed, false, err
}

// listCheckpointRefsWithGoGit lists the checkpoint namespace through repo's
// storer. Only refs with a non-zero hash are listed: a symbolic ref carries no
// hash here and an unparsable one reads as the zero hash, so both are left to
// the per-ref lookup, which resolves the first and reports the second.
func listCheckpointRefsWithGoGit(ctx context.Context, repo *git.Repository) (map[plumbing.ReferenceName]plumbing.Hash, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}
	iter, err := repo.References()
	if err != nil {
		return nil, fmt.Errorf("list refs: %w", err)
	}
	defer iter.Close()
	listed := make(map[plumbing.ReferenceName]plumbing.Hash)
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if !ref.Hash().IsZero() && strings.HasPrefix(ref.Name().String(), checkpoint.CheckpointRefPrefix) {
			listed[ref.Name()] = ref.Hash()
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterate refs: %w", err)
	}
	return listed, nil
}

// listCheckpointRefsWithGit lists the checkpoint namespace with git
// for-each-ref in repo's common dir, where checkpoint refs live for all
// worktrees.
func listCheckpointRefsWithGit(ctx context.Context, repo *git.Repository) (map[plumbing.ReferenceName]plumbing.Hash, error) {
	commonDir, err := checkpoint.RepositoryCommonDir(repo)
	if err != nil {
		return nil, fmt.Errorf("resolve git common dir: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir="+commonDir,
		"for-each-ref", "--format=%(objectname) %(refname)", "--end-of-options", checkpoint.CheckpointRefPrefix)
	cmd.Dir = commonDir
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list checkpoint refs: %w", err)
	}
	listed := make(map[plumbing.ReferenceName]plumbing.Hash)
	for line := range strings.SplitSeq(string(out), "\n") {
		oid, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		// An unparsable line leaves its ref to the per-ref lookup.
		if hash, ok := plumbing.FromHex(oid); ok {
			listed[plumbing.ReferenceName(name)] = hash
		}
	}
	return listed, nil
}

// listErrorAttrs describes a failed listing, with git's stderr when it ran.
func listErrorAttrs(err error) []any {
	attrs := []any{slog.String("error", err.Error())}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		attrs = append(attrs, slog.String("stderr", strings.TrimSpace(string(exitErr.Stderr))))
	}
	return attrs
}

// batchPushCheckpointRefs pushes each ref at its pinned hash to target in a
// single git push, fast-forward-only (NOT a force push). Batching keeps a chunk
// of many refs to one network round-trip. Per-checkpoint refs normally advance by
// fast-forward (each write parents on the prior tip), so the common case
// succeeds; a non-fast-forward update — genuine divergence, e.g. the same
// checkpoint written differently on another machine — is REJECTED rather than
// silently overwriting the remote. We deliberately do not force: there is no
// server-side ref protection, so a force push would make a buggy or racing client
// clobber good remote history with no signal. On rejection the whole push errors;
// the caller retries the rejected refs individually with fetch+replay recovery
// (pushCheckpointRefWithRecovery).
func batchPushCheckpointRefs(ctx context.Context, target string, refs []checkpointRefPush) error {
	if len(refs) == 0 {
		return nil
	}
	refSpecs := make([]string, 0, len(refs))
	for _, ref := range refs {
		refSpecs = append(refSpecs, ref.refSpec())
	}
	if _, err := remote.PushWithOptions(ctx, remote.PushOptions{Remote: target, RefSpecs: refSpecs}); err != nil {
		return fmt.Errorf("push %d checkpoint refs: %w", len(refs), err)
	}
	return nil
}

// chunkPushResult is what the chunked batch phase of a flush left behind.
type chunkPushResult struct {
	landed        int                 // refs in chunks that pushed cleanly
	failed        []checkpointRefPush // refs of chunks that failed, for the per-ref fallback
	untried       []checkpointRefPush // refs never attempted because the phase stopped
	stopReason    string              // why untried is non-empty
	firstErr      error
	sshAuthFailed bool
	// unreachable is the line of git output showing the remote could not be
	// reached at all; set only when no chunk had landed. See
	// remote.UnreachableRemoteLine.
	unreachable string
}

// pushRefChunks batch-pushes refs size at a time (see
// checkpointRefPushChunkSize), calling onLanded with each chunk that lands so
// the caller can dequeue it before a later chunk is cut. A failed chunk does not
// stop the phase — a rejection fails only its own chunk — but an SSH auth
// failure, an expired or cancelled ctx, or maxConsecutiveChunkPushFailures in a
// row does, and so does an unreachable remote before any chunk landed: per-ref
// retries cannot reach it either, and each would wait out its own connect
// timeout. The first chunk is always attempted, matching the per-ref fallback.
func pushRefChunks(ctx context.Context, target string, refs []checkpointRefPush, size int, onLanded func([]checkpointRefPush)) chunkPushResult {
	var res chunkPushResult
	consecutive := 0
	for start := 0; start < len(refs); start += size {
		if start > 0 {
			if reason := chunkStopReason(ctx, consecutive); reason != "" {
				res.untried = refs[start:]
				res.stopReason = reason
				logging.Warn(ctx, "git-refs push: batch push stopped early",
					slog.String("reason", reason), slog.Int("untried", len(res.untried)))
				return res
			}
		}
		chunk := refs[start:min(start+size, len(refs))]
		err := batchPushCheckpointRefs(ctx, target, chunk)
		if err == nil {
			consecutive = 0
			res.landed += len(chunk)
			onLanded(chunk)
			continue
		}
		consecutive++
		// The only account of a wholesale failure. The per-ref retries re-derive
		// a *rejection* reason, but a transport failure — an unreachable remote,
		// a stalled SSH connection — matches nothing in
		// checkpointRefRejectionReason, so without this line its cause reached
		// neither the terminal nor .entire/logs. Logged, not printed: the
		// caller's one actionable line is the useful part.
		logging.Warn(ctx, "git-refs push: batch checkpoint ref push failed; retrying individually",
			slog.Int("refs", len(chunk)), slog.String("error", err.Error()))
		if res.firstErr == nil {
			res.firstErr = err
		}
		res.failed = append(res.failed, chunk...)
		if nonInteractiveSSHAuthFailure(ctx, err) {
			res.sshAuthFailed = true
			res.untried = refs[start+len(chunk):]
			return res
		}
		// A remote that already answered this flush — took a chunk, or failed
		// one any way other than a connect failure — was reachable; a later
		// connect failure is transient, and the per-ref fallback may still land
		// or recover those refs. Only a connect failure on the first chunk the
		// remote saw means unreachable.
		if line, ok := remote.UnreachableRemoteLine(err); ok && res.landed == 0 && len(res.failed) == len(chunk) {
			res.unreachable = line
			res.untried = refs[start+len(chunk):]
			return res
		}
	}
	return res
}

// chunkStopReason reports why the batch phase should stop before its next
// chunk, or "" to continue. Same bare phrasing as flushAbortReason.
func chunkStopReason(ctx context.Context, consecutiveFailures int) string {
	if reason := flushAbortReason(ctx, 0); reason != "" {
		return reason
	}
	if consecutiveFailures >= maxConsecutiveChunkPushFailures {
		return fmt.Sprintf("%d consecutive failed batches", consecutiveFailures)
	}
	return ""
}

// pushCheckpointRefWithRecovery pushes a single checkpoint ref fast-forward-only;
// confirmed remote policy/hook rejections return immediately. Other failures —
// typically the ref diverged on the remote (the same checkpoint re-written
// elsewhere) — fetch the remote ref and replay the local-only commits on top via
// recoverCheckpointRef, then retry. The retry is still
// non-force: after the replay the local ref is a fast-forward over the remote, so
// the remote commit is preserved as an ancestor rather than overwritten. The
// cherry-pick is delta-based, so non-overlapping changes merge; a genuine overlap
// (e.g. both sides rewrote the root metadata.json) surfaces as a rebase error and
// the ref is left for a later pre-push. Returns nil only if the ref reached the
// remote.
func pushCheckpointRefWithRecovery(
	ctx context.Context,
	repo *git.Repository,
	target string,
	candidate checkpointRefPush,
) (checkpointRefPush, error) {
	// One shared budget across the initial push, fetch+replay, and retry, matching
	// doPushRef (fetchAndRebaseRefCommon relies on the caller's deadline).
	ctx, cancel := context.WithTimeout(ctx, checkpointPushBudget)
	defer cancel()

	pushErr := batchPushCheckpointRefs(ctx, target, []checkpointRefPush{candidate})
	if pushErr == nil {
		return candidate, nil
	}
	if checkpointRefRejectionReason(pushErr) != "" {
		// Fetch+replay cannot fix a remote policy/hook rejection. If the ref
		// already exists remotely, replay would needlessly rewrite local
		// commits (including their committer timestamps) on every push.
		return checkpointRefPush{}, pushErr
	}
	recovered, err := recoverCheckpointRef(ctx, repo, target, candidate)
	if err != nil {
		// Recovery is speculative: a missing remote ref may mean the push was
		// blocked, not that it diverged. Keep the push failure primary.
		return checkpointRefPush{}, &checkpointRefRecoveryError{pushErr: pushErr, recoveryErr: err}
	}
	if err := batchPushCheckpointRefs(ctx, target, []checkpointRefPush{recovered}); err != nil {
		return checkpointRefPush{}, err
	}
	return recovered, nil
}

// recoverCheckpointRef replays exactly candidate.hash onto the fetched remote
// tip and installs the result only if the local ref still names that same
// generation. A concurrent checkpoint write therefore wins the CAS and remains
// both locally reachable and queued for a later delivery attempt.
func recoverCheckpointRef(
	ctx context.Context,
	repo *git.Repository,
	target string,
	candidate checkpointRefPush,
) (checkpointRefPush, error) {
	fetchTarget, err := remote.ResolveFetchTarget(ctx, target)
	if err != nil {
		return checkpointRefPush{}, fmt.Errorf("resolve fetch target: %w", err)
	}
	fetchedRefName := plumbing.ReferenceName(
		"refs/entire-fetch-tmp/" + strings.TrimPrefix(candidate.name.String(), "refs/"),
	)
	refSpec := fmt.Sprintf("+%s:%s", candidate.name, fetchedRefName)
	if err := fetchCheckpointRef(ctx, fetchTarget, refSpec, 0); err != nil {
		return checkpointRefPush{}, err
	}
	defer func() {
		_ = repo.Storer.RemoveReference(fetchedRefName) //nolint:errcheck // cleanup is best-effort
	}()

	repoPath, err := getRepoPath(repo)
	if err != nil {
		return checkpointRefPush{}, fmt.Errorf("get repository path: %w", err)
	}
	var replay fetchedReplay
	err = RetryAfterDeepFetch(
		func() (replayErr error) {
			replay, replayErr = replayFetchedCheckpointRef(ctx, repoPath, fetchedRefName, candidate.hash)
			return replayErr
		},
		func() error { return fetchCheckpointRef(ctx, fetchTarget, refSpec, remote.FullRefDepth) },
	)
	if err != nil {
		return checkpointRefPush{}, err
	}
	fetched := replay.repo
	defer fetched.Close()
	newTip, replayed := replay.tip, replay.replayed
	if err := checkpointRefRecoveryCAS(ctx, fetched, candidate.name, newTip, candidate.hash); err != nil {
		// newTip has no unique writer data: it is derived entirely from the
		// immutable candidate plus the fetched remote. On a CAS conflict the
		// writer's winning generation remains the live ref and its queue entry
		// remains untouched. A later delivery recomputes the replay from that
		// winner; retaining this losing intermediate under another ref would
		// create a second source of truth for the checkpoint.
		if errors.Is(err, gitrepo.ErrRefCASConflict) {
			err = fmt.Errorf("%w: %w", errCheckpointRefSuperseded, err)
		}
		return checkpointRefPush{}, fmt.Errorf("install recovered checkpoint ref %s: %w", candidate.name, err)
	}
	if replayed > 0 {
		// Recovery prints nothing on success, so the rewrite of local commits
		// must still leave a trace in the log. Metadata only.
		logging.Info(ctx, "replayed checkpoint ref onto fetched remote tip; local ref rewritten",
			slog.String("ref", candidate.name.String()),
			slog.String("local_tip", candidate.hash.String()),
			slog.String("remote_tip", replay.remoteTip.String()),
			slog.Int("commits_replayed", replayed))
	}
	candidate.hash = newTip
	return candidate, nil
}

// fetchCheckpointRef fetches refSpec from fetchTarget; a non-zero depth fetches
// that ref's history to that absolute depth.
func fetchCheckpointRef(ctx context.Context, fetchTarget, refSpec string, depth int) error {
	_, fetchSpan := perf.Start(ctx, "git_fetch")
	_, fetchErr := remote.Fetch(ctx, remote.FetchOptions{
		Remote:   fetchTarget,
		RefSpecs: []string{refSpec},
		NoTags:   true,
		Depth:    depth,
	})
	fetchSpan.RecordError(fetchErr)
	fetchSpan.End()
	if fetchErr != nil {
		return fmt.Errorf("fetch failed: %w", fetchErr)
	}
	return nil
}

// fetchedReplay is a replay computed from a freshly opened repository, which
// the caller closes.
type fetchedReplay struct {
	repo      *git.Repository
	remoteTip plumbing.Hash
	tip       plumbing.Hash
	replayed  int
}

// replayFetchedCheckpointRef replays localHash onto the ref a fetch just wrote.
// go-git loads a handle's pack index once and never reloads it, so objects the
// fetch wrote into a new pack are invisible to a handle that has already read
// objects; each call therefore opens the repository afresh.
func replayFetchedCheckpointRef(ctx context.Context, repoPath string, fetchedRefName plumbing.ReferenceName, localHash plumbing.Hash) (fetchedReplay, error) {
	fetched, err := gitrepo.OpenPath(repoPath)
	if err != nil {
		return fetchedReplay{}, fmt.Errorf("reopen repository after fetch: %w", err)
	}
	remoteRef, err := fetched.Reference(fetchedRefName, true)
	if err != nil {
		_ = fetched.Close()
		return fetchedReplay{}, fmt.Errorf("resolve fetched checkpoint ref: %w", err)
	}
	tip, replayed, err := replayOntoRemote(ctx, fetched, localHash, remoteRef.Hash())
	if err != nil {
		_ = fetched.Close()
		return fetchedReplay{}, err
	}
	return fetchedReplay{repo: fetched, remoteTip: remoteRef.Hash(), tip: tip, replayed: replayed}, nil
}

// replayOntoRemote returns the tip that results from replaying the commits
// only localHash reaches onto remoteHash. It never writes a ref; each caller
// installs the tip under its own update rule. Disconnected history (no merge
// base) is replayed with replayDisconnectedOnto. replayed counts the local
// commits handed to the cherry-pick, zero when the remote tip is returned as is.
func replayOntoRemote(
	ctx context.Context,
	repo *git.Repository,
	localHash, remoteHash plumbing.Hash,
) (tip plumbing.Hash, replayed int, err error) {
	if localHash.Equal(remoteHash) {
		return remoteHash, 0, nil
	}
	repoPath, err := getRepoPath(repo)
	if err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("get repository path: %w", err)
	}
	mergeBase, err := getMergeBase(ctx, repoPath, localHash.String(), remoteHash.String())
	if err == nil && mergeBase.Equal(localHash) {
		return remoteHash, 0, nil
	}

	shallow, shallowErr := loadShallowHashes(ctx, repoPath)
	if shallowErr != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("load shallow boundaries: %w", shallowErr)
	}
	if errors.Is(err, errNoMergeBase) {
		tip, replayed, err := replayDisconnectedOnto(ctx, repo, repoPath, localHash, remoteHash, shallow, io.Discard)
		if err != nil {
			return plumbing.ZeroHash, 0, fmt.Errorf("replay disconnected checkpoint commits: %w", err)
		}
		return tip, replayed, nil
	}
	if err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("find checkpoint merge base: %w", err)
	}

	commits, err := collectCommitsSince(ctx, repo, repoPath, localHash, remoteHash)
	if err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("collect local checkpoint commits: %w", err)
	}
	if len(commits) == 0 {
		return remoteHash, 0, nil
	}
	newTip, err := cherryPickOnto(ctx, repo, remoteHash, commits, shallow)
	if err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("replay checkpoint commits: %w", err)
	}
	return newTip, len(commits), nil
}

// checkpointRefRecoveryError keeps both failures inspectable, while allowing the
// terminal to show just the bounded push reason, not the speculative fetch error.
type checkpointRefRecoveryError struct {
	pushErr     error
	recoveryErr error
}

func (e *checkpointRefRecoveryError) Error() string {
	return fmt.Sprintf("%v (checkpoint ref recovery failed: %v)", e.pushErr, e.recoveryErr)
}

func (e *checkpointRefRecoveryError) Unwrap() []error {
	return []error{e.pushErr, e.recoveryErr}
}

// checkpointRefRejectionReason reports only confirmed remote rejections. Network
// failures must not be described as rejections, and plain divergence stays quiet.
func checkpointRefRejectionReason(err error) string {
	var recoveryErr *checkpointRefRecoveryError
	if errors.As(err, &recoveryErr) {
		err = recoveryErr.pushErr
	}
	if err == nil {
		return ""
	}
	detail := err.Error() // Already collapsed and elided by remote.PushWithOptions.
	if strings.Contains(detail, "[remote rejected]") || isProtectedRefRejection(detail) {
		var pushErr *remote.PushError
		if errors.As(err, &pushErr) {
			return pushErr.Output() // Keep line breaks for the terminal, not log formatting.
		}
		return detail
	}
	return ""
}

// pushRefIfNeeded pushes a ref to the given target if it has unpushed changes.
// The target can be a remote name (e.g., "origin") or a URL for direct push.
// For branch refs, the "has unpushed" optimization consults the remote-tracking
// ref. Non-branch refs and URL targets skip the optimization and let git
// handle the no-op case.
// Does not check any settings — callers are responsible for gating.
//
// moved reports that this call moved the ref on the target, as opposed to
// finding it already there. Only then did the remote just finish receiving a
// push, which is what the pre-push hook has to let settle before git sends the
// user's own (see settleBeforeUserPush).
func pushRefIfNeeded(ctx context.Context, target string, ref plumbing.ReferenceName) (delivered, moved bool, err error) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Debug(ctx, "push skipped: open repository failed",
			slog.String("ref", ref.String()),
			slog.String("error", err.Error()))
		return false, false, nil
	}
	defer repo.Close()

	localRef, err := repo.Reference(ref, true)
	if err != nil {
		// Ref doesn't exist locally — nothing to push, and nothing delivered.
		// Reporting false rather than "vacuously delivered" keeps a caller that
		// gates on delivery from acting on a ref that never existed.
		return false, false, nil
	}

	if ref.IsBranch() && !remote.IsURL(target) && !hasUnpushedBranchRef(repo, target, localRef.Hash(), ref.Short()) {
		// Local matches the target's tracking ref: the data is already there,
		// which is delivery as far as any caller can tell.
		return true, false, nil
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
	return !localHash.Equal(remoteRef.Hash())
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

// checkpointFlushBudget bounds a pre-push flushCheckpointRefsQueue as a whole —
// the chunked batch push and the individual-retry fallback share it — which
// checkpointPushBudget cannot: that one caps a single ref, and the fallback walks
// the queue serially, so a backlog multiplies it. The batch needs the bound too:
// it carries the whole backlog, so a slow uplink or a stalled connection would
// otherwise hold the user's git push for as long as the upload takes. A
// healthy push costs roughly one SSH round-trip per ref, so a few hundred
// queued refs is already tens of minutes of a `git push` that looks
// hung; when the destination is unreachable every ref instead pays the full
// per-ref budget and the same queue runs for hours. Refs that do not fit stay
// queued and go out on the next push, so this bounds progress, never data.
//
// Applied as a context deadline, not a wall-clock check between refs, so it can
// cut a ref that is already hung rather than waiting out its own budget first.
// context.WithTimeout keeps the earlier of parent and child deadlines, so the
// per-ref checkpointPushBudget automatically shrinks to whatever is left.
//
// Shared, so a batch the budget cuts ends the flush there: the per-ref fallback
// does not run on a spent budget, a chunk cut mid-push rotates to the back of
// the queue, and the refs stay queued for the next push. The explicit pushes
// (PushQueuedCheckpointRefs, `entire checkpoint push`) leave the batch
// unbounded; the user asked for that upload and is waiting on it.
//
// Declared as a var so tests can shrink it.
var checkpointFlushBudget = 2 * time.Minute

// checkpointRefPushChunkSize is the most queued refs one batch push carries.
//
// It must keep one push's command line under Windows' 32,767-character
// CreateProcess limit: every ref is one "<40-hex>:refs/entire/checkpoints/..."
// refspec of about 95 characters on argv, so a single push of ~340 or more refs
// fails before git starts (#2986). 200 refs is about 19,000 characters.
//
// Within that bound it is large on purpose: every push pays a fixed cost — the
// connection and the remote advertising every ref it holds — so a healthy link
// is fastest with the fewest pushes. Against GitHub and Entire, 200 checkpoints
// went out in 3-7s as one push and took 11-20s in chunks of 25.
//
// Chunks are also what make the flush budget bound progress rather than discard
// it: each chunk that lands leaves the queue at once, so a backlog too large for
// one budget drains over several pushes instead of being cut at the same point
// by every one of them. A link too slow to land one chunk within the budget
// would still never move the head of the queue, so the size actually used
// adapts (see PushQueue.ChunkSizeHint and adaptChunkSize): a chunk the budget
// cuts before anything landed quarters it, down to one ref, and a flush that
// lands everything doubles it back. Declared as a var so tests can shrink it.
var checkpointRefPushChunkSize = 200

// maxConsecutiveChunkPushFailures stops the batch phase once this many chunks in
// a row have failed. A rejection fails only its own chunk, so batching carries on
// past one; several in a row point at the destination, and the per-ref fallback
// already probes that with its own cap.
const maxConsecutiveChunkPushFailures = 2

// maxConsecutiveRefPushFailures stops the individual-retry fallback once this
// many refs in a row have failed. The fallback exists to isolate the odd
// diverged or blocked ref from an otherwise pushable queue; once several fail in
// a row the cause is the destination rather than the refs, and walking the rest
// of the queue only repeats it. A success resets the count, so one poisoned ref
// — say a single checkpoint tripping push protection — does not strand the refs
// queued behind it.
const maxConsecutiveRefPushFailures = 5

// flushAbortReason reports why the individual-retry fallback should stop before
// the end of the queue, or "" to continue.
//
// The reason is a bare phrase: it is printed inside the user's `git push` and
// names what stopped the retry, never a diagnosis of the underlying failure —
// the same discipline flushCheckpointRefsQueue's retry line follows, and for the
// same reason. A stalled remote and a rejecting one abort identically here.
func flushAbortReason(ctx context.Context, consecutiveFailures int) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Sprintf("budget (%s) exhausted", checkpointFlushBudget)
	case ctx.Err() != nil:
		return "interrupted"
	case consecutiveFailures >= maxConsecutiveRefPushFailures:
		return fmt.Sprintf("%d consecutive failures", consecutiveFailures)
	}
	return ""
}

// doPushRef pushes the given ref to the target with fetch+rebase recovery.
// The target can be a remote name or a URL.
//
// The error return is fail-soft by design and is nil on every push failure: a
// checkpoint that cannot be synced must never break the user's own `git push`.
// That makes err useless as a "did it land" signal, so delivery is reported
// separately — delivered is true only where finishPush runs, meaning the ref is
// on the target (freshly pushed or already up-to-date). Callers that act on
// delivery, such as latching the captured checkpoint sync remote, must read
// delivered and not err. moved reports that a destination took the ref rather
// than already having it, which a failed push to several URLs can also report
// for the ones it reached before the failure.
func doPushRef(ctx context.Context, target string, ref plumbing.ReferenceName) (delivered, moved bool, err error) {
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
		return true, !result.upToDate, nil
	}
	stop("")
	// A destination that took the ref before another refused it has still just
	// received a push (see pushResult.updated).
	moved = result.updated

	// Protected refs cannot be fixed by syncing and retrying.
	var protectedErr *protectedRefError
	if errors.As(err, &protectedErr) {
		printProtectedRefBlock(os.Stderr, refLabel, target)
		return false, moved, nil
	}

	// Non-interactive SSH (pre-push BatchMode): auth failures cannot be fixed by
	// fetch+rebase, and retrying would just reprint the same opaque error.
	// Surface an actionable ssh-agent hint and skip recovery (issue #1523).
	if nonInteractiveSSHAuthFailure(ctx, err) {
		fmt.Fprintf(os.Stderr, "[entire] Warning: couldn't push %s: %v\n", refLabel, err)
		printNonInteractiveSSHAuthHint()
		printCheckpointRemoteHint(target)
		return false, moved, nil
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
		if nonInteractiveSSHAuthFailure(ctx, syncErr) {
			printNonInteractiveSSHAuthHint()
		}
		printCheckpointRemoteHint(target)
		return false, moved, nil // Don't fail the main push
	}
	stop(" done")

	// Try pushing again after rebase
	fmt.Fprintf(os.Stderr, "[entire] Pushing %s to %s...", refLabel, displayTarget)
	stop = startProgressDots(os.Stderr)

	result, retryErr := tryPushRefCommon(ctx, target, ref)
	moved = moved || result.updated
	if retryErr != nil {
		stop("")
		fmt.Fprintf(os.Stderr, "[entire] Warning: failed to push %s after sync: %v\n", refLabel, retryErr)
		if nonInteractiveSSHAuthFailure(ctx, retryErr) {
			printNonInteractiveSSHAuthHint()
		}
		printCheckpointRemoteHint(target)
		return false, moved, nil
	}
	finishPush(ctx, stop, result, target)
	return true, moved || !result.upToDate, nil
}

// refDisplayName returns a user-readable name for ref. Branch refs use the
// short name (e.g. "entire/checkpoints/v1"); other refs use the full name.
func refDisplayName(ref plumbing.ReferenceName) string {
	if ref.IsBranch() {
		return ref.Short()
	}
	return ref.String()
}

// nonInteractiveSSHAuthFailure reports whether err is an SSH auth-shaped
// failure under a BatchMode (non-interactive) context. Used to print the
// actionable ssh-agent hint and skip useless recovery retries.
func nonInteractiveSSHAuthFailure(ctx context.Context, err error) bool {
	return err != nil && remote.IsNonInteractiveSSH(ctx) && remote.LooksLikeSSHAuthFailure(err.Error())
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

// sshAuthHintOnce ensures the ssh-agent hint prints at most once per process
// (pre-push can push multiple refs).
var sshAuthHintOnce sync.Once

// printNonInteractiveSSHAuthHint tells the user how to unblock checkpoint pushes
// that failed because SSH needed interactive auth under BatchMode (issue #1523).
func printNonInteractiveSSHAuthHint() {
	sshAuthHintOnce.Do(func() {
		fmt.Fprintln(os.Stderr, "[entire] Checkpoint push skipped: SSH needs interactive auth (passphrase/PIN) and cannot prompt during git hooks.")
		fmt.Fprintln(os.Stderr, "[entire] Load your key into ssh-agent (`ssh-add`), then push again. Checkpoints are saved locally until then.")
		fmt.Fprintln(os.Stderr, "[entire] PIN-protected security keys: unlock/add them to the agent first. To allow prompts in this path, set GIT_SSH_COMMAND (or core.sshCommand) with an explicit BatchMode=no.")
	})
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
	// upToDate is true when every destination already had the ref (nothing
	// transferred anywhere).
	upToDate bool
	// updated is true when at least one destination took the ref. A remote
	// with several push URLs can report it alongside a failure: git pushes to
	// the URLs in turn and stops at the first that refuses.
	updated bool
}

// parsePushResult checks git push --porcelain output for ref status flags.
// In porcelain mode, each ref gets a tab-delimited status line:
//
//	<flag>\t<from>:<to>\t<summary>
//
// where flag '=' means the ref was already up-to-date. This is locale-independent,
// unlike the human-readable "Everything up-to-date" message.
//
// A remote with several push URLs prints one block per destination, so one can
// report '=' while another took the ref (' ' fast-forward, '+' forced, '*' new,
// '-' deleted). The push is up-to-date only if no destination took anything.
// Output with no status line at all is neither.
func parsePushResult(output string) pushResult {
	var result pushResult
	sawUpToDate := false
	for _, line := range strings.Split(output, "\n") {
		if len(line) < 2 || line[1] != '\t' {
			continue
		}
		switch line[0] {
		case '=':
			sawUpToDate = true
		case ' ', '+', '*', '-':
			result.updated = true
		}
	}
	result.upToDate = sawUpToDate && !result.updated
	return result
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
		// Parsed anyway: a push to several URLs can land on some before one
		// refuses (see pushResult.updated).
		return parsePushResult(outputStr), classifyPushFailure(ctx, outputStr, err)
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
	// .git/shallow (collectCommitChain / collectCommitsSince). When a boundary
	// hides the merge base they refuse with errShallowHidesMergeBase, and the
	// retry below deepens only this ref before trying once more.
	// Span the fetch separately so a slow sync can be attributed to the network
	// fetch versus the local reconcile/rebase that follows it.
	if err := fetchCheckpointRef(ctx, fetchTarget, refSpec, 0); err != nil {
		return err
	}
	return RetryAfterDeepFetch(
		func() error { return syncFetchedRef(ctx, ref, fetchedRefName, usedTempRef) },
		func() error { return fetchCheckpointRef(ctx, fetchTarget, refSpec, remote.FullRefDepth) },
	)
}

// syncFetchedRef reconciles and rebases the local ref onto fetchedRefName, the
// ref a fetch just wrote. It opens the repository itself so objects from that
// fetch are visible.
func syncFetchedRef(ctx context.Context, ref, fetchedRefName plumbing.ReferenceName, usedTempRef bool) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	// Reconcile disconnected metadata branches before rebasing.
	// The fetch updated the remote-tracking ref, so reconciliation
	// can compare fresh local vs remote. If disconnected (empty-orphan bug),
	// this cherry-picks local commits onto remote tip, updating the local ref.
	// If reconciliation fails, abort so the caller sees its error.
	if reconcileErr := ReconcileDisconnectedMetadataRef(ctx, repo, ref, fetchedRefName, os.Stderr); reconcileErr != nil {
		return fmt.Errorf("metadata reconciliation failed: %w", reconcileErr)
	}

	// Get local ref (re-read after potential reconciliation update)
	localRef, err := repo.Reference(ref, true)
	if err != nil {
		return fmt.Errorf("failed to get local ref: %w", err)
	}

	// Get fetched ref (remote-tracking or temp ref, updated by the fetch)
	remoteRef, err := repo.Reference(fetchedRefName, true)
	if err != nil {
		return fmt.Errorf("failed to get remote ref: %w", err)
	}

	newTip, _, err := replayOntoRemote(ctx, repo, localRef.Hash(), remoteRef.Hash())
	if err != nil {
		return fmt.Errorf("failed to rebase local commits onto remote: %w", err)
	}
	if err := setRefHash(repo, ref, newTip); err != nil {
		return err
	}
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

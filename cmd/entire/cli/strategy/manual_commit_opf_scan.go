// Detached OPF scan worker, shared by both checkpoint backends.
//
// OPF inference is slow (about 1.14 s per KB of prose on CPU), so a real
// session takes minutes to hours of model time. The pre-push hook therefore
// never runs the model: it rewrites checkpoints from the OPF span cache and
// holds back anything not scanned yet. This worker fills the cache in the
// background and then delivers what it scanned, so the user's `git push`
// returns immediately and their checkpoints follow as soon as OPF finishes.
//
// The worker only acts on the decision the user's push made. It is spawned
// only when that push resolved OPFRun, it delivers only to the remote that
// push named, and delivery goes through the same pre-push code, which ships a
// checkpoint only once its exact commit carries Entire-OPF-Applied. If the
// worker cannot reach the remote, the checkpoints stay queued and the next
// push delivers them.
package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/spawnmarker"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
)

const opfScanComponent = "opf-scan"

// opfScanSpawnThrottle collapses a burst of pushes into one spawn. The worker
// lock already keeps a second worker from doing anything, so this only saves
// the fork.
const opfScanSpawnThrottle = time.Minute

// opfScanMaxPasses bounds the worker's loop. A pass repeats only while new
// unscanned content keeps appearing (a session still writing checkpoints), so
// this exists to stop a detached process nobody watches from spinning.
const opfScanMaxPasses = 10

// opfScanDeliveryTimeout bounds one delivery attempt. Delivery is a push of
// already-rewritten refs; a push that takes longer than this is treated as
// failed and left for the next user push.
const opfScanDeliveryTimeout = 10 * time.Minute

// opfScanWorkerLockName is held for a worker's whole run, in the shared git
// common dir so workers spawned from different worktrees exclude each other.
const opfScanWorkerLockName = "entire/opf-scan-worker.lock"

// opfScanWorkerLockWait turns the blocking flock into a try-lock: a worker that
// cannot take the lock almost immediately leaves the work to the one holding
// it, which re-collects its work after every pass.
const opfScanWorkerLockWait = 100 * time.Millisecond

// opfScanSpawn is the process-spawn seam, swapped in tests: execx.SpawnDetached
// is a no-op under `go test`, which would make the spawn decision unobservable.
var opfScanSpawn = spawnDetachedOPFScanProcess

func spawnDetachedOPFScanProcess(worktreeRoot, remote string) {
	execx.SpawnDetached(worktreeRoot, "__opf_scan", remote)
}

type opfScanWorkerKey struct{}

// withinOPFScanWorker marks ctx as running inside the scan worker. Pre-push
// code consults it to take the decision the spawning push already made, and to
// never spawn another worker from inside one.
func withinOPFScanWorker(ctx context.Context) context.Context {
	return context.WithValue(ctx, opfScanWorkerKey{}, true)
}

func inOPFScanWorker(ctx context.Context) bool {
	v, _ := ctx.Value(opfScanWorkerKey{}).(bool) //nolint:errcheck // absent means false
	return v
}

// maybeSpawnOPFScan starts the scan worker for remote unless this already is
// the worker or a worker was spawned within opfScanSpawnThrottle. Callers
// invoke it only when the push resolved OPFRun and something was held back as
// not yet scanned.
func maybeSpawnOPFScan(ctx context.Context, remote string) {
	if inOPFScanWorker(ctx) || !redact.OPFEnabled() {
		return
	}
	logCtx := logging.WithComponent(ctx, opfScanComponent)
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Debug(logCtx, "skipping OPF scan spawn: could not resolve worktree root",
			slog.String("error", err.Error()))
		return
	}
	commonDir, err := gitdir.CommonDir(ctx)
	if err != nil {
		logging.Debug(logCtx, "skipping OPF scan spawn: could not resolve git common dir",
			slog.String("error", err.Error()))
		return
	}
	if spawnmarker.RecentlySpawned(commonDir, "opf-scan-spawn", opfScanSpawnThrottle, time.Now()) {
		return
	}
	opfScanSpawn(root, remote)
	logging.Info(logCtx, "checkpoints held for OPF; spawned background scan", slog.String("remote", remote))
}

// RunOPFScan is the body of the detached `entire __opf_scan <remote>` worker.
// Each pass collects the content waiting for OPF, scans what the span cache
// lacks, and then runs the pre-push delivery for remote, which rewrites from
// the cache and pushes whatever is now covered. It stops when nothing is left,
// when a pass finds exactly the work the previous one did (nothing will change
// without new input), or after opfScanMaxPasses.
//
// Best-effort by construction: every failure is logged, never returned as a
// process error, because nothing watches this child's exit code.
func RunOPFScan(ctx context.Context, remote string) error {
	logCtx := logging.WithComponent(ctx, opfScanComponent)
	// OPFEnabled reads process-global config that only EnsureRedactionConfigured
	// sets; without it the worker would read "OPF off" and do nothing. Unlike
	// the hook, a scanner-config error stops the worker: stamping commits with a
	// scanner set the settings did not choose is not a call a background
	// process gets to make.
	if err := EnsureRedactionConfigured(ctx); err != nil {
		logging.Warn(logCtx, "opf scan skipped: redaction could not be configured",
			slog.String("error", err.Error()))
		return nil
	}
	if !redact.OPFEnabled() {
		logging.Debug(logCtx, "opf scan skipped: OPF is not enabled")
		return nil
	}
	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Warn(logCtx, "opf scan skipped: could not open repo", slog.String("error", err.Error()))
		return nil
	}
	defer repo.Close()

	release, held := acquireOPFScanWorkerLock(ctx)
	if held {
		logging.Debug(logCtx, "opf scan skipped: another worker is already running")
		return nil
	}
	defer release()
	// One line on every exit of a worker that ran, so a failed or stalled
	// background scan can be told apart from one that never started.
	defer logging.Info(logCtx, "opf scan worker finished")

	cache, err := checkpoint.OPFSpanCacheForRepo(repo)
	if err != nil {
		logging.Warn(logCtx, "opf scan skipped: span cache unavailable", slog.String("error", err.Error()))
		return nil
	}
	if commonDir, cdErr := gitdir.CommonDir(ctx); cdErr == nil {
		if _, pruneErr := checkpoint.PruneOPFSpanCache(commonDir, time.Now(), checkpoint.OPFSpanCacheMaxAge); pruneErr != nil {
			logging.Debug(logCtx, "opf scan: cache prune failed", slog.String("error", pruneErr.Error()))
		}
	}

	ctx = withinOPFScanWorker(ctx)
	var previous []string
	for range opfScanMaxPasses {
		units, collectErr := collectOPFScanUnits(ctx, repo, remote)
		if collectErr != nil {
			logging.Warn(logCtx, "opf scan: could not collect pending checkpoints",
				slog.String("error", collectErr.Error()))
			return nil
		}
		if len(units) == 0 {
			return nil
		}
		ids := opfScanUnitIDs(units)
		if slices.Equal(ids, previous) {
			// The last pass scanned and delivered this exact set and it is
			// still pending, so delivery failed for a reason a retry in this
			// process will not fix. The next user push tries again.
			return nil
		}
		previous = ids

		scanned, scanErr := scanOPFUnits(ctx, repo, units, cache)
		if scanErr != nil {
			logging.Warn(logCtx, "opf scan failed; checkpoints stay held", slog.String("error", scanErr.Error()))
			return nil
		}
		if scanned == 0 {
			// Every pending unit is over a cap; the rewrite would refuse them
			// too, so there is nothing to deliver.
			return nil
		}
		deliverCtx, cancel := context.WithTimeout(ctx, opfScanDeliveryTimeout)
		deliverErr := NewManualCommitStrategy().PrePushFromGitHook(deliverCtx, remote)
		cancel()
		if deliverErr != nil {
			logging.Warn(logCtx, "opf scan: delivery failed; checkpoints stay queued for the next push",
				slog.String("error", deliverErr.Error()))
			return nil
		}
	}
	logging.Warn(logCtx, "opf scan: stopping after the maximum number of passes",
		slog.Int("passes", opfScanMaxPasses))
	return nil
}

// collectOPFScanUnits returns the checkpoint commits that still lack the OPF
// trailer on the primary backend, one unit per queued ref (git-refs) or the
// unpushed v1 chain (git-branch). It loads commits only, no blob content, so a
// long backlog costs little memory here; scanOPFUnits loads content in bounded
// batches.
func collectOPFScanUnits(ctx context.Context, repo *git.Repository, remote string) ([][]*object.Commit, error) {
	var units [][]*object.Commit
	if primaryIsGitRefs(ctx) {
		queue, err := checkpoint.PushQueueForRepo(ctx, repo)
		if err != nil {
			return nil, fmt.Errorf("resolve push queue: %w", err)
		}
		queued, err := queue.Peek()
		if err != nil {
			return nil, fmt.Errorf("peek push queue: %w", err)
		}
		existing, _ := partitionLocalRefs(repo, queued)
		for _, refName := range existing {
			ref, refErr := repo.Reference(refName, true)
			if refErr != nil {
				continue
			}
			chain, _, chainErr := unappliedAncestry(repo, ref.Hash())
			if chainErr != nil {
				return nil, fmt.Errorf("walk ancestry of %s: %w", refName, chainErr)
			}
			if len(chain) > 0 {
				units = append(units, chain)
			}
		}
		return units, nil
	}
	chain, err := v1CommitsAwaitingOPF(ctx, repo, remote)
	if err != nil {
		return nil, err
	}
	if len(chain) > 0 {
		units = append(units, chain)
	}
	return units, nil
}

// opfScanBlobs is the scan seam, swapped in tests to observe batch sizes.
var opfScanBlobs = redact.ScanBlobsWithPrivacyFilter //nolint:gochecknoglobals // batch-size test seam

// scanOPFUnits scans units into the cache in batches whose raw blob bytes stay
// within one unit's raw cap, so the worker's resident input is bounded however
// long the backlog is. Each unit is capped the way the rewrite caps it; a unit
// over a cap is left out and logged, because the rewrite would refuse it anyway
// and scanning pathological content only burns model time. It returns how many
// units were scanned.
func scanOPFUnits(ctx context.Context, repo *git.Repository, units [][]*object.Commit, cache redact.OPFSpanCache) (int, error) {
	logCtx := logging.WithComponent(ctx, opfScanComponent)
	batchLimit := resolveBatchLimit()
	rawCap := rawByteCapForBatchLimit(batchLimit)

	var batch []redact.NamedBlob
	batchRaw, scanned := 0, 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := opfScanBlobs(ctx, batch, cache)
		batch, batchRaw = nil, 0
		return err
	}
	for _, unit := range units {
		unitBlobs, err := collectCommitBlobsForOPF(repo, unit, rawCap)
		if err != nil {
			var rawTooLarge *OPFRawBytesTooLargeError
			if errors.As(err, &rawTooLarge) {
				logging.Warn(logCtx, "opf scan: skipping checkpoint over the raw-byte cap", slog.String("error", err.Error()))
				continue
			}
			return scanned, err
		}
		if leafBytes := redact.SumProseLeafBytes(unitBlobs); leafBytes > batchLimit {
			logging.Warn(logCtx, "opf scan: skipping checkpoint over the prose-leaf cap",
				slog.Int("leaf_bytes", leafBytes), slog.Int("limit", batchLimit))
			continue
		}
		unitRaw := 0
		for _, b := range unitBlobs {
			unitRaw += len(b.Content)
		}
		if batchRaw+unitRaw > rawCap {
			if err := flush(); err != nil {
				return scanned, err
			}
		}
		batch = append(batch, unitBlobs...)
		batchRaw += unitRaw
		scanned++
	}
	return scanned, flush()
}

// v1CommitsAwaitingOPF returns the unpushed v1 commits that lack the OPF
// trailer, oldest first, measured against remote's live v1 tip the same way
// RewriteUnpushedV1WithOPF measures them.
func v1CommitsAwaitingOPF(ctx context.Context, repo *git.Repository, remote string) ([]*object.Commit, error) {
	localTip, err := readV1Tip(repo, plumbing.NewBranchReferenceName(paths.MetadataBranchName))
	if err != nil {
		return nil, fmt.Errorf("read local v1: %w", err)
	}
	if localTip.IsZero() {
		return nil, nil
	}
	ps := resolvePushSettings(ctx, remote)
	remoteTip, err := resolveRemoteV1Tip(ctx, repo, ps.pushTarget())
	if err != nil {
		return nil, fmt.Errorf("read remote v1: %w", err)
	}
	unpushed, err := listUnpushedV1Commits(repo, localTip, remoteTip)
	if err != nil {
		return nil, fmt.Errorf("list unpushed v1 commits: %w", err)
	}
	pending := unpushed[:0]
	for _, c := range unpushed {
		if !trailers.HasOPFApplied(c.Message) {
			pending = append(pending, c)
		}
	}
	return pending, nil
}

func collectCommitBlobsForOPF(repo *git.Repository, commits []*object.Commit, rawCap int) ([]redact.NamedBlob, error) {
	budget := newOPFRawByteBudget(rawCap)
	var blobs []redact.NamedBlob
	var treePaths []string
	for _, c := range commits {
		tree, err := repo.TreeObject(c.TreeHash)
		if err != nil {
			return nil, fmt.Errorf("load tree for %s: %w", c.Hash.String()[:7], err)
		}
		if err := collectTreeBlobsWithinBudget(repo, tree, "", &blobs, &treePaths, budget); err != nil {
			return nil, fmt.Errorf("collect blobs %s: %w", c.Hash.String()[:7], err)
		}
	}
	return blobs, nil
}

// opfScanUnitIDs identifies a pass's pending work by its commits, so a pass
// that finds exactly the work the previous one delivered can stop.
func opfScanUnitIDs(units [][]*object.Commit) []string {
	var ids []string
	for _, unit := range units {
		for _, c := range unit {
			ids = append(ids, c.Hash.String())
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// acquireOPFScanWorkerLock takes the per-repo worker lock. held reports that
// another worker has it. Any other failure returns a no-op release with held
// false: running unguarded is never worse than two workers racing, which the
// ref CAS already makes safe.
func acquireOPFScanWorkerLock(ctx context.Context) (release func(), held bool) {
	noop := func() {}
	commonDir, err := gitdir.CommonDir(ctx)
	if err != nil {
		return noop, false
	}
	root, err := gitdir.OpenAt(commonDir)
	if err != nil {
		return noop, false
	}
	if err := osroot.MkdirAllNoSymlink(root, path.Dir(opfScanWorkerLockName), 0o750); err != nil {
		return noop, false
	}
	lockCtx, cancel := context.WithTimeout(ctx, opfScanWorkerLockWait)
	defer cancel()
	release, err = flock.AcquireContextIn(lockCtx, root, opfScanWorkerLockName)
	if err != nil {
		return noop, errors.Is(err, context.DeadlineExceeded)
	}
	return release, false
}

// opfPendingV1WalkLimit bounds the v1 history walk behind CheckpointsAwaitingOPF,
// which runs on every `entire status`.
const opfPendingV1WalkLimit = 1000

// CheckpointsAwaitingOPF counts checkpoint commits on the primary backend that
// still lack the OPF trailer and have not been pushed: queued refs on git-refs,
// unpushed v1 commits on git-branch. It reads local refs and the push queue
// only, so it is safe on a read-only status path. `entire status` reports it,
// because with scanning in a detached worker that is the one place held work
// is visible.
func CheckpointsAwaitingOPF(ctx context.Context, repo *git.Repository) (int, error) {
	if primaryIsGitRefs(ctx) {
		queue, err := checkpoint.PushQueueForRepo(ctx, repo)
		if err != nil {
			return 0, fmt.Errorf("resolve push queue: %w", err)
		}
		queued, err := queue.Peek()
		if err != nil {
			return 0, fmt.Errorf("peek push queue: %w", err)
		}
		existing, _ := partitionLocalRefs(repo, queued)
		n := 0
		for _, refName := range existing {
			ref, refErr := repo.Reference(refName, true)
			if refErr != nil {
				continue
			}
			commit, commitErr := repo.CommitObject(ref.Hash())
			if commitErr == nil && !trailers.HasOPFApplied(commit.Message) {
				n++
			}
		}
		return n, nil
	}
	return v1CommitsAwaitingOPFLocally(repo)
}

// v1CommitsAwaitingOPFLocally counts v1 commits from the local tip back to the
// first one that carries the OPF trailer or that some remote-tracking v1 ref
// already names, without contacting any remote.
func v1CommitsAwaitingOPFLocally(repo *git.Repository) (int, error) {
	localTip, err := readV1Tip(repo, plumbing.NewBranchReferenceName(paths.MetadataBranchName))
	if err != nil {
		return 0, fmt.Errorf("read local v1: %w", err)
	}
	if localTip.IsZero() {
		return 0, nil
	}
	pushed := make(map[plumbing.Hash]struct{})
	refs, err := repo.References()
	if err != nil {
		return 0, fmt.Errorf("list references: %w", err)
	}
	suffix := "/" + paths.MetadataBranchName
	_ = refs.ForEach(func(r *plumbing.Reference) error { //nolint:errcheck // callback never fails
		if name := r.Name(); name.IsRemote() && strings.HasSuffix(name.String(), suffix) {
			pushed[r.Hash()] = struct{}{}
		}
		return nil
	})
	iter, err := repo.Log(&git.LogOptions{From: localTip})
	if err != nil {
		return 0, fmt.Errorf("log local v1: %w", err)
	}
	defer iter.Close()
	n := 0
	walkErr := iter.ForEach(func(c *object.Commit) error {
		if _, ok := pushed[c.Hash]; ok || trailers.HasOPFApplied(c.Message) || n >= opfPendingV1WalkLimit {
			return errStop
		}
		n++
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errStop) {
		return 0, fmt.Errorf("walk local v1: %w", walkErr)
	}
	return n, nil
}

// opfGitRefsHintInterval spaces out the git-refs suggestion below so it reads
// as a tip, not as a warning on every push.
const opfGitRefsHintInterval = 30 * 24 * time.Hour

const opfGitRefsHint = "[entire] Tip: with the OpenAI Privacy Filter on, the git-refs checkpoint backend " +
	"ships each checkpoint as soon as it is scanned instead of the whole branch at once. " +
	"Switch with `entire doctor migrate-checkpoints`."

// maybeHintGitRefsForOPF suggests the git-refs backend to a git-branch user
// whose checkpoints were just held for a scan, at most once per
// opfGitRefsHintInterval per repository. git-branch is fully supported; the
// hint only points at the backend where one slow checkpoint cannot hold back
// the rest.
func maybeHintGitRefsForOPF(ctx context.Context) {
	if inOPFScanWorker(ctx) {
		return
	}
	commonDir, err := gitdir.CommonDir(ctx)
	if err != nil {
		return
	}
	if spawnmarker.RecentlySpawned(commonDir, "opf-git-refs-hint", opfGitRefsHintInterval, time.Now()) {
		return
	}
	fmt.Fprintln(stderrWriter, opfGitRefsHint)
}

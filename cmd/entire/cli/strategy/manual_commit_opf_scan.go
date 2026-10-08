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
// the worker or a worker is running. A running worker picks up the new work: it
// re-collects after every pass and once more after releasing its lock (see
// RunOPFScan). Callers invoke it only when the push resolved OPFRun and
// something was held back as not yet scanned.
//
// Gating on the lock rather than on time since the last spawn means a push
// right after a worker finished always gets a new one. The cost is that a
// persistently failing OPF runtime is retried on every push.
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
	release, held := acquireOPFScanWorkerLock(ctx)
	release()
	if held {
		return
	}
	opfScanSpawn(root, remote)
	logging.Info(logCtx, "checkpoints held for OPF; spawned background scan", slog.String("remote", remote))
}

// RunOPFScan is the body of the detached `entire __opf_scan <remote>` worker.
// Each pass collects the content waiting for OPF, scans what the span cache
// lacks, and runs the pre-push delivery for remote after each scan batch. It
// stops when nothing new is pending, or after opfScanMaxPasses.
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
	// One line on every exit of a worker that ran, so a failed or stalled
	// background scan can be told apart from one that never started.
	defer logging.Info(logCtx, "opf scan worker finished")

	cache, err := checkpoint.OPFSpanCacheForRepo(repo)
	if err != nil {
		release()
		logging.Warn(logCtx, "opf scan skipped: span cache unavailable", slog.String("error", err.Error()))
		return nil
	}
	if commonDir, cdErr := gitdir.CommonDir(ctx); cdErr == nil {
		if _, pruneErr := checkpoint.PruneOPFSpanCache(commonDir, time.Now(), checkpoint.OPFSpanCacheMaxAge); pruneErr != nil {
			logging.Debug(logCtx, "opf scan: cache prune failed", slog.String("error", pruneErr.Error()))
		}
	}

	ctx = withinOPFScanWorker(ctx)
	w := &opfScanWorker{remote: remote, repo: repo, cache: cache, seen: make(map[string]struct{})}
	for {
		w.runPasses(ctx)
		opfScanBeforeRelease()
		release()
		// A push that held new work while this worker held the lock saw it
		// held and spawned nothing, so look once more now that it is free. Only
		// work this process has not already tried brings it back, so units it
		// could not scan or deliver do not make it spin.
		if w.passes >= opfScanMaxPasses || !w.hasUnseenWork(ctx) {
			return nil
		}
		if release, held = acquireOPFScanWorkerLock(ctx); held {
			return nil // another worker took it and owns the new work
		}
	}
}

// opfScanBeforeRelease runs after a worker's passes and before it releases the
// lock: the window in which a push holds new work but sees a worker running.
var opfScanBeforeRelease = func() {} //nolint:gochecknoglobals // exit-race test seam

// opfScanWorker carries one worker process's state across its passes and
// across re-acquiring the lock.
type opfScanWorker struct {
	remote string
	repo   *git.Repository
	cache  redact.OPFSpanCache
	// seen holds the IDs of every commit a pass has already worked on.
	seen   map[string]struct{}
	passes int
}

// runPasses collects, scans and delivers until nothing new is pending, a pass
// fails, or the pass budget runs out. Each pass delivers after every scan
// batch, so a checkpoint ships about one model call after it is scanned rather
// than after the whole backlog.
func (w *opfScanWorker) runPasses(ctx context.Context) {
	logCtx := logging.WithComponent(ctx, opfScanComponent)
	for ; w.passes < opfScanMaxPasses; w.passes++ {
		units, collectErr := collectOPFScanUnits(ctx, w.repo, w.remote)
		if collectErr != nil {
			logging.Warn(logCtx, "opf scan: could not collect pending checkpoints",
				slog.String("error", collectErr.Error()))
			return
		}
		if !w.markSeen(units) {
			// Everything still pending was already scanned and delivered by
			// this process, so delivery failed for a reason a retry here will
			// not fix. The next user push tries again.
			return
		}
		scanned, scanErr := scanOPFUnits(ctx, w.repo, units, w.cache, w.deliver)
		if scanErr != nil {
			logging.Warn(logCtx, "opf scan stopped; checkpoints stay held", slog.String("error", scanErr.Error()))
			return
		}
		if scanned == 0 {
			// Every pending unit is over a cap; the rewrite would refuse them
			// too, so there is nothing to deliver.
			return
		}
	}
	logging.Warn(logCtx, "opf scan: stopping after the maximum number of passes",
		slog.Int("passes", opfScanMaxPasses))
}

// deliver runs the pre-push delivery for the worker's remote, which rewrites
// from the cache and pushes whatever is now covered.
func (w *opfScanWorker) deliver(ctx context.Context) error {
	deliverCtx, cancel := context.WithTimeout(ctx, opfScanDeliveryTimeout)
	defer cancel()
	if err := NewManualCommitStrategy().PrePushFromGitHook(deliverCtx, w.remote); err != nil {
		return fmt.Errorf("delivery failed; checkpoints stay queued for the next push: %w", err)
	}
	return nil
}

// markSeen records units' commits and reports whether any was new.
func (w *opfScanWorker) markSeen(units [][]*object.Commit) bool {
	added := false
	for _, id := range opfScanUnitIDs(units) {
		if _, ok := w.seen[id]; !ok {
			w.seen[id] = struct{}{}
			added = true
		}
	}
	return added
}

// hasUnseenWork reports whether anything pending is new to this process.
func (w *opfScanWorker) hasUnseenWork(ctx context.Context) bool {
	units, err := collectOPFScanUnits(ctx, w.repo, w.remote)
	if err != nil {
		return false
	}
	for _, id := range opfScanUnitIDs(units) {
		if _, ok := w.seen[id]; !ok {
			return true
		}
	}
	return false
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
		bootstrapLimit := resolveBootstrapLimit()
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
			if len(chain) > bootstrapLimit {
				// The rewrite refuses it (BootstrapTooLargeError), so scanning
				// it would only burn model time.
				logging.Warn(logging.WithComponent(ctx, opfScanComponent), "opf scan: skipping checkpoint over the bootstrap limit",
					slog.String("ref", refName.String()), slog.Int("commits", len(chain)), slog.Int("limit", bootstrapLimit))
				continue
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
// long the backlog is, and whose prose stays within about one model call, so
// deliver runs after each batch and scanned checkpoints ship without waiting
// for the rest of the backlog. Each unit is capped the way the rewrite caps it; a unit
// over a cap is left out and logged, because the rewrite would refuse it anyway
// and scanning pathological content only burns model time. It returns how many
// units were scanned.
func scanOPFUnits(ctx context.Context, repo *git.Repository, units [][]*object.Commit, cache redact.OPFSpanCache, deliver func(context.Context) error) (int, error) {
	logCtx := logging.WithComponent(ctx, opfScanComponent)
	batchLimit := resolveBatchLimit()
	rawCap := rawByteCapForBatchLimit(batchLimit)

	var batch []redact.NamedBlob
	batchRaw, batchLeaf, scanned := 0, 0, 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := opfScanBlobs(ctx, batch, cache)
		batch, batchRaw, batchLeaf = nil, 0, 0
		if err != nil {
			return err
		}
		return deliver(ctx)
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
		leafBytes := redact.SumProseLeafBytes(unitBlobs)
		if leafBytes > batchLimit {
			logging.Warn(logCtx, "opf scan: skipping checkpoint over the prose-leaf cap",
				slog.Int("leaf_bytes", leafBytes), slog.Int("limit", batchLimit))
			continue
		}
		unitRaw := 0
		for _, b := range unitBlobs {
			unitRaw += len(b.Content)
		}
		if batchRaw+unitRaw > rawCap || batchLeaf+leafBytes > redact.OPFBatchChunkBytes {
			if err := flush(); err != nil {
				return scanned, err
			}
		}
		batch = append(batch, unitBlobs...)
		batchRaw += unitRaw
		batchLeaf += leafBytes
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
	if limit := resolveBootstrapLimit(); remoteTip.IsZero() && len(unpushed) > limit {
		// The rewrite refuses this bootstrap (BootstrapTooLargeError), so
		// scanning it would only burn model time.
		logging.Warn(logging.WithComponent(ctx, opfScanComponent), "opf scan: skipping v1 over the bootstrap limit",
			slog.Int("commits", len(unpushed)), slog.Int("limit", limit))
		return nil, nil
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

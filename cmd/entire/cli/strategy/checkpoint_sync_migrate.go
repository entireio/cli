package strategy

// Checkpoint sync migration engine: the primitives `entire checkpoint migrate`
// composes to move a repository's checkpoint artifacts — per-checkpoint refs
// (refs/entire/checkpoints/<shard>/<id>) and the legacy entire/checkpoints/v1
// branch — from one remote to another and then retire the old copies.
//
// Every function here runs from a foreground command in the user's own working
// directory. That is why none of the git subprocesses set
// gitrepo.EnvWithoutRepoOverrides: the rule (CLAUDE.md, "git status Is a
// Write") exists for subprocesses that can run inside a git hook, where git's
// exported GIT_DIR/GIT_WORK_TREE would silently retarget cmd.Dir. Here a GIT_DIR
// the user exported is an instruction, not contamination. If any of these
// functions is ever wired into a hook, note that remote.Fetch, remote.PushWithOptions
// and remote.LsRemoteInDir do not scrub the environment either.
//
// HydrateCheckpointArtifacts is the one sanctioned place that advances local
// checkpoint refs from a remote that is NOT the elected sync remote. The
// "only the elected remote may seed or advance local refs" rule
// (CheckpointReadRemotes) protects automatic read paths from a stale legacy tier
// silently replaying onto local state during a hook or a resume. Hydration is
// the opposite situation: the user asked for it, the source is the remote being
// migrated FROM and about to have its copies deleted — so pulling everything it
// holds into local refs is what makes the deletion safe — and promotion runs
// through the same non-destructive SafelyAdvanceLocalRef (ahead → untouched,
// diverged → replay). The fetch itself lands in a temporary namespace so no
// canonical ref is ever written directly by git.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

const (
	// migrateRemoteTimeout bounds each network step of a foreground migration
	// (one ls-remote, one fetch chunk, one delete chunk). Same budget as the
	// other foreground checkpoint fetches: a checkpoint archive is the largest
	// thing Entire ever transfers, and a too-small bound fails the same way on
	// every retry with nothing to show for it.
	migrateRemoteTimeout = checkpointRemoteForegroundFetchTimeout

	// refChunkSize bounds the refspecs handed to one git invocation. A
	// checkpoint refspec ("refs/entire/checkpoints/xx/<id>:refs/entire/…") is
	// about 110 bytes and macOS caps a single argv at 256 KiB, so an unbounded
	// batch of a few thousand refs — the shape a migrated backlog has — overruns
	// it. 200 keeps a chunk under 25 KiB.
	refChunkSize = 200

	// syncTmpRefPrefix is the namespace hydration fetches into before promoting
	// through SafelyAdvanceLocalRef. Distinct from FetchTmpRefPrefix so a
	// concurrent metadata fetch never collides with a migration in progress.
	syncTmpRefPrefix = "refs/entire-sync-tmp/"
)

// v1BranchRef is the full ref name of the legacy shared checkpoint branch.
var v1BranchRef = plumbing.NewBranchReferenceName(paths.MetadataBranchName)

// ProgressFunc receives human-readable progress lines for a foreground spinner.
// Callers may pass nil.
type ProgressFunc func(format string, args ...any)

func (p ProgressFunc) report(format string, args ...any) {
	if p != nil {
		p(format, args...)
	}
}

// CheckpointInventory is what one remote holds of checkpoint data.
type CheckpointInventory struct {
	Remote string
	// Refs maps each refs/entire/checkpoints/<shard>/<id> ref to the hash the
	// remote advertises for it.
	Refs map[plumbing.ReferenceName]plumbing.Hash
	// V1Branch is the advertised tip of refs/heads/entire/checkpoints/v1, zero
	// when the remote has no such branch.
	V1Branch plumbing.Hash
}

// Empty reports whether the remote holds no checkpoint artifacts at all.
func (inv CheckpointInventory) Empty() bool {
	return len(inv.Refs) == 0 && inv.V1Branch.IsZero()
}

// RefNames returns the checkpoint ref names in sorted order.
func (inv CheckpointInventory) RefNames() []plumbing.ReferenceName {
	names := make([]plumbing.ReferenceName, 0, len(inv.Refs))
	for name := range inv.Refs {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

// HydrateResult summarizes what HydrateCheckpointArtifacts changed locally.
type HydrateResult struct {
	// RefsFetched is how many remote refs were fetched because the local ref
	// was absent or at a different hash.
	RefsFetched int
	// RefsAdvanced is how many local refs were set or fast-forwarded to the
	// remote tip.
	RefsAdvanced int
	// RefsReplayed is how many diverged local refs had their local-only commits
	// replayed onto the remote tip (SafelyAdvanceLocalRef's divergence path).
	RefsReplayed int
	// V1Advanced reports that the local v1 branch was set, fast-forwarded, or
	// replayed onto the remote's v1 tip.
	V1Advanced bool
}

// RemoveResult summarizes what RemoveCheckpointArtifacts deleted remotely.
type RemoveResult struct {
	RefsDeleted   int
	V1Deleted     bool
	AlreadyAbsent int
}

// InventoryCheckpointArtifacts lists the checkpoint refs and the v1 branch that
// remoteName currently advertises, with one `git ls-remote` and no object
// transfer. The remote is addressed by NAME so git applies url.<base>.insteadOf
// and dispatches the entire:// helper itself; ls-remote runs in the worktree
// root so repo-local git config applies.
func InventoryCheckpointArtifacts(ctx context.Context, remoteName string) (CheckpointInventory, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return CheckpointInventory{}, fmt.Errorf("resolve worktree root: %w", err)
	}
	return inventoryCheckpointArtifactsIn(ctx, worktreeRoot, remoteName)
}

func inventoryCheckpointArtifactsIn(ctx context.Context, dir, target string) (CheckpointInventory, error) {
	ctx, cancel := context.WithTimeout(ctx, migrateRemoteTimeout)
	defer cancel()

	out, err := remote.LsRemoteInDir(ctx, dir, target, checkpoint.CheckpointRefPrefix+"*", v1BranchRef.String())
	if err != nil {
		return CheckpointInventory{}, fmt.Errorf("list checkpoint artifacts on %s: %w", remote.RedactURLOrPath(target), err)
	}
	inv := CheckpointInventory{Remote: target, Refs: checkpoint.ParseLsRemoteRefs(out)}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == v1BranchRef.String() && plumbing.IsHash(fields[0]) {
			inv.V1Branch = plumbing.NewHash(fields[0])
		}
	}
	return inv, nil
}

// HydrateCheckpointArtifacts brings everything inv (the inventory of remote
// from) holds into this repository's local refs: checkpoint refs absent locally
// or at a different hash, and the v1 branch when the remote has one. Each ref
// is fetched into syncTmpRefPrefix (chunked, unfiltered, no tags), promoted onto
// its canonical name through SafelyAdvanceLocalRef, and the temporary ref is
// removed. See the file header for why advancing from a non-elected remote is
// sanctioned here.
//
// The fetch is unfiltered on purpose: a --filter=blob:none fetch leaves refs
// whose blobs are missing, and the ref's existence then suppresses every later
// recovery (see the doc on fetchMetadataBranchIfMissing).
func HydrateCheckpointArtifacts(ctx context.Context, repo *git.Repository, from string, inv CheckpointInventory, progress ProgressFunc) (HydrateResult, error) {
	var result HydrateResult
	repoPath, err := getRepoPath(repo)
	if err != nil {
		return result, err
	}

	candidates := hydrationCandidates(repo, inv)
	if len(candidates) == 0 && inv.V1Branch.IsZero() {
		return result, nil
	}

	for start := 0; start < len(candidates); start += refChunkSize {
		chunk := candidates[start:min(start+refChunkSize, len(candidates))]
		progress.report("Fetching checkpoint refs from %s (%d/%d)", from, start+len(chunk), len(candidates))

		refSpecs := make([]string, 0, len(chunk))
		for _, ref := range chunk {
			refSpecs = append(refSpecs, "+"+ref.String()+":"+syncTmpRef(ref).String())
		}
		if err := fetchChunk(ctx, repoPath, from, refSpecs); err != nil {
			return result, fmt.Errorf("fetch %d checkpoint refs from %s: %w", len(chunk), from, err)
		}
		result.RefsFetched += len(chunk)

		for _, ref := range chunk {
			advanced, replayed, err := promoteSyncTmpRef(ctx, repo, ref, inv.Refs[ref])
			if err != nil {
				return result, err
			}
			if replayed {
				result.RefsReplayed++
			} else if advanced {
				result.RefsAdvanced++
			}
		}
	}

	if !inv.V1Branch.IsZero() {
		primary := checkpoint.ResolveRefs(ctx).Primary
		if local, err := repo.Reference(primary, true); err != nil || local.Hash() != inv.V1Branch {
			progress.report("Fetching %s from %s", paths.MetadataBranchName, from)
			tmp := syncTmpRef(v1BranchRef)
			if err := fetchChunk(ctx, repoPath, from, []string{"+" + v1BranchRef.String() + ":" + tmp.String()}); err != nil {
				return result, fmt.Errorf("fetch %s from %s: %w", paths.MetadataBranchName, from, err)
			}
			advanced, replayed, err := promoteTmpRef(ctx, repo, tmp, primary, inv.V1Branch)
			if err != nil {
				return result, err
			}
			result.V1Advanced = advanced || replayed
		}
	}
	return result, nil
}

// hydrationCandidates returns the inventory refs whose local counterpart is
// absent or points elsewhere, in sorted order.
func hydrationCandidates(repo *git.Repository, inv CheckpointInventory) []plumbing.ReferenceName {
	var candidates []plumbing.ReferenceName
	for _, ref := range inv.RefNames() {
		local, err := repo.Reference(ref, true)
		if err != nil || local.Hash() != inv.Refs[ref] {
			candidates = append(candidates, ref)
		}
	}
	return candidates
}

func syncTmpRef(ref plumbing.ReferenceName) plumbing.ReferenceName {
	return plumbing.ReferenceName(syncTmpRefPrefix + strings.TrimPrefix(ref.String(), "refs/"))
}

func fetchChunk(ctx context.Context, dir, from string, refSpecs []string) error {
	ctx, cancel := context.WithTimeout(ctx, migrateRemoteTimeout)
	defer cancel()
	out, err := remote.Fetch(ctx, remote.FetchOptions{
		Remote:   from,
		RefSpecs: refSpecs,
		NoTags:   true,
		NoFilter: true,
		Dir:      dir,
	})
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// promoteSyncTmpRef promotes the temporary copy of ref onto ref itself.
func promoteSyncTmpRef(ctx context.Context, repo *git.Repository, ref plumbing.ReferenceName, remoteHash plumbing.Hash) (advanced, replayed bool, err error) {
	return promoteTmpRef(ctx, repo, syncTmpRef(ref), ref, remoteHash)
}

// promoteTmpRef advances dest to the commit tmp points at via
// SafelyAdvanceLocalRef, then removes tmp. It reports whether dest moved, and
// whether the move was a replay (dest ended somewhere other than the remote
// hash, which is what divergence recovery produces) rather than a set or
// fast-forward.
func promoteTmpRef(ctx context.Context, repo *git.Repository, tmp, dest plumbing.ReferenceName, remoteHash plumbing.Hash) (advanced, replayed bool, err error) {
	defer func() { _ = repo.Storer.RemoveReference(tmp) }() //nolint:errcheck // cleanup is best-effort

	tmpRef, err := repo.Reference(tmp, true)
	if err != nil {
		return false, false, fmt.Errorf("%s not found after fetch (tmp ref %s missing): %w", dest, tmp, err)
	}
	var before plumbing.Hash
	if local, localErr := repo.Reference(dest, true); localErr == nil {
		before = local.Hash()
	}
	if err := SafelyAdvanceLocalRef(ctx, repo, dest, tmpRef.Hash()); err != nil {
		return false, false, fmt.Errorf("advance local %s: %w", dest, err)
	}
	after, err := repo.Reference(dest, true)
	if err != nil {
		return false, false, fmt.Errorf("read %s after advance: %w", dest, err)
	}
	if after.Hash() == before {
		return false, false, nil
	}
	if after.Hash() == remoteHash || after.Hash() == tmpRef.Hash() {
		return true, false, nil
	}
	logging.Info(ctx, "checkpoint sync: diverged local ref replayed onto remote tip",
		slog.String("ref", dest.String()),
		slog.String("remote_hash", remoteHash.String()),
		slog.String("replayed_tip", after.Hash().String()))
	return true, true, nil
}

// RequeueAllCheckpointRefs enqueues every local refs/entire/checkpoints/* ref so
// the next flush publishes the whole backlog, not just the refs the hook
// recorded since the last push. Returns the number of refs enqueued; Drain
// collapses duplicates with anything already queued.
func RequeueAllCheckpointRefs(ctx context.Context, repo *git.Repository) (int, error) {
	refs, err := localCheckpointRefs(repo)
	if err != nil {
		return 0, err
	}
	if len(refs) == 0 {
		return 0, nil
	}
	queue, err := checkpoint.PushQueueForRepo(ctx, repo)
	if err != nil {
		return 0, fmt.Errorf("resolve push queue: %w", err)
	}
	if err := queue.EnqueueAll(refs); err != nil {
		return 0, fmt.Errorf("enqueue %d checkpoint refs: %w", len(refs), err)
	}
	return len(refs), nil
}

// localCheckpointRefs lists the well-formed checkpoint refs in repo, sorted.
func localCheckpointRefs(repo *git.Repository) ([]plumbing.ReferenceName, error) {
	iter, err := repo.References()
	if err != nil {
		return nil, fmt.Errorf("list local refs: %w", err)
	}
	var refs []plumbing.ReferenceName
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if _, ok := checkpoint.ParseRef(ref.Name()); ok {
			refs = append(refs, ref.Name())
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk local refs: %w", err)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i] < refs[j] })
	return refs, nil
}

// VerifyCheckpointArtifacts lists newRemote once and partitions expected into
// the refs it advertises at the LOCAL hash (verified) and everything else
// (missing: absent remotely, at a different hash, or absent locally). Publishing
// is fast-forward-only and hydration already folded the old remote's tips into
// the local refs, so "remote == local" is the condition under which the old
// copy is fully represented on the new remote and safe to delete.
func VerifyCheckpointArtifacts(ctx context.Context, repo *git.Repository, newRemote string, expected []plumbing.ReferenceName) (verified, missing []plumbing.ReferenceName, err error) {
	repoPath, err := getRepoPath(repo)
	if err != nil {
		return nil, nil, err
	}
	inv, err := inventoryCheckpointArtifactsIn(ctx, repoPath, newRemote)
	if err != nil {
		return nil, nil, err
	}
	for _, ref := range expected {
		local, localErr := repo.Reference(ref, true)
		remoteHash, onRemote := inv.Refs[ref]
		if localErr == nil && onRemote && remoteHash == local.Hash() {
			verified = append(verified, ref)
		} else {
			missing = append(missing, ref)
		}
	}
	return verified, missing, nil
}

// RemoveCheckpointArtifacts deletes refs (and, when deleteV1, the
// entire/checkpoints/v1 branch) from oldRemote in chunks, then prunes the stale
// local remote-tracking ref of the v1 branch so tracking-ref readers stop
// seeing a branch the remote no longer has. Only pass refs that
// VerifyCheckpointArtifacts reported as verified.
//
// The remote is resolved to its single push URL and the delete is aimed at the
// URL, never the name: remote.PushWithOptions rewrites a remote NAME to the
// dedicated checkpoint_remote URL when one is configured, and a delete
// redirected there would empty the wrong repository. A remote whose push fans
// out to several URLs is refused — there is no single "old copy" to retire.
// Refs the remote no longer has count as AlreadyAbsent, not as failures
// (remote.DeleteRefs lists before deleting, so the count is exact).
func RemoveCheckpointArtifacts(ctx context.Context, oldRemote string, refs []plumbing.ReferenceName, deleteV1 bool, progress ProgressFunc) (RemoveResult, error) {
	var result RemoveResult
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return result, fmt.Errorf("resolve worktree root: %w", err)
	}
	pushURLs, err := gitremote.GetPushURLs(ctx, oldRemote)
	if err != nil {
		return result, fmt.Errorf("resolve push URL of %q: %w", oldRemote, err)
	}
	if len(pushURLs) != 1 {
		return result, fmt.Errorf("remote %q pushes to %d URLs; checkpoint artifacts can only be removed from a remote with a single push URL", oldRemote, len(pushURLs))
	}
	target := pushURLs[0]

	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.String())
	}
	for start := 0; start < len(names); start += refChunkSize {
		chunk := names[start:min(start+refChunkSize, len(names))]
		progress.report("Removing checkpoint refs from %s (%d/%d)", oldRemote, start+len(chunk), len(names))
		absent, err := deleteRefsChunk(ctx, target, worktreeRoot, chunk)
		if err != nil {
			return result, fmt.Errorf("delete %d checkpoint refs from %s: %w", len(chunk), oldRemote, err)
		}
		result.AlreadyAbsent += len(absent)
		result.RefsDeleted += len(chunk) - len(absent)
	}

	if deleteV1 {
		progress.report("Removing %s from %s", paths.MetadataBranchName, oldRemote)
		absent, err := deleteRefsChunk(ctx, target, worktreeRoot, []string{v1BranchRef.String()})
		if err != nil {
			return result, fmt.Errorf("delete %s from %s: %w", paths.MetadataBranchName, oldRemote, err)
		}
		result.V1Deleted = len(absent) == 0
		pruneRemoteTrackingRef(ctx, worktreeRoot, oldRemote)
	}
	return result, nil
}

func deleteRefsChunk(ctx context.Context, target, dir string, refs []string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, migrateRemoteTimeout)
	defer cancel()
	return remote.DeleteRefs(ctx, target, dir, refs) //nolint:wrapcheck // callers add the remote and count
}

// pruneRemoteTrackingRef drops refs/remotes/<remote>/entire/checkpoints/v1
// after the branch was deleted remotely. Best-effort: the git CLI is used
// because go-git's RemoveReference does not persist with packed refs, and a
// ref that is already absent exits non-zero harmlessly.
func pruneRemoteTrackingRef(ctx context.Context, dir, remoteName string) {
	tracking := plumbing.NewRemoteReferenceName(remoteName, paths.MetadataBranchName)
	cmd := exec.CommandContext(ctx, "git", "update-ref", "-d", tracking.String())
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || !strings.Contains(string(out), "not exist") {
			logging.Debug(ctx, "checkpoint sync: could not prune remote-tracking ref",
				slog.String("ref", tracking.String()),
				slog.String("error", strings.TrimSpace(string(out))))
		}
	}
}

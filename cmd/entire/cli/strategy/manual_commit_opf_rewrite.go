// Pre-push OPF rewrite for entire/checkpoints/v1.
//
// This is the ONLY production code path that runs the OPF-augmented
// redaction entry points. Post-commit condensation stays on 7-layer
// for predictable latency; OPF runs here, once per push, after the
// user opted in via settings.
package strategy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage"
)

// V1DivergedError: local entire/checkpoints/v1 has commits that aren't
// ancestors of the remote tip (force-push or another machine pushed).
// Rewriting under divergence would silently rebase rejected work, so
// we refuse.
type V1DivergedError struct {
	Local, Remote, MergeBase plumbing.Hash
}

func (e *V1DivergedError) Error() string {
	return fmt.Sprintf("entire/checkpoints/v1 has diverged from remote (local=%s remote=%s merge_base=%s); "+
		"fetch the remote and either reset entire/checkpoints/v1 to <remote>/entire/checkpoints/v1 "+
		"or run `entire doctor --recover-v1` before pushing",
		e.Local.String()[:7], e.Remote.String()[:7], e.MergeBase.String()[:7])
}

// BootstrapTooLargeError: first push to a remote with no v1 yet, but
// more unpushed commits than the safety cap. OPF inference is ~30s per
// commit, so unbounded bootstraps could take hours.
type BootstrapTooLargeError struct {
	Count, Limit int
}

func (e *BootstrapTooLargeError) Error() string {
	return fmt.Sprintf("OPF bootstrap would rewrite %d entire/checkpoints/v1 commits "+
		"(limit %d). Set ENTIRE_OPF_BOOTSTRAP_LIMIT=<N> or =unlimited to override, "+
		"or push without OPF (ENTIRE_OPF=no git push) to bring the remote into sync first",
		e.Count, e.Limit)
}

// V1RefMovedError: another worktree advanced the local ref during our
// rewrite (CAS conflict). Orphan rewritten objects sit in .git/objects
// until git gc --prune; no manual cleanup needed.
type V1RefMovedError struct {
	Expected, Actual plumbing.Hash
}

func (e *V1RefMovedError) Error() string {
	return fmt.Sprintf("entire/checkpoints/v1 moved during OPF rewrite "+
		"(expected %s, found %s); another local worktree advanced the ref "+
		"mid-rewrite — re-run `git push` (no fetch needed; the move was local)",
		e.Expected.String()[:7], e.Actual.String()[:7])
}

// OPFRuntimeFailedError: the OPF circuit breaker tripped mid-rewrite.
// Some blobs were silently downgraded to 7-layer; tagging those commits
// as Entire-OPF-Applied would be a privacy regression (future pushes
// would skip them while their content is 7-layer-only). Abort before
// CAS so the user fixes their OPF install and retries.
type OPFRuntimeFailedError struct {
	OPFCommand string
}

func (e *OPFRuntimeFailedError) Error() string {
	return fmt.Sprintf("OPF runtime failed during pre-push rewrite (command=%q); "+
		"aborting push so 7-layer content isn't tagged as 8-layer-applied. "+
		"Run `%s --help` to verify your OPF install, then retry. Or set "+
		"ENTIRE_OPF=no on the push to skip OPF for this push only.",
		e.OPFCommand, e.OPFCommand)
}

const (
	// bootstrapDefaultLimit caps first-push history rewrites. Picked
	// to bound worst-case wall-clock at ~50min @ 30s/commit.
	bootstrapDefaultLimit = 100
	bootstrapEnvVar       = "ENTIRE_OPF_BOOTSTRAP_LIMIT"
)

func resolveBootstrapLimit() int {
	v := strings.TrimSpace(os.Getenv(bootstrapEnvVar))
	switch {
	case v == "":
		return bootstrapDefaultLimit
	case strings.EqualFold(v, "unlimited"):
		return math.MaxInt
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return bootstrapDefaultLimit
}

// OPFBatchTooLargeError: a single push has more prose-leaf content
// than ENTIRE_OPF_BATCH_LIMIT will allow OPF to chew through in one
// inference call. Pushing under the limit yields a single ~10-30s
// pause; without the cap, a 100MB-of-prose push could take an hour.
//
// The user-facing remediation is identical in shape to
// BootstrapTooLargeError: bump the limit, push without OPF, or break
// the push into smaller pieces.
type OPFBatchTooLargeError struct {
	LeafBytes int
	Limit     int
}

func (e *OPFBatchTooLargeError) Error() string {
	return fmt.Sprintf("OPF would run inference on %d prose-leaf bytes "+
		"(limit %d). Set ENTIRE_OPF_BATCH_LIMIT=<bytes> or =unlimited to override, "+
		"or push without OPF (ENTIRE_OPF=no git push) and let a smaller follow-up push run OPF",
		e.LeafBytes, e.Limit)
}

const (
	// batchDefaultLimit caps the cumulative prose-leaf bytes one push
	// will hand to OPF. 2 MB at ~5.4s/100KB ≈ ~110s of inference, on
	// the high end of what's acceptable as a single push pause but
	// generous enough that any realistic push fits.
	batchDefaultLimit = 2 * 1024 * 1024
	batchEnvVar       = "ENTIRE_OPF_BATCH_LIMIT"
)

func resolveBatchLimit() int {
	v := strings.TrimSpace(os.Getenv(batchEnvVar))
	switch {
	case v == "":
		return batchDefaultLimit
	case strings.EqualFold(v, "unlimited"):
		return math.MaxInt
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return batchDefaultLimit
}

// scaleBatchLimit multiplies a batch-limit value by mult, saturating
// at math.MaxInt to avoid signed-overflow when the limit is "unlimited"
// (math.MaxInt) or a very large explicit value. Returns 0 if either
// operand is non-positive, so the caller can treat 0 as "no cap" too.
func scaleBatchLimit(limit, mult int) int {
	if limit <= 0 || mult <= 0 {
		return 0
	}
	if limit > math.MaxInt/mult {
		return math.MaxInt
	}
	return limit * mult
}

// OPFRawBytesTooLargeError: the cumulative raw blob bytes the
// collection pass loaded into memory exceeded the safety ceiling.
// Unlike the leaf-byte cap, this is about RAM headroom rather than
// inference wall-clock — a 200 MiB push of mostly-structural JSON has
// tiny leaf content but huge raw bytes, and would OOM the user's
// shell before the leaf-byte cap got a chance to fire.
//
// The raw ceiling is derived from ENTIRE_OPF_BATCH_LIMIT (raw =
// leaf × rawByteCapMultiplier) so a user bumping the leaf cap
// proportionally bumps the raw ceiling and doesn't need a second
// env var.
type OPFRawBytesTooLargeError struct {
	RawBytes int
	Limit    int
}

func (e *OPFRawBytesTooLargeError) Error() string {
	return fmt.Sprintf("OPF rewrite would buffer %d raw blob bytes across "+
		"all unpushed commits (limit %d, ~%d× the prose-leaf cap as a RAM ceiling). "+
		"Bump ENTIRE_OPF_BATCH_LIMIT (the raw ceiling scales with it) "+
		"or push without OPF (ENTIRE_OPF=no git push) and let a smaller "+
		"follow-up push run OPF",
		e.RawBytes, e.Limit, rawByteCapMultiplier)
}

// rawByteCapMultiplier ties the raw-byte RAM ceiling to the leaf-byte
// inference cap. 100× means: leaf cap 2 MiB → raw ceiling 200 MiB.
// Picked to comfortably exceed any realistic JSON-scaffolding ratio
// (a 10 MiB JSONL with 300 KB of leaves still fits) while preventing
// pathological RAM blowups (a 5 GiB pasted dump aborts before loading).
const rawByteCapMultiplier = 100

// RewriteUnpushedV1WithOPF re-redacts unpushed entire/checkpoints/v1
// commits with OPF, builds new commits carrying Entire-OPF-Applied:
// true, and CAS-updates the local ref. Idempotent: already-applied
// commits are re-parented without re-running OPF.
//
// Caller checks redact.OPFEnabled() and skips this when OPF is off.
// Returns one of {V1DivergedError, BootstrapTooLargeError,
// V1RefMovedError, OPFRuntimeFailedError} for privacy-critical
// failures — the pre-push hook propagates these so git push aborts.
func RewriteUnpushedV1WithOPF(ctx context.Context, repo *git.Repository, target string) (plumbing.Hash, error) {
	localTip, err := readV1Tip(repo, plumbing.NewBranchReferenceName(paths.MetadataBranchName))
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read local v1: %w", err)
	}
	if localTip.IsZero() {
		return plumbing.ZeroHash, nil // no checkpoints yet
	}
	remoteTip, err := resolveRemoteV1Tip(ctx, repo, target)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read remote v1: %w", err)
	}

	if !remoteTip.IsZero() {
		mergeBase, mbErr := computeMergeBase(repo, localTip, remoteTip)
		if mbErr != nil {
			return plumbing.ZeroHash, fmt.Errorf("compute merge-base: %w", mbErr)
		}
		if mergeBase != remoteTip {
			return plumbing.ZeroHash, &V1DivergedError{Local: localTip, Remote: remoteTip, MergeBase: mergeBase}
		}
	}

	unpushed, err := listUnpushedV1Commits(repo, localTip, remoteTip)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("list unpushed v1 commits: %w", err)
	}
	if len(unpushed) == 0 {
		return localTip, nil
	}
	if remoteTip.IsZero() {
		if limit := resolveBootstrapLimit(); len(unpushed) > limit {
			return plumbing.ZeroHash, &BootstrapTooLargeError{Count: len(unpushed), Limit: limit}
		}
	}

	// Fail-closed defense: if OPF is already broken at the start of
	// this push (an earlier process step tripped the breaker), abort
	// before tagging any commits as OPF-applied. Without this, the
	// per-blob fallback inside the no-OPF cases of BatchBytesWithPrivacyFilter
	// could let 7-layer content slip out with the trailer attached.
	if redact.OPFBreakerTripped() {
		return plumbing.ZeroHash, &OPFRuntimeFailedError{OPFCommand: redact.OPFCommand()}
	}

	// Pass 1: collect every redactable blob from every unpushed commit
	// that still needs OPF. Already-applied commits get re-parented
	// later without collecting (their tree stays as-is). We also track
	// each blob's source commit + tree path so we can route the redacted
	// bytes back during apply.
	type pendingCommit struct {
		commit *object.Commit
		// blobs and paths are parallel slices; len(paths)==len(blobs).
		// startIdx is this commit's offset into the global redacted slice.
		blobs    []redact.NamedBlob
		paths    []string
		startIdx int
	}
	var globalBlobs []redact.NamedBlob
	pendings := make([]pendingCommit, 0, len(unpushed))
	// Bound raw-bytes-in-memory incrementally so a pathological push
	// (e.g. 5 GiB of pasted dumps) aborts before exhausting the user's
	// shell RAM. The leaf-byte cap downstream is about inference cost;
	// this one is about memory ceiling and fires earlier. scaleBatchLimit
	// saturates at math.MaxInt so "unlimited" actually means unlimited
	// — without saturation, "unlimited" × 100 overflows int and the cap
	// trips on every push.
	rawCap := scaleBatchLimit(resolveBatchLimit(), rawByteCapMultiplier)
	var rawBytesSoFar int
	for _, c := range unpushed {
		pc := pendingCommit{commit: c}
		if !trailers.HasOPFApplied(c.Message) {
			tree, err := repo.TreeObject(c.TreeHash)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("load tree for %s: %w", c.Hash.String()[:7], err)
			}
			pc.startIdx = len(globalBlobs)
			// Whole-tree redaction: each v1 commit tree is cumulative, so the
			// newest commit can still carry older shards that were 7-layer-only
			// before this rewrite. Redacting the whole tree for every unapplied
			// commit keeps the final rewritten tip from reintroducing an
			// un-OPF-redacted older shard. collect and apply walk the tree the
			// same way so the cached keys line up.
			if err := collectTreeBlobs(repo, tree, "", &pc.blobs, &pc.paths); err != nil {
				return plumbing.ZeroHash, fmt.Errorf("collect blobs %s: %w", c.Hash.String()[:7], err)
			}
			for _, b := range pc.blobs {
				rawBytesSoFar += len(b.Content)
			}
			if rawBytesSoFar > rawCap {
				return plumbing.ZeroHash, &OPFRawBytesTooLargeError{RawBytes: rawBytesSoFar, Limit: rawCap}
			}
			globalBlobs = append(globalBlobs, pc.blobs...)
		}
		pendings = append(pendings, pc)
	}

	// Pass 2: enforce the leaf-byte cap, then make exactly ONE OPF
	// shell-out for the whole push. The cap runs before the shell-out
	// so a too-large push fails fast with a clear remediation message.
	var globalRedacted [][]byte
	if len(globalBlobs) > 0 {
		leafBytes := redact.SumProseLeafBytes(globalBlobs)
		if limit := resolveBatchLimit(); leafBytes > limit {
			return plumbing.ZeroHash, &OPFBatchTooLargeError{LeafBytes: leafBytes, Limit: limit}
		}
		globalRedacted, err = redact.BatchBytesWithPrivacyFilter(ctx, globalBlobs)
		if err != nil {
			return plumbing.ZeroHash, &OPFRuntimeFailedError{OPFCommand: redact.OPFCommand()}
		}
	}

	// Pass 3: rebuild each commit. For OPF-applied commits, this is
	// just a re-parent (no tree changes). For unapplied commits, the
	// per-commit redaction map routes cached bytes by tree path.
	parent := remoteTip
	for _, pc := range pendings {
		var redactedByPath map[string][]byte
		if len(pc.blobs) > 0 {
			redactedByPath = make(map[string][]byte, len(pc.blobs))
			for i, path := range pc.paths {
				redactedByPath[path] = globalRedacted[pc.startIdx+i]
			}
		}
		newHash, err := rebuildV1Commit(ctx, repo, pc.commit, parent, redactedByPath)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("rebuild commit %s: %w", pc.commit.Hash.String()[:7], err)
		}
		parent = newHash
	}

	if err := atomicSetV1Ref(repo, localTip, parent); err != nil {
		return plumbing.ZeroHash, err
	}
	return parent, nil
}

func readV1Tip(repo *git.Repository, refName plumbing.ReferenceName) (plumbing.Hash, error) {
	ref, err := repo.Reference(refName, true)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return plumbing.ZeroHash, nil
		}
		return plumbing.ZeroHash, fmt.Errorf("resolve ref %s: %w", refName, err)
	}
	return ref.Hash(), nil
}

// opfRewriteFetchTmpRef is the temp ref used to stage the URL-fetched
// remote v1 tip during OPF rewrite. Cleaned up at the end of each
// resolveRemoteV1Tip call so the tracking is invisible to the user.
const opfRewriteFetchTmpRef = FetchTmpRefPrefix + "opf-rewrite-v1"

// resolveRemoteV1Tip returns the hash of the remote's
// entire/checkpoints/v1 tip.
//
// Fetches the v1 ref from target into a temporary local ref so the
// rewrite compares against the current remote tip rather than a stale
// remote-tracking ref. target may be either a remote name (e.g. "origin")
// or a URL (checkpoint_remote configured). Fetching is especially important
// for URL-based remotes, which have no tracking refs locally; otherwise every
// push would re-redact the entire history as a "bootstrap."
//
// Returns ZeroHash with no error when the remote has no v1 yet (genuine
// bootstrap case). Fetch failures fall back to ZeroHash + a warning
// log; the rewrite then treats the push as bootstrap rather than
// blocking the user on a transient network issue.
func resolveRemoteV1Tip(ctx context.Context, repo *git.Repository, target string) (plumbing.Hash, error) {
	srcRef := "refs/heads/" + paths.MetadataBranchName
	worktreeRoot := ""
	if wt, wtErr := repo.Worktree(); wtErr == nil {
		worktreeRoot = wt.Filesystem().Root()
	}
	if err := fetchURLIntoTmpRef(ctx, worktreeRoot, target, srcRef, opfRewriteFetchTmpRef, "v1 for OPF rewrite", true); err != nil {
		if !remote.IsURL(target) {
			logging.Warn(ctx, "OPF rewrite: failed to fetch remote v1; using local remote-tracking ref",
				slog.String("remote", target),
				slog.String("error", err.Error()),
			)
			return readV1Tip(repo, plumbing.NewRemoteReferenceName(target, paths.MetadataBranchName))
		}
		logging.Warn(ctx, "OPF rewrite: failed to fetch remote v1 from URL; treating push as bootstrap",
			slog.String("error", err.Error()),
		)
		return plumbing.ZeroHash, nil
	}
	defer func() {
		if err := repo.Storer.RemoveReference(plumbing.ReferenceName(opfRewriteFetchTmpRef)); err != nil {
			logging.Debug(ctx, "OPF rewrite: failed to clean up temp ref",
				slog.String("error", err.Error()),
			)
		}
	}()
	ref, err := repo.Reference(plumbing.ReferenceName(opfRewriteFetchTmpRef), true)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return plumbing.ZeroHash, nil
		}
		return plumbing.ZeroHash, fmt.Errorf("resolve fetched v1 ref: %w", err)
	}
	return ref.Hash(), nil
}

// computeMergeBase returns the merge-base commit hash. Multi-base
// (criss-cross) and unrelated-histories both return ZeroHash —
// caller treats those as diverged.
func computeMergeBase(repo *git.Repository, local, remote plumbing.Hash) (plumbing.Hash, error) {
	lc, err := repo.CommitObject(local)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("load local commit: %w", err)
	}
	rc, err := repo.CommitObject(remote)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("load remote commit: %w", err)
	}
	bases, err := lc.MergeBase(rc)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("merge-base: %w", err)
	}
	if len(bases) != 1 {
		return plumbing.ZeroHash, nil
	}
	return bases[0].Hash, nil
}

// listUnpushedV1Commits returns commits reachable from localTip but not
// remoteTip, in graph order (oldest-first). Graph order matters more
// than timestamp order — commits made in rapid succession can share
// Author.When; the parent chain is the unambiguous truth.
//
// Optimization: the caller (RewriteUnpushedV1WithOPF) has already
// validated that remoteTip is the unique merge-base of local and
// remote, which means v1 is linear and remoteTip is an ancestor of
// localTip. So walking back from localTip, the FIRST commit we hit
// whose hash equals remoteTip is the boundary — no need to pre-build
// a full remote-reachability set. This drops the cost from
// O(local + remote history) to O(unpushed) per call.
func listUnpushedV1Commits(repo *git.Repository, localTip, remoteTip plumbing.Hash) ([]*object.Commit, error) {
	iter, err := repo.Log(&git.LogOptions{From: localTip})
	if err != nil {
		return nil, fmt.Errorf("log local tip: %w", err)
	}
	defer iter.Close()

	var unpushed []*object.Commit
	if walkErr := iter.ForEach(func(c *object.Commit) error {
		if !remoteTip.IsZero() && c.Hash == remoteTip {
			return errStop
		}
		unpushed = append(unpushed, c)
		return nil
	}); walkErr != nil && !errors.Is(walkErr, errStop) {
		return nil, fmt.Errorf("walk local v1 history: %w", walkErr)
	}
	// reverse for oldest-first
	for i, j := 0, len(unpushed)-1; i < j; i, j = i+1, j-1 {
		unpushed[i], unpushed[j] = unpushed[j], unpushed[i]
	}
	return unpushed, nil
}

// rebuildV1Commit re-parents the commit onto parent. Already-applied
// commits keep their tree (idempotent); unapplied commits get a tree
// rebuilt from redactedByPath (precomputed by the orchestrator's single
// OPF batch call) plus an Entire-OPF-Applied: true trailer.
//
// redactedByPath maps each redactable blob's full tree path (e.g.
// "ab/cd.../0/full.jsonl") to its OPF-redacted bytes. Only required for
// unapplied commits; pass nil for already-applied ones.
//
// Correctness note: each v1 commit tree is cumulative. During a multi-commit
// rewrite, the newest original commit can still contain older shards that were
// 7-layer-only before this rewrite. The collect/apply walkers redact the whole
// tree for every unapplied commit so the final rewritten tip cannot
// reintroduce an older un-OPF-redacted shard.
func rebuildV1Commit(ctx context.Context, repo *git.Repository, oldCommit *object.Commit, parent plumbing.Hash, redactedByPath map[string][]byte) (plumbing.Hash, error) {
	newTree := oldCommit.TreeHash
	if !trailers.HasOPFApplied(oldCommit.Message) {
		tree, err := repo.TreeObject(oldCommit.TreeHash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("load tree: %w", err)
		}
		// Whole-tree redaction — see the collect pass in
		// RewriteUnpushedV1WithOPF for why we never scope to a single shard.
		newTree, err = rebuildTreeWithCachedRedaction(repo, tree, "", redactedByPath)
		if err != nil {
			return plumbing.ZeroHash, err
		}
	}

	parents := []plumbing.Hash{}
	if !parent.IsZero() {
		parents = append(parents, parent)
	}
	c := &object.Commit{
		Author:       oldCommit.Author,
		Committer:    oldCommit.Committer,
		Message:      trailers.AppendOPFAppliedTrailer(oldCommit.Message),
		TreeHash:     newTree,
		ParentHashes: parents,
	}
	// Sign the rewritten commit when commit signing is enabled, matching
	// every other commit-construction site in this package (common.go,
	// metadata_reconcile.go, push_common.go). Without this, a user who
	// has signed checkpoint commits would see the rewrite produce
	// unsigned commits — silently degrading their integrity story.
	checkpoint.SignCommitBestEffort(ctx, c)
	obj := repo.Storer.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode commit: %w", err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store commit: %w", err)
	}
	return hash, nil
}

// isRedactableBlobName reports whether a file at the given name should
// flow through OPF. The policy is fail-closed: every regular file
// inside the shard is redacted EXCEPT content_hash.txt, which is
// recomputed against the new full.jsonl in a deferred pass.
//
// Why an open policy: a closed allowlist (.jsonl/.txt/.json only)
// would silently skip any future blob type that lands in a shard
// — .md prose, agent dumps, no-extension transcript blobs — and ship
// it verbatim with the Entire-OPF-Applied trailer attached. Inverting
// to "redact everything except the deferred file" means new types are
// covered by default; opting out requires an explicit code change.
//
// RedactBlobBytes itself handles arbitrary content: .jsonl/.json go
// through JSON-aware leaf redaction; anything else gets byte
// redaction over the raw content. The OPF has-space gate excludes
// binary blobs from paying inference cost.
func isRedactableBlobName(name string) bool {
	return name != paths.ContentHashFileName
}

// collectTreeBlobs walks the whole tree and appends every redactable
// blob's content + full tree path to the parallel output slices. It
// walks the entire tree (no shard scoping — see RewriteUnpushedV1WithOPF
// for why); rebuildTreeWithCachedRedaction applies the same walk so the
// cached keys line up.
//
// blobs[i] and paths[i] correspond: paths[i] is the full path within
// the tree (e.g. "ab/cd.../0/full.jsonl"), used later by the apply
// walker to find each blob's redacted bytes in the cached map.
func collectTreeBlobs(repo *git.Repository, tree *object.Tree, pathPrefix string, blobs *[]redact.NamedBlob, paths *[]string) error {
	for _, e := range tree.Entries {
		switch e.Mode { //nolint:exhaustive // non-tree/blob modes are unreachable here
		case filemode.Dir:
			subPath := e.Name
			if pathPrefix != "" {
				subPath = pathPrefix + "/" + e.Name
			}
			subTree, err := repo.TreeObject(e.Hash)
			if err != nil {
				return fmt.Errorf("load subtree %s/%s: %w", pathPrefix, e.Name, err)
			}
			if err := collectTreeBlobs(repo, subTree, subPath, blobs, paths); err != nil {
				return err
			}
		case filemode.Regular, filemode.Executable:
			if !isRedactableBlobName(e.Name) {
				continue
			}
			content, err := readBlob(repo, e.Hash)
			if err != nil {
				return fmt.Errorf("read blob %s/%s: %w", pathPrefix, e.Name, err)
			}
			fullPath := e.Name
			if pathPrefix != "" {
				fullPath = pathPrefix + "/" + e.Name
			}
			*blobs = append(*blobs, redact.NamedBlob{Name: e.Name, Content: content})
			*paths = append(*paths, fullPath)
		}
	}
	return nil
}

// rebuildTreeWithCachedRedaction walks the whole tree and produces a new
// tree using the precomputed redactedByPath map for redactable blobs.
// content_hash.txt files are recomputed in a second pass against the
// new full.jsonl in the same directory.
//
// Path-specific behavior:
//   - content_hash.txt → SHA256 of the sibling full.jsonl's new bytes
//     (deferred; not redacted itself)
//   - everything else → bytes looked up in redactedByPath. The
//     fail-closed policy redacts ANY regular file except content_hash.txt,
//     not just a closed allowlist of suffixes — a future blob type
//     (.md prose, agent dumps, no-extension transcript blobs) is
//     covered by default rather than slipping through. The collect
//     pass (collectTreeBlobs) populated redactedByPath using the same
//     "everything except content_hash.txt" predicate so the keys
//     match.
//
// A redactable blob missing from redactedByPath is either a
// programmer error (collect-pass / apply-pass walks went out of sync)
// OR a concurrent storage mutation that changed the tree between
// passes; we abort the rewrite rather than silently shipping
// unredacted content with the OPF-applied trailer.
func rebuildTreeWithCachedRedaction(repo *git.Repository, tree *object.Tree, pathPrefix string, redactedByPath map[string][]byte) (plumbing.Hash, error) {
	entries := make([]object.TreeEntry, 0, len(tree.Entries))
	// deferredHashes records indexes of content_hash.txt entries we
	// need to recompute after the full.jsonl in the same dir is built.
	type deferred struct {
		idx       int
		entryName string
		entryMode filemode.FileMode
	}
	var deferredHashes []deferred
	var newFullJSONLHash plumbing.Hash

	for _, e := range tree.Entries {
		switch e.Mode { //nolint:exhaustive // non-tree/blob modes fall through to copy
		case filemode.Dir:
			subPath := e.Name
			if pathPrefix != "" {
				subPath = pathPrefix + "/" + e.Name
			}
			subTree, err := repo.TreeObject(e.Hash)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("load subtree %s/%s: %w", pathPrefix, e.Name, err)
			}
			newSub, err := rebuildTreeWithCachedRedaction(repo, subTree, subPath, redactedByPath)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			entries = append(entries, object.TreeEntry{Name: e.Name, Mode: e.Mode, Hash: newSub})

		case filemode.Regular, filemode.Executable:
			switch e.Name {
			case paths.ContentHashFileName:
				deferredHashes = append(deferredHashes, deferred{idx: len(entries), entryName: e.Name, entryMode: e.Mode})
				entries = append(entries, e) // placeholder; fixed in second pass
			default:
				// Inverted policy: every regular file except
				// content_hash.txt expects redacted bytes from the
				// collect pass. A missing key means the collect pass
				// didn't visit this path — either the storage changed
				// between passes or the two walkers disagree on which
				// files to include. Fail-closed.
				fullPath := e.Name
				if pathPrefix != "" {
					fullPath = pathPrefix + "/" + e.Name
				}
				newBytes, ok := redactedByPath[fullPath]
				if !ok {
					return plumbing.ZeroHash, fmt.Errorf("redacted bytes missing for %s "+
						"(collect/apply walk drift or concurrent storage mutation)", fullPath)
				}
				newHash, err := checkpoint.CreateBlobFromContent(repo, newBytes)
				if err != nil {
					return plumbing.ZeroHash, fmt.Errorf("write redacted blob %s: %w", fullPath, err)
				}
				entries = append(entries, object.TreeEntry{Name: e.Name, Mode: e.Mode, Hash: newHash})
				if e.Name == paths.TranscriptFileName {
					newFullJSONLHash = newHash
				}
			}
		default:
			entries = append(entries, e)
		}
	}

	for _, d := range deferredHashes {
		if newFullJSONLHash.IsZero() {
			continue // no transcript in this dir; keep original hash
		}
		jsonlBytes, err := readBlob(repo, newFullJSONLHash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("read new transcript for content_hash: %w", err)
		}
		sum := sha256.Sum256(jsonlBytes)
		hashBlob, err := checkpoint.CreateBlobFromContent(repo, []byte(fmt.Sprintf("sha256:%x", sum)))
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("write content_hash: %w", err)
		}
		entries[d.idx] = object.TreeEntry{Name: d.entryName, Mode: d.entryMode, Hash: hashBlob}
	}

	newTree := &object.Tree{Entries: entries}
	obj := repo.Storer.NewEncodedObject()
	if err := newTree.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode tree: %w", err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store tree: %w", err)
	}
	return hash, nil
}

func readBlob(repo *git.Repository, hash plumbing.Hash) ([]byte, error) {
	blob, err := repo.BlobObject(hash)
	if err != nil {
		return nil, fmt.Errorf("blob: %w", err)
	}
	r, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("blob reader: %w", err)
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("blob read: %w", err)
	}
	return data, nil
}

// atomicSetV1Ref CAS-updates the local v1 ref. A concrete
// ErrReferenceHasChanged from the storer means another worktree
// advanced the ref during our rewrite — return V1RefMovedError so the
// hook aborts the push. Other errors (I/O, packed-ref locks, storage
// bugs) get wrapped as-is so they aren't misreported as concurrency
// failures.
func atomicSetV1Ref(repo *git.Repository, expectedOld, newHash plumbing.Hash) error {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	err := repo.Storer.CheckAndSetReference(
		plumbing.NewHashReference(refName, newHash),
		plumbing.NewHashReference(refName, expectedOld),
	)
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrReferenceHasChanged) {
		actual := plumbing.ZeroHash
		if cur, refErr := repo.Reference(refName, true); refErr == nil {
			actual = cur.Hash()
		}
		return &V1RefMovedError{Expected: expectedOld, Actual: actual}
	}
	return fmt.Errorf("set v1 ref: %w", err)
}

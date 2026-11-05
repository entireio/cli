package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// MigrateResult summarizes a git-branch → git-refs checkpoint migration.
type MigrateResult struct {
	// Total is the number of checkpoints found on the v1 branch.
	Total int
	// Migrated lists the checkpoints whose ref was newly written or advanced.
	Migrated []id.CheckpointID
	// Skipped counts checkpoints already up to date (idempotent no-ops).
	Skipped int
	// Diverged lists checkpoints left without a ref because two v1 copies
	// both changed them and nobody chose which copy to keep.
	Diverged []DivergedLegacyCheckpoint
}

// MigrateOptions controls MigrateBranchToRefs and
// ResolveDivergedLegacyCheckpoints.
type MigrateOptions struct {
	// DryRun reports what would change without writing or enqueuing anything.
	// It never asks Choose.
	DryRun bool
	// Choose picks the copy of a diverged checkpoint to keep. When nil, every
	// diverged checkpoint is left without a ref and listed in
	// MigrateResult.Diverged.
	Choose LegacyCopyChooser
}

// LegacyCopyChooser returns the Ref of the copy to keep, or "" to leave the
// checkpoint without a ref. Keeping one copy hides the other copy's changes
// from the checkpoint; they stay only in that v1 branch's history.
type LegacyCopyChooser func(context.Context, DivergedLegacyCheckpoint) (plumbing.ReferenceName, error)

// DivergedLegacyCheckpoint is a checkpoint that two v1 copies (normally this
// clone's and a remote-tracking one) both changed since they split.
type DivergedLegacyCheckpoint struct {
	ID id.CheckpointID
	// Local is the copy first in read precedence, which is this clone's v1
	// whenever it holds the checkpoint. Remote is the other.
	Local, Remote LegacyCheckpointCopy
}

// RemoteName returns the remote whose tracking ref holds this copy, or "" for
// this clone's own v1 branch.
func (c LegacyCheckpointCopy) RemoteName() string {
	rest, ok := strings.CutPrefix(c.Ref.String(), "refs/remotes/")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/"+DefaultV1Refs().Primary.Short())
	return name
}

// LegacyCheckpointCopy is one v1 branch's copy of a checkpoint.
type LegacyCheckpointCopy struct {
	Ref plumbing.ReferenceName
	// SessionIDs lists the copy's sessions in index order. It is empty when
	// the copy's session metadata is not available locally.
	SessionIDs []string
	subtree    plumbing.Hash
	// sessionTrees maps each session ID to its folder's tree in this copy.
	sessionTrees map[string]plumbing.Hash
}

// DifferingSessions returns the sessions both copies hold with different
// content, in Local's order. Keeping either copy hides the other's version
// of these, which a session ID present in both would otherwise not show.
func (d DivergedLegacyCheckpoint) DifferingSessions() []string {
	var differing []string
	for _, sessionID := range d.Local.SessionIDs {
		remote, ok := d.Remote.sessionTrees[sessionID]
		if ok && remote != d.Local.sessionTrees[sessionID] {
			differing = append(differing, sessionID)
		}
	}
	return differing
}

// MigrateBranchToRefs converts every checkpoint stored on the git-branch v1
// branch (entire/checkpoints/v1) into a per-checkpoint ref under
// refs/entire/checkpoints/<shard>/<id> — the layout the git-refs store uses.
//
// Each checkpoint's current subtree is wrapped in a fresh commit,
// byte-identical except for the root metadata.json, which is normalized for
// the refs layout (see normalizeMigratedMetadata). Existing branch commits are
// not remapped. The subtree is taken from whichever v1 copy, local or
// remote-tracking, changed the checkpoint last (see pickCheckpointCopy). When
// both changed it, opts.Choose decides; without a choice the checkpoint gets
// no ref and is listed in MigrateResult.Diverged.
//
// It is idempotent: a ref whose history already contains the normalized tree
// is skipped (even when refs-store writes have advanced the tip past it), and
// a re-run after more branch activity fast-forwards the ref (parenting on the
// existing commit).
//
// Refs are enqueued for push — a failed enqueue is an error, not best-effort —
// including already-imported refs, so a ref left unqueued by a partial earlier
// run still gets pushed. Checkpoints on this clone's deleted-checkpoints list
// (see DeletedCheckpoints) are skipped and not counted. This function does not
// push.
func MigrateBranchToRefs(ctx context.Context, repo *git.Repository, opts MigrateOptions) (MigrateResult, error) {
	return migrateBranchToRefs(ctx, repo, opts, updatePersistentRef)
}

type persistentRefUpdater func(context.Context, *git.Repository, plumbing.ReferenceName, persistentRefBuilder) error

func migrateBranchToRefs(
	ctx context.Context,
	repo *git.Repository,
	opts MigrateOptions,
	updateRef persistentRefUpdater,
) (MigrateResult, error) {
	var result MigrateResult

	v1, ok, err := openLegacyV1(ctx, repo)
	if err != nil || !ok {
		return result, err
	}

	refsStore := newGitRefsStore(repo)
	queue, err := PushQueueForRepo(ctx, repo)
	if err != nil {
		return result, fmt.Errorf("resolve push queue: %w", err)
	}

	walkErr := v1.walk(ctx, func(cid id.CheckpointID) error {
		result.Total++

		refName, err := RefName(cid)
		if err != nil {
			return fmt.Errorf("ref name for checkpoint %s: %w", cid, err)
		}

		cpTreeHash, unresolved, err := v1.subtreeToKeep(ctx, cid, opts)
		if err != nil {
			return err
		}
		if unresolved != nil {
			result.Diverged = append(result.Diverged, *unresolved)
			return nil
		}

		if opts.DryRun {
			// Report only what a real run would newly write; an already-imported
			// checkpoint is a skip, not a would-migrate. No refs or objects are
			// enqueued or written on this path.
			_, imported, err := importedTree(ctx, repo, refsStore, cid, cpTreeHash, false)
			if err != nil {
				return err
			}
			if imported {
				result.Skipped++
			} else {
				result.Migrated = append(result.Migrated, cid)
			}
			return nil
		}

		migrated, err := importBranchCheckpoint(ctx, repo, refsStore, cid, cpTreeHash, updateRef, importAdvancingRef)
		if err != nil {
			return err
		}

		// The migration's queued-for-push contract is guaranteed for both new
		// and already-imported refs. Duplicates collapse on Drain.
		if err := queue.EnqueueRef(repo, refName); err != nil {
			return fmt.Errorf("enqueue checkpoint %s for push: %w", cid, err)
		}
		if migrated {
			result.Migrated = append(result.Migrated, cid)
		} else {
			result.Skipped++
		}
		return nil
	})
	if walkErr != nil {
		return result, fmt.Errorf("walk v1 checkpoints: %w", walkErr)
	}
	return result, nil
}

// ResolveDivergedLegacyCheckpoints gives a ref to each v1 checkpoint that the
// write path refuses to copy after the switch to git-refs: one without a ref
// whose v1 copies both changed it (see ErrLegacyCheckpointDiverged). Every
// other checkpoint is left to that write path, which copies it when it is next
// written, and a checkpoint that already has a ref is never touched: after the
// switch the ref is the checkpoint's current state.
//
// fetch, when set, first asks the remote for the checkpoint's ref, since a ref
// another clone already pushed supersedes both v1 copies. Otherwise
// opts.Choose decides which copy becomes the ref; it is written only while
// the ref is still absent (importOnlyIntoAbsentRef) and enqueued for push.
// Without a choice the checkpoint is listed in MigrateResult.Diverged.
func ResolveDivergedLegacyCheckpoints(ctx context.Context, repo *git.Repository, opts MigrateOptions, fetch RefFetchFunc) (MigrateResult, error) {
	var result MigrateResult

	v1, ok, err := openLegacyV1(ctx, repo)
	if err != nil || !ok {
		return result, err
	}

	refsStore := newGitRefsStore(repo)
	queue, err := PushQueueForRepo(ctx, repo)
	if err != nil {
		return result, fmt.Errorf("resolve push queue: %w", err)
	}

	walkErr := v1.walk(ctx, func(cid id.CheckpointID) error {
		result.Total++
		refName, err := RefName(cid)
		if err != nil {
			return fmt.Errorf("ref name for checkpoint %s: %w", cid, err)
		}
		stuck, err := v1.refusedByWritePath(ctx, cid, refName, fetch)
		if err != nil || !stuck {
			return err
		}
		cpTreeHash, unresolved, err := v1.subtreeToKeep(ctx, cid, opts)
		if err != nil {
			return err
		}
		if unresolved != nil {
			result.Diverged = append(result.Diverged, *unresolved)
			return nil
		}
		migrated, err := importBranchCheckpoint(ctx, repo, refsStore, cid, cpTreeHash, updatePersistentRef, importOnlyIntoAbsentRef)
		if err != nil {
			return err
		}
		if !migrated {
			// A ref appeared while the person chose; it is the checkpoint's
			// current state now.
			result.Skipped++
			return nil
		}
		if err := queue.EnqueueRef(repo, refName); err != nil {
			return fmt.Errorf("enqueue checkpoint %s for push: %w", cid, err)
		}
		result.Migrated = append(result.Migrated, cid)
		return nil
	})
	if walkErr != nil {
		return result, fmt.Errorf("walk v1 checkpoints: %w", walkErr)
	}
	return result, nil
}

// refusedByWritePath reports whether cid is one the write path refuses to copy
// after the switch: it has no ref, locally or (when fetch is set) on the
// remote, and its v1 copies diverged. A ref the remote holds is fetched, which
// resolves the checkpoint. The write path copies every other checkpoint when
// it is next written.
func (v *legacyV1) refusedByWritePath(ctx context.Context, cid id.CheckpointID, refName plumbing.ReferenceName, fetch RefFetchFunc) (bool, error) {
	if exists, err := refExists(v.repo, refName); err != nil || exists {
		return false, err
	}
	_, _, err := v.branch.pickCheckpointCopy(ctx, v.trees, cid)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, ErrLegacyCheckpointDiverged) {
		return false, fmt.Errorf("read checkpoint %s from %s: %w", cid, DefaultV1Refs().Primary, err)
	}
	if fetch == nil {
		return true, nil
	}
	if err := fetch(ctx, refName); err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, fmt.Errorf("ask the remote for checkpoint ref %s: %w", refName, err)
	}
	exists, err := refExists(v.repo, refName)
	return !exists && err == nil, err
}

// legacyV1 is the v1 branch as both migration paths read it: every tier in
// read precedence, walked through the first one, as reads list checkpoints.
type legacyV1 struct {
	repo    *git.Repository
	branch  *GitStore
	trees   []metadataTree
	deleted DeletedCheckpointSet
}

// openLegacyV1 resolves the v1 tiers. ok is false when there is no v1 branch
// locally or on a remote, so there is nothing to migrate.
func openLegacyV1(ctx context.Context, repo *git.Repository) (v1 *legacyV1, ok bool, err error) {
	branch := NewGitStore(repo, DefaultV1Refs())
	trees, err := branch.getSessionsBranchTrees(ctx)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read v1 checkpoint branch: %w", err)
	}
	deleted, err := LoadDeletedForRepo(repo)
	if err != nil {
		return nil, false, err
	}
	return &legacyV1{repo: repo, branch: branch, trees: trees, deleted: deleted}, true, nil
}

// walk calls fn for each checkpoint on the first tier that this clone has not
// deleted. A checkpoint deleted here must not come back as a ref, even when a
// stale tracking ref or a remote the delete could not reach still holds it.
func (v *legacyV1) walk(ctx context.Context, fn func(id.CheckpointID) error) error {
	return WalkCheckpointShards(ctx, v.repo, v.trees[0].tree, func(cid id.CheckpointID, _ plumbing.Hash) error {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // propagate context cancellation
		}
		if v.deleted.Contains(cid) {
			return nil
		}
		return fn(cid)
	})
}

// subtreeToKeep returns the v1 subtree that becomes cid's ref: the copy that
// changed it last, or, when two copies both changed it, the one opts.Choose
// picks. When no copy is kept, because nobody can choose (a dry run or no
// chooser) or the choice was to skip, it returns the unresolved divergence.
func (v *legacyV1) subtreeToKeep(ctx context.Context, cid id.CheckpointID, opts MigrateOptions) (plumbing.Hash, *DivergedLegacyCheckpoint, error) {
	picked, diverged, err := v.branch.pickCheckpointCopy(ctx, v.trees, cid)
	if err == nil {
		return picked.subtree, nil, nil
	}
	if !errors.Is(err, ErrLegacyCheckpointDiverged) {
		return plumbing.ZeroHash, nil, fmt.Errorf("read checkpoint %s from %s: %w", cid, DefaultV1Refs().Primary, err)
	}
	question := DivergedLegacyCheckpoint{
		ID:     cid,
		Local:  v.describeCopy(diverged[0]),
		Remote: v.describeCopy(diverged[1]),
	}
	if opts.DryRun || opts.Choose == nil {
		return plumbing.ZeroHash, &question, nil
	}
	keep, err := opts.Choose(ctx, question)
	if err != nil {
		return plumbing.ZeroHash, nil, err
	}
	for _, c := range []LegacyCheckpointCopy{question.Local, question.Remote} {
		if keep != "" && keep == c.Ref {
			return c.subtree, nil, nil
		}
	}
	return plumbing.ZeroHash, &question, nil
}

func (v *legacyV1) describeCopy(c *checkpointCopy) LegacyCheckpointCopy {
	described := LegacyCheckpointCopy{Ref: c.ref, subtree: c.subtree, sessionTrees: map[string]plumbing.Hash{}}
	tree, err := v.repo.TreeObject(c.subtree)
	if err != nil {
		return described
	}
	for i := 0; ; i++ {
		meta, ok := readCommittedMetadataFromCheckpointTree(tree, i)
		if !ok {
			return described
		}
		described.SessionIDs = append(described.SessionIDs, meta.SessionID)
		if entry, err := tree.FindEntry(strconv.Itoa(i)); err == nil {
			described.sessionTrees[meta.SessionID] = entry.Hash
		}
	}
}

func refExists(repo *git.Repository, refName plumbing.ReferenceName) (bool, error) {
	_, err := repo.Reference(refName, true)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, plumbing.ErrReferenceNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("resolve checkpoint ref %s: %w", refName, err)
	}
}

// importMode says what importBranchCheckpoint does with a ref that already
// exists when it commits.
type importMode int

const (
	// importAdvancingRef commits v1's tree on top of an existing ref. Migration
	// runs while git-branch is primary, so v1 is the checkpoint's current state.
	importAdvancingRef importMode = iota
	// importOnlyIntoAbsentRef leaves an existing ref alone. After the switch the
	// ref is the checkpoint's current state: one that appears while a copy
	// runs (another process fetched a teammate's copy, say) can hold sessions
	// v1 lacks, and laying v1's tree over it would drop them from the tip.
	importOnlyIntoAbsentRef
)

// importBranchCheckpoint writes the checkpoint subtree cpTreeHash, taken from
// the v1 branch, into cid's own ref in the refs layout (see
// migratedCheckpointTree). An existing ref is never replaced: mode says whether
// it is advanced or left alone. A ref whose history already holds that tree is
// left alone too. migrated reports whether an import commit was written. It
// does not enqueue the ref for push.
func importBranchCheckpoint(
	ctx context.Context,
	repo *git.Repository,
	refsStore *gitRefsStore,
	cid id.CheckpointID,
	cpTreeHash plumbing.Hash,
	updateRef persistentRefUpdater,
	mode importMode,
) (migrated bool, err error) {
	migratedTree, imported, err := importedTree(ctx, repo, refsStore, cid, cpTreeHash, true)
	if err != nil {
		return false, err
	}
	if imported {
		return false, nil
	}
	refName, err := RefName(cid)
	if err != nil {
		return false, fmt.Errorf("ref name for checkpoint %s: %w", cid, err)
	}

	authorName, authorEmail := GetGitAuthorFromRepo(repo)
	if err := updateRef(ctx, repo, refName, func() (plumbing.Hash, plumbing.Hash, error) {
		// Re-read the parent and idempotency state on every CAS retry so an
		// import commit never overwrites a concurrently advanced ref.
		parent, expected, err := importBase(repo, refsStore, cid)
		if err != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, err
		}
		if mode == importOnlyIntoAbsentRef && !expected.IsZero() {
			migrated = false
			return expected, expected, nil
		}
		if treeInRefHistory(repo, parent, migratedTree) {
			migrated = false
			return parent, expected, nil
		}

		msg := fmt.Sprintf("Import checkpoint %s (migrated from git-branch)", cid)
		commitHash, commitErr := CreateCommit(ctx, repo, migratedTree, parent, msg, authorName, authorEmail)
		if commitErr != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash,
				fmt.Errorf("commit checkpoint %s: %w", cid, commitErr)
		}
		migrated = true
		return commitHash, expected, nil
	}); err != nil {
		return false, fmt.Errorf("set ref for checkpoint %s: %w", cid, err)
	}
	return migrated, nil
}

// importedTree normalizes cid's v1 subtree cpTreeHash as an import writes it
// (persisting the normalized objects when persist is set) and reports whether
// the ref's history already holds that tree.
func importedTree(
	ctx context.Context,
	repo *git.Repository,
	refsStore *gitRefsStore,
	cid id.CheckpointID,
	cpTreeHash plumbing.Hash,
	persist bool,
) (tree plumbing.Hash, imported bool, err error) {
	tree, err = migratedCheckpointTree(ctx, repo, cid, cpTreeHash, persist)
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("normalize checkpoint %s: %w", cid, err)
	}
	parent, _, err := importBase(repo, refsStore, cid)
	if err != nil {
		return plumbing.ZeroHash, false, err
	}
	return tree, treeInRefHistory(repo, parent, tree), nil
}

// importBase returns what an import of cid builds on. parent is the ref's tip,
// or zero when the ref is absent or its commit is unreadable, which an import
// repairs with an orphan commit. expected is the ref's current hash, which the
// import's compare-and-swap checks, so a concurrent repair wins cleanly.
func importBase(repo *git.Repository, refsStore *gitRefsStore, cid id.CheckpointID) (parent, expected plumbing.Hash, err error) {
	parent, _, err = refsStore.refBase(cid)
	switch {
	case err == nil:
		return parent, parent, nil
	case errors.Is(err, plumbing.ErrObjectNotFound):
		refName, nameErr := RefName(cid)
		if nameErr != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("ref name for checkpoint %s: %w", cid, nameErr)
		}
		ref, refErr := repo.Reference(refName, true)
		if refErr != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("resolve unreadable ref %s: %w", refName, refErr)
		}
		return plumbing.ZeroHash, ref.Hash(), nil
	default:
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("resolve existing ref for checkpoint %s: %w", cid, err)
	}
}

// treeInRefHistory reports whether any commit on the first-parent chain
// starting at tip carries the given tree.
func treeInRefHistory(repo *git.Repository, tip, tree plumbing.Hash) bool {
	for h := tip; h != plumbing.ZeroHash; {
		commit, err := repo.CommitObject(h)
		if err != nil {
			return false
		}
		if commit.TreeHash == tree {
			return true
		}
		if len(commit.ParentHashes) == 0 {
			return false
		}
		h = commit.ParentHashes[0]
	}
	return false
}

// migratedCheckpointTree returns the branch subtree with its root metadata.json
// normalized for the refs layout — unchanged when already normalized or absent.
//
// When persist is false (dry-run) it computes the resulting tree hash WITHOUT
// writing the normalized blob or tree into the object store. git object hashes
// are content-addressed, so the hash returned is byte-identical to the one the
// persisting path produces — idempotency reporting stays exact while a dry-run
// leaves no loose objects behind.
func migratedCheckpointTree(ctx context.Context, repo *git.Repository, cid id.CheckpointID, cpTreeHash plumbing.Hash, persist bool) (plumbing.Hash, error) {
	subtree, err := repo.TreeObject(cpTreeHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read checkpoint tree: %w", err)
	}
	metadataFile, err := subtree.File(paths.MetadataFileName)
	if err != nil {
		if errors.Is(err, object.ErrFileNotFound) {
			return cpTreeHash, nil
		}
		return plumbing.ZeroHash, fmt.Errorf("read metadata.json: %w", err)
	}
	raw, err := metadataFile.Contents()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read metadata.json: %w", err)
	}

	normalized, changed, err := normalizeMigratedMetadata([]byte(raw), cid)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if !changed {
		return cpTreeHash, nil
	}

	if !persist {
		blobHash, err := hashBlob(repo, normalized)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("hash normalized metadata.json: %w", err)
		}
		return hashRootFileSwap(repo, subtree, paths.MetadataFileName, blobHash)
	}

	blobHash, err := CreateBlobFromContent(repo, normalized)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("write normalized metadata.json: %w", err)
	}
	newTree, err := ApplyTreeChanges(ctx, repo, cpTreeHash, []TreeChange{{
		Path:  paths.MetadataFileName,
		Entry: &object.TreeEntry{Name: paths.MetadataFileName, Mode: filemode.Regular, Hash: blobHash},
	}})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("build normalized checkpoint tree: %w", err)
	}
	return newTree, nil
}

// hashBlob encodes content as a git blob and returns its hash without storing
// it — mirroring CreateBlobFromContent's encoding so the two hash identically.
func hashBlob(repo *git.Repository, content []byte) (plumbing.Hash, error) {
	obj := repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(content)))
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("open blob writer: %w", err)
	}
	if _, err := w.Write(content); err != nil {
		_ = w.Close()
		return plumbing.ZeroHash, fmt.Errorf("write blob: %w", err)
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("close blob writer: %w", err)
	}
	return obj.Hash(), nil
}

// hashRootFileSwap returns the hash of subtree with one root-level file entry
// replaced by blobHash, without storing the new tree. It mirrors ApplyTreeChanges
// + storeTree for a single root-level file (force Regular mode, then
// sortTreeEntries before encoding) so the hash matches the persisting path.
func hashRootFileSwap(repo *git.Repository, subtree *object.Tree, name string, blobHash plumbing.Hash) (plumbing.Hash, error) {
	entries := make([]object.TreeEntry, len(subtree.Entries))
	copy(entries, subtree.Entries)
	swapped := false
	for i := range entries {
		if entries[i].Name == name {
			entries[i] = object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: blobHash}
			swapped = true
			break
		}
	}
	if !swapped {
		// The caller only reaches here after reading name from this same tree.
		return plumbing.ZeroHash, fmt.Errorf("%s not found in checkpoint tree", name)
	}
	sortTreeEntries(entries)
	obj := repo.Storer.NewEncodedObject()
	if err := (&object.Tree{Entries: entries}).Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode dry-run tree: %w", err)
	}
	return obj.Hash(), nil
}

// normalizeMigratedMetadata rewrites a checkpoint's root metadata.json for the
// refs layout: it drops the legacy checkpoint_version field and strips the
// "/<shard>/<id>" prefix from sessions[] paths. Any session string value under
// the prefix is rebased, so path fields added by other CLI versions are covered
// without naming them. The raw JSON is edited in place so fields this CLI
// doesn't model are preserved. changed is false when the metadata already
// matches the refs layout.
func normalizeMigratedMetadata(raw []byte, cid id.CheckpointID) (normalized []byte, changed bool, err error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false, fmt.Errorf("parse metadata.json: %w", err)
	}

	if _, ok := doc["checkpoint_version"]; ok {
		delete(doc, "checkpoint_version")
		changed = true
	}

	branchPrefix := "/" + cid.Path()
	if sessions, ok := doc["sessions"].([]any); ok {
		for _, entry := range sessions {
			session, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			for field, raw := range session {
				value, ok := raw.(string)
				if !ok {
					continue
				}
				if rest, found := strings.CutPrefix(value, branchPrefix); found && strings.HasPrefix(rest, "/") {
					session[field] = rest
					changed = true
				}
			}
		}
	}
	if !changed {
		return nil, false, nil
	}

	normalized, err = jsonutil.MarshalIndentWithNewline(doc, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("encode metadata.json: %w", err)
	}
	return normalized, true, nil
}

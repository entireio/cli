package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
}

// MigrateBranchToRefs converts every checkpoint stored on the git-branch v1
// branch (entire/checkpoints/v1) into a per-checkpoint ref under
// refs/entire/checkpoints/<shard>/<id> — the layout the git-refs store uses.
//
// Each checkpoint's current subtree from the v1 branch tip is wrapped in a
// fresh commit, byte-identical except for the root metadata.json, which is
// normalized for the refs layout (see normalizeMigratedMetadata). Existing
// branch commits are not remapped.
//
// It is idempotent: a ref whose history already contains the normalized tree
// is skipped (even when refs-store writes have advanced the tip past it), and
// a re-run after more branch activity fast-forwards the ref (parenting on the
// existing commit).
//
// Refs are enqueued for push — a failed enqueue is an error, not best-effort —
// including already-imported refs, so a ref left unqueued by a partial earlier
// run still gets pushed. This function does not push. When dryRun is true it
// reports what would change without writing or enqueuing anything.
func MigrateBranchToRefs(ctx context.Context, repo *git.Repository, dryRun bool) (MigrateResult, error) {
	var result MigrateResult

	branch := NewGitStore(repo, DefaultV1Refs())
	tree, err := branch.getSessionsBranchTree()
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			// No v1 branch locally or on origin → nothing to migrate.
			return result, nil
		}
		return result, fmt.Errorf("read v1 checkpoint branch: %w", err)
	}

	refsStore := newGitRefsStore(repo)
	authorName, authorEmail := GetGitAuthorFromRepo(repo)
	queue, err := PushQueueForRepo(ctx, repo)
	if err != nil {
		return result, fmt.Errorf("resolve push queue: %w", err)
	}

	walkErr := WalkCheckpointShards(ctx, repo, tree, func(cid id.CheckpointID, cpTreeHash plumbing.Hash) error {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // propagate context cancellation
		}
		result.Total++

		migratedTree, err := migratedCheckpointTree(ctx, repo, cid, cpTreeHash, !dryRun)
		if err != nil {
			return fmt.Errorf("normalize checkpoint %s: %w", cid, err)
		}

		refName, err := RefName(cid)
		if err != nil {
			return fmt.Errorf("ref name for checkpoint %s: %w", cid, err)
		}

		// The existing ref drives the idempotency check and the new commit's
		// parent. refBase separates three cases we must not conflate:
		//   - no ref yet (nil error, zero hash): a brand-new orphan.
		//   - ref present but its commit object is missing (corrupt or pruned):
		//     treat as absent and re-import as an orphan rather than parenting on
		//     a bad hash, which would corrupt the commit graph for fetch+replay.
		//   - a genuine read failure (transient IO, a concurrent repack): do NOT
		//     clobber a possibly-valid ref with an orphan; abort this checkpoint
		//     so an idempotent re-run can retry once the repo is readable again.
		parent, _, err := refsStore.refBase(cid)
		switch {
		case err == nil:
			// parent is the ref tip, or zero when the ref is absent.
		case errors.Is(err, plumbing.ErrObjectNotFound):
			parent = plumbing.ZeroHash
		default:
			return fmt.Errorf("resolve existing ref for checkpoint %s: %w", cid, err)
		}
		// This snapshot is already imported when it appears anywhere on the
		// ref's first-parent chain: the ref may have advanced past it through
		// refs-store writes, and re-wrapping the old snapshot would regress the
		// tip.
		alreadyImported := treeInRefHistory(repo, parent, migratedTree)

		if dryRun {
			// Report only what a real run would newly write; an already-imported
			// checkpoint is a skip, not a would-migrate. No refs or objects are
			// enqueued or written on this path.
			if alreadyImported {
				result.Skipped++
			} else {
				result.Migrated = append(result.Migrated, cid)
			}
			return nil
		}

		if alreadyImported {
			// The snapshot is on the ref, but a prior run may have written the
			// ref and then failed before enqueuing it (an Enqueue error, or a
			// crash between setRef and Enqueue), leaving it queued for a push
			// that never comes — and every later run would skip it here. Enqueue
			// unconditionally so the "queued for push" contract survives a
			// partial earlier run; duplicates collapse on Drain and an
			// already-pushed ref is a no-op on the next push.
			if err := queue.Enqueue(refName); err != nil {
				return fmt.Errorf("enqueue checkpoint %s for push: %w", cid, err)
			}
			result.Skipped++
			return nil
		}

		msg := fmt.Sprintf("Import checkpoint %s (migrated from git-branch)", cid)
		commitHash, err := CreateCommit(ctx, repo, migratedTree, parent, msg, authorName, authorEmail)
		if err != nil {
			return fmt.Errorf("commit checkpoint %s: %w", cid, err)
		}
		if err := refsStore.setRef(ctx, cid, commitHash); err != nil {
			return fmt.Errorf("set ref for checkpoint %s: %w", cid, err)
		}
		// setRef's own enqueue is best-effort (a condensation write must not
		// fail on it); the migration's queued-for-push contract needs a
		// guaranteed one. Duplicates collapse on Drain.
		if err := queue.Enqueue(refName); err != nil {
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

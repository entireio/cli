package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// ULIDMapping records one legacy-hex checkpoint re-identified as a ULID.
type ULIDMapping struct {
	OldID id.CheckpointID // legacy 12-hex id (on the v1 branch)
	NewID id.CheckpointID // freshly minted ULID (now a per-checkpoint ref)
}

// ULIDMigrateResult summarizes a hex → ULID checkpoint migration.
type ULIDMigrateResult struct {
	// Total is the number of checkpoints found on the v1 branch.
	Total int
	// Mapping lists each re-identified checkpoint, in walk order. Callers use it
	// to rewrite the Entire-Checkpoint commit trailers from hex to ULID.
	Mapping []ULIDMapping
	// Skipped counts checkpoints left as-is because their id is already a ULID.
	Skipped int
}

// MigrateBranchHexToULIDRefs re-identifies every legacy-hex checkpoint on the
// git-branch v1 branch (entire/checkpoints/v1) as a fresh ULID stored under a
// per-checkpoint ref (refs/entire/checkpoints/<shard>/<ulid>), the layout the
// git-refs store uses. For each checkpoint it re-stamps the embedded checkpoint_id
// in the checkpoint's metadata.json files (root summary + each session) so the
// on-disk data matches its new ULID, then commits the re-stamped tree at a fresh
// ref and enqueues it for push like any git-refs write.
//
// Each ULID is minted with the checkpoint's original CreatedAt timestamp, so the
// ULIDs sort chronologically and the repo reads as if it had used git-refs +
// ULIDs from the start. Checkpoint content other than checkpoint_id is carried
// over byte-for-byte (transcripts, summaries, attribution); no commit SHAs are
// embedded in checkpoint content, so a later history rewrite does not stale it.
//
// It returns the hex → ULID mapping so the caller can rewrite the
// Entire-Checkpoint trailers across the user's commit history. A checkpoint whose
// id is already a ULID is skipped (idempotent re-runs, or a mixed repo). When
// dryRun is true it mints the mapping without writing any refs or objects.
func MigrateBranchHexToULIDRefs(ctx context.Context, repo *git.Repository, dryRun bool) (ULIDMigrateResult, error) {
	var result ULIDMigrateResult

	trees := v1CheckpointTrees(repo)
	if len(trees) == 0 {
		return result, nil // no v1 branch anywhere → nothing to migrate
	}

	refsStore := newGitRefsStore(repo)
	authorName, authorEmail := GetGitAuthorFromRepo(repo)
	// Dedup across v1 sources: the local and origin v1 branches can diverge (each
	// machine condenses onto its own), so a checkpoint may appear in more than one
	// tree — migrate it once.
	seen := make(map[id.CheckpointID]bool)

	for _, tree := range trees {
		walkErr := WalkCheckpointShards(ctx, repo, tree, func(cid id.CheckpointID, cpTreeHash plumbing.Hash) error {
			if err := ctx.Err(); err != nil {
				return err //nolint:wrapcheck // propagate context cancellation
			}
			if seen[cid] {
				return nil
			}
			seen[cid] = true
			result.Total++

			// Already a ULID (re-run, or a mixed repo): its content and ref are already
			// in the target shape, so leave it untouched.
			if cid.Kind() == id.KindULID {
				result.Skipped++
				return nil
			}

			cpTree, err := repo.TreeObject(cpTreeHash)
			if err != nil {
				return fmt.Errorf("read checkpoint tree %s: %w", cid, err)
			}
			info := readCommittedInfoFromCheckpointTree(cid, cpTree)
			// Mint the ULID from the checkpoint's original creation time so the ULIDs
			// sort chronologically — the repo reads as if it had used ULIDs all along.
			newID, err := id.GenerateULIDAt(info.CreatedAt)
			if err != nil {
				return fmt.Errorf("mint ULID for checkpoint %s: %w", cid, err)
			}
			result.Mapping = append(result.Mapping, ULIDMapping{OldID: cid, NewID: newID})

			if dryRun {
				return nil
			}

			newTreeHash, err := restampCheckpointID(repo, cpTreeHash, newID, info.SessionCount)
			if err != nil {
				return fmt.Errorf("re-stamp checkpoint %s: %w", cid, err)
			}
			msg := fmt.Sprintf("Import checkpoint %s (migrated from git-branch hex %s)", newID, cid)
			commitHash, err := CreateCommit(ctx, repo, newTreeHash, plumbing.ZeroHash, msg, authorName, authorEmail)
			if err != nil {
				return fmt.Errorf("commit checkpoint %s: %w", newID, err)
			}
			if err := refsStore.setRef(ctx, newID, commitHash); err != nil {
				return fmt.Errorf("set ref for checkpoint %s: %w", newID, err)
			}
			logging.Debug(ctx, "migrate-to-ulid: re-identified checkpoint",
				slog.String("old_id", cid.String()), slog.String("new_id", newID.String()))
			return nil
		})
		if walkErr != nil {
			return result, fmt.Errorf("walk v1 checkpoints: %w", walkErr)
		}
	}
	return result, nil
}

// v1CheckpointTrees returns every available checkpoint tree to migrate: the local
// entire/checkpoints/v1 branch AND origin's remote-tracking copy. The two can
// diverge (each machine condenses checkpoints onto its own v1 before they are
// reconciled), so reading only one silently drops the checkpoints that live on
// the other — leaving their commit trailers un-remapped. Missing/unreadable refs
// are skipped. Run `git fetch` first if origin's tracking ref may be stale.
func v1CheckpointTrees(repo *git.Repository) []*object.Tree {
	refNames := []plumbing.ReferenceName{
		plumbing.NewBranchReferenceName(paths.MetadataBranchName),
		plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName),
	}
	var trees []*object.Tree
	for _, refName := range refNames {
		ref, err := repo.Reference(refName, true)
		if err != nil {
			continue
		}
		commit, err := repo.CommitObject(ref.Hash())
		if err != nil {
			continue
		}
		tree, err := commit.Tree()
		if err != nil {
			continue
		}
		trees = append(trees, tree)
	}
	return trees
}

// restampCheckpointID rewrites the embedded checkpoint_id (hex → newID) in the
// checkpoint's metadata.json files and returns the new checkpoint tree hash. All
// other blobs are preserved byte-for-byte.
//
// The root metadata.json (CheckpointSummary) and each <n>/metadata.json (Metadata)
// carry the checkpoint's id; nothing else in the tree does.
func restampCheckpointID(repo *git.Repository, cpTreeHash plumbing.Hash, newID id.CheckpointID, sessionCount int) (plumbing.Hash, error) {
	current, err := restampMetadataDir(repo, cpTreeHash, "", newID) // root summary
	if err != nil {
		return plumbing.ZeroHash, err
	}
	for i := range sessionCount {
		current, err = restampMetadataDir(repo, current, strconv.Itoa(i), newID)
		if err != nil {
			return plumbing.ZeroHash, err
		}
	}
	return current, nil
}

// restampMetadataDir re-stamps checkpoint_id → newID in the metadata.json under
// sessionDir ("" = the checkpoint root) within treeHash and grafts the patched
// file back, returning the new tree hash. treeHash is returned unchanged when
// there is no metadata.json there or it carries no checkpoint_id.
func restampMetadataDir(repo *git.Repository, treeHash plumbing.Hash, sessionDir string, newID id.CheckpointID) (plumbing.Hash, error) {
	tree, err := repo.TreeObject(treeHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read tree %s: %w", treeHash, err)
	}
	path := paths.MetadataFileName
	var segs []string
	if sessionDir != "" {
		path = sessionDir + "/" + paths.MetadataFileName
		segs = []string{sessionDir}
	}
	file, err := tree.File(path)
	if err != nil {
		return treeHash, nil //nolint:nilerr // absent metadata.json → nothing to re-stamp
	}
	content, err := file.Contents()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read %s: %w", path, err)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("parse %s: %w", path, err)
	}
	if _, has := doc["checkpoint_id"]; !has {
		return treeHash, nil // not an id-bearing metadata file
	}
	idJSON, err := json.Marshal(newID.String())
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode checkpoint id: %w", err)
	}
	doc["checkpoint_id"] = idJSON
	out, err := json.Marshal(doc)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode %s: %w", path, err)
	}
	blobHash, err := CreateBlobFromContent(repo, out)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("write %s blob: %w", path, err)
	}
	newTree, err := UpdateSubtree(repo, treeHash, segs,
		[]object.TreeEntry{{Name: paths.MetadataFileName, Mode: file.Mode, Hash: blobHash}},
		UpdateSubtreeOptions{MergeMode: MergeKeepExisting})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("graft %s: %w", path, err)
	}
	return newTree, nil
}

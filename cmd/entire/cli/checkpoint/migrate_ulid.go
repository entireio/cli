package checkpoint

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	ulidpkg "github.com/oklog/ulid/v2"

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

	branch := NewGitStore(repo, DefaultV1Refs())
	tree, err := branch.getSessionsBranchTree()
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return result, nil // no v1 branch → nothing to migrate
		}
		return result, fmt.Errorf("read v1 checkpoint branch: %w", err)
	}

	refsStore := newGitRefsStore(repo)
	authorName, authorEmail := GetGitAuthorFromRepo(repo)

	walkErr := WalkCheckpointShards(ctx, repo, tree, func(cid id.CheckpointID, cpTreeHash plumbing.Hash) error {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // propagate context cancellation
		}
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
		newID, err := mintULIDAt(info.CreatedAt)
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
	return result, nil
}

// mintULIDAt mints a canonical ULID checkpoint id whose timestamp is t (falling
// back to now when t is zero), with crypto-random entropy so concurrent-second
// checkpoints stay unique.
func mintULIDAt(t time.Time) (id.CheckpointID, error) {
	if t.IsZero() {
		t = time.Now()
	}
	u, err := ulidpkg.New(ulidpkg.Timestamp(t.UTC()), rand.Reader)
	if err != nil {
		return id.EmptyCheckpointID, fmt.Errorf("generate ULID: %w", err)
	}
	return id.NewCheckpointID(u.String()) //nolint:wrapcheck // id.NewCheckpointID already returns a descriptive error
}

// restampCheckpointID rewrites the embedded checkpoint_id (hex → newID) in the
// checkpoint's metadata.json files — the root summary and each session's metadata
// — and returns the new checkpoint tree hash. All other blobs are preserved
// byte-for-byte. Files without a checkpoint_id key are left untouched.
func restampCheckpointID(repo *git.Repository, cpTreeHash plumbing.Hash, newID id.CheckpointID, sessionCount int) (plumbing.Hash, error) {
	// The root metadata.json (CheckpointSummary) and each <n>/metadata.json
	// (Metadata) carry the checkpoint's id; nothing else in the tree does.
	dirs := [][]string{nil} // root
	for i := range sessionCount {
		dirs = append(dirs, []string{strconv.Itoa(i)})
	}

	current := cpTreeHash
	for _, dir := range dirs {
		patched, mode, ok, err := restampMetadataAt(repo, current, dir, newID)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if !ok {
			continue
		}
		current, err = UpdateSubtree(repo, current, dir,
			[]object.TreeEntry{{Name: paths.MetadataFileName, Mode: mode, Hash: patched}},
			UpdateSubtreeOptions{MergeMode: MergeKeepExisting})
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("graft metadata.json: %w", err)
		}
	}
	return current, nil
}

// restampMetadataAt reads the metadata.json under dir within treeHash, rewrites
// its checkpoint_id to newID, and stores the patched blob. ok is false when there
// is no metadata.json at that path (skip) or it carries no checkpoint_id.
func restampMetadataAt(repo *git.Repository, treeHash plumbing.Hash, dir []string, newID id.CheckpointID) (plumbing.Hash, filemode.FileMode, bool, error) {
	tree, err := repo.TreeObject(treeHash)
	if err != nil {
		return plumbing.ZeroHash, 0, false, fmt.Errorf("read tree %s: %w", treeHash, err)
	}
	path := paths.MetadataFileName
	if len(dir) > 0 {
		path = dir[0] + "/" + paths.MetadataFileName
	}
	file, err := tree.File(path)
	if err != nil {
		return plumbing.ZeroHash, 0, false, nil //nolint:nilerr // absent metadata.json → nothing to re-stamp
	}
	content, err := file.Contents()
	if err != nil {
		return plumbing.ZeroHash, 0, false, fmt.Errorf("read %s: %w", path, err)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		return plumbing.ZeroHash, 0, false, fmt.Errorf("parse %s: %w", path, err)
	}
	if _, has := doc["checkpoint_id"]; !has {
		return plumbing.ZeroHash, 0, false, nil // not an id-bearing metadata file
	}
	idJSON, err := json.Marshal(newID.String())
	if err != nil {
		return plumbing.ZeroHash, 0, false, fmt.Errorf("encode checkpoint id: %w", err)
	}
	doc["checkpoint_id"] = idJSON
	out, err := json.Marshal(doc)
	if err != nil {
		return plumbing.ZeroHash, 0, false, fmt.Errorf("encode %s: %w", path, err)
	}
	blobHash, err := CreateBlobFromContent(repo, out)
	if err != nil {
		return plumbing.ZeroHash, 0, false, fmt.Errorf("write %s blob: %w", path, err)
	}
	return blobHash, file.Mode, true, nil
}

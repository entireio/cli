package checkpoint

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
)

// ShardRenameResult reports what NormalizeShardSpelling did.
type ShardRenameResult struct {
	// Renamed counts checkpoints whose ref moved to its lowercase shard.
	Renamed int
	// Diverged lists checkpoints with a ref under each spelling whose
	// histories diverged. Both refs are left for the user to compare.
	Diverged []id.CheckpointID
}

// shardRename moves one checkpoint's ref from its uppercase-shard name to its
// lowercase one (LocalRefName).
type shardRename struct {
	from plumbing.ReferenceName
	to   plumbing.ReferenceName
	// fromHash is what from holds. It guards from's delete.
	fromHash plumbing.Hash
	// toHash is the commit to must hold: from's, unless to already holds a
	// later version of it.
	toHash plumbing.Hash
	// toExpected is what to holds now, zero when it does not exist.
	toExpected plumbing.Hash
}

// NormalizeShardSpelling renames this repository's checkpoint refs to their
// lowercase-shard names (LocalRefName), where git recorded a case-insensitive
// filesystem. Elsewhere it does nothing: refs there cannot fold, and keep
// RefName.
//
// A checkpoint with a ref under each spelling keeps the later version (see
// SupersededBy). One whose two refs diverged is left as it is and reported.
//
// Renaming is safe only while the refs are packed. On a case-insensitive
// filesystem both spellings of a loose ref name one file, so creating the
// lowercase name and then deleting the uppercase one deletes the checkpoint.
// So this packs, creates every lowercase name in one transaction, packs again,
// confirms each lowercase name is now its own ref holding the expected commit,
// and only then deletes the uppercase names in a second transaction, each
// guarded by the commit it held. A ref that changed in the meantime makes git
// refuse the transaction and nothing is deleted. Nothing is lost on failure:
// the next run starts from whatever refs exist.
func NormalizeShardSpelling(ctx context.Context, repo *git.Repository) (ShardRenameResult, error) {
	if !foldsCase(repo) {
		return ShardRenameResult{}, nil
	}
	root, err := repositoryRoot(repo)
	if err != nil {
		return ShardRenameResult{}, fmt.Errorf("resolve repository root: %w", err)
	}
	plan, diverged, err := planShardRenames(ctx, repo, root)
	result := ShardRenameResult{Diverged: diverged}
	if err != nil || len(plan) == 0 {
		return result, err
	}
	result.Renamed, err = applyShardRenames(ctx, root, plan)
	return result, err
}

// PendingShardRenames reports what NormalizeShardSpelling would do in repo
// without changing anything: the refs it would rename and the checkpoints it
// would leave diverged. folds is false where git recorded a case-sensitive
// filesystem, where nothing is renamed.
func PendingShardRenames(ctx context.Context, repo *git.Repository) (renames []plumbing.ReferenceName, diverged []id.CheckpointID, folds bool, err error) {
	if !foldsCase(repo) {
		return nil, nil, false, nil
	}
	root, err := repositoryRoot(repo)
	if err != nil {
		return nil, nil, true, fmt.Errorf("resolve repository root: %w", err)
	}
	plan, diverged, err := planShardRenames(ctx, repo, root)
	for _, r := range plan {
		renames = append(renames, r.from)
	}
	return renames, diverged, true, err
}

// planShardRenames lists the checkpoint refs and plans a rename for each
// checkpoint with a ref outside its lowercase-shard name.
func planShardRenames(ctx context.Context, repo *git.Repository, root string) ([]shardRename, []id.CheckpointID, error) {
	tips, err := gitrepo.ListRefs(ctx, root, CheckpointRefPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("list checkpoint refs: %w", err)
	}
	var plan []shardRename
	var diverged []id.CheckpointID
	for name, hash := range tips {
		cid, ok := ParseRef(name)
		if !ok {
			continue
		}
		local, err := LocalRefName(cid)
		if err != nil || name == local {
			continue
		}
		// ParseRef accepts only two spellings of a shard, and one of them is
		// local, so name is the only other ref this checkpoint can have.
		rename := shardRename{from: name, to: local, fromHash: hash, toHash: hash}
		if localHash, ok := tips[local]; ok {
			rename.toExpected = localHash
			switch {
			case localHash.Equal(hash), SupersededBy(repo, hash, localHash):
				rename.toHash = localHash
			case SupersededBy(repo, localHash, hash):
				// from is the later version: move to onto it.
			default:
				diverged = append(diverged, cid)
				continue
			}
		}
		plan = append(plan, rename)
	}
	slices.SortFunc(plan, func(a, b shardRename) int { return strings.Compare(a.from.String(), b.from.String()) })
	slices.Sort(diverged)
	return plan, diverged, nil
}

// applyShardRenames carries out plan and returns how many renames completed.
// See NormalizeShardSpelling for why the steps run in this order.
func applyShardRenames(ctx context.Context, root string, plan []shardRename) (int, error) {
	pattern := CheckpointRefPrefix + "*"
	if err := gitrepo.PackRefs(ctx, root, pattern); err != nil {
		return 0, fmt.Errorf("pack checkpoint refs: %w", err)
	}
	var creates []gitrepo.RefUpdate
	for _, r := range plan {
		if !r.toHash.Equal(r.toExpected) {
			creates = append(creates, gitrepo.RefUpdate{Name: r.to, New: r.toHash, Old: r.toExpected})
		}
	}
	if err := gitrepo.UpdateRefs(ctx, root, creates); err != nil {
		return 0, fmt.Errorf("create lowercase checkpoint refs: %w", err)
	}
	if err := gitrepo.PackRefs(ctx, root, pattern); err != nil {
		return 0, fmt.Errorf("pack checkpoint refs: %w", err)
	}

	// Listed by exact name, a lowercase ref that git stored under the
	// uppercase spelling instead (because that directory still existed) is
	// missing here, and its uppercase twin must not be deleted.
	tips, err := gitrepo.ListRefs(ctx, root, CheckpointRefPrefix)
	if err != nil {
		return 0, fmt.Errorf("list checkpoint refs: %w", err)
	}
	var deletes []gitrepo.RefUpdate
	for _, r := range plan {
		if tips[r.to].Equal(r.toHash) && tips[r.from].Equal(r.fromHash) {
			deletes = append(deletes, gitrepo.RefUpdate{Name: r.from, Old: r.fromHash})
		}
	}
	if err := gitrepo.UpdateRefs(ctx, root, deletes); err != nil {
		return 0, fmt.Errorf("remove uppercase checkpoint refs: %w", err)
	}
	if len(deletes) < len(plan) {
		return len(deletes), fmt.Errorf("%d of %d checkpoint refs were not renamed: a lowercase ref did not appear as its own ref", len(plan)-len(deletes), len(plan))
	}
	return len(deletes), nil
}

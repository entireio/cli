package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// ErrReplayConflict identifies incompatible changes to the same checkpoint data.
var ErrReplayConflict = errors.New("checkpoint replay content conflict")

// ReplayTrees merges the changes from before to after into target.
// It matches sessions by ID and returns ErrReplayConflict for conflicting edits.
func ReplayTrees(ctx context.Context, repo *git.Repository, before, after, target plumbing.Hash) (plumbing.Hash, error) {
	return mergeReplayTrees(ctx, repo, before, after, target, "")
}

// Equal subtrees are reused. A zero hash means the directory is absent.
func mergeReplayTrees(ctx context.Context, repo *git.Repository, before, after, onto plumbing.Hash, prefix string) (plumbing.Hash, error) {
	if err := ctx.Err(); err != nil {
		return plumbing.ZeroHash, err //nolint:wrapcheck // preserve context cancellation
	}
	if before == after || after == onto {
		return onto, nil
	}
	if before == onto {
		return after, nil
	}

	base, err := replayTreeEntries(repo, before)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	local, err := replayTreeEntries(repo, after)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	remote, err := replayTreeEntries(repo, onto)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	if merged, handled, mergeErr := mergeCheckpointSessions(ctx, repo, base, local, remote, prefix); handled || mergeErr != nil {
		return merged, mergeErr
	}
	return mergeReplayEntries(ctx, repo, base, local, remote, onto, prefix)
}

func mergeReplayEntries(ctx context.Context, repo *git.Repository, base, local, remote map[string]object.TreeEntry, onto plumbing.Hash, prefix string) (plumbing.Hash, error) {
	// Visit changed entries in a stable order.
	names := make([]string, 0, len(base)+len(local))
	for name := range base {
		names = append(names, name)
	}
	for name := range local {
		if _, exists := base[name]; !exists {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	changes := make([]TreeChange, 0, len(names))
	for _, name := range names {
		merged, mergeErr := mergeReplayEntry(ctx, repo, base[name], local[name], remote[name], path.Join(prefix, name))
		if mergeErr != nil {
			return plumbing.ZeroHash, mergeErr
		}
		if merged == remote[name] {
			continue
		}
		change := TreeChange{Path: name}
		if merged.Mode != 0 {
			change.Entry = &merged
		}
		changes = append(changes, change)
	}
	merged, err := ApplyTreeChanges(ctx, repo, onto, changes)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("apply replay changes: %w", err)
	}
	return merged, nil
}

func mergeReplayEntry(ctx context.Context, repo *git.Repository, before, after, onto object.TreeEntry, name string) (object.TreeEntry, error) {
	switch {
	case after == before, after == onto:
		return onto, nil
	case onto == before:
		return after, nil
	}
	if merged, handled, err := mergeReplaySessionMetadata(repo, before, after, onto, name); handled || err != nil {
		return merged, err
	}
	// Directories can contain independent edits. Other conflicting entries cannot merge.
	for _, entry := range []object.TreeEntry{before, after, onto} {
		if entry.Mode != 0 && entry.Mode != filemode.Dir {
			return object.TreeEntry{}, fmt.Errorf("%w at %q", ErrReplayConflict, name)
		}
	}
	hash, err := mergeReplayTrees(ctx, repo, before.Hash, after.Hash, onto.Hash, name)
	if err != nil {
		return object.TreeEntry{}, err
	}
	return object.TreeEntry{Name: path.Base(name), Mode: filemode.Dir, Hash: hash}, nil
}

func replayTreeEntries(repo *git.Repository, hash plumbing.Hash) (map[string]object.TreeEntry, error) {
	entries := make(map[string]object.TreeEntry)
	if hash.IsZero() {
		return entries, nil
	}
	tree, err := repo.TreeObject(hash)
	if err != nil {
		return nil, fmt.Errorf("read replay tree %s: %w", hash, err)
	}
	for _, entry := range tree.Entries {
		entries[entry.Name] = entry
	}
	return entries, nil
}

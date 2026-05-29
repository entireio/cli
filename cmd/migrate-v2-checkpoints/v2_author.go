package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	checkpointID "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/utils/merkletrie"
)

type v2SessionAuthorIndex struct {
	authors map[string]object.Signature
}

func findV2SessionAuthor(ctx context.Context, repo *git.Repository, cpID checkpointID.CheckpointID, sessionIndex int) (object.Signature, error) {
	index, err := buildV2SessionAuthorIndex(ctx, repo)
	if err != nil {
		return object.Signature{}, err
	}
	return index.find(cpID, sessionIndex)
}

func buildV2SessionAuthorIndex(ctx context.Context, repo *git.Repository) (*v2SessionAuthorIndex, error) {
	ref, err := repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", paths.V2MainRefName, err)
	}

	iter, err := repo.Log(&git.LogOptions{
		From:  ref.Hash(),
		Order: git.LogOrderCommitterTime,
	})
	if err != nil {
		return nil, fmt.Errorf("read %s history: %w", paths.V2MainRefName, err)
	}
	defer iter.Close()

	index := &v2SessionAuthorIndex{authors: make(map[string]object.Signature)}
	err = iter.ForEach(func(commit *object.Commit) error {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // Propagating context cancellation
		}
		if commit.NumParents() > 1 {
			return nil
		}
		paths, err := changedV2SessionMetadataPaths(ctx, commit)
		if err != nil {
			return fmt.Errorf("read changed v2 session metadata paths in %s: %w", commit.Hash, err)
		}
		for _, path := range paths {
			if _, exists := index.authors[path]; !exists {
				index.authors[path] = commit.Author
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s history: %w", paths.V2MainRefName, err)
	}
	return index, nil
}

func (index *v2SessionAuthorIndex) find(cpID checkpointID.CheckpointID, sessionIndex int) (object.Signature, error) {
	metadataPath := v2SessionMetadataPath(cpID, sessionIndex)
	author, ok := index.authors[metadataPath]
	if !ok {
		return object.Signature{}, fmt.Errorf("%s not found in %s history", metadataPath, paths.V2MainRefName)
	}
	return author, nil
}

func changedV2SessionMetadataPaths(ctx context.Context, commit *object.Commit) ([]string, error) {
	commitTree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("read commit tree: %w", err)
	}

	var parentTree *object.Tree
	if commit.NumParents() > 0 {
		parent, err := commit.Parent(0)
		if err != nil {
			return nil, fmt.Errorf("read parent: %w", err)
		}
		parentTree, err = parent.Tree()
		if err != nil {
			return nil, fmt.Errorf("read parent tree: %w", err)
		}
	}

	changes, err := object.DiffTreeContext(ctx, parentTree, commitTree)
	if err != nil {
		return nil, fmt.Errorf("diff commit tree: %w", err)
	}

	var paths []string
	for _, change := range changes {
		path, ok, err := v2SessionMetadataPathFromChange(change)
		if err != nil {
			return nil, err
		}
		if ok {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

func v2SessionMetadataPathFromChange(change *object.Change) (string, bool, error) {
	action, err := change.Action()
	if err != nil {
		return "", false, fmt.Errorf("read change action: %w", err)
	}
	if action != merkletrie.Insert && action != merkletrie.Modify {
		return "", false, nil
	}
	if !isV2SessionMetadataPath(change.To.Name) {
		return "", false, nil
	}
	return change.To.Name, true, nil
}

func isV2SessionMetadataPath(path string) bool {
	shard, rest, ok := strings.Cut(path, "/")
	if !ok || len(shard) != 2 {
		return false
	}
	suffix, rest, ok := strings.Cut(rest, "/")
	if !ok || len(suffix) != 10 {
		return false
	}
	if _, err := checkpointID.NewCheckpointID(shard + suffix); err != nil {
		return false
	}
	sessionDir, fileName, ok := strings.Cut(rest, "/")
	if !ok || fileName != paths.MetadataFileName {
		return false
	}
	sessionIndex, err := strconv.Atoi(sessionDir)
	if err != nil {
		return false
	}
	return sessionIndex >= 0
}

func v2SessionMetadataPath(cpID checkpointID.CheckpointID, sessionIndex int) string {
	return cpID.Path() + "/" + strconv.Itoa(sessionIndex) + "/" + paths.MetadataFileName
}

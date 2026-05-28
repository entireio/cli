package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	checkpointID "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

var errFoundV2SessionAuthor = errors.New("found v2 session author")

func findV2SessionAuthor(ctx context.Context, repo *git.Repository, cpID checkpointID.CheckpointID, sessionIndex int) (object.Signature, error) {
	if err := ctx.Err(); err != nil {
		return object.Signature{}, err //nolint:wrapcheck // Propagating context cancellation
	}

	ref, err := repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	if err != nil {
		return object.Signature{}, fmt.Errorf("resolve %s: %w", paths.V2MainRefName, err)
	}

	metadataPath := v2SessionMetadataPath(cpID, sessionIndex)
	iter, err := repo.Log(&git.LogOptions{
		From:  ref.Hash(),
		Order: git.LogOrderCommitterTime,
		PathFilter: func(path string) bool {
			return path == metadataPath
		},
	})
	if err != nil {
		return object.Signature{}, fmt.Errorf("read %s history for %s: %w", paths.V2MainRefName, metadataPath, err)
	}
	defer iter.Close()

	var author object.Signature
	err = iter.ForEach(func(commit *object.Commit) error {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // Propagating context cancellation
		}
		author = commit.Author
		return errFoundV2SessionAuthor
	})
	if errors.Is(err, errFoundV2SessionAuthor) {
		return author, nil
	}
	if err != nil {
		return object.Signature{}, fmt.Errorf("walk %s history for %s: %w", paths.V2MainRefName, metadataPath, err)
	}
	return object.Signature{}, fmt.Errorf("%s not found in %s history", metadataPath, paths.V2MainRefName)
}

func v2SessionMetadataPath(cpID checkpointID.CheckpointID, sessionIndex int) string {
	return cpID.Path() + "/" + strconv.Itoa(sessionIndex) + "/" + paths.MetadataFileName
}

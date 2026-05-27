package checkpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6"
)

func TestGetV2MetadataTree_LocalRef(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	cpID := id.MustCheckpointID("a1a2a3a4a5a6")
	ctx := context.Background()

	// Write a checkpoint fixture so the /main ref exists.
	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-1",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"test": true}`)),
	})

	openRepoFn := func(_ context.Context) (*git.Repository, error) {
		return repo, nil
	}

	// nil fetch functions — only local ref lookup should be tried
	tree, returnedRepo, err := GetV2MetadataTree(ctx, nil, nil, openRepoFn)
	require.NoError(t, err)
	require.NotNil(t, tree)
	assert.Equal(t, repo, returnedRepo)

	// Verify the tree contains the checkpoint subtree
	cpTree, err := tree.Tree(cpID.Path())
	require.NoError(t, err)
	require.NotNil(t, cpTree)
}

func TestGetV2MetadataTree_NoRef_ReturnsError(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	ctx := context.Background()

	openRepoFn := func(_ context.Context) (*git.Repository, error) {
		return repo, nil
	}

	// No v2 ref exists, no fetch functions — should fail
	tree, _, err := GetV2MetadataTree(ctx, nil, nil, openRepoFn)
	require.Error(t, err)
	assert.Nil(t, tree)
}

func TestGetV2MetadataTree_FetchSucceeds(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	cpID := id.MustCheckpointID("b1b2b3b4b5b6")
	ctx := context.Background()

	// Write checkpoint fixture so the ref exists after "fetch".
	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-1",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"test": true}`)),
	})

	fetchCalled := false
	treelessFetchFn := func(_ context.Context) error {
		fetchCalled = true
		return nil // Simulate successful fetch
	}

	openRepoFn := func(_ context.Context) (*git.Repository, error) {
		return repo, nil
	}

	tree, _, err := GetV2MetadataTree(ctx, treelessFetchFn, nil, openRepoFn)
	require.NoError(t, err)
	require.NotNil(t, tree)
	assert.True(t, fetchCalled, "treeless fetch should have been called")
}

func TestGetV2MetadataTree_TreelessFetchFails_FallsBackToFullFetch(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t)
	cpID := id.MustCheckpointID("c1c2c3c4c5c6")
	ctx := context.Background()

	writeV2TestCheckpoint(t, repo, v2TestCheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-1",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"test": true}`)),
	})

	treelessFetchFn := func(_ context.Context) error {
		return errors.New("treeless fetch failed")
	}
	fullFetchCalled := false
	fullFetchFn := func(_ context.Context) error {
		fullFetchCalled = true
		return nil
	}

	openRepoFn := func(_ context.Context) (*git.Repository, error) {
		return repo, nil
	}

	tree, _, err := GetV2MetadataTree(ctx, treelessFetchFn, fullFetchFn, openRepoFn)
	require.NoError(t, err)
	require.NotNil(t, tree)
	assert.True(t, fullFetchCalled, "full fetch should be tried before accepting a stale local ref")
}

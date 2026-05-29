package main

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointID "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestFindV2SessionAuthorSkipsV2MainMergeCommits(t *testing.T) {
	t.Parallel()

	repo := setupV2AuthorRepo(t)
	cpID := checkpointID.MustCheckpointID("aaaaaaaaaaaa")
	checkpointAuthor := object.Signature{
		Name:  "Checkpoint Author",
		Email: "checkpoint@example.com",
		When:  time.Date(2024, 5, 11, 17, 19, 31, 0, time.UTC),
	}
	baseAuthor := object.Signature{
		Name:  "Base Author",
		Email: "base@example.com",
		When:  checkpointAuthor.When.Add(-48 * time.Hour),
	}
	baseHash := writeTestEmptyV2MainCommit(t, repo, nil, baseAuthor, "base v2/main")
	mergeAuthor := object.Signature{
		Name:  "Merge Author",
		Email: "merge@example.com",
		When:  time.Date(2024, 5, 20, 16, 0, 6, 0, time.UTC),
	}
	writeTestV2Checkpoint(t, repo, testV2CheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-with-merge-history",
		AuthorName:   checkpointAuthor.Name,
		AuthorEmail:  checkpointAuthor.Email,
		AuthorWhen:   checkpointAuthor.When,
	})
	ref, err := repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	require.NoError(t, err)
	writeTestV2MergeCommitWithCheckpointParent(t, repo, baseHash, ref.Hash(), mergeAuthor)

	author, err := findV2SessionAuthor(context.Background(), repo, cpID, 0)

	require.NoError(t, err)
	require.Equal(t, checkpointAuthor.Name, author.Name)
	require.Equal(t, checkpointAuthor.Email, author.Email)
	require.True(t, author.When.Equal(checkpointAuthor.When), "author time = %s, want %s", author.When, checkpointAuthor.When)
}

func TestFindV2SessionAuthorSkipsLaterCheckpointCommitsThatOnlyCarryPath(t *testing.T) {
	t.Parallel()

	repo := setupV2AuthorRepo(t)
	cpID := checkpointID.MustCheckpointID("0b0206eed178")
	metadataPath := v2SessionMetadataPath(cpID, 0)
	metadataBlob, err := checkpoint.CreateBlobFromContent(repo, []byte(`{"session_id":"original"}`+"\n"))
	require.NoError(t, err)
	metadataEntries := map[string]object.TreeEntry{
		metadataPath: {
			Name: metadataPath,
			Mode: filemode.Regular,
			Hash: metadataBlob,
		},
	}
	metadataTree, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, metadataEntries)
	require.NoError(t, err)
	emptyTree, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, map[string]object.TreeEntry{})
	require.NoError(t, err)

	checkpointAuthor := object.Signature{
		Name:  "Checkpoint Author",
		Email: "checkpoint@example.com",
		When:  time.Date(2024, 5, 11, 17, 19, 31, 0, time.UTC),
	}
	baseAuthor := object.Signature{
		Name:  "Base Author",
		Email: "base@example.com",
		When:  checkpointAuthor.When.Add(-24 * time.Hour),
	}
	baseHash := writeTestCommitObject(t, repo, &object.Commit{
		TreeHash:  emptyTree,
		Author:    baseAuthor,
		Committer: baseAuthor,
		Message:   "base v2/main",
	})
	checkpointHash := writeTestCommitObject(t, repo, &object.Commit{
		TreeHash:     metadataTree,
		ParentHashes: []plumbing.Hash{baseHash},
		Author:       checkpointAuthor,
		Committer:    checkpointAuthor,
		Message:      "Checkpoint: 0b0206eed178",
	})

	sideAuthor := object.Signature{
		Name:  "Side Author",
		Email: "side@example.com",
		When:  checkpointAuthor.When.Add(47 * time.Hour),
	}
	sideHash := writeTestCommitObject(t, repo, &object.Commit{
		TreeHash:     emptyTree,
		ParentHashes: []plumbing.Hash{baseHash},
		Author:       sideAuthor,
		Committer:    sideAuthor,
		Message:      "side commit without metadata",
	})
	mergeAuthor := object.Signature{
		Name:  "Merge Author",
		Email: "merge@example.com",
		When:  checkpointAuthor.When.Add(24 * time.Hour),
	}
	mergeHash := writeTestCommitObject(t, repo, &object.Commit{
		TreeHash:     metadataTree,
		ParentHashes: []plumbing.Hash{checkpointHash, sideHash},
		Author:       mergeAuthor,
		Committer:    mergeAuthor,
		Message:      "Merge remote v2/main",
	})
	laterAuthor := object.Signature{
		Name:  "Later Checkpoint Author",
		Email: "later@example.com",
		When:  checkpointAuthor.When.Add(48 * time.Hour),
	}
	laterBlob, err := checkpoint.CreateBlobFromContent(repo, []byte(`{"session_id":"later"}`+"\n"))
	require.NoError(t, err)
	laterEntries := map[string]object.TreeEntry{
		metadataPath: metadataEntries[metadataPath],
		"68/0da8552908/0/metadata.json": {
			Name: "68/0da8552908/0/metadata.json",
			Mode: filemode.Regular,
			Hash: laterBlob,
		},
	}
	laterTree, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, laterEntries)
	require.NoError(t, err)
	laterHash := writeTestCommitObject(t, repo, &object.Commit{
		TreeHash:     laterTree,
		ParentHashes: []plumbing.Hash{mergeHash},
		Author:       laterAuthor,
		Committer:    laterAuthor,
		Message:      "Checkpoint: 680da8552908",
	})
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(paths.V2MainRefName), laterHash)))

	author, err := findV2SessionAuthor(context.Background(), repo, cpID, 0)

	require.NoError(t, err)
	require.Equal(t, checkpointAuthor.Name, author.Name)
	require.Equal(t, checkpointAuthor.Email, author.Email)
	require.True(t, author.When.Equal(checkpointAuthor.When), "author time = %s, want %s", author.When, checkpointAuthor.When)
}

func TestFindV2SessionAuthorReturnsNotFoundWhenOnlyMergeTouchedPath(t *testing.T) {
	t.Parallel()

	repo := setupV2AuthorRepo(t)
	cpID := checkpointID.MustCheckpointID("bbbbbbbbbbbb")
	metadataPath := v2SessionMetadataPath(cpID, 0)
	metadataBlob, err := checkpoint.CreateBlobFromContent(repo, []byte("{}\n"))
	require.NoError(t, err)
	treeWithMetadata, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, map[string]object.TreeEntry{
		metadataPath: {
			Name: metadataPath,
			Mode: filemode.Regular,
			Hash: metadataBlob,
		},
	})
	require.NoError(t, err)

	emptyTree, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, map[string]object.TreeEntry{})
	require.NoError(t, err)
	parentAuthor := object.Signature{
		Name:  "Parent Author",
		Email: "parent@example.com",
		When:  time.Date(2024, 5, 10, 10, 0, 0, 0, time.UTC),
	}
	firstParent := writeTestCommitObject(t, repo, &object.Commit{
		TreeHash:  emptyTree,
		Author:    parentAuthor,
		Committer: parentAuthor,
		Message:   "parent one",
	})
	secondParent := writeTestCommitObject(t, repo, &object.Commit{
		TreeHash:  emptyTree,
		Author:    parentAuthor,
		Committer: parentAuthor,
		Message:   "parent two",
	})
	mergeAuthor := object.Signature{
		Name:  "Merge Author",
		Email: "merge@example.com",
		When:  time.Date(2024, 5, 20, 16, 0, 6, 0, time.UTC),
	}
	mergeHash := writeTestCommitObject(t, repo, &object.Commit{
		TreeHash:     treeWithMetadata,
		ParentHashes: []plumbing.Hash{firstParent, secondParent},
		Author:       mergeAuthor,
		Committer:    mergeAuthor,
		Message:      "Merge remote v2/main",
	})
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(paths.V2MainRefName), mergeHash)))

	_, err = findV2SessionAuthor(context.Background(), repo, cpID, 0)

	require.ErrorContains(t, err, metadataPath+" not found in "+paths.V2MainRefName+" history")
}

func setupV2AuthorRepo(t *testing.T) *git.Repository {
	t.Helper()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	return repo
}

func writeTestEmptyV2MainCommit(t *testing.T, repo *git.Repository, parentHashes []plumbing.Hash, author object.Signature, message string) plumbing.Hash {
	t.Helper()

	emptyTree, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, map[string]object.TreeEntry{})
	require.NoError(t, err)
	hash := writeTestCommitObject(t, repo, &object.Commit{
		TreeHash:     emptyTree,
		ParentHashes: parentHashes,
		Author:       author,
		Committer:    author,
		Message:      message,
	})
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(paths.V2MainRefName), hash)))
	return hash
}

func writeTestV2MergeCommitWithCheckpointParent(t *testing.T, repo *git.Repository, baseHash, checkpointParent plumbing.Hash, author object.Signature) plumbing.Hash {
	t.Helper()

	checkpointCommit, err := repo.CommitObject(checkpointParent)
	require.NoError(t, err)
	mainParentAuthor := object.Signature{
		Name:  "Main Parent",
		Email: "main-parent@example.com",
		When:  author.When.Add(-24 * time.Hour),
	}
	mainParent := writeTestEmptyV2MainCommit(t, repo, []plumbing.Hash{baseHash}, mainParentAuthor, "main-side parent")
	mergeHash := writeTestCommitObject(t, repo, &object.Commit{
		TreeHash:     checkpointCommit.TreeHash,
		ParentHashes: []plumbing.Hash{mainParent, checkpointParent},
		Author:       author,
		Committer:    author,
		Message:      "Merge remote v2/main",
	})
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(paths.V2MainRefName), mergeHash)))
	return mergeHash
}

func writeTestCommitObject(t *testing.T, repo *git.Repository, commit *object.Commit) plumbing.Hash {
	t.Helper()

	encoded := repo.Storer.NewEncodedObject()
	require.NoError(t, commit.Encode(encoded))
	hash, err := repo.Storer.SetEncodedObject(encoded)
	require.NoError(t, err)
	return hash
}

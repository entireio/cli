package checkpoint

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
)

// Older CLIs left checkpoints whose session folders have gaps (0, 2, 3) while
// root metadata.json lists exactly the folders that exist. Readers must follow
// sessions[i] rather than assuming session i lives in folder i.

var gappedSessionIDs = []string{"sess-0", "sess-1", "sess-2", "sess-3"}

// wantAfterGap is what positions 0, 1, 2 must resolve to once folder 1 is gone.
var wantAfterGap = []string{"sess-0", "sess-2", "sess-3"}

type gappedStore interface {
	Writer
	PersistentStore
}

func writeGappedSessions(t *testing.T, store gappedStore, cid id.CheckpointID) {
	t.Helper()
	for _, sessionID := range gappedSessionIDs {
		require.NoError(t, store.Write(context.Background(), Session{
			CheckpointID:     cid,
			SessionID:        sessionID,
			Strategy:         "manual-commit",
			Transcript:       redact.AlreadyRedacted([]byte(`{"session":"` + sessionID + `"}` + "\n")),
			Prompts:          []string{"prompt of " + sessionID},
			FilesTouched:     []string{sessionID + ".go"},
			CheckpointsCount: 1,
			AuthorName:       "Test Author",
			AuthorEmail:      "test@example.com",
		}))
	}
}

// gapCheckpointTree drops session folder dropFolder and its sessions[] entry
// from the checkpoint stored at prefix within rootTree, returning the new root.
// Each remaining entry is rewritten through edit, so callers can also blank
// fields of the summary the writer produced.
func gapCheckpointTree(t *testing.T, repo *git.Repository, rootTree *object.Tree, prefix, dropFolder string, edit func(map[string]any)) plumbing.Hash {
	t.Helper()
	ctx := context.Background()
	metaPath := path.Join(prefix, paths.MetadataFileName)
	file, err := rootTree.File(metaPath)
	require.NoError(t, err)
	raw, err := file.Contents()
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &doc))
	sessions, ok := doc["sessions"].([]any)
	require.True(t, ok, "root metadata.json must list sessions")
	kept := make([]any, 0, len(sessions))
	dropped := false
	for _, s := range sessions {
		entry, ok := s.(map[string]any)
		require.True(t, ok)
		metadata, ok := entry["metadata"].(string)
		require.True(t, ok, "sessions[] entry must name its metadata path")
		if strings.HasSuffix(metadata, "/"+dropFolder+"/"+paths.MetadataFileName) {
			dropped = true
			continue
		}
		if edit != nil {
			edit(entry)
		}
		kept = append(kept, entry)
	}
	require.True(t, dropped, "no sessions[] entry points at folder %s", dropFolder)
	doc["sessions"] = kept

	edited, err := json.Marshal(doc)
	require.NoError(t, err)
	blobHash, err := CreateBlobFromContent(repo, edited)
	require.NoError(t, err)

	newRoot, err := ApplyTreeChanges(ctx, repo, rootTree.Hash, []TreeChange{
		{Path: path.Join(prefix, dropFolder)},
		{Path: metaPath, Entry: &object.TreeEntry{Name: metaPath, Mode: filemode.Regular, Hash: blobHash}},
	})
	require.NoError(t, err)
	return newRoot
}

// commitOnRef replaces ref's tip with a commit of tree on top of it.
func commitOnRef(t *testing.T, repo *git.Repository, refName plumbing.ReferenceName, tree plumbing.Hash) {
	t.Helper()
	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)
	commitHash, err := CreateCommit(context.Background(), repo, tree, ref.Hash(), "test: gap session folder", "Test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)))
}

func refTipTree(t *testing.T, repo *git.Repository, refName plumbing.ReferenceName) *object.Tree {
	t.Helper()
	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	return tree
}

func gapBranchCheckpoint(t *testing.T, store *GitStore, cid id.CheckpointID, edit func(map[string]any)) {
	t.Helper()
	branch := DefaultV1Refs().Primary
	root := refTipTree(t, store.repo, branch)
	commitOnRef(t, store.repo, branch, gapCheckpointTree(t, store.repo, root, cid.Path(), "1", edit))
}

func gapRefsCheckpoint(t *testing.T, store *gitRefsStore, cid id.CheckpointID, edit func(map[string]any)) {
	t.Helper()
	refName := mustRefName(t, cid)
	root := refTipTree(t, store.repo, refName)
	commitOnRef(t, store.repo, refName, gapCheckpointTree(t, store.repo, root, "", "1", edit))
}

type gappedFixture struct {
	name  string
	build func(t *testing.T, cid id.CheckpointID, edit func(map[string]any)) gappedStore
}

func gappedFixtures() []gappedFixture {
	return []gappedFixture{
		{name: "branch store", build: func(t *testing.T, cid id.CheckpointID, edit func(map[string]any)) gappedStore {
			t.Helper()
			repo, _ := setupBranchTestRepo(t)
			store := NewGitStore(repo, DefaultV1Refs())
			writeGappedSessions(t, store, cid)
			gapBranchCheckpoint(t, store, cid, edit)
			return store
		}},
		{name: "refs store", build: func(t *testing.T, cid id.CheckpointID, edit func(map[string]any)) gappedStore {
			t.Helper()
			store := newRefsStore(t)
			writeGappedSessions(t, store, cid)
			gapRefsCheckpoint(t, store, cid, edit)
			return store
		}},
	}
}

func TestGappedSessionFolders_ReadsFollowSessionsList(t *testing.T) {
	t.Parallel()
	for _, fx := range gappedFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cid := id.MustCheckpointID("ec38b74f08e3")
			store := fx.build(t, cid, nil)

			summary, err := store.Read(ctx, cid)
			require.NoError(t, err)
			require.Len(t, summary.Sessions, len(wantAfterGap))

			for i, want := range wantAfterGap {
				meta, err := store.ReadSessionMetadata(ctx, cid, i)
				require.NoError(t, err, "ReadSessionMetadata(%d)", i)
				assert.Equal(t, want, meta.SessionID, "ReadSessionMetadata(%d)", i)

				prompts, err := store.ReadSessionPrompts(ctx, cid, i)
				require.NoError(t, err, "ReadSessionPrompts(%d)", i)
				assert.Contains(t, prompts, "prompt of "+want, "ReadSessionPrompts(%d)", i)

				meta, prompts, err = store.ReadSessionMetadataAndPrompts(ctx, cid, i)
				require.NoError(t, err, "ReadSessionMetadataAndPrompts(%d)", i)
				assert.Equal(t, want, meta.SessionID, "ReadSessionMetadataAndPrompts(%d)", i)
				assert.Contains(t, prompts, "prompt of "+want, "ReadSessionMetadataAndPrompts(%d)", i)

				content, err := store.ReadSessionContent(ctx, cid, i)
				require.NoError(t, err, "ReadSessionContent(%d)", i)
				assert.Equal(t, want, content.Metadata.SessionID, "ReadSessionContent(%d)", i)
				assert.Contains(t, string(content.Transcript), want, "ReadSessionContent(%d)", i)
			}

			_, err = store.ReadSessionMetadata(ctx, cid, len(wantAfterGap))
			require.ErrorIs(t, err, ErrCheckpointNotFound, "a position past the sessions list has no session")
		})
	}
}

func TestGappedSessionFolders_ListReportsEverySession(t *testing.T) {
	t.Parallel()
	for _, fx := range gappedFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			cid := id.MustCheckpointID("ec38b74f08e3")
			store := fx.build(t, cid, nil)

			infos, err := store.List(context.Background())
			require.NoError(t, err)
			var info *CheckpointInfo
			for i := range infos {
				if infos[i].CheckpointID == cid {
					info = &infos[i]
				}
			}
			require.NotNil(t, info, "List must include the gapped checkpoint")
			assert.Equal(t, len(wantAfterGap), info.SessionCount)
			assert.Equal(t, wantAfterGap, info.SessionIDs)
			assert.Equal(t, "sess-3", info.SessionID, "the latest session is the last sessions[] entry")
		})
	}
}

// A summary entry without a metadata path gives no folder to follow, so the
// reader keeps today's positional lookup for it.
func TestSessionsEntryWithoutMetadataPath_FallsBackToPosition(t *testing.T) {
	t.Parallel()
	for _, fx := range gappedFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cid := id.MustCheckpointID("ec38b74f08e3")
			store := fx.build(t, cid, func(entry map[string]any) { delete(entry, "metadata") })

			meta, err := store.ReadSessionMetadata(ctx, cid, 0)
			require.NoError(t, err)
			assert.Equal(t, "sess-0", meta.SessionID)

			meta, err = store.ReadSessionMetadata(ctx, cid, 2)
			require.NoError(t, err)
			assert.Equal(t, "sess-2", meta.SessionID, "without a path, position 2 reads folder 2")
		})
	}
}

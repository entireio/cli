package strategy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// These tests change process CWD/backend selection and cannot run in parallel.
func TestCheckpointReplay_NonOverlappingUpdatesPreserved(t *testing.T) {
	a, b, remoteDir, cid, repoA, repoB, storeA, storeB := checkpointReplayFixture(t)
	ctx := t.Context()
	ref := mustRefName(t, cid)

	t.Chdir(a)
	paths.ClearWorktreeRootCache()
	require.NoError(t, storeA.Write(ctx, checkpoint.SessionSummary{
		CheckpointID: cid, Summary: &checkpoint.Summary{Intent: "remote summary must survive"},
	}))
	require.NoError(t, pushCheckpointRefsAtTips(t, repoA, remoteDir, []plumbing.ReferenceName{ref}))

	t.Chdir(b)
	paths.ClearWorktreeRootCache()
	require.NoError(t, storeB.Write(ctx, checkpoint.SessionTranscript{
		CheckpointID: cid, SessionID: "shared-session",
		Transcript: redact.AlreadyRedacted([]byte("{\"text\":\"local finalized transcript\"}\n")),
	}))
	queue, err := checkpoint.PushQueueForRepo(ctx, repoB)
	require.NoError(t, err)
	require.NoError(t, queue.EnqueueRef(repoB, ref))
	_, flushErr := flushCheckpointRefsQueue(ctx, repoB, pushSettings{remote: remoteDir})
	remaining, err := queue.Drain()
	require.NoError(t, err)
	require.NoError(t, flushErr)
	require.NotContains(t, remaining, ref)
	meta, err := storeB.ReadSessionMetadata(ctx, cid, 0)
	require.NoError(t, err)
	if meta.Summary == nil || meta.Summary.Intent != "remote summary must survive" {
		t.Errorf("successful recovery erased the other clone's summary; queue=%v, metadata=%+v", remaining, meta)
	}
	content, err := storeB.ReadSessionContent(ctx, cid, 0)
	require.NoError(t, err)
	require.Contains(t, string(content.Transcript), "local finalized transcript")
}

func TestCheckpointReplay_ConflictingTranscriptUpdatesStayQueued(t *testing.T) {
	a, b, remoteDir, cid, repoA, repoB, storeA, storeB := checkpointReplayFixture(t)
	ctx := t.Context()
	ref := mustRefName(t, cid)
	writeTranscript := func(store checkpoint.PersistentStore, content string) {
		require.NoError(t, store.Write(ctx, checkpoint.SessionTranscript{
			CheckpointID: cid, SessionID: "shared-session",
			Transcript: redact.AlreadyRedacted([]byte(content)),
		}))
	}
	t.Chdir(a)
	paths.ClearWorktreeRootCache()
	writeTranscript(storeA, "remote transcript\n")
	require.NoError(t, pushCheckpointRefsAtTips(t, repoA, remoteDir, []plumbing.ReferenceName{ref}))
	t.Chdir(b)
	paths.ClearWorktreeRootCache()
	writeTranscript(storeB, "provisional local transcript\n")
	writeTranscript(storeB, "local transcript\n")
	localBefore, err := repoB.Reference(ref, true)
	require.NoError(t, err)
	remoteBefore := remoteRefHash(t, remoteDir, ref)
	queue, err := checkpoint.PushQueueForRepo(ctx, repoB)
	require.NoError(t, err)
	require.NoError(t, queue.EnqueueRef(repoB, ref))
	queuedBefore, err := queue.DrainEntries()
	require.NoError(t, err)

	pushed, flushErr := flushCheckpointRefsQueue(ctx, repoB, pushSettings{remote: remoteDir})
	require.ErrorIs(t, flushErr, checkpoint.ErrReplayConflict, "conflicting appends must not be reported as delivered")
	require.Zero(t, pushed)
	localAfter, err := repoB.Reference(ref, true)
	require.NoError(t, err)
	require.Equal(t, localBefore.Hash(), localAfter.Hash())
	require.Equal(t, remoteBefore, remoteRefHash(t, remoteDir, ref))
	queuedAfter, err := queue.DrainEntries()
	require.NoError(t, err)
	require.Equal(t, queuedBefore, queuedAfter, "retain the exact queued generation")
	localSession, err := storeB.ReadSessionContent(ctx, cid, 0)
	require.NoError(t, err)
	require.Equal(t, "local transcript\n", string(localSession.Transcript))
	t.Chdir(a)
	paths.ClearWorktreeRootCache()
	remoteSession, err := storeA.ReadSessionContent(ctx, cid, 0)
	require.NoError(t, err)
	require.Equal(t, "remote transcript\n", string(remoteSession.Transcript))
}

func checkpointReplayFixture(t *testing.T) (string, string, string, id.CheckpointID, *git.Repository, *git.Repository, checkpoint.PersistentStore, checkpoint.PersistentStore) {
	t.Helper()
	return checkpointReplayFixtureFor(t, "git-refs")
}

func checkpointReplayFixtureFor(t *testing.T, backend string) (string, string, string, id.CheckpointID, *git.Repository, *git.Repository, checkpoint.PersistentStore, checkpoint.PersistentStore) {
	t.Helper()
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", backend)
	t.Setenv("ENTIRE_CHECKPOINTS_MIRRORS", "")
	t.Cleanup(paths.ClearWorktreeRootCache)
	a, b, remoteDir := t.TempDir(), t.TempDir(), t.TempDir()
	for _, dir := range []string{a, b} {
		testutil.InitRepo(t, dir)
		testutil.WriteFile(t, dir, "seed.txt", "seed\n")
		testutil.GitAdd(t, dir, "seed.txt")
		testutil.GitCommit(t, dir, "seed")
	}
	testutil.RunGit(t, remoteDir, "init", "--bare")
	for _, dir := range []string{a, b} {
		testutil.RunGit(t, dir, "remote", "add", "origin", remoteDir)
	}
	t.Chdir(a)
	paths.ClearWorktreeRootCache()
	repoA, err := gitrepo.OpenPath(a)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repoA.Close() })
	storesA, err := checkpoint.Open(t.Context(), repoA, checkpoint.OpenOptions{})
	require.NoError(t, err)
	cid := id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if backend == "git-branch" {
		cid = id.MustCheckpointID("a1b2c3d4e5f6")
	}
	require.NoError(t, storesA.Persistent.Write(t.Context(), checkpoint.Session{
		CheckpointID: cid, SessionID: "shared-session", Strategy: StrategyNameManualCommit,
		Transcript: redact.AlreadyRedacted([]byte("{\"text\":\"base transcript\"}\n")),
		AuthorName: "Test", AuthorEmail: "test@example.com",
	}))
	ref := metadataLocalRef()
	if backend == "git-refs" {
		ref = mustRefName(t, cid)
	}
	require.NoError(t, pushCheckpointRefsAtTips(t, repoA, remoteDir, []plumbing.ReferenceName{ref}))
	testutil.RunGit(t, b, "fetch", remoteDir, ref.String()+":"+ref.String())
	t.Chdir(b)
	paths.ClearWorktreeRootCache()
	repoB, err := gitrepo.OpenPath(b)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repoB.Close() })
	storesB, err := checkpoint.Open(t.Context(), repoB, checkpoint.OpenOptions{})
	require.NoError(t, err)
	return a, b, remoteDir, cid, repoA, repoB, storesA.Persistent, storesB.Persistent
}

// Not parallel: the push wrapper resolves the repository from CWD and writes stderr.
func TestDoPushRef_ContentConflictStaysFailSoft(t *testing.T) {
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-branch")
	t.Setenv("ENTIRE_CHECKPOINTS_MIRRORS", "")
	repo := newCheckpointReplayRepo(t)
	dir, err := getRepoPath(repo)
	require.NoError(t, err)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	base := makeTreeCommit(t, repo, nil, "base", map[string]string{"checkpoint/metadata.json": "base"})
	local := makeTreeCommit(t, repo, []plumbing.Hash{base}, "local", map[string]string{"checkpoint/metadata.json": "local"})
	remote := makeTreeCommit(t, repo, []plumbing.Hash{base}, "remote", map[string]string{"checkpoint/metadata.json": "remote"})
	ref := metadataLocalRef()
	bareDir := t.TempDir()
	testutil.RunGit(t, bareDir, "init", "--bare")
	testutil.RunGit(t, dir, "remote", "add", "origin", bareDir)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, remote)))
	require.NoError(t, pushCheckpointRefsAtTips(t, repo, bareDir, []plumbing.ReferenceName{ref}))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, local)))

	restore := captureStderr(t)
	delivered, pushErr := doPushRef(t.Context(), "origin", ref)
	output := restore()
	require.NoError(t, pushErr, "checkpoint conflict must not fail the user's code push")
	require.False(t, delivered)
	require.Contains(t, output, checkpoint.ErrReplayConflict.Error())
	localAfter, err := repo.Reference(ref, true)
	require.NoError(t, err)
	require.Equal(t, local, localAfter.Hash())
	require.Equal(t, remote.String(), remoteRefHash(t, bareDir, ref))
}

func TestCheckpointReplay_ConcurrentSessionAppendsArePublished(t *testing.T) {
	// Not parallel: backend selection and repository CWD are process-global.
	for _, backend := range []string{"git-refs", "git-branch"} {
		for _, disconnected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/disconnected=%t", backend, disconnected), func(t *testing.T) {
				a, b, remoteDir, cid, repoA, repoB, storeA, storeB := checkpointReplayFixtureFor(t, backend)
				ctx := t.Context()
				ref := metadataLocalRef()
				if backend == "git-refs" {
					ref = mustRefName(t, cid)
				}
				if disconnected {
					seed, err := repoB.Reference(ref, true)
					require.NoError(t, err)
					commit, err := repoB.CommitObject(seed.Hash())
					require.NoError(t, err)
					orphan := makeOrphanCommit(t, repoB, commit.TreeHash, nil, "independent migration")
					require.NoError(t, repoB.Storer.SetReference(plumbing.NewHashReference(ref, orphan)))
				}
				writeSession := func(store checkpoint.PersistentStore, sid string) {
					require.NoError(t, store.Write(ctx, checkpoint.Session{
						CheckpointID: cid, SessionID: sid, Strategy: StrategyNameManualCommit, Branch: sid,
						Transcript: redact.AlreadyRedacted([]byte("{\"text\":\"" + sid + "\"}\n")),
						Prompts:    []string{"prompt for " + sid}, FilesTouched: []string{sid + ".go"},
						CheckpointsCount: 2, TokenUsage: &types.TokenUsage{InputTokens: 10},
						Assets:     []checkpoint.TranscriptAsset{{Name: "image.png", MediaType: "image/png", Data: []byte("image for " + sid)}},
						AuthorName: "Test", AuthorEmail: "test@example.com",
					}))
				}
				t.Chdir(a)
				paths.ClearWorktreeRootCache()
				writeSession(storeA, "remote-added-session")
				require.NoError(t, pushCheckpointRefsAtTips(t, repoA, remoteDir, []plumbing.ReferenceName{ref}))
				t.Chdir(b)
				paths.ClearWorktreeRootCache()
				writeSession(storeB, "local-added-session")
				for _, version := range []string{"provisional", "final"} {
					require.NoError(t, storeB.Write(ctx, checkpoint.SessionTranscript{
						CheckpointID: cid, SessionID: "local-added-session",
						Transcript:              redact.AlreadyRedacted([]byte("local-added-session " + version + "\n")),
						PreserveAssetsWhenEmpty: true,
					}))
				}
				// Backfills must still find this session after replay changes its slot.
				require.NoError(t, storeB.Write(ctx, checkpoint.SessionSummary{
					CheckpointID: cid, Summary: &checkpoint.Summary{Intent: "local summary"},
				}))
				writeSession(storeB, "second-local-session")
				staleLocal, err := repoB.Reference(ref, true)
				require.NoError(t, err)
				var queue *checkpoint.PushQueue
				if backend == "git-refs" {
					var err error
					queue, err = checkpoint.PushQueueForRepo(ctx, repoB)
					require.NoError(t, err)
					require.NoError(t, queue.EnqueueRef(repoB, ref))
					pushed, err := flushCheckpointRefsQueue(ctx, repoB, pushSettings{remote: remoteDir})
					require.NoError(t, err)
					require.Equal(t, 1, pushed)
					waiting, err := queue.Drain()
					require.NoError(t, err)
					require.NotContains(t, waiting, ref)
				} else {
					delivered, err := doPushRef(ctx, "origin", ref)
					require.NoError(t, err)
					require.True(t, delivered)
				}
				localRef, err := repoB.Reference(ref, true)
				require.NoError(t, err)
				require.Equal(t, localRef.Hash().String(), remoteRefHash(t, remoteDir, ref))
				summary, err := storeB.Read(ctx, cid)
				require.NoError(t, err)
				require.Len(t, summary.Sessions, 4)
				require.Equal(t, "remote-added-session", summary.Branch)
				require.Equal(t, 6, summary.CheckpointsCount)
				require.ElementsMatch(t, []string{"remote-added-session.go", "local-added-session.go", "second-local-session.go"}, summary.FilesTouched)
				require.NotNil(t, summary.TokenUsage)
				require.Equal(t, 30, summary.TokenUsage.InputTokens)
				wantIDs := []string{"shared-session", "remote-added-session", "local-added-session", "second-local-session"}
				for index, sid := range wantIDs {
					content, err := storeB.ReadSessionContent(ctx, cid, index)
					require.NoError(t, err)
					require.Equal(t, sid, content.Metadata.SessionID)
					if index > 0 {
						require.Equal(t, sid, content.Metadata.Branch)
					}
					if index == 0 {
						continue
					}
					require.Contains(t, string(content.Transcript), sid)
					require.Equal(t, "prompt for "+sid, strings.TrimSpace(content.Prompts))
					if sid == "local-added-session" {
						require.NotNil(t, content.Metadata.Summary)
						require.Equal(t, "local summary", content.Metadata.Summary.Intent)
					}
				}
				commit, err := repoB.CommitObject(localRef.Hash())
				require.NoError(t, err)
				tree, err := commit.Tree()
				require.NoError(t, err)
				for _, files := range summary.Sessions {
					for _, name := range []string{files.Metadata, files.Transcript, files.CompactTranscript, files.ContentHash, files.Prompt, files.AssetsManifest} {
						if name != "" {
							_, err := tree.FindEntry(strings.TrimPrefix(name, "/"))
							require.NoError(t, err, "rebased path %s must resolve", name)
						}
					}
				}
				// Retry the original history, including provisional transcript writes.
				require.NoError(t, repoB.Storer.SetReference(plumbing.NewHashReference(ref, staleLocal.Hash())))
				if backend == "git-refs" {
					require.NoError(t, queue.EnqueueRef(repoB, ref))
					_, err = flushCheckpointRefsQueue(ctx, repoB, pushSettings{remote: remoteDir})
					require.NoError(t, err)
				} else {
					delivered, err := doPushRef(ctx, "origin", ref)
					require.NoError(t, err)
					require.True(t, delivered)
				}
				retried, err := repoB.Reference(ref, true)
				require.NoError(t, err)
				require.Equal(t, localRef.Hash(), retried.Hash(), "replay of already-delivered changes must add no commits")
				require.Equal(t, localRef.Hash().String(), remoteRefHash(t, remoteDir, ref))
			})
		}
	}
}

// Not parallel: the checkpoint store uses process CWD and backend settings.
func TestCheckpointReplay_PeerCanAppendAfterTranscriptRecovery(t *testing.T) {
	a, b, remoteDir, cid, repoA, repoB, storeA, storeB := checkpointReplayFixture(t)
	ctx := t.Context()
	ref := mustRefName(t, cid)
	t.Chdir(a)
	paths.ClearWorktreeRootCache()
	require.NoError(t, storeA.Write(ctx, checkpoint.Session{
		CheckpointID: cid, SessionID: "remote-added", Strategy: StrategyNameManualCommit,
		Transcript: redact.AlreadyRedacted([]byte("remote transcript\n")),
		AuthorName: "Test", AuthorEmail: "test@example.com",
	}))
	require.NoError(t, pushCheckpointRefsAtTips(t, repoA, remoteDir, []plumbing.ReferenceName{ref}))
	t.Chdir(b)
	paths.ClearWorktreeRootCache()
	for _, transcript := range []string{"provisional\n", "final\n"} {
		require.NoError(t, storeB.Write(ctx, checkpoint.SessionTranscript{
			CheckpointID: cid, SessionID: "shared-session",
			Transcript: redact.AlreadyRedacted([]byte(transcript)),
		}))
	}
	originalLocal, err := repoB.Reference(ref, true)
	require.NoError(t, err)
	queue, err := checkpoint.PushQueueForRepo(ctx, repoB)
	require.NoError(t, err)
	require.NoError(t, queue.EnqueueRef(repoB, ref))
	_, err = flushCheckpointRefsQueue(ctx, repoB, pushSettings{remote: remoteDir})
	require.NoError(t, err, "first recovery succeeds")
	delivered, err := repoB.Reference(ref, true)
	require.NoError(t, err)
	// Model a peer that kept the original history and adds another session.
	require.NoError(t, repoB.Storer.SetReference(plumbing.NewHashReference(ref, originalLocal.Hash())))
	require.NoError(t, storeB.Write(ctx, checkpoint.Session{
		CheckpointID: cid, SessionID: "third-clone-added", Strategy: StrategyNameManualCommit,
		Transcript: redact.AlreadyRedacted([]byte("third clone transcript\n")),
		AuthorName: "Test", AuthorEmail: "test@example.com",
	}))
	require.NoError(t, queue.EnqueueRef(repoB, ref))
	_, err = flushCheckpointRefsQueue(ctx, repoB, pushSettings{remote: remoteDir})
	require.NoError(t, err, "the peer's new session must sync")
	localAfter, err := repoB.Reference(ref, true)
	require.NoError(t, err)
	require.NotEqual(t, delivered.Hash(), localAfter.Hash(), "new session is delivered")
	summary, err := storeB.Read(ctx, cid)
	require.NoError(t, err)
	require.Len(t, summary.Sessions, 3)
	for index, want := range []struct{ id, transcript string }{
		{"shared-session", "final\n"},
		{"remote-added", "remote transcript\n"},
		{"third-clone-added", "third clone transcript\n"},
	} {
		content, err := storeB.ReadSessionContent(ctx, cid, index)
		require.NoError(t, err)
		require.Equal(t, want.id, content.Metadata.SessionID)
		require.Equal(t, want.transcript, string(content.Transcript))
	}
	remaining, err := queue.Drain()
	require.NoError(t, err)
	require.NotContains(t, remaining, ref)
	require.Equal(t, localAfter.Hash().String(), remoteRefHash(t, remoteDir, ref))
}

// Not parallel: the deletion fixture changes CWD and backend settings.
func TestCheckpointDelete_ReconcilesUnpushedFinalization(t *testing.T) {
	t.Setenv("ENTIRE_CHECKPOINTS_MIRRORS", "")
	f := newDeleteFixture(t, "git-branch")
	t.Cleanup(paths.ClearWorktreeRootCache)
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	f.writeCheckpoint(t, cid, "shared-session")
	testutil.RunGit(t, f.workDir, "push", "--no-verify", "origin", "entire/checkpoints/v1")
	repo, err := gitrepo.OpenPath(f.workDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	stores, err := checkpoint.Open(t.Context(), repo, checkpoint.OpenOptions{})
	require.NoError(t, err)
	require.NoError(t, stores.Persistent.Write(t.Context(), checkpoint.SessionTranscript{
		CheckpointID: cid, SessionID: "shared-session",
		Transcript: redact.AlreadyRedacted([]byte("final transcript\n")),
	}))
	other := id.MustCheckpointID("b2c3d4e5f6a1")
	f.writeCheckpoint(t, other, "unrelated-session")
	plan, err := PlanCheckpointDelete(t.Context(), cid, CheckpointDeletePlanOptions{})
	require.NoError(t, err)
	result, err := ExecuteCheckpointDelete(t.Context(), plan, CheckpointDeleteOptions{Targets: plan.HolderTargets()})
	require.NoError(t, err)
	require.False(t, result.Failed())
	remote := plumbing.NewHash(remoteRefHash(t, f.bareDir, v1BranchRef))
	local := refTip(repo, v1BranchRef)
	base, err := getMergeBase(t.Context(), f.workDir, local.String(), remote.String())
	require.NoError(t, err)
	require.Equal(t, remote, base, "the local branch must include the remote deletion")
	commit, err := repo.CommitObject(local)
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	_, err = tree.Tree(cid.Path())
	require.ErrorIs(t, err, object.ErrDirectoryNotFound)
	content, err := stores.Persistent.ReadSessionContent(t.Context(), other, 0)
	require.NoError(t, err)
	require.Contains(t, string(content.Transcript), "unrelated-session")
}

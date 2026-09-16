package strategy

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// initBareRemote creates an empty bare repository to push at.
func initBareRemote(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	_, err := git.PlainInit(dir, true)
	require.NoError(t, err)
	return dir
}

// TestCheckpointRefsRedeliverWhenTheDestinationChanges is the git-refs half of
// the re-sync. A successful push empties the queue, so without the re-sync a
// checkpoint delivered to the wrong store would never be offered to the right
// one: fixing a misrouted checkpoint_remote would repair future checkpoints and
// strand every earlier one.
//
// Not parallel: t.Chdir.
func TestCheckpointRefsRedeliverWhenTheDestinationChanges(t *testing.T) {
	workDir, firstStore, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	enqueueRefs(t, repo, refs)

	pushed, _, err := PushQueuedCheckpointRefs(context.Background(), repo, firstStore)
	require.NoError(t, err)
	require.Equal(t, len(refs), pushed, "the first push delivers the queued refs")

	// Nothing is enqueued now — that empty queue is the whole point. A second
	// store is named, and every checkpoint has to follow it.
	secondStore := initBareRemote(t, "second-store.git")
	pushed, _, err = PushQueuedCheckpointRefs(context.Background(), repo, secondStore)
	require.NoError(t, err)
	assert.Equal(t, len(refs), pushed, "a changed destination re-delivers what the first one already took")

	for _, ref := range refs {
		assert.NotEmpty(t, remoteRefHash(t, secondStore, ref),
			"%s must reach the store the user moved to", ref)
	}
}

// TestCheckpointRefsDoNotRedeliverToTheSameDestination is the control for the
// test above: re-queueing every local ref is a large push, so it must happen
// only on an actual change.
//
// Not parallel: t.Chdir.
func TestCheckpointRefsDoNotRedeliverToTheSameDestination(t *testing.T) {
	workDir, store, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	enqueueRefs(t, repo, refs)

	pushed, _, err := PushQueuedCheckpointRefs(context.Background(), repo, store)
	require.NoError(t, err)
	require.Equal(t, len(refs), pushed)

	pushed, _, err = PushQueuedCheckpointRefs(context.Background(), repo, store)
	require.NoError(t, err)
	assert.Equal(t, 0, pushed, "the same destination re-queues nothing")
}

// TestFirstCheckpointPushDoesNotRedeliver pins the other suppression: with no
// destination on record, "changed" cannot be told from "first push ever", and a
// fresh clone must not re-queue its whole history the first time it pushes.
//
// Not parallel: t.Chdir.
func TestFirstCheckpointPushDoesNotRedeliver(t *testing.T) {
	workDir, store, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	// Refs exist locally but none was ever queued or pushed, so nothing has been
	// recorded — exactly the state a clone that fetched checkpoints is in.
	require.NotEmpty(t, refs)

	pushed, _, err := PushQueuedCheckpointRefs(context.Background(), repo, store)
	require.NoError(t, err)
	assert.Equal(t, 0, pushed, "an unrecorded destination is not a changed one")
	assertRefsAbsentFromRemote(t, store, refs, "a first push must not sweep up local refs")
}

// TestPushedDestinationRecordsNoCredential pins why the file stores a hash: a
// checkpoint push target can be a URL carrying a token (deriveTokenOriginURL
// builds one), and the only question this file answers is "same place as last
// time", which a hash answers without putting the secret on disk.
//
// Not parallel: t.Chdir.
func TestPushedDestinationRecordsNoCredential(t *testing.T) {
	workDir := setupGitRepo(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	const target = "https://x-access-token:ghs_supersecret@github.com/acme/checkpoints.git"
	recordPushedDestination(context.Background(), target)

	root, err := gitdir.Open(context.Background())
	require.NoError(t, err)
	data, err := osroot.ReadFileNoFollow(root, pushedDestinationFileName)
	require.NoError(t, err)

	assert.NotContains(t, string(data), "ghs_supersecret", "the token must not reach disk")
	assert.NotContains(t, string(data), "github.com", "the destination itself must not reach disk")

	var f pushedDestinationFile
	require.NoError(t, json.Unmarshal(data, &f))
	assert.Equal(t, destinationFingerprint(target), f.Fingerprint)
	assert.Equal(t, f.Fingerprint, loadPushedDestination(context.Background()))
}

// TestV1BranchCarriesItsWholeHistoryToANewDestination is the git-branch half of
// the same guarantee, and the reason that backend needs no re-sync: it pushes
// the entire entire/checkpoints/v1 branch rather than a queue, so a destination
// change delivers every earlier checkpoint without being asked. Asserted rather
// than assumed, because the "has unpushed" shortcut in pushRefIfNeeded consults
// a remote-tracking ref and a remote-agnostic one would strand the history here
// exactly as the emptied queue does on git-refs.
//
// Both transitions, because they clear the shortcut by different routes and
// only the second is what this work is actually about: claiming a
// checkpoint_remote makes pushTarget return a URL, and remote.IsURL skips the
// tracking-ref check outright, while swapping one remote NAME for another
// clears it by looking up a tracking ref that does not exist yet.
//
// Not parallel: t.Chdir.
func TestV1BranchCarriesItsWholeHistoryToANewDestination(t *testing.T) {
	for _, tc := range []struct {
		name string
		// target builds the second destination from its bare repo path.
		target func(bareDir string) string
	}{
		{"another remote", func(string) string { return "second" }},
		{"a claimed checkpoint store", func(bareDir string) string { return "file://" + bareDir }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := setupRepoWithCheckpointBranch(t)
			t.Chdir(workDir)
			paths.ClearWorktreeRootCache()

			v1 := plumbing.NewBranchReferenceName(paths.MetadataBranchName)

			// Two checkpoint commits, so "the tip landed" and "the history
			// landed" are different assertions.
			testutil.WriteFile(t, workDir, "f.txt", "second")
			testutil.GitAdd(t, workDir, "f.txt")
			testutil.GitCommit(t, workDir, "second")
			repo, err := git.PlainOpen(workDir)
			require.NoError(t, err)
			head, err := repo.Head()
			require.NoError(t, err)
			require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(v1, head.Hash())))
			require.NoError(t, repo.Close())

			firstStore := initBareRemote(t, "first-store.git")
			testutil.RunGit(t, workDir, "remote", "add", "first", firstStore)
			secondStore := initBareRemote(t, "second-store.git")
			testutil.RunGit(t, workDir, "remote", "add", "second", secondStore)

			ctx := context.Background()
			delivered, err := pushRefIfNeeded(ctx, "first", v1)
			require.NoError(t, err)
			require.True(t, delivered)
			require.Equal(t, "2", v1CommitCount(t, firstStore))

			delivered, err = pushRefIfNeeded(ctx, tc.target(secondStore), v1)
			require.NoError(t, err)
			assert.True(t, delivered, "a store that has never seen this branch is not up to date")
			assert.Equal(t, "2", v1CommitCount(t, secondStore),
				"the whole checkpoint history follows the destination, not just its tip")
		})
	}
}

// v1CommitCount counts the commits on the checkpoint branch in a bare remote.
func v1CommitCount(t *testing.T, bareDir string) string {
	t.Helper()
	out := testutil.RunGit(t, bareDir, "rev-list", "--count",
		plumbing.NewBranchReferenceName(paths.MetadataBranchName).String())
	return strings.TrimSpace(out)
}

// TestDestinationResyncRedeliversUnderOPF pins the ordering between the
// re-sync and the OPF gate. RewriteQueuedCheckpointRefsWithOPF PEEKS the queue
// rather than draining it, so it rewrites what is queued at the moment it runs;
// a re-sync that enqueued refs afterwards would hand the batch push a set of
// refs the gate never saw, and they would ship with the 8-layer content while
// the rest of the batch shipped with nine.
//
// The population that makes this reachable is the one the bootstrap cap already
// exists for: refs written before OPF was turned on. They are pushed here to
// the first store with OPF off, so they carry no trailer and are not skipped as
// already-rewritten when the destination changes.
//
// Not parallel: setupGitRefsOPFRepo chdirs.
func TestDestinationResyncRedeliversUnderOPF(t *testing.T) {
	firstStore, repo, refs := setupGitRefsOPFRepo(t, "a1b2c3d4e5f6", "b2c3d4e5f6a1")

	// OPF off for this push: this is a repo that adopts OPF later.
	pushed, _, err := PushQueuedCheckpointRefs(t.Context(), repo, firstStore)
	require.NoError(t, err)
	require.Equal(t, len(refs), pushed)
	for i, hash := range refHashes(t, repo, refs) {
		commit, commitErr := repo.CommitObject(hash)
		require.NoError(t, commitErr)
		require.False(t, trailers.HasOPFApplied(commit.Message),
			"fixture precondition: %s must reach the first store un-rewritten", refs[i])
	}

	// OPF is turned on, and the store moves. Every ref the re-sync re-queues has
	// to pass the gate on its way to the new store.
	configureFakeOPF(t, &fakeOPFForRewrite{})
	secondStore := initBareRemote(t, "second-store.git")

	pushed, _, err = PushQueuedCheckpointRefs(t.Context(), repo, secondStore)
	require.NoError(t, err)
	require.Equal(t, len(refs), pushed, "the changed destination re-delivers both refs")

	for i, hash := range refHashes(t, repo, refs) {
		commit, commitErr := repo.CommitObject(hash)
		require.NoError(t, commitErr)
		assert.True(t, trailers.HasOPFApplied(commit.Message),
			"%s was re-queued by the re-sync and must still have been OPF-rewritten", refs[i])
		assert.NotContains(t, treeContents(t, repo, hash), "PERSONABC",
			"the sentinel must be scrubbed before the ref reaches the new store")
		assert.Equal(t, hash.String(), remoteRefHash(t, secondStore, refs[i]),
			"the rewritten commit is what reached the new store, not the 8-layer one")
	}
}

// TestFlushDoesNotRecordADestinationTheResyncDidNotReach is the other half of
// the partial-re-queue fix: a push can succeed for the refs that did get queued
// while the re-sync gave up on the rest, and recording the destination then
// would make the next push see no change and leave the remainder stranded.
//
// Not parallel: t.Chdir.
func TestFlushDoesNotRecordADestinationTheResyncDidNotReach(t *testing.T) {
	workDir, store, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	enqueueRefs(t, repo, refs)

	ctx := context.Background()
	ps := resolvePushSettings(ctx, store)
	pushed, err := flushCheckpointRefsQueue(ctx, repo, ps, false)
	require.NoError(t, err)
	require.Equal(t, len(refs), pushed, "the refs that were queued still push")

	assert.Empty(t, loadPushedDestination(ctx),
		"an unfinished re-sync leaves the destination unrecorded so the next push retries it")
}

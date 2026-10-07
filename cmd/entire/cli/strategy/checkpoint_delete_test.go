package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

func TestClassifyRemoteDelete(t *testing.T) {
	t.Parallel()
	pushErr := errors.New("git push: exit status 1")
	tests := []struct {
		name       string
		output     string
		err        error
		want       DeleteOutcome
		wantReason string
	}{
		{name: "deleted", output: "-\t:refs/entire/checkpoints/f6/a1b2c3d4e5f6\t[deleted]\nDone\n", want: DeleteOutcomeDeleted},
		{name: "already gone", output: "error: unable to delete 'x': remote ref does not exist\n", err: pushErr, want: DeleteOutcomeAbsent},
		{name: "lease mismatch", output: "!\t:refs/x\t[rejected] (stale info)\n", err: pushErr, want: DeleteOutcomeFailed, wantReason: "changed since it was inspected"},
		{name: "hook rejection", output: "!\t:refs/x\t[remote rejected] (pre-receive hook declined)\n", err: pushErr, want: DeleteOutcomeFailed, wantReason: "rejected by the remote: pre-receive hook declined"},
		{name: "protected ref", output: "!\t:refs/x\t[remote rejected] (deletion of protected ref prohibited)\n", err: pushErr, want: DeleteOutcomeFailed, wantReason: "deletion of protected ref prohibited"},
		{name: "transport failure", output: "fatal: could not read from remote repository\n", err: pushErr, want: DeleteOutcomeFailed, wantReason: "exit status 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, reason := classifyRemoteDelete(tt.output, tt.err)
			assert.Equal(t, tt.want, got)
			if tt.wantReason != "" {
				assert.Contains(t, reason, tt.wantReason)
			}
		})
	}
}

// A remote on a case-insensitive filesystem can store a ULID's ref under an
// existing differently-cased shard directory; the probe must still find it.
func TestParseCheckpointLsRemote_MixedCaseShard(t *testing.T) {
	t.Parallel()
	cid := id.MustCheckpointID("01K6ZQ2M8E3V7R5T9Y4X6W2A6B")
	out := []byte(strings.Join([]string{
		"1111111111111111111111111111111111111111\trefs/entire/checkpoints/6b/" + cid.String(),
		"2222222222222222222222222222222222222222\trefs/heads/entire/checkpoints/v1",
		"3333333333333333333333333333333333333333\trefs/entire/checkpoints/zz/" + cid.String(), // wrong shard
	}, "\n"))

	listing := parseCheckpointLsRemote(out, cid)
	assert.Equal(t, plumbing.ReferenceName("refs/entire/checkpoints/6b/"+cid.String()), listing.ref)
	assert.Equal(t, plumbing.NewHash("1111111111111111111111111111111111111111"), listing.refOID)
	assert.Equal(t, plumbing.NewHash("2222222222222222222222222222222222222222"), listing.v1Tip)
}

func TestDedupeDeleteTargets_MergesNamesByURL(t *testing.T) {
	t.Parallel()
	targets := dedupeDeleteTargets([]deleteTargetCandidate{
		{name: "upstream", url: "/repos/a.git"},
		{name: "origin", url: "/repos/b.git"},
		{name: "origin", url: "/repos/a.git"},
		{name: "upstream", url: "/repos/a.git"},
	})
	require.Len(t, targets, 2)
	assert.Equal(t, "/repos/a.git", targets[0].URL)
	assert.Equal(t, []string{"upstream", "origin"}, targets[0].Remotes)
	assert.Equal(t, []string{"origin"}, targets[1].Remotes)
}

// Deleting a checkpoint clears only the fields that would write its ID again.
// Token offsets, usage and baselines must stay exactly as they were: resetting
// any of them makes the next checkpoint re-count tokens that surviving
// checkpoints already carry.
func TestClearDeletedCheckpointFromState_LeavesTokenFieldsUntouched(t *testing.T) {
	t.Parallel()
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	complete := true
	state := &SessionState{
		SessionID:                      "s1",
		LastCheckpointID:               cid,
		LastCheckpointCommitHash:       "abc123",
		TurnCheckpointIDs:              []string{"ffffffffffff", cid.String()},
		CheckpointTranscriptStart:      42,
		TokenUsage:                     &types.TokenUsage{InputTokens: 10, OutputTokens: 20},
		CheckpointTokenUsage:           &types.TokenUsage{InputTokens: 1, OutputTokens: 2},
		SubagentTokensBaseline:         &types.TokenUsage{OutputTokens: 5},
		SubagentTokensBaselineComplete: &complete,
	}
	state.BeginCondensationAttempt(cid)
	before := snapshotState(t, state)

	assert.Equal(t, []string{"last_checkpoint_id", "condensation_attempt", "turn_checkpoint_ids"}, deleteStateFields(state, cid))
	require.True(t, clearDeletedCheckpointFromState(state, cid))

	assert.Empty(t, state.LastCheckpointID)
	assert.Empty(t, state.LastCheckpointCommitHash)
	assert.Nil(t, state.CondensationAttempt)
	assert.Equal(t, []string{"ffffffffffff"}, state.TurnCheckpointIDs)

	after := snapshotState(t, state)
	for _, cleared := range []string{"last_checkpoint_id", "last_checkpoint_commit_hash", "condensation_attempt", "turn_checkpoint_ids"} {
		delete(before, cleared)
		delete(after, cleared)
	}
	assert.Equal(t, before, after, "only the checkpoint-ID fields may change")

	assert.False(t, clearDeletedCheckpointFromState(state, cid), "a second clear changes nothing")
}

func snapshotState(t *testing.T, state *SessionState) map[string]any {
	t.Helper()
	data, err := json.Marshal(state)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(data, &out))
	return out
}

// deleteFixture is a work repo with one bare "origin" remote, CWD set to the
// work repo, on the given checkpoint backend.
type deleteFixture struct {
	workDir string
	bareDir string
}

func newDeleteFixture(t *testing.T, backend string) deleteFixture {
	t.Helper()
	workDir := t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "README.md", "# test")
	testutil.GitAdd(t, workDir, "README.md")
	testutil.GitCommit(t, workDir, "init")
	bareDir := t.TempDir()
	testutil.RunGit(t, bareDir, "init", "--bare")
	testutil.RunGit(t, workDir, "remote", "add", "origin", bareDir)
	testutil.RunGit(t, workDir, "push", "--no-verify", "origin", "HEAD")

	t.Chdir(workDir)
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", backend)
	paths.ClearWorktreeRootCache()
	gitdir.ClearCache()
	return deleteFixture{workDir: workDir, bareDir: bareDir}
}

func (f deleteFixture) writeCheckpoint(t *testing.T, cid id.CheckpointID, sessionID string) {
	t.Helper()
	repo, err := gitrepo.OpenPath(f.workDir)
	require.NoError(t, err)
	defer repo.Close()
	stores, err := checkpoint.Open(t.Context(), repo, checkpoint.OpenOptions{})
	require.NoError(t, err)
	require.NoError(t, stores.Persistent.Write(t.Context(), checkpoint.Session{
		CheckpointID: cid,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Agent:        "Claude Code",
		Transcript:   redact.AlreadyRedacted([]byte("transcript for " + sessionID + "\n")),
		Prompts:      []string{"do the thing"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
		TokenUsage:   &types.TokenUsage{InputTokens: 100, OutputTokens: 7},
	}))
}

func (f deleteFixture) remoteRefs(t *testing.T) string {
	t.Helper()
	return testutil.RunGit(t, f.bareDir, "for-each-ref", "--format=%(refname)")
}

func saveDeleteState(t *testing.T, state *SessionState) {
	t.Helper()
	if state.StartedAt.IsZero() {
		state.StartedAt = time.Now()
	}
	require.NoError(t, SaveSessionState(context.Background(), state))
}

func TestCheckpointDelete_RefsBackend_PlanThenExecute(t *testing.T) {
	f := newDeleteFixture(t, "git-refs")
	cid := id.MustCheckpointID("01K6ZQ2M8E3V7R5T9Y4X6W2A1B")
	other := id.MustCheckpointID("01K6ZQ2M8E3V7R5T9Y4X6W2A2C")
	f.writeCheckpoint(t, cid, "sess-1")
	f.writeCheckpoint(t, other, "sess-1")
	ref, err := checkpoint.RefName(cid)
	require.NoError(t, err)
	testutil.RunGit(t, f.workDir, "push", "--no-verify", "origin", ref.String()+":"+ref.String())
	saveDeleteState(t, &SessionState{SessionID: "sess-1", Phase: session.PhaseEnded, LastCheckpointID: cid, LastCheckpointCommitHash: "deadbeef"})

	plan, err := PlanCheckpointDelete(t.Context(), cid)
	require.NoError(t, err)
	assert.Equal(t, ref, plan.LocalRef)
	assert.False(t, plan.LocalV1)
	require.Len(t, plan.HolderTargets(), 1)
	assert.Equal(t, []string{"origin"}, plan.HolderTargets()[0].Remotes)
	require.Len(t, plan.Sessions, 1)
	assert.Equal(t, CheckpointDeleteSession{SessionID: "sess-1", Agent: "Claude Code", InputTokens: 100, OutputTokens: 7}, plan.Sessions[0])
	require.Len(t, plan.SessionStates, 1)
	assert.False(t, plan.SessionStates[0].Active, "an ended session is not active")
	require.Len(t, plan.OtherLocalCheckpoints, 1, "the other checkpoint of sess-1 is reported")
	assert.Equal(t, other, plan.OtherLocalCheckpoints[0].CheckpointID)

	// Planning wrote nothing.
	assert.Contains(t, testutil.RunGit(t, f.workDir, "for-each-ref", "--format=%(refname)"), ref.String())
	assert.Contains(t, f.remoteRefs(t), ref.String())
	st, err := LoadSessionState(t.Context(), "sess-1")
	require.NoError(t, err)
	assert.Equal(t, cid, st.LastCheckpointID)

	repo, err := gitrepo.OpenPath(f.workDir)
	require.NoError(t, err)
	defer repo.Close()
	queue, err := checkpoint.PushQueueForRepo(t.Context(), repo)
	require.NoError(t, err)
	require.NoError(t, queue.Enqueue(ref))

	result, err := ExecuteCheckpointDelete(t.Context(), plan, CheckpointDeleteOptions{Targets: plan.HolderTargets()})
	require.NoError(t, err)
	assert.False(t, result.Failed())
	assert.Equal(t, DeleteOutcomeDeleted, result.LocalRef)
	require.Len(t, result.Targets, 1)
	assert.Equal(t, DeleteOutcomeDeleted, result.Targets[0].Ref)

	assert.NotContains(t, testutil.RunGit(t, f.workDir, "for-each-ref", "--format=%(refname)"), ref.String())
	assert.NotContains(t, f.remoteRefs(t), ref.String())
	queued, err := queue.Peek()
	require.NoError(t, err)
	assert.NotContains(t, queued, ref, "the deleted ref must leave the push queue")
	st, err = LoadSessionState(t.Context(), "sess-1")
	require.NoError(t, err)
	assert.Empty(t, st.LastCheckpointID)
	deleted, err := checkpoint.LoadDeletedCheckpoints(t.Context())
	require.NoError(t, err)
	assert.True(t, deleted.Contains(cid))
	otherRef, err := checkpoint.RefName(other)
	require.NoError(t, err)
	assert.Contains(t, testutil.RunGit(t, f.workDir, "for-each-ref", "--format=%(refname)"), otherRef.String(),
		"other checkpoints of the same session are only reported, never deleted")
}

func TestPlanCheckpointDelete_NotFound(t *testing.T) {
	newDeleteFixture(t, "git-refs")
	_, err := PlanCheckpointDelete(t.Context(), id.MustCheckpointID("01K6ZQ2M8E3V7R5T9Y4X6W2A1B"))
	require.ErrorIs(t, err, ErrCheckpointDeleteNotFound)
}

// A remote-only checkpoint (already gone locally) is still deletable; the
// lease comes from the probe.
func TestCheckpointDelete_RemoteOnly(t *testing.T) {
	f := newDeleteFixture(t, "git-refs")
	cid := id.MustCheckpointID("01K6ZQ2M8E3V7R5T9Y4X6W2A1B")
	f.writeCheckpoint(t, cid, "sess-1")
	ref, err := checkpoint.RefName(cid)
	require.NoError(t, err)
	testutil.RunGit(t, f.workDir, "push", "--no-verify", "origin", ref.String()+":"+ref.String())
	testutil.RunGit(t, f.workDir, "update-ref", "-d", ref.String())

	plan, err := PlanCheckpointDelete(t.Context(), cid)
	require.NoError(t, err)
	assert.False(t, plan.HasLocalCopy())
	result, err := ExecuteCheckpointDelete(t.Context(), plan, CheckpointDeleteOptions{Targets: plan.HolderTargets()})
	require.NoError(t, err)
	assert.Equal(t, DeleteOutcomeAbsent, result.LocalRef)
	assert.NotContains(t, f.remoteRefs(t), ref.String())
}

// A remote that rejects the delete is reported per target; the local copy is
// still gone (no restore) and nothing re-queues it.
func TestCheckpointDelete_RejectedTargetIsReported(t *testing.T) {
	f := newDeleteFixture(t, "git-refs")
	cid := id.MustCheckpointID("01K6ZQ2M8E3V7R5T9Y4X6W2A1B")
	f.writeCheckpoint(t, cid, "sess-1")
	ref, err := checkpoint.RefName(cid)
	require.NoError(t, err)
	testutil.RunGit(t, f.workDir, "push", "--no-verify", "origin", ref.String()+":"+ref.String())
	require.NoError(t, os.WriteFile(filepath.Join(f.bareDir, "hooks", "pre-receive"),
		[]byte("#!/bin/sh\necho 'checkpoint deletes are not allowed' >&2\nexit 1\n"), 0o755))

	plan, err := PlanCheckpointDelete(t.Context(), cid)
	require.NoError(t, err)
	result, err := ExecuteCheckpointDelete(t.Context(), plan, CheckpointDeleteOptions{Targets: plan.HolderTargets()})
	require.NoError(t, err)
	assert.True(t, result.Failed())
	assert.Equal(t, DeleteOutcomeFailed, result.Targets[0].Ref)
	assert.Contains(t, result.Targets[0].Error, "rejected by the remote")
	assert.Equal(t, DeleteOutcomeDeleted, result.LocalRef, "the local delete is not rolled back")
	assert.Contains(t, f.remoteRefs(t), ref.String())
}

func TestCheckpointDelete_LocalOnlyLeavesRemote(t *testing.T) {
	f := newDeleteFixture(t, "git-refs")
	cid := id.MustCheckpointID("01K6ZQ2M8E3V7R5T9Y4X6W2A1B")
	f.writeCheckpoint(t, cid, "sess-1")
	ref, err := checkpoint.RefName(cid)
	require.NoError(t, err)
	testutil.RunGit(t, f.workDir, "push", "--no-verify", "origin", ref.String()+":"+ref.String())

	plan, err := PlanCheckpointDelete(t.Context(), cid)
	require.NoError(t, err)
	result, err := ExecuteCheckpointDelete(t.Context(), plan, CheckpointDeleteOptions{Targets: plan.HolderTargets(), LocalOnly: true})
	require.NoError(t, err)
	assert.Empty(t, result.Targets)
	assert.Equal(t, DeleteOutcomeDeleted, result.LocalRef)
	assert.Contains(t, f.remoteRefs(t), ref.String())
}

// The remote's v1 branch can be ahead of this clone, carrying a session another
// clone added under the same ID. The delete removes the whole subtree on the
// remote's own tip, and never pushes this clone's unpushed v1 commits.
func TestCheckpointDelete_V1RemoteAheadAndUnpushedLocalCommits(t *testing.T) {
	f := newDeleteFixture(t, "git-branch")
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	f.writeCheckpoint(t, cid, "sess-1")
	testutil.RunGit(t, f.workDir, "push", "--no-verify", "origin", "entire/checkpoints/v1")

	// Another clone adds a second session under the same checkpoint.
	cloneDir := t.TempDir()
	testutil.RunGit(t, cloneDir, "clone", "--no-checkout", f.bareDir, ".")
	testutil.RunGit(t, cloneDir, "config", "user.email", "o@example.com")
	testutil.RunGit(t, cloneDir, "config", "user.name", "Other")
	testutil.RunGit(t, cloneDir, "fetch", "origin", "entire/checkpoints/v1:entire/checkpoints/v1")
	extraTree := addFileToBranchTree(t, cloneDir, "entire/checkpoints/v1", cid.Path()+"/1/full.jsonl", "other clone's session")
	extraCommit := strings.TrimSpace(testutil.RunGit(t, cloneDir, "commit-tree", extraTree, "-p", "entire/checkpoints/v1", "-m", "other clone"))
	testutil.RunGit(t, cloneDir, "push", "--no-verify", "origin", extraCommit+":refs/heads/entire/checkpoints/v1")

	// This clone has an unpushed, unrelated v1 commit.
	unpushed := id.MustCheckpointID("b2c3d4e5f6a1")
	f.writeCheckpoint(t, unpushed, "sess-2")

	plan, err := PlanCheckpointDelete(t.Context(), cid)
	require.NoError(t, err)
	assert.True(t, plan.LocalV1)
	holders := plan.HolderTargets()
	require.Len(t, holders, 1)
	assert.Equal(t, V1CopyUnknown, holders[0].V1, "the remote tip is not local, so presence is verified at delete time")

	result, err := ExecuteCheckpointDelete(t.Context(), plan, CheckpointDeleteOptions{Targets: holders})
	require.NoError(t, err)
	require.False(t, result.Failed(), "%+v", result.Targets)
	assert.Equal(t, DeleteOutcomeDeleted, result.LocalV1)
	assert.Equal(t, DeleteOutcomeDeleted, result.Targets[0].V1)

	remoteTree := testutil.RunGit(t, f.bareDir, "ls-tree", "-r", "--name-only", "entire/checkpoints/v1")
	assert.NotContains(t, remoteTree, cid.Path()+"/", "the whole subtree, including the other clone's session, is gone")
	assert.NotContains(t, remoteTree, unpushed.Path()+"/", "unpushed local v1 commits must not be pushed by a delete")
	localTree := testutil.RunGit(t, f.workDir, "ls-tree", "-r", "--name-only", "entire/checkpoints/v1")
	assert.NotContains(t, localTree, cid.Path()+"/")
	assert.Contains(t, localTree, unpushed.Path()+"/")
}

// addFileToBranchTree returns a tree hash: branch's tree plus one file.
func addFileToBranchTree(t *testing.T, dir, branch, path, content string) string {
	t.Helper()
	indexFile := filepath.Join(t.TempDir(), "index")
	env := append(testutil.GitIsolatedEnv(), "GIT_INDEX_FILE="+indexFile)
	run := func(stdin string, args ...string) string {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = env
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	run("", "read-tree", branch)
	blob := run(content, "hash-object", "-w", "--stdin")
	run("", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+path)
	return run("", "write-tree")
}

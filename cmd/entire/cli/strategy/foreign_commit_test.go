package strategy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// foreignCommitFixture is a session homed in A, identified by its caller
// variable, whose live Claude transcript writes <B>/b.txt by absolute path,
// with b.txt staged in B (the CWD).
func foreignCommitFixture(t *testing.T, mutate func(*SessionState)) foreignRepos {
	t.Helper()
	r := newForeignRepos(t)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	transcript := filepath.Join(t.TempDir(), foreignTestSessionID+".jsonl")
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"` +
		filepath.Join(r.b, "b.txt") + `","content":"x"}}]}}`
	require.NoError(t, os.WriteFile(transcript, []byte(`{"type":"human","message":{"content":"make b"}}`+"\n"+line+"\n"), 0o600))
	r.saveForeignState(t, func(s *SessionState) {
		s.TranscriptPath = transcript
		if mutate != nil {
			mutate(s)
		}
	})
	t.Setenv("CLAUDE_CODE_SESSION_ID", foreignTestSessionID)
	testutil.WriteFile(t, r.b, "b.txt", "b\n")
	testutil.GitAdd(t, r.b, "b.txt")
	return r
}

// prepareForeign runs PrepareCommitMsg on text and returns the message.
func prepareForeign(t *testing.T, text, source string) string {
	t.Helper()
	msgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(msgFile, []byte(text), 0o600))
	require.NoError(t, NewManualCommitStrategy().PrepareCommitMsg(context.Background(), msgFile, source))
	got, err := os.ReadFile(msgFile)
	require.NoError(t, err)
	return string(got)
}

// commitForeign prepares, commits and post-commits text in B, returning the
// stamped checkpoint ID (empty when none was stamped).
func commitForeign(t *testing.T, r foreignRepos, text string) id.CheckpointID {
	t.Helper()
	msg := prepareForeign(t, text, commitSourceMessage)
	testutil.GitCommit(t, r.b, msg)
	require.NoError(t, NewManualCommitStrategy().PostCommit(context.Background()))
	cpID, _ := trailers.ParseCheckpoint(msg)
	return cpID
}

func loadForeignTestState(t *testing.T, r foreignRepos) *SessionState {
	t.Helper()
	state, err := r.aStore(t).LoadReadOnly(context.Background(), foreignTestSessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	return state
}

// readBCheckpoint returns the checkpoint's content in B's store, or nil.
func readBCheckpoint(t *testing.T, r foreignRepos, cpID id.CheckpointID) *checkpoint.SessionContent {
	t.Helper()
	repo, err := gitrepo.OpenPath(r.b)
	require.NoError(t, err)
	defer repo.Close()
	content, err := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs()).ReadSessionContent(context.Background(), cpID, 0)
	if err != nil {
		return nil
	}
	return content
}

func TestPrepareCommitMsg_ForeignAgentCommit(t *testing.T) {
	t.Run("stamped", func(t *testing.T) {
		foreignCommitFixture(t, nil)
		_, found := trailers.ParseCheckpoint(prepareForeign(t, "add b\n", commitSourceMessage))
		assert.True(t, found)
	})
	t.Run("stamped beside a local session without content", func(t *testing.T) {
		r := foreignCommitFixture(t, nil)
		saveIdentitySession(t, "local-empty", func(st *SessionState) { st.WorktreePath = r.b })
		_, found := trailers.ParseCheckpoint(prepareForeign(t, "add b\n", commitSourceMessage))
		assert.True(t, found)
	})
	t.Run("terminal is never prompted", func(t *testing.T) {
		foreignCommitFixture(t, nil)
		t.Setenv("ENTIRE_TEST_TTY", "1")
		assert.NotContains(t, prepareForeign(t, "add b\n", commitSourceMessage), trailers.CheckpointTrailerKey)
	})
	t.Run("terminal under commit_linking always", func(t *testing.T) {
		foreignCommitFixture(t, nil)
		t.Setenv("ENTIRE_TEST_TTY", "1")
		require.NoError(t, saveCommitLinkingAlways(context.Background()))
		assert.Contains(t, prepareForeign(t, "add b\n", commitSourceMessage), trailers.CheckpointTrailerKey)
	})
	t.Run("amend", func(t *testing.T) {
		foreignCommitFixture(t, nil)
		assert.Equal(t, "amended\n", prepareForeign(t, "amended\n", "commit"))
	})
	t.Run("message already stamped", func(t *testing.T) {
		foreignCommitFixture(t, nil)
		text := "typed\n\nEntire-Checkpoint: aabbccdd0003\n"
		assert.Equal(t, text, prepareForeign(t, text, commitSourceMessage))
	})
}

func TestPostCommit_ForeignAgentCommitCondensed(t *testing.T) {
	r := foreignCommitFixture(t, func(s *SessionState) {
		s.LastCheckpointID = id.MustCheckpointID("a0a0a0a0a0a0")
		s.FilesTouched = []string{"README.md"} // A's path; B's commit has one too
		s.TurnCheckpointIDs = []string{"a0a0a0a0a0a0"}
	})
	testutil.WriteFile(t, r.b, "README.md", "b readme\n")
	testutil.GitAdd(t, r.b, "README.md")
	before := loadForeignTestState(t, r)

	cpID := commitForeign(t, r, "add b\n")
	require.False(t, cpID.IsEmpty())

	content := readBCheckpoint(t, r, cpID)
	require.NotNil(t, content)
	assert.Equal(t, foreignTestSessionID, content.Metadata.SessionID)
	assert.Equal(t, []string{"b.txt"}, content.Metadata.FilesTouched, "only what the transcript wrote under B")

	after := loadForeignTestState(t, r)
	assert.Greater(t, after.CheckpointTranscriptStart, before.CheckpointTranscriptStart, "the transcript window advanced")
	assert.Equal(t, before.LastCheckpointID, after.LastCheckpointID)
	assert.Equal(t, before.BaseCommit, after.BaseCommit)
	assert.Equal(t, before.FilesTouched, after.FilesTouched)
	assert.Equal(t, before.TurnCheckpointIDs, after.TurnCheckpointIDs)
	assert.False(t, after.HomeNotHere)
}

// A trailer typed by hand has no marker and is not condensed.
func TestPostCommit_ForeignTrailerWithoutMarker(t *testing.T) {
	r := foreignCommitFixture(t, nil)
	cpID := id.MustCheckpointID("aabbccdd0005")
	testutil.GitCommit(t, r.b, "typed\n\nEntire-Checkpoint: "+cpID.String()+"\n")
	require.NoError(t, NewManualCommitStrategy().PostCommit(context.Background()))
	assert.Nil(t, readBCheckpoint(t, r, cpID))
}

// A's lock held by A's own work: the commit gives up within the bound and is
// left unlinked rather than blocked.
func TestPostCommit_ForeignLockHeld(t *testing.T) {
	r := foreignCommitFixture(t, nil)
	before := loadForeignTestState(t, r)
	lock, err := stateLockInCommonDir(filepath.Join(r.a, ".git"), foreignTestSessionID)
	require.NoError(t, err)
	held, err := flock.AcquireIn(lock.root, lock.name)
	require.NoError(t, err)
	defer held()

	start := time.Now()
	cpID := commitForeign(t, r, "add b\n")
	assert.Less(t, time.Since(start), 10*time.Second)
	require.False(t, cpID.IsEmpty())
	assert.Nil(t, readBCheckpoint(t, r, cpID))
	assert.Equal(t, before.CheckpointTranscriptStart, loadForeignTestState(t, r).CheckpointTranscriptStart)
}

// After the B commit, a trailer-less amend at home restores A's own ID, not B's.
func TestForeignAgentCommit_HomeAmendKeepsOwnCheckpoint(t *testing.T) {
	homeID := id.MustCheckpointID("a0a0a0a0a0a1")
	var aHead string
	r := foreignCommitFixture(t, func(s *SessionState) {
		s.LastCheckpointID = homeID
		aHead = strings.TrimSpace(testutil.RunGit(t, s.WorktreePath, "rev-parse", "HEAD"))
		s.BaseCommit = aHead
	})
	require.False(t, commitForeign(t, r, "add b\n").IsEmpty())

	t.Chdir(r.a)
	paths.ClearWorktreeRootCache()
	cpID, found := trailers.ParseCheckpoint(prepareForeign(t, "home amend\n", "commit"))
	require.True(t, found)
	assert.Equal(t, homeID, cpID)
	assert.Equal(t, aHead, loadForeignTestState(t, r).BaseCommit)
}

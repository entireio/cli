package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A mid-turn commit of an ACTIVE session with no turn-end step yet takes its
// files from the transcript (resolveFilesTouched), and carry-forward writes
// what remains into FilesTouched. A gitignored file or a submodule path the
// transcript names must not survive that route either: neither can ever be
// committed, so it would keep the session pending forever.
// Uses t.Chdir — do NOT add t.Parallel().
func TestPostCommit_MidTurnCarryForward_DropsIgnoredAndSubmodulePaths(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	testutil.WriteFile(t, dir, ".gitignore", "ignored.env\n")
	testutil.GitAdd(t, dir, ".gitignore")
	testutil.GitCommit(t, dir, "ignore rules")

	subSrc := t.TempDir()
	testutil.InitRepo(t, subSrc)
	testutil.WriteFile(t, subSrc, "lib.txt", "v1\n")
	testutil.GitAdd(t, subSrc, "lib.txt")
	testutil.GitCommit(t, subSrc, "lib v1")
	testutil.RunGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subSrc, "sub")
	testutil.RunGit(t, dir, "commit", "-q", "-m", "add submodule")

	worktreePath, err := paths.WorktreeRoot(context.Background())
	require.NoError(t, err)
	for _, name := range []string{"committed.txt", "pending.txt", "ignored.env"} {
		testutil.WriteFile(t, dir, name, name+"\n")
	}
	writeLine := func(path string) string {
		return `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"` +
			filepath.Join(worktreePath, path) + `","content":"x"}}]}}` + "\n"
	}
	transcript := `{"type":"human","message":{"content":"write files"}}` + "\n" +
		writeLine("committed.txt") + writeLine("pending.txt") + writeLine("ignored.env") + writeLine("sub")
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(transcript), 0o644))
	stale := time.Now().Add(-3 * time.Minute)
	require.NoError(t, os.Chtimes(transcriptPath, stale, stale))

	s := &ManualCommitStrategy{}
	now := time.Now()
	head := testutil.GetHeadHash(t, dir)
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	require.NoError(t, err)
	sessionID := "test-midturn-untrackable"
	require.NoError(t, s.saveSessionState(context.Background(), &SessionState{
		SessionID:           sessionID,
		BaseCommit:          head,
		WorktreePath:        worktreePath,
		WorktreeID:          worktreeID,
		StartedAt:           now,
		Phase:               session.PhaseActive,
		LastInteractionTime: &now,
		AgentType:           agent.AgentTypeClaudeCode,
		TranscriptPath:      transcriptPath,
	}))

	testutil.GitAdd(t, dir, "committed.txt")
	testutil.GitCommit(t, dir, "commit one file\n\n"+trailers.CheckpointTrailerKey+": "+"ef12ab34cd56")
	require.NoError(t, s.PostCommit(context.Background()))

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Contains(t, state.FilesTouched, "pending.txt", "fixture: carry-forward ran for the uncommitted file")
	assert.NotContains(t, state.FilesTouched, "ignored.env")
	assert.NotContains(t, state.FilesTouched, "sub")
}

// Deleted paths are never ignore-checked (git check-ignore reports a path
// whose deletion is staged); modified and added ones are. Uses t.Chdir — do
// NOT add t.Parallel().
func TestFilterTrackableChanges_IgnoreRulesSkipDeletions(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	testutil.WriteFile(t, dir, ".gitignore", "*.log\n")
	testutil.WriteFile(t, dir, "tracked.log", "x\n")
	testutil.GitAdd(t, dir, ".gitignore")
	testutil.GitAddForce(t, dir, "tracked.log")
	testutil.GitCommit(t, dir, "track a log")
	testutil.RunGit(t, dir, "rm", "-q", "tracked.log")

	modified, added, deleted := FilterTrackableChanges(context.Background(), dir,
		[]string{"src.go", "debug.log"}, []string{"new.log", "new.go"}, []string{"tracked.log"})
	assert.Equal(t, []string{"src.go"}, modified)
	assert.Equal(t, []string{"new.go"}, added)
	assert.Equal(t, []string{"tracked.log"}, deleted)
}

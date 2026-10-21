package strategy

import (
	"cmp"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Uses t.Setenv and t.Chdir — do NOT add t.Parallel().

// claudeProjectsDir points Claude's config dir at a temp dir (clearing the
// test override that ignores the repo path) and returns a function giving the
// session file Claude writes for a session working in dir.
func claudeProjectsDir(t *testing.T) func(dir, sessionID string) string {
	t.Helper()
	config := resolvedTempDir(t)
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", "")
	return func(dir, sessionID string) string {
		return filepath.Join(config, "projects", claudecode.SanitizePathForClaude(dir), sessionID+".jsonl")
	}
}

func writeTranscriptAt(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(testTranscriptPromptResponse), 0o600))
}

// Claude Code keys its session dir by working directory: a session started in
// the main checkout that moves into a worktree (EnterWorktree) writes its
// transcript under the worktree's dir, and the path its hooks reported at
// session start never exists. The transcript is found there, from the
// worktree and from the main checkout (after ExitWorktree), and the state
// follows it — also for a session launched from a subdirectory, whichever
// directory of the worktree it moves to.
func TestResolveTranscriptPath_FollowsTranscriptMovedToAnotherWorktree(t *testing.T) {
	for _, tc := range []struct{ name, from, launchedIn, movedTo string }{
		{name: "from the worktree", from: "worktree"},
		{name: "from the main checkout", from: "main"},
		{name: "launched in a subdirectory, moved to the worktree root", from: "worktree", launchedIn: "frontend"},
		{name: "launched in a subdirectory, moved to the same subdirectory", from: "worktree", launchedIn: "frontend", movedTo: "frontend"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mainDir, linkedDir := twoWorktrees(t)
			sessionFile := claudeProjectsDir(t)
			recorded := sessionFile(filepath.Join(mainDir, tc.launchedIn), "sess-moved")
			moved := sessionFile(filepath.Join(linkedDir, tc.movedTo), "sess-moved")
			writeTranscriptAt(t, moved)
			if tc.from == "worktree" {
				t.Chdir(linkedDir)
			} else {
				t.Chdir(mainDir)
			}
			paths.ClearWorktreeRootCache()
			t.Cleanup(paths.ClearWorktreeRootCache)

			state := &SessionState{SessionID: "sess-moved", AgentType: agent.AgentTypeClaudeCode, WorktreePath: mainDir, TranscriptPath: recorded}
			got, err := resolveTranscriptPath(t.Context(), state)
			require.NoError(t, err)
			assert.Equal(t, moved, got)
			assert.Equal(t, moved, state.TranscriptPath, "the state follows the transcript")
		})
	}
}

// Only a path the agent derived for the session's home is second-guessed, and
// a session whose file is in no worktree's dir still reports it missing.
func TestResolveTranscriptPath_MovedLookupLeavesOtherPathsAlone(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	sessionFile := claudeProjectsDir(t)
	t.Chdir(linkedDir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	t.Run("not in any worktree's dir", func(t *testing.T) {
		state := &SessionState{SessionID: "sess-gone", AgentType: agent.AgentTypeClaudeCode, WorktreePath: mainDir, TranscriptPath: sessionFile(mainDir, "sess-gone")}
		_, err := resolveTranscriptPath(t.Context(), state)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
	t.Run("a recorded dir in another projects dir", func(t *testing.T) {
		writeTranscriptAt(t, sessionFile(linkedDir, "sess-elsewhere"))
		elsewhere := filepath.Join(t.TempDir(), "projects", filepath.Base(filepath.Dir(sessionFile(mainDir, "x"))), "sess-elsewhere.jsonl")
		state := &SessionState{SessionID: "sess-elsewhere", AgentType: agent.AgentTypeClaudeCode, WorktreePath: mainDir, TranscriptPath: elsewhere}
		_, err := resolveTranscriptPath(t.Context(), state)
		require.ErrorIs(t, err, os.ErrNotExist)
		assert.Equal(t, elsewhere, state.TranscriptPath)
	})
	t.Run("a recorded path the agent would not derive for the home", func(t *testing.T) {
		writeTranscriptAt(t, sessionFile(linkedDir, "sess-attached"))
		custom := filepath.Join(t.TempDir(), "sess-attached.jsonl")
		state := &SessionState{SessionID: "sess-attached", AgentType: agent.AgentTypeClaudeCode, WorktreePath: mainDir, TranscriptPath: custom}
		_, err := resolveTranscriptPath(t.Context(), state)
		require.ErrorIs(t, err, os.ErrNotExist)
		assert.Equal(t, custom, state.TranscriptPath)
	})
}

// ENT-2463 end to end, first turn: Claude Code starts in the main checkout,
// moves into a worktree and commits there mid-turn. No turn has ended, so
// there's no stored copy of the transcript either; the checkpoint the commit's
// trailer names must still be written, from the moved transcript.
func TestCommitHooks_FirstTurnCommitAfterMovingIntoWorktreeWritesItsCheckpoint(t *testing.T) {
	for _, launchedIn := range []string{"", "frontend"} {
		t.Run("launched in "+cmp.Or(launchedIn, "the repository root"), func(t *testing.T) {
			firstTurnCommitAfterMovingIntoWorktree(t, launchedIn)
		})
	}
}

func firstTurnCommitAfterMovingIntoWorktree(t *testing.T, launchedIn string) {
	t.Helper()
	ctx := context.Background()
	homeDir, err := filepath.EvalSymlinks(setupGitRepo(t))
	require.NoError(t, err)
	sessionFile := claudeProjectsDir(t)
	t.Chdir(homeDir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	s := NewManualCommitStrategy()
	head := testutil.GetHeadHash(t, homeDir)
	worktreeDir, err := filepath.EvalSymlinks(addSiblingWorktree(t, homeDir, "moved"))
	require.NoError(t, err)
	writeTranscriptAt(t, sessionFile(worktreeDir, "sess-first-turn"))
	now := time.Now()
	require.NoError(t, s.saveSessionState(ctx, &SessionState{
		SessionID:           "sess-first-turn",
		StartedAt:           now,
		LastInteractionTime: &now,
		AgentType:           agent.AgentTypeClaudeCode,
		Phase:               session.PhaseActive,
		BaseCommit:          head,
		WorktreePath:        homeDir,
		TranscriptPath:      sessionFile(filepath.Join(homeDir, launchedIn), "sess-first-turn"),
		Owner:               selfAncestorOwner(t),
	}))

	t.Chdir(worktreeDir)
	paths.ClearWorktreeRootCache()
	testutil.WriteFile(t, worktreeDir, "moved.txt", "made after EnterWorktree")
	testutil.GitAdd(t, worktreeDir, "moved.txt")
	msg := prepareMessage(t, "work in the worktree\n")
	cps := checkpointIDs(msg)
	require.Len(t, cps, 1, "the agent's commit is stamped: %q", msg)
	testutil.GitCommit(t, worktreeDir, msg)
	require.NoError(t, NewManualCommitStrategy().PostCommit(ctx))

	state, err := s.loadSessionState(ctx, "sess-first-turn")
	require.NoError(t, err)
	assert.Equal(t, cps[0], state.LastCheckpointID.String(), "the trailer's checkpoint is written")
	assert.Equal(t, sessionFile(worktreeDir, "sess-first-turn"), state.TranscriptPath, "the state follows the moved transcript")
}

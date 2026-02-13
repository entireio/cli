package strategy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode" // Register Claude Code agent for transcript analysis
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionHasNewContentFromLiveTranscript_NormalizesAbsolutePaths verifies
// that sessionHasNewContentFromLiveTranscript correctly normalizes absolute paths
// from the transcript to repo-relative paths before comparing with staged files.
//
// Bug: ExtractModifiedFilesFromOffset returns absolute paths (e.g., /tmp/repo/src/main.go)
// but getStagedFiles returns repo-relative paths (e.g., src/main.go). The exact-string
// comparison in hasOverlappingFiles never matches, causing "no content to link".
func TestSessionHasNewContentFromLiveTranscript_NormalizesAbsolutePaths(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}

	// Create a file that we'll stage
	srcDir := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	testFile := filepath.Join(srcDir, "main.go")
	require.NoError(t, os.WriteFile(testFile, []byte("package main\n"), 0o644))

	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("src/main.go")
	require.NoError(t, err)

	// Get the resolved worktree path first — on macOS, t.TempDir() returns /var/...
	// but git resolves symlinks to /private/var/... . Claude Code uses the resolved
	// path in its transcript, so we must too.
	worktreePath, err := GetWorktreePath()
	require.NoError(t, err)
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	require.NoError(t, err)

	// Create a transcript file that references the file by absolute path
	// (this is what Claude Code does — tool_use Write has absolute file_path)
	absFilePath := filepath.Join(worktreePath, "src", "main.go")
	transcriptContent := `{"type":"human","message":{"content":"write a main.go file"}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"` + absFilePath + `","content":"package main\n"}}]}}
`
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(transcriptContent), 0o644))

	// Create session state: no shadow branch (it was deleted after last condensation),
	// transcript path points to the file, agent type is Claude Code
	now := time.Now()

	head, err := repo.Head()
	require.NoError(t, err)

	state := &SessionState{
		SessionID:                 "test-abs-path-normalize",
		BaseCommit:                head.Hash().String(),
		WorktreePath:              worktreePath,
		WorktreeID:                worktreeID,
		StartedAt:                 now,
		Phase:                     session.PhaseActive,
		LastInteractionTime:       &now,
		AgentType:                 agent.AgentTypeClaudeCode,
		TranscriptPath:            transcriptPath,
		CheckpointTranscriptStart: 0, // No prior condensation
	}
	require.NoError(t, s.saveSessionState(state))

	// Call sessionHasNewContent — should fall through to live transcript check
	// since there's no shadow branch
	hasNew, err := s.sessionHasNewContent(repo, state)
	require.NoError(t, err)
	assert.True(t, hasNew,
		"sessionHasNewContent should return true when transcript has absolute paths "+
			"that match repo-relative staged files after normalization")
}

// TestPostCommit_NoTrailer_UpdatesBaseCommit verifies that when a commit has no
// Entire-Checkpoint trailer, PostCommit still updates BaseCommit for active sessions.
//
// Bug: PostCommit early-returns when no trailer is found (line ~530-536). EventGitCommit
// never fires, BaseCommit never updates. All subsequent commits fail the
// BaseCommit == currentHeadHash filter in PrepareCommitMsg.
func TestPostCommit_NoTrailer_UpdatesBaseCommit(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-postcommit-no-trailer"

	// Initialize session and save a checkpoint
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to ACTIVE
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(state))

	originalBaseCommit := state.BaseCommit

	// Create a commit WITHOUT a trailer (user removed it, or mid-turn commit
	// where PrepareCommitMsg couldn't add one due to Bug 1)
	testFile := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("no trailer commit"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	_, err = wt.Commit("commit without trailer", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	})
	require.NoError(t, err)

	// Get the new HEAD
	head, err := repo.Head()
	require.NoError(t, err)
	newHeadHash := head.Hash().String()
	require.NotEqual(t, originalBaseCommit, newHeadHash, "HEAD should have changed")

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// Verify BaseCommit was updated to new HEAD
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, newHeadHash, state.BaseCommit,
		"BaseCommit should be updated to new HEAD even when commit has no trailer")

	// Phase should stay ACTIVE (no state machine transition, just BaseCommit update)
	assert.Equal(t, session.PhaseActive, state.Phase,
		"Phase should remain ACTIVE when commit has no trailer")
}

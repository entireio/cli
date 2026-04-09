package strategy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostCommit_Issue591_SubagentScaleRegression is a regression test for GitHub
// issue #591: accumulated stale ENDED sessions caused O(N) overhead on every commit
// (~73-103ms per session, indefinitely). After the fix, sessions with FilesTouched
// are condensed on the first overlapping PostCommit and then marked FullyCondensed,
// so all subsequent commits skip them entirely.
//
// Behavioral coverage (FullyCondensed/StepCount/FilesTouched assertions) lives in
// TestPostCommit_EndedSessionCarryForward_NotCondensedIntoUnrelatedCommit. This test
// focuses on the performance contract: the second PostCommit must be significantly
// faster than the first once sessions are marked FullyCondensed.
func TestPostCommit_Issue591_SubagentScaleRegression(t *testing.T) {
	const sessionCount = 10

	dir := setupGitRepo(t)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}

	// Create sessions with shadow branches, then mark them ENDED with FilesTouched.
	// FilesTouched prevents eager-condense-on-stop (CondenseAndMarkFullyCondensed
	// skips sessions with FilesTouched), so these sessions remain for PostCommit.
	for i := range sessionCount {
		sessionID := fmt.Sprintf("ended-session-%d", i)
		setupSessionWithCheckpoint(t, s, repo, dir, sessionID)
		state, err := s.loadSessionState(context.Background(), sessionID)
		require.NoError(t, err)
		endedAt := time.Now().Add(-2 * time.Hour)
		state.Phase = session.PhaseEnded
		state.EndedAt = &endedAt
		// FilesTouched = ["test.txt"] — triggers overlap path when test.txt is committed.
		state.FilesTouched = []string{"test.txt"}
		require.NoError(t, s.saveSessionState(context.Background(), state))
	}

	// Commit test.txt — overlaps with each session's FilesTouched.
	// First PostCommit condenses all N sessions and marks them FullyCondensed.
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("session work"), 0o644))
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	_, err = wt.Commit("commit session work\n\nEntire-Checkpoint: a1b2c3d4e5f6\n", &git.CommitOptions{
		Author: &object.Signature{Name: "User", Email: "user@test.com", When: time.Now()},
	})
	require.NoError(t, err)
	paths.ClearWorktreeRootCache()

	firstStart := time.Now()
	require.NoError(t, s.PostCommit(context.Background()))
	firstElapsed := time.Since(firstStart)

	// Commit an unrelated file — all sessions are now FullyCondensed and skipped.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("unrelated"), 0o644))
	_, err = wt.Add("unrelated.txt")
	require.NoError(t, err)
	_, err = wt.Commit("unrelated work\n\nEntire-Checkpoint: b1b2b3b4b5b6\n", &git.CommitOptions{
		Author: &object.Signature{Name: "User", Email: "user@test.com", When: time.Now()},
	})
	require.NoError(t, err)
	paths.ClearWorktreeRootCache()

	s2 := &ManualCommitStrategy{}
	secondStart := time.Now()
	require.NoError(t, s2.PostCommit(context.Background()))
	secondElapsed := time.Since(secondStart)

	// Second PostCommit must be significantly faster — sessions are FullyCondensed and skipped.
	assert.Less(t, secondElapsed, firstElapsed/2,
		"second PostCommit should be much faster once sessions are FullyCondensed (issue #591 regression)")
	t.Logf("first PostCommit (%d ended sessions, overlap condense): %v, second (all skipped): %v",
		sessionCount, firstElapsed, secondElapsed)
}

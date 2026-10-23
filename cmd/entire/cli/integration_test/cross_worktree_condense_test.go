//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Uses t.Chdir to exercise the strategy entry point shared by sweep and doctor.
func TestCrossWorktreeCondenseSessionByID(t *testing.T) {
	for _, tc := range []struct {
		name         string
		removed      bool
		malformed    bool
		detached     bool
		switchBranch bool
		wantBranch   string
	}{
		{name: "existing worktree", wantBranch: "wt/session-a"},
		{name: "removed worktree", removed: true, wantBranch: "wt/session-a"},
		{name: "malformed worktree metadata", malformed: true, wantBranch: "wt/session-a"},
		{name: "detached session", detached: true},
		{name: "branch switched during turn", switchBranch: true, wantBranch: "wt/during-turn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := NewFeatureBranchEnv(t)
			home := worktreeEnv(t, caller, "session-a")
			if tc.detached {
				testutil.RunGit(t, home.RepoDir, "checkout", "--detach")
			}
			if tc.switchBranch {
				home.GitCheckoutNewBranch("main")
			}
			sess := home.NewSession()
			require.NoError(t, home.SimulateUserPromptSubmit(sess.ID))
			if tc.switchBranch {
				started, err := caller.GetSessionState(sess.ID)
				require.NoError(t, err)
				require.NotNil(t, started)
				require.Equal(t, "main", started.Branch)
				home.GitCheckoutNewBranch(tc.wantBranch)
			}
			home.WriteFile("pending.go", "package pending\n")
			sess.CreateTranscript("Create pending work", []FileChange{{Path: "pending.go", Content: "package pending\n"}})
			require.NoError(t, home.SimulateStop(sess.ID, sess.TranscriptPath))

			// Leave the session ended with pending files, as when its eager
			// session-end condensation did not run or was interrupted.
			state, err := caller.GetSessionState(sess.ID)
			require.NoError(t, err)
			require.NotNil(t, state)
			assert.Equal(t, tc.wantBranch, state.Branch)
			require.Equal(t, home.RepoDir, state.WorktreePath)
			require.True(t, state.HasPendingWork())
			endedAt := time.Now().UTC()
			state.Phase = session.PhaseEnded
			state.EndedAt = &endedAt
			require.NoError(t, caller.WriteSessionState(sess.ID, state))

			if tc.removed {
				testutil.RunGit(t, caller.RepoDir, "worktree", "remove", "--force", home.RepoDir)
			} else {
				// Even the home worktree's current HEAD is not authoritative:
				// the session recorded its branch before this checkout.
				home.GitCheckoutNewBranch("wt/after-session")
			}
			if tc.malformed {
				require.NoError(t, os.WriteFile(filepath.Join(home.RepoDir, ".git"), []byte("invalid metadata\n"), 0o600))
			}
			t.Chdir(caller.RepoDir)
			require.NoError(t, strategy.NewManualCommitStrategy().CondenseSessionByID(t.Context(), sess.ID))

			state, err = caller.GetSessionState(sess.ID)
			require.NoError(t, err)
			require.NotNil(t, state)
			require.False(t, state.LastCheckpointID.IsEmpty())
			data, found := caller.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(state.LastCheckpointID.String()))
			require.True(t, found, "condensation must write checkpoint metadata")
			var summary checkpoint.CheckpointSummary
			require.NoError(t, json.Unmarshal([]byte(data), &summary))
			assert.Equal(t, tc.wantBranch, summary.Branch, "checkpoint must use the target session's recorded branch, not either worktree's HEAD")
			assert.Equal(t, []string{"pending.go"}, summary.FilesTouched)
		})
	}
}

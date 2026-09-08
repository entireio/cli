package strategy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/stretchr/testify/require"
)

func TestMetadataHookTrace(t *testing.T) {
	// Trace2 and worktree discovery use process-global state.
	testutil.IsolateGitConfigEnv(t)
	for _, active := range []bool{false, true} {
		name := "no-session"
		if active {
			name = "active-session"
		}
		t.Run(name, func(t *testing.T) {
			dir := setupGitRepo(t)
			t.Chdir(dir)
			clearSessionMatchCaches()
			repo, err := gitrepo.OpenPath(dir)
			require.NoError(t, err)
			defer repo.Close()
			s := &ManualCommitStrategy{}
			if active {
				setupSessionWithCheckpoint(t, s, repo, dir, "trace-session")
				state, err := s.loadSessionState(t.Context(), "trace-session")
				require.NoError(t, err)
				state.Phase = session.PhaseActive
				require.NoError(t, s.saveSessionState(t.Context(), state))
				testutil.GitAdd(t, dir, "test.txt")
			}
			message := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
			require.NoError(t, os.WriteFile(message, []byte("trace commit\n"), 0o600))
			s = &ManualCommitStrategy{}
			traceHookOperation(t, "prepare-commit-msg", func() error {
				return s.PrepareCommitMsg(t.Context(), message, "")
			})
			content, err := os.ReadFile(message)
			require.NoError(t, err)
			_, linked := trailers.ParseCheckpoint(string(content))
			require.Equal(t, active, linked)
			if active {
				testutil.GitCommit(t, dir, string(content))
			}
			s = &ManualCommitStrategy{}
			traceHookOperation(t, "post-commit", func() error { return s.PostCommit(t.Context()) })
			if active {
				state, err := s.loadSessionState(t.Context(), "trace-session")
				require.NoError(t, err)
				require.NotNil(t, state)
				require.Zero(t, state.StepCount)
				require.False(t, state.LastCheckpointID.IsEmpty())
			}
		})
	}
}

func traceHookOperation(t *testing.T, name string, operation func() error) {
	t.Helper()
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	gitdir.ClearCache()
	tracePath := filepath.Join(t.TempDir(), "git-trace.jsonl")
	t.Setenv("GIT_TRACE2_EVENT", tracePath)
	start := time.Now()
	require.NoError(t, operation())
	elapsed := time.Since(start)
	data, err := os.ReadFile(tracePath)
	require.NoError(t, err)
	var commands [][]string
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var event struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &event))
		if event.Event == "start" {
			require.NotContains(t, event.Argv, "--git-dir", "hooks obtain per-worktree metadata without spawning Git")
			commands = append(commands, event.Argv)
		}
	}
	require.NotEmpty(t, commands, "Trace2 must observe the operation's Git queries")
	t.Logf("%s: %d Git subprocesses, %s; argv=%q", name, len(commands), elapsed, commands)
	t.Setenv("GIT_TRACE2_EVENT", "")
}

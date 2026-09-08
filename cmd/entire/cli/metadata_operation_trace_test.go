package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestMetadataCompleteOperationTrace(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	repoRoot := setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)
	writeClaudeHooksFixture(t)
	repo, err := gitrepo.OpenPath(repoRoot)
	require.NoError(t, err)
	defer repo.Close()
	message := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(message, []byte("trace commit\n"), 0o600))
	for _, tc := range []struct {
		name      string
		operation func(t *testing.T)
	}{
		{"session-cwd", func(t *testing.T) {
			store, err := session.NewStateStore(t.Context())
			require.NoError(t, err)
			require.NotNil(t, store)
		}},
		{"session-explicit", func(t *testing.T) {
			store, err := session.NewStateStoreForWorktree(t.Context(), repoRoot)
			require.NoError(t, err)
			require.NotNil(t, store)
		}},
		{"checkpoint-queue", func(t *testing.T) {
			queue, err := checkpoint.PushQueueForRepo(t.Context(), repo)
			require.NoError(t, err)
			require.NotNil(t, queue)
		}},
		{"prepare-commit", func(t *testing.T) {
			s := &strategy.ManualCommitStrategy{}
			require.NoError(t, s.PrepareCommitMsg(t.Context(), message, ""))
			content, err := os.ReadFile(message)
			require.NoError(t, err)
			require.Equal(t, "trace commit\n", string(content))
		}},
		{"status", func(t *testing.T) {
			var output bytes.Buffer
			require.NoError(t, runStatus(t.Context(), &output, false, false))
			require.Contains(t, output.String(), "Enabled")
		}},
		{"codex-discovery", func(t *testing.T) {
			discovery := codex.ResolveHookDiscovery(t.Context())
			require.Equal(t, codex.HookDiscoveryResolved, discovery.State)
			require.NoError(t, discovery.Diagnostic)
			canonicalRoot, err := filepath.EvalSymlinks(repoRoot)
			require.NoError(t, err)
			require.Equal(t, filepath.Join(canonicalRoot, ".codex", "hooks.json"), discovery.DiscoveredHooks.Path())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths.ClearWorktreeRootCache()
			gitdir.Reset()
			t.Cleanup(paths.ClearWorktreeRootCache)
			t.Cleanup(gitdir.Reset)
			tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
			t.Setenv("GIT_TRACE2_EVENT", tracePath)
			tc.operation(t)
			commands := metadataOperationCommands(t, tracePath)
			want := map[string]int{"session-cwd": 1, "session-explicit": 0, "checkpoint-queue": 0, "prepare-commit": 2, "status": 4, "codex-discovery": 1}
			require.Len(t, commands, want[tc.name], "complete-operation Git subprocess budget; argv=%q", commands)
			t.Logf("complete operation: %d Git subprocesses; argv=%q", len(commands), commands)
			testutil.RunGit(t, repoRoot, "rev-parse", "--show-toplevel")
			require.Len(t, metadataOperationCommands(t, tracePath), len(commands)+1, "positive control must add a real Git start event")
		})
	}
}

func metadataOperationCommands(t *testing.T, tracePath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(tracePath)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var commands [][]string
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var event struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &event))
		if event.Event == "start" {
			commands = append(commands, event.Argv)
		}
	}
	return commands
}

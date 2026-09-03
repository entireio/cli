package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestGitMetadataOperationsDoNotAddGitSubprocesses(t *testing.T) {
	// Git tracing and CWD are process-global.
	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	testutil.WriteFile(t, repoRoot, "initial.txt", "initial\n")
	testutil.GitAdd(t, repoRoot, "initial.txt")
	testutil.GitCommit(t, repoRoot, "initial")
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".entire"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, EntireSettingsFile), []byte("{\"enabled\":false}\n"), 0o600))
	t.Chdir(repoRoot)

	tracePath := filepath.Join(t.TempDir(), "git-trace.jsonl")
	t.Setenv("GIT_TRACE2_EVENT", tracePath)
	ctx := context.Background()

	t.Run("session state construction from CWD", func(t *testing.T) {
		starts := traceGitStarts(t, tracePath, func() {
			paths.ClearWorktreeRootCache()
			_, err := session.NewStateStore(ctx)
			require.NoError(t, err)
		})
		requireGitStartCount(t, starts, 1, 2, 0)
	})

	t.Run("checkpoint queue with open repository", func(t *testing.T) {
		starts := traceGitStarts(t, tracePath, func() {
			repo, err := gitrepo.OpenPath(repoRoot)
			require.NoError(t, err)
			defer repo.Close()
			queue, err := checkpoint.PushQueueForRepo(ctx, repo)
			require.NoError(t, err)
			require.NoError(t, queue.Enqueue(plumbing.NewBranchReferenceName("entire/trace-proof")))
		})
		requireGitStartCount(t, starts, 0, 1, 0)
	})

	t.Run("manual prepare-commit-msg without active session", func(t *testing.T) {
		messagePath := filepath.Join(repoRoot, "COMMIT_MSG")
		require.NoError(t, os.WriteFile(messagePath, []byte("test\n"), 0o600))
		starts := traceGitStarts(t, tracePath, func() {
			paths.ClearWorktreeRootCache()
			require.NoError(t, strategy.NewManualCommitStrategy().PrepareCommitMsg(ctx, messagePath, ""))
		})
		requireGitStartCount(t, starts, 2, 4, 1)
	})

	t.Run("status", func(t *testing.T) {
		starts := traceGitStarts(t, tracePath, func() {
			paths.ClearWorktreeRootCache()
			require.NoError(t, runStatus(ctx, io.Discard, false, false))
		})
		requireGitStartCount(t, starts, 3, 4, 0)
	})

	t.Run("Codex hook discovery", func(t *testing.T) {
		starts := traceGitStarts(t, tracePath, func() {
			paths.ClearWorktreeRootCache()
			discovery := codex.ResolveHookDiscovery(ctx)
			require.NotEqual(t, codex.HookDiscoveryUnresolved, discovery.State)
		})
		requireGitStartCount(t, starts, 1, 1, 0)
	})
}

type gitTraceStart struct {
	Event string   `json:"event"`
	Argv  []string `json:"argv"`
}

func traceGitStarts(t *testing.T, tracePath string, fn func()) []gitTraceStart {
	t.Helper()
	require.NoError(t, os.WriteFile(tracePath, nil, 0o600))
	fn()

	trace, err := os.Open(tracePath)
	require.NoError(t, err)
	defer trace.Close()

	var starts []gitTraceStart
	scanner := bufio.NewScanner(trace)
	for scanner.Scan() {
		var event gitTraceStart
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Event == "start" {
			starts = append(starts, event)
		}
	}
	require.NoError(t, scanner.Err())
	return starts
}

func requireGitStartCount(t *testing.T, starts []gitTraceStart, after, historicalBefore, retainedSemanticCommonDir int) {
	t.Helper()
	commonDirStarts := 0
	for _, start := range starts {
		if strings.Contains(strings.Join(start.Argv, " "), "--git-common-dir") {
			commonDirStarts++
		}
	}
	require.Equalf(t, retainedSemanticCommonDir, commonDirStarts,
		"unexpected common-dir Git query; only documented repository-identity queries may remain, argv=%v", starts)
	require.Lenf(t, starts, after, "Git subprocess count changed; historical pre-consolidation count was %d, argv=%v", historicalBefore, starts)
}

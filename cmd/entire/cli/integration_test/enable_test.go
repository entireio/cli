//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// freshRepoEnv builds a repo with an initial commit but WITHOUT Entire enabled,
// so `entire enable` runs its real first-time flow.
func freshRepoEnv(t *testing.T) *TestEnv {
	t.Helper()
	env := NewTestEnv(t)
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	return env
}

// TestEnable_KeepsOperationalLogsOffTheTerminal pins the regression that made
// enable build a logger at all: without one, its operational lines landed on the
// user's terminal mid-flow instead of in .entire/logs.
//
// It cannot assert the log file exists. The file is created on the first line
// written, and enable's happy path logs nothing — so requiring it here only ever
// proved a logger had been constructed, not that anything was routed.
func TestEnable_KeepsOperationalLogsOffTheTerminal(t *testing.T) {
	t.Parallel()
	env := freshRepoEnv(t)

	out := env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")
	require.Contains(t, out, "Ready.", "enable should complete; got: %s", out)

	// slog's text handler, which is where a line goes when no logger is
	// installed. Its shape is level-then-message: "INFO msg key=value".
	for _, marker := range []string{"level=INFO", "level=WARN", "level=DEBUG", "component="} {
		require.NotContains(t, out, marker,
			"enable put an operational log line on the terminal; got: %s", out)
	}
}

// TestEnable_RejectedInvocationLeavesRepoUntouched pins that enable's logging
// init cannot seed a repo that enable then refuses to configure. Init creates
// .entire/logs/, so running it before the invocation is known-valid left an
// untracked .entire/ behind on every rejected `entire enable` — in a repo that
// does not yet carry Entire's gitignore entry to cover it.
func TestEnable_RejectedInvocationLeavesRepoUntouched(t *testing.T) {
	t.Parallel()
	env := freshRepoEnv(t)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"mutually exclusive scopes", []string{"enable", "--local", "--project"}},
		{"unknown agent", []string{"enable", "--agent", "definitely-not-an-agent"}},
	} {
		out, err := env.RunCLIWithError(tc.args...)
		require.Error(t, err, "%s: expected enable to be rejected; got: %s", tc.name, out)
		require.NoDirExists(t, filepath.Join(env.RepoDir, ".entire"),
			"%s: a rejected enable must not create .entire/; got: %s", tc.name, out)
	}
}

// History import is withdrawn until it is redesigned. --import-history stays
// accepted so scripts that pass it keep working: a first-time enable with
// discoverable history must complete, say the flag is deprecated, and write no
// checkpoints.
func TestEnableImportHistoryFlagIsDeprecatedNoOp(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := freshRepoEnv(t)
		env.CheckpointStore = backend
		require.NoError(t, os.WriteFile(filepath.Join(env.ClaudeProjectDir, "sess1.jsonl"), []byte(
			`{"type":"user","uuid":"u1","timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":"first"}}`+"\n"), 0o644))

		out := env.RunCLI("enable", "--agent", agentClaudeCode, "--import-history", "--telemetry=false")
		require.Contains(t, out, "Ready.", "enable should complete; got: %s", out)
		require.Contains(t, out, "--import-history has been deprecated", "the flag should say it is deprecated; got: %s", out)
		if env.usingGitRefs() {
			require.False(t, env.CheckpointsPresentLocally(), "a deprecated --import-history must not import history")
		} else {
			// enable creates the v1 branch itself, so check its tree for checkpoints.
			tree := gitOutput(t, env.RepoDir, "ls-tree", "-r", "--name-only", paths.MetadataBranchName)
			require.NotContains(t, tree, "metadata.json", "a deprecated --import-history must not import history")
		}
	})
}

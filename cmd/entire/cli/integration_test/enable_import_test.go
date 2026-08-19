//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// claudeImportFixture is a two-turn Claude transcript used to verify enable-time import.
const claudeImportFixture = `{"type":"user","uuid":"u1","timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a1","message":{"id":"m1","model":"claude-x","content":[{"type":"text","text":"ok"}],"usage":{"output_tokens":5}}}
{"type":"user","uuid":"u2","timestamp":"2026-06-20T00:01:00Z","message":{"role":"user","content":"second"}}
`

// writeClaudeHistory drops a discoverable two-turn Claude transcript for the
// env's repo, so a first-time enable has something to offer to import.
func writeClaudeHistory(t *testing.T, env *TestEnv) {
	t.Helper()
	require.NoError(t, os.WriteFile(
		filepath.Join(env.ClaudeProjectDir, "sess1.jsonl"),
		[]byte(claudeImportFixture), 0o644))
}

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

func TestEnableOffersImport_FirstRunImportsWithImportHistory(t *testing.T) {
	t.Parallel()
	env := freshRepoEnv(t)

	writeClaudeHistory(t, env)

	// --import-history is the explicit, non-interactive opt-in to importing
	// the selected agent's discoverable history on first-time enable.
	out := env.RunCLI("enable", "--agent", agentClaudeCode, "--import-history", "--telemetry=false")
	require.Contains(t, out, "Ready.", "enable should complete; got: %s", out)
	require.Contains(t, out, "Imported 2 turn(s)", "--import-history should import discovered history; got: %s", out)

	// The imported turns are real checkpoints on the v1 metadata branch.
	require.Contains(t, env.RunCLI("checkpoint", "list"), "[imported]",
		"imported checkpoints should be listed")
}

// TestEnableOffersImport_YesDoesNotImport pins the decision that --yes does
// not carry a history import with it. --yes means "accept all defaults", and
// the interactive default is to import nothing, so an unattended enable must
// not ingest a month of local transcripts on its own.
func TestEnableOffersImport_YesDoesNotImport(t *testing.T) {
	t.Parallel()
	env := freshRepoEnv(t)

	writeClaudeHistory(t, env)

	out := env.RunCLI("enable", "--agent", agentClaudeCode, "--yes", "--telemetry=false")
	require.Contains(t, out, "Ready.", "enable should complete; got: %s", out)
	require.NotContains(t, out, "Imported", "--yes must not import agent history; got: %s", out)
	require.Contains(t, out, "entire import", "should point at the manual import command; got: %s", out)

	require.NotContains(t, env.RunCLI("checkpoint", "list"), "[imported]",
		"no checkpoints should be imported under --yes alone")
}

// TestEnableImportHistory_OnConfiguredRepoIsReported proves the flag is not
// silently dropped when the repo is already set up (the offer is first-run
// only), so a user cannot come away believing history was imported.
func TestEnableImportHistory_OnConfiguredRepoIsReported(t *testing.T) {
	t.Parallel()
	env := freshRepoEnv(t)

	writeClaudeHistory(t, env)

	env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")

	out := env.RunCLI("enable", "--import-history")
	require.Contains(t, out, "import-history", "re-run should report the flag does not apply; got: %s", out)
	require.Contains(t, out, "entire import", "should point at the manual import command; got: %s", out)
	require.NotContains(t, out, "Imported", "a non-first-run enable must not import; got: %s", out)
}

func TestEnableOffersImport_NonInteractiveWithoutYesHints(t *testing.T) {
	t.Parallel()
	env := freshRepoEnv(t)

	writeClaudeHistory(t, env)

	// A non-interactive (no-TTY) enable without --yes must NOT silently import;
	// it points at the manual command instead.
	out := env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")
	require.Contains(t, out, "Ready.", "enable should complete; got: %s", out)
	require.NotContains(t, out, "Imported", "non-interactive enable without --yes must not auto-import; got: %s", out)
	require.Contains(t, out, "entire import", "should point at the manual import command; got: %s", out)

	// Nothing was written to the checkpoint metadata branch.
	require.NotContains(t, env.RunCLI("checkpoint", "list"), "[imported]",
		"no checkpoints should be imported without --yes")
}

func TestEnableOffersImport_NoHistoryIsSilent(t *testing.T) {
	t.Parallel()
	env := freshRepoEnv(t)
	// No transcripts written: nothing discoverable.

	out := env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")
	require.Contains(t, out, "Ready.", "enable should complete; got: %s", out)
	require.NotContains(t, out, "Imported", "no history => import offer must be a silent no-op; got: %s", out)
}

func TestEnableOffersImport_NotOfferedOnReEnable(t *testing.T) {
	t.Parallel()
	env := freshRepoEnv(t)
	writeClaudeHistory(t, env)

	// First enable imports (--import-history opts in).
	first := env.RunCLI("enable", "--agent", agentClaudeCode, "--import-history", "--telemetry=false")
	require.Contains(t, first, "Imported 2 turn(s)", "first enable should import; got: %s", first)

	// Re-enable must not re-offer or re-import, even though history is still present.
	second := env.RunCLI("enable", "--agent", agentClaudeCode, "--yes", "--telemetry=false")
	require.NotContains(t, second, "Imported", "re-enable must not offer import again; got: %s", second)
	require.NotContains(t, second, "already imported",
		"re-enable must not run import at all; got: %s", second)
}

// TestEnable_RoutesLoggingToLogFile pins that `entire enable` initializes file
// logging like every other command — see the logging.Init call in enable's
// RunE for why its absence put operational warnings on the user's terminal
// instead of in the log.
func TestEnable_RoutesLoggingToLogFile(t *testing.T) {
	t.Parallel()
	env := freshRepoEnv(t)
	logPath := filepath.Join(env.RepoDir, ".entire", "logs", "entire.log")

	// Nothing has run in this repo yet, so the log file's existence after
	// enable can only come from enable itself.
	require.NoFileExists(t, logPath)

	out := env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")
	require.Contains(t, out, "Ready.", "enable should complete; got: %s", out)
	require.FileExists(t, logPath, "enable should route its logging to .entire/logs; got output: %s", out)
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

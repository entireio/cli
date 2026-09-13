package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAgentCheckDetectEcosystem(t *testing.T) {
	t.Run("go", func(t *testing.T) {
		dir := t.TempDir()
		writeAgentCheckTestFile(t, dir, "go.mod", "module example.com/ac\n")
		writeAgentCheckTestFile(t, dir, "package.json", `{"scripts":{"test":"npm test"}}`)

		require.Equal(t, agentCheckEcosystemGo, detectAgentCheckEcosystem(dir))
	})

	t.Run("node", func(t *testing.T) {
		dir := t.TempDir()
		writeAgentCheckTestFile(t, dir, "package.json", `{"scripts":{"test":"node test.js"}}`)

		require.Equal(t, agentCheckEcosystemNode, detectAgentCheckEcosystem(dir))
	})

	t.Run("python", func(t *testing.T) {
		dir := t.TempDir()
		writeAgentCheckTestFile(t, dir, "pytest.ini", "[pytest]\n")

		require.Equal(t, agentCheckEcosystemPython, detectAgentCheckEcosystem(dir))
	})

	t.Run("unknown", func(t *testing.T) {
		require.Equal(t, agentCheckEcosystemUnknown, detectAgentCheckEcosystem(t.TempDir()))
	})
}

func TestAgentCheckSelectVerificationCommand(t *testing.T) {
	t.Run("go uses repository test command", func(t *testing.T) {
		dir := t.TempDir()
		writeAgentCheckTestFile(t, dir, "go.mod", "module example.com/ac\n")

		command, ok := selectAgentCheckVerificationCommand(dir, agentCheckEcosystemGo)

		require.True(t, ok)
		require.Equal(t, "go test ./...", command.String())
		require.Equal(t, "go", command.Name)
		require.Equal(t, []string{"test", "./..."}, command.Args)
	})

	t.Run("node skips placeholder test script", func(t *testing.T) {
		dir := t.TempDir()
		writeAgentCheckTestFile(t, dir, "package.json", `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`)

		_, ok := selectAgentCheckVerificationCommand(dir, agentCheckEcosystemNode)

		require.False(t, ok)
	})

	t.Run("unknown has no command", func(t *testing.T) {
		_, ok := selectAgentCheckVerificationCommand(t.TempDir(), agentCheckEcosystemUnknown)

		require.False(t, ok)
	})
}

func TestAgentCheckRunVerificationSuccess(t *testing.T) {
	dir := t.TempDir()
	writeAgentCheckTestFile(t, dir, "go.mod", "module example.com/ac\n")

	evidence := runAgentCheckVerification(context.Background(), agentCheckVerificationOptions{
		RepoRoot: dir,
		Runner: fakeAgentCheckRunner{
			exitCode: 0,
			stdout:   "ok example.com/ac\n",
		},
	})

	require.Equal(t, agentCheckEcosystemGo, evidence.Ecosystem)
	require.Equal(t, "go test ./...", evidence.Command)
	require.Equal(t, agentCheckVerificationSuccess, evidence.Status)
	require.Equal(t, 0, evidence.ExitCode)
	require.Contains(t, evidence.Stdout, "ok example.com/ac")
	require.Empty(t, evidence.Stderr)
	require.GreaterOrEqual(t, evidence.Duration, time.Duration(0))
}

func TestAgentCheckRunVerificationFailure(t *testing.T) {
	dir := t.TempDir()
	writeAgentCheckTestFile(t, dir, "go.mod", "module example.com/ac\n")

	evidence := runAgentCheckVerification(context.Background(), agentCheckVerificationOptions{
		RepoRoot: dir,
		Runner: fakeAgentCheckRunner{
			exitCode: 1,
			err:      errors.New("exit status 1"),
			stdout:   "--- FAIL: TestThing\n",
			stderr:   "failure detail\n",
		},
	})

	require.Equal(t, agentCheckVerificationFailed, evidence.Status)
	require.Equal(t, 1, evidence.ExitCode)
	require.Contains(t, evidence.Stdout, "--- FAIL")
	require.Contains(t, evidence.Stderr, "failure detail")
}

func TestAgentCheckRunVerificationUnableToRun(t *testing.T) {
	dir := t.TempDir()
	writeAgentCheckTestFile(t, dir, "go.mod", "module example.com/ac\n")

	evidence := runAgentCheckVerification(context.Background(), agentCheckVerificationOptions{
		RepoRoot: dir,
		Runner: fakeAgentCheckRunner{
			exitCode: -1,
			err:      errors.New("executable file not found"),
		},
	})

	require.Equal(t, agentCheckVerificationUnableToRun, evidence.Status)
	require.Equal(t, -1, evidence.ExitCode)
	require.Contains(t, evidence.Stderr, "executable file not found")
}

func TestAgentCheckRunVerificationUnableWhenNoCommandSelected(t *testing.T) {
	evidence := runAgentCheckVerification(context.Background(), agentCheckVerificationOptions{
		RepoRoot: t.TempDir(),
		Runner: fakeAgentCheckRunner{
			exitCode: 0,
			stdout:   "should not run",
		},
	})

	require.Equal(t, agentCheckEcosystemUnknown, evidence.Ecosystem)
	require.Equal(t, agentCheckVerificationUnableToRun, evidence.Status)
	require.Equal(t, -1, evidence.ExitCode)
	require.Empty(t, evidence.Command)
	require.Contains(t, evidence.Stderr, "no verification command selected")
	require.Empty(t, evidence.Stdout)
}

func TestAgentCheckRunVerificationBoundsStdoutAndStderr(t *testing.T) {
	dir := t.TempDir()
	writeAgentCheckTestFile(t, dir, "go.mod", "module example.com/ac\n")

	evidence := runAgentCheckVerification(context.Background(), agentCheckVerificationOptions{
		RepoRoot:       dir,
		MaxOutputBytes: 5,
		Runner: fakeAgentCheckRunner{
			exitCode: 1,
			err:      errors.New("exit status 1"),
			stdout:   "stdout-too-long",
			stderr:   "stderr-too-long",
		},
	})

	require.Equal(t, agentCheckVerificationFailed, evidence.Status)
	require.Equal(t, "stdou", evidence.Stdout)
	require.True(t, evidence.StdoutTruncated)
	require.Equal(t, "stder", evidence.Stderr)
	require.True(t, evidence.StderrTruncated)
}

func TestAgentCheckExecRunnerReportsStartFailure(t *testing.T) {
	var stdout, stderr strings.Builder
	exitCode, err := (agentCheckExecRunner{}).Run(context.Background(), t.TempDir(), agentCheckVerificationCommand{
		Name: "agentcheck-command-that-should-not-exist",
	}, &stdout, &stderr)

	require.Error(t, err)
	require.Equal(t, -1, exitCode)
	require.Empty(t, stdout.String())
}

type fakeAgentCheckRunner struct {
	exitCode int
	err      error
	stdout   string
	stderr   string
}

func (r fakeAgentCheckRunner) Run(_ context.Context, _ string, _ agentCheckVerificationCommand, stdout, stderr io.Writer) (int, error) {
	if r.stdout != "" {
		_, _ = io.WriteString(stdout, r.stdout)
	}
	if r.stderr != "" {
		_, _ = io.WriteString(stderr, r.stderr)
	}
	return r.exitCode, r.err
}

func writeAgentCheckTestFile(t *testing.T, dir, relPath, contents string) {
	t.Helper()
	path := filepath.Join(dir, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
}

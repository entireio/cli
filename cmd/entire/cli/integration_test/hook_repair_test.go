//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/stretchr/testify/require"
)

func TestHookRepair_UserEditWarnsOnAgentTurn(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")
	hookPath := filepath.Join(env.RepoDir, ".git", "hooks", "pre-push")
	original, err := os.ReadFile(hookPath)
	require.NoError(t, err)
	edited := []byte(string(original) + "echo custom-validation\n")
	require.NoError(t, os.WriteFile(hookPath, edited, 0o755))
	sess := env.NewSession()
	input, err := json.Marshal(map[string]string{
		"session_id": sess.ID, "transcript_path": sess.TranscriptPath, "prompt": "Inspect repository",
	})
	require.NoError(t, err)
	cmd := execx.NonInteractive(t.Context(), getTestBinary(), "hooks", "claude-code", "user-prompt-submit")
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	require.NoError(t, err)
	var response struct {
		SystemMessage string `json:"systemMessage"`
	}
	require.NoError(t, json.Unmarshal(out, &response))
	require.Contains(t, response.SystemMessage, "Git hook integration is outdated")
	require.Contains(t, response.SystemMessage, "entire doctor")
	after, err := os.ReadFile(hookPath)
	require.NoError(t, err)
	require.Equal(t, edited, after)
	status := env.RunCLI("status")
	require.Contains(t, status, "Checkpoint sync blocked")
	require.Contains(t, status, "pre-push")
	require.Contains(t, status, "left it unchanged")
}

func TestHookRepair_NativePermissionStatusAndRecovery(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not use Unix executable bits")
	}
	env := NewRepoWithCommit(t)
	env.RunCLI("enable", "--agent", agentClaudeCode, "--telemetry=false")
	hookPath := filepath.Join(env.RepoDir, ".git", "hooks", "pre-push")
	require.NoError(t, os.Chmod(hookPath, 0o644))
	status := env.RunCLI("status")
	require.Contains(t, status, "Checkpoint sync blocked")
	require.Contains(t, status, "not executable")
	info, err := os.Stat(hookPath)
	require.NoError(t, err)
	require.Zero(t, info.Mode().Perm()&0o111, "status must remain read-only")
	sess := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmit(sess.ID))
	info, err = os.Stat(hookPath)
	require.NoError(t, err)
	require.NotZero(t, info.Mode().Perm()&0o111)
	require.NotContains(t, env.RunCLI("status"), "Checkpoint sync blocked")
}

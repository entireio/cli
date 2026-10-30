//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/require"
)

// TestTokenScope_AttachStateSaveFailureDoesNotRecount: when attach can't save
// the session state, the tokens it would have stored must not end up counted
// twice — once in an attach checkpoint and again by the session's next
// hook checkpoint — and the attach must fail rather than report success.
func TestTokenScope_AttachStateSaveFailureDoesNotRecount(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("read-only directories via chmod are unix-only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	env := NewFeatureBranchEnv(t)
	s := env.NewSession()

	// Turn 1 (100 tokens) writes a and b; only a is committed. Carry-forward
	// clears LastCheckpointID, so attach would write a new checkpoint.
	require.NoError(t, env.SimulateUserPromptSubmit(s.ID))
	env.WriteFile("a.txt", "a")
	env.WriteFile("b.txt", "b")
	appendUsageMessage(s, "msg-1", 100)
	s.CreateTranscript("make a and b", []FileChange{{Path: "a.txt", Content: "a"}, {Path: "b.txt", Content: "b"}})
	require.NoError(t, env.SimulateStop(s.ID, s.TranscriptPath))
	env.GitCommitWithHooks("a only", "a.txt")
	cp1 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp1)

	// Turn 2 (7 tokens) changes no files.
	require.NoError(t, env.SimulateUserPromptSubmit(s.ID))
	appendUsageMessage(s, "msg-2", 7)
	s.CreateTranscript("explain b", nil)
	require.NoError(t, env.SimulateStop(s.ID, s.TranscriptPath))

	transcriptData, err := os.ReadFile(s.TranscriptPath)
	require.NoError(t, err)
	s.TranscriptPath = filepath.Join(env.ClaudeProjectDir, s.ID+".jsonl")
	require.NoError(t, os.WriteFile(s.TranscriptPath, transcriptData, 0o600))
	env.WriteFile("notes.txt", "notes")
	env.GitAdd("notes.txt")
	env.GitCommit("notes")

	// Session state can be read but not written while attach runs.
	stateDir := filepath.Join(env.RepoDir, ".git", session.SessionStateDirName)
	require.NoError(t, os.Chmod(stateDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o750) }) //nolint:errcheck // best-effort restore for TempDir cleanup

	env.ExtraEnv = append(env.ExtraEnv,
		"PATH="+filepath.Dir(getTestBinary())+string(os.PathListSeparator)+os.Getenv("PATH"))
	headBefore := env.GetHeadHash()
	output, attachErr := env.RunCLIWithError("session", "attach", s.ID, "-a", agentClaudeCode, "-f")
	require.NoError(t, os.Chmod(stateDir, 0o750))
	require.Error(t, attachErr, "attach must not report success when it couldn't save the session state:\n%s", output)
	require.Contains(t, output, "nothing was attached")
	require.Equal(t, cp1, env.TryGetLatestCheckpointID(), "no checkpoint was written")
	require.Equal(t, headBefore, env.GetHeadHash(), "HEAD was not rewritten")

	// Turn 3 (3 tokens) writes c; b and c are committed through hooks.
	require.NoError(t, env.SimulateUserPromptSubmit(s.ID))
	env.WriteFile("c.txt", "c")
	appendUsageMessage(s, "msg-3", 3)
	s.CreateTranscript("make c", []FileChange{{Path: "c.txt", Content: "c"}})
	require.NoError(t, env.SimulateStop(s.ID, s.TranscriptPath))
	env.GitCommitWithHooks("b and c", "b.txt", "c.txt")
	cp3 := env.TryGetLatestCheckpointID()
	require.NotEmpty(t, cp3)
	require.NotEqual(t, cp1, cp3)
	u3 := readCommittedTokenUsage(t, env, cp3)
	require.NotNil(t, u3)
	require.Equal(t, 10, u3.OutputTokens, "turns 2 and 3 (7+3 tokens) are counted once, by the next checkpoint")
}

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

const tokenRebaseSessionID = "token-rebase-session"

// setupTokenRebaseRepo creates a repo holding a session whose token offset is
// tokenStart and a transcript of lines lines (none when lines < 0), and
// returns the transcript path.
func setupTokenRebaseRepo(t *testing.T, tokenStart, lines int) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

	state := &strategy.SessionState{
		SessionID:    tokenRebaseSessionID,
		AgentType:    agent.AgentTypeClaudeCode,
		WorktreePath: dir,
		StartedAt:    time.Now().UTC(),
		Phase:        session.PhaseIdle,
	}
	state.SetTokenStart(tokenStart)
	require.NoError(t, strategy.SaveSessionState(context.Background(), state))

	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	if lines >= 0 {
		line := `{"type":"user","uuid":"u","message":{"content":"hi"}}` + "\n"
		require.NoError(t, os.WriteFile(transcriptPath, []byte(strings.Repeat(line, lines)), 0o600))
	}
	return transcriptPath
}

// captureTokenStarts runs CapturePrePromptState and returns the session's
// token offset and the turn's recorded baseline.
func captureTokenStarts(ctx context.Context, t *testing.T, transcriptPath string) (sessionStart, baseline int) {
	t.Helper()
	require.NoError(t, CapturePrePromptState(ctx, claudecode.NewClaudeCodeAgent(), tokenRebaseSessionID, transcriptPath))
	state, err := strategy.LoadSessionState(context.Background(), tokenRebaseSessionID)
	require.NoError(t, err)
	pre, err := LoadPrePromptState(context.Background(), tokenRebaseSessionID)
	require.NoError(t, err)
	require.NotNil(t, pre)
	require.NotNil(t, pre.TokenStart)
	return state.TokenStart(), *pre.TokenStart
}

func TestCapturePrePromptState_RebasesTokenStartPastTranscript(t *testing.T) {
	transcriptPath := setupTokenRebaseRepo(t, 8, 3)

	sessionStart, baseline := captureTokenStarts(context.Background(), t, transcriptPath)

	require.Equal(t, 3, sessionStart, "the offset past the transcript moves to turn start")
	require.Equal(t, 3, baseline, "the turn's baseline is the rebased offset")
}

func TestCapturePrePromptState_KeepsTokenStartWithinTranscript(t *testing.T) {
	for name, tokenStart := range map[string]int{"offset inside": 2, "offset at the end": 3} {
		t.Run(name, func(t *testing.T) {
			transcriptPath := setupTokenRebaseRepo(t, tokenStart, 3)

			sessionStart, baseline := captureTokenStarts(context.Background(), t, transcriptPath)

			require.Equal(t, tokenStart, sessionStart)
			require.Equal(t, tokenStart, baseline)
		})
	}
}

// A missing transcript reads as position 0, which says nothing about where the
// turn starts, so it must not reset the offset and recount stored tokens.
func TestCapturePrePromptState_MissingTranscriptKeepsTokenStart(t *testing.T) {
	transcriptPath := setupTokenRebaseRepo(t, 8, -1)

	sessionStart, baseline := captureTokenStarts(context.Background(), t, transcriptPath)

	require.Equal(t, 8, sessionStart)
	require.Equal(t, 8, baseline)
}

// When the TurnStart lock wait expires, the turn keeps the offset it read.
func TestCapturePrePromptState_LockTimeoutKeepsTokenStart(t *testing.T) {
	transcriptPath := setupTokenRebaseRepo(t, 8, 3)
	lockPath := filepath.Join(".git", "entire-session-locks", tokenRebaseSessionID+".lock")
	require.NoError(t, os.MkdirAll(filepath.Dir(lockPath), 0o750))
	release, err := flock.Acquire(lockPath)
	require.NoError(t, err)

	ctx := strategy.WithSessionLockWait(context.Background(), 50*time.Millisecond)
	require.NoError(t, CapturePrePromptState(ctx, claudecode.NewClaudeCodeAgent(), tokenRebaseSessionID, transcriptPath))
	release()

	state, err := strategy.LoadSessionState(context.Background(), tokenRebaseSessionID)
	require.NoError(t, err)
	require.Equal(t, 8, state.TokenStart())
	pre, err := LoadPrePromptState(context.Background(), tokenRebaseSessionID)
	require.NoError(t, err)
	require.NotNil(t, pre)
	require.NotNil(t, pre.TokenStart)
	require.Equal(t, 8, *pre.TokenStart)
}

// A condensation that moved the offset after the caller read it wins.
func TestRebaseStaleTokenStart_ConcurrentAdvanceWins(t *testing.T) {
	setupTokenRebaseRepo(t, 10, 3)

	got := rebaseStaleTokenStart(context.Background(), tokenRebaseSessionID, 8, 3)

	require.Equal(t, 8, got, "the turn keeps the offset it read")
	state, err := strategy.LoadSessionState(context.Background(), tokenRebaseSessionID)
	require.NoError(t, err)
	require.Equal(t, 10, state.TokenStart(), "the concurrent advance is not overwritten")
}

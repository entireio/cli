package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestDropFilesSince(t *testing.T) {
	t.Parallel()
	anchor := time.Unix(1000, 0)
	stats := map[string]UntrackedFileStat{
		"new.js": {ModTime: anchor.UnixNano()},
		"old.js": {ModTime: anchor.Add(-time.Second).UnixNano()},
	}
	files, stats, dropped := dropFilesSince([]string{"new.js", "old.js", "nostat.js"}, stats, anchor)
	assert.Equal(t, []string{"old.js", "nostat.js"}, files)
	assert.NotContains(t, stats, "new.js")
	assert.Contains(t, stats, "old.js")
	assert.Equal(t, 1, dropped)

	files, _, _ = dropFilesSince([]string{"new.js"}, map[string]UntrackedFileStat{"new.js": {ModTime: anchor.UnixNano()}}, anchor)
	assert.NotNil(t, files, "an emptied baseline must stay non-nil: nil means the scan was skipped")
}

// claudeTranscript returns a Claude transcript whose prompts were submitted at
// promptTimes, each answered by the assistant.
func claudeTranscript(promptTimes ...time.Time) string {
	var out strings.Builder
	for i, ts := range promptTimes {
		stamp := ts.UTC().Format(time.RFC3339Nano)
		fmt.Fprintf(&out, `{"type":"user","uuid":"u%d","timestamp":%q,"message":{"content":"prompt %d"}}`+"\n", i, stamp, i)
		fmt.Fprintf(&out, `{"type":"assistant","uuid":"a%d","timestamp":%q,"message":{"content":[{"type":"text","text":"ok"}]}}`+"\n", i, stamp)
	}
	return out.String()
}

// captureLateJoinBaseline sets up a repository holding old.js (written before
// previousTurn) and new.js (written now), runs CapturePrePromptState, and
// returns the recorded untracked files.
func captureLateJoinBaseline(t *testing.T, ag agent.Agent, transcript string, withState bool) []string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README", "readme")
	testutil.GitAdd(t, dir, "README")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

	testutil.WriteFile(t, dir, "old.js", "old")
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "old.js"), old, old))
	testutil.WriteFile(t, dir, "new.js", "new")

	transcriptPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(transcript), 0o600))

	ctx := context.Background()
	const sessionID = "late-join-session"
	if withState {
		require.NoError(t, strategy.SaveSessionState(ctx, &strategy.SessionState{
			SessionID:    sessionID,
			WorktreePath: dir,
			StartedAt:    time.Now(),
			Phase:        session.PhaseIdle,
		}))
	}
	require.NoError(t, CapturePrePromptState(ctx, ag, sessionID, transcriptPath))
	state, err := LoadPrePromptState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	return state.UntrackedFiles
}

func TestCapturePrePromptState_LateJoinDropsFilesSincePreviousTurn(t *testing.T) {
	previousTurn := time.Now().Add(-time.Hour)
	files := captureLateJoinBaseline(t, claudecode.NewClaudeCodeAgent(), claudeTranscript(previousTurn), false)
	assert.ElementsMatch(t, []string{"old.js"}, files)
}

func TestCapturePrePromptState_LateJoinKeepsBaseline(t *testing.T) {
	previousTurn := time.Now().Add(-time.Hour)
	tests := []struct {
		name       string
		ag         agent.Agent
		transcript string
		withState  bool
	}{
		{"no capability", newMockAgent(), claudeTranscript(previousTurn), false},
		{"session state exists", claudecode.NewClaudeCodeAgent(), claudeTranscript(previousTurn), true},
		{"no earlier prompt", claudecode.NewClaudeCodeAgent(), "", false},
		{"only the current prompt flushed", claudecode.NewClaudeCodeAgent(),
			`{"type":"user","uuid":"u1","timestamp":"` + previousTurn.UTC().Format(time.RFC3339) + `","message":{"content":"hi"}}` + "\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := captureLateJoinBaseline(t, tt.ag, tt.transcript, tt.withState)
			assert.ElementsMatch(t, []string{"old.js", "new.js"}, files)
		})
	}
}

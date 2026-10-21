package cli

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// A hook that checkpoints while attach waits at its prompt moves the token
// offset and records newer totals; attach must not move the offset back or
// overwrite those totals with the older ones it computed before the wait.
func TestSaveAttachSessionState_KeepsTokenOffsetMovedMeanwhile(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	ctx := context.Background()

	store, err := session.NewStateStore(ctx)
	require.NoError(t, err)
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")

	for _, tc := range []struct {
		name string
		// hookMovedTo is the token offset on disk when attach saves; -1 means
		// the session had no state at all.
		hookMovedTo int
		// pendingOnDisk is the pending total on disk; attach read 3.
		pendingOnDisk int
		// checkpointOnDisk is LastCheckpointID on disk; attach read none.
		checkpointOnDisk id.CheckpointID
		// phaseOnDisk is the session's phase on disk; empty means running.
		phaseOnDisk session.Phase
		// quietFor is how long the session has had no interaction.
		quietFor time.Duration
		// owner is the session's recorded agent process; "" records none,
		// "alive" this test process and "dead" this process
		// with a different start time, which reads as an exited one.
		owner         string
		wantStart     int
		wantPendingOK bool
		wantTotal     int
	}{
		// The session is running: its Stop adds the turn, so attach keeps the hooks' total.
		{name: "unchanged", hookMovedTo: 2, pendingOnDisk: 3, wantStart: 4, wantTotal: 12},
		{name: "moved by a hook", hookMovedTo: 6, pendingOnDisk: 3, wantStart: 6, wantPendingOK: true, wantTotal: 12},
		{name: "turn added by a stop", hookMovedTo: 2, pendingOnDisk: 5, wantStart: 2, wantPendingOK: true, wantTotal: 12},
		{name: "condensed in place", hookMovedTo: 2, pendingOnDisk: 3, checkpointOnDisk: id.MustCheckpointID("b1b2c3d4e5f6"), wantStart: 2, wantPendingOK: true, wantTotal: 12},
		// No Stop will come for these, so attach takes the whole-transcript total.
		{name: "stuck running session", hookMovedTo: 2, pendingOnDisk: 3, quietFor: 2 * session.StuckActiveThreshold, wantStart: 4, wantTotal: 9},
		{name: "exited agent", hookMovedTo: 2, pendingOnDisk: 3, owner: "dead", wantStart: 4, wantTotal: 9},
		// A live agent's long turn still ends in a Stop, however quiet.
		{name: "quiet live agent", hookMovedTo: 2, pendingOnDisk: 3, quietFor: 2 * session.StuckActiveThreshold, owner: "alive", wantStart: 4, wantTotal: 12},
		{name: "idle session", hookMovedTo: 2, pendingOnDisk: 3, phaseOnDisk: session.PhaseIdle, wantStart: 4, wantTotal: 9},
		{name: "no earlier state", hookMovedTo: -1, wantStart: 4, wantTotal: 9},
	} {
		sessionID := "attach-window-" + strings.ReplaceAll(tc.name, " ", "-")
		var loaded *session.State
		if tc.hookMovedTo >= 0 {
			loaded = &session.State{
				SessionID:            sessionID,
				AgentType:            agent.AgentTypeClaudeCode,
				CheckpointTokenUsage: &agent.TokenUsage{OutputTokens: 3},
			}
			loaded.SetTokenStart(2)

			phase := tc.phaseOnDisk
			if phase == "" {
				phase = session.PhaseActive
			}
			lastInteraction := time.Now().Add(-tc.quietFor)
			onDisk := &session.State{
				SessionID:            sessionID,
				AgentType:            agent.AgentTypeClaudeCode,
				Phase:                phase,
				StartedAt:            lastInteraction,
				LastInteractionTime:  &lastInteraction,
				TokenUsage:           &agent.TokenUsage{OutputTokens: 12, SubagentTokens: &agent.TokenUsage{OutputTokens: 1}},
				CheckpointTokenUsage: &agent.TokenUsage{OutputTokens: tc.pendingOnDisk},
				LastCheckpointID:     tc.checkpointOnDisk,
			}
			if tc.owner != "" {
				owner, ok := proclive.IdentityOf(os.Getpid())
				if !ok {
					if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
						t.Fatalf("%s: cannot fingerprint the test process", tc.name)
					}
					continue // process liveness is unknown on this platform
				}
				if tc.owner == "dead" {
					owner.Start = "not-" + owner.Start
				}
				onDisk.Owner = &owner
			}
			onDisk.SetTokenStart(tc.hookMovedTo)
			require.NoError(t, store.Save(ctx, onDisk))
		}

		// transcriptEnd 7 moves an ended session's display offset, which a
		// state without its own token offset would otherwise be read through.
		require.NoError(t, saveAttachSessionState(ctx, nil, loaded, sessionID, agent.AgentTypeClaudeCode,
			"/t.jsonl", "", cpID, transcriptMetadata{}, &agent.TokenUsage{OutputTokens: 9}, 4, 7, attachOptions{}, nil, false), tc.name)

		got, err := store.Load(ctx, sessionID)
		require.NoError(t, err)
		require.Equal(t, tc.wantStart, got.TokenStart(), tc.name)
		require.Equal(t, tc.wantPendingOK, got.CheckpointTokenUsage != nil, tc.name)
		require.NotNil(t, got.TokenUsage, tc.name)
		require.Equal(t, tc.wantTotal, got.TokenUsage.OutputTokens, tc.name)
		if tc.hookMovedTo >= 0 {
			// The transcript reports no subagents, so the hooks' total is kept.
			require.NotNil(t, got.TokenUsage.SubagentTokens, tc.name)
		}
	}
}

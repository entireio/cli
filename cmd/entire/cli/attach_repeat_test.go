package cli

import (
	"bytes"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// This test changes CWD and agent environment through setupAttachTestRepo.
func TestAttach_RepeatedSessionTokenScope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		transcript string
		start      int
		input      int
		total      int
	}{
		{name: "appended", transcript: attachFirstTurn + attachNextTurn, start: 2, input: 20, total: 30},
		{name: "unchanged", transcript: attachFirstTurn, start: 2, input: 0, total: 10},
		{name: "rewritten", transcript: attachNextTurn, start: 0, input: 20, total: 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupAttachTestRepo(t)
			const sessionID = "repeat-attach-tokens"
			ctx := t.Context()
			opts := attachOptions{Force: true}
			var out bytes.Buffer
			setupClaudeTranscript(t, sessionID, attachFirstTurn)
			if err := runAttach(ctx, &out, &out, sessionID, agent.AgentNameClaudeCode, opts); err != nil {
				t.Fatal(err)
			}
			testutil.RunGit(t, mustGetwd(t), "commit", "--allow-empty", "-m", "second commit")
			setupClaudeTranscript(t, sessionID, tc.transcript)
			if err := runAttach(ctx, &out, &out, sessionID, agent.AgentNameClaudeCode, opts); err != nil {
				t.Fatal(err)
			}
			states, err := session.NewStateStore(ctx)
			if err != nil {
				t.Fatal(err)
			}
			state, err := states.Load(ctx, sessionID)
			if err != nil || state == nil {
				t.Fatalf("load session: state=%v err=%v", state, err)
			}
			if state.TokenUsage == nil || state.TokenUsage.InputTokens != tc.total {
				t.Fatalf("session usage=%+v, want input=%d", state.TokenUsage, tc.total)
			}
			repo, err := openRepository(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := repo.Close(); err != nil {
					t.Error(err)
				}
			})
			store, err := openAttachStore(ctx, repo, opts.committedRefs(ctx))
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := store.ReadSessionMetadata(ctx, state.LastCheckpointID, 0)
			if err != nil || metadata == nil {
				t.Fatalf("read checkpoint metadata: metadata=%v err=%v", metadata, err)
			}
			if metadata.CheckpointTranscriptStart != tc.start {
				t.Errorf("transcript start=%d, want %d", metadata.CheckpointTranscriptStart, tc.start)
			}
			if metadata.TokenUsage == nil || metadata.TokenUsage.InputTokens != tc.input {
				t.Errorf("checkpoint usage=%+v, want input=%d", metadata.TokenUsage, tc.input)
			}
		})
	}
}

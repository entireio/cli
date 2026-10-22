//go:build integration

package integration

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// agentCommit commits files mid-turn through the hooks as the agent does, so
// the session condenses while its turn runs.
func (r *tokenTurnRun) agentCommit(files ...string) {
	r.t.Helper()
	r.env.GitCommitWithHooksAsAgent(fmt.Sprintf("agent commit in turn %d", r.turn), files...)
	r.record(fmt.Sprintf("agent commit in turn %d", r.turn))
}

// repeatLine writes a line of an assistant message the transcript already
// holds, as Claude Code does for each content block of one message: the same
// message ID and usage, which must count once.
func (r *tokenTurnRun) repeatLine(messageID string, tokens int) {
	r.t.Helper()
	appendUsageMessage(r.s, messageID, tokens)
	require.NoError(r.t, r.s.TranscriptBuilder.WriteToFile(r.s.TranscriptPath))
}

// rewind replaces the transcript with its first lines, as restoring an older
// copy of the session does. Tokens already counted stay counted.
func (r *tokenTurnRun) rewind(keep int) {
	r.t.Helper()
	r.s.TranscriptBuilder.messages = r.s.TranscriptBuilder.messages[:keep]
	require.NoError(r.t, os.Remove(r.s.TranscriptPath))
	require.NoError(r.t, r.s.TranscriptBuilder.WriteToFile(r.s.TranscriptPath))
}

// TestTokenLedger_ClaudeCode_EachCallCountedOnce runs the shapes that the
// per-turn accounting counted twice or dropped. Every one must leave the
// session total equal to the transcript's usage, and the checkpoints summing
// to it.
func TestTokenLedger_ClaudeCode_EachCallCountedOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		run  func(r *tokenTurnRun)
	}{
		{
			name: "a repeated Stop on a turn that changed files",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.stop()
				r.stop()
				r.checkTotal()
				r.start(20, 0, "b.txt")
				r.stop()
				r.stop()
				r.checkTotal()
				r.commit("a.txt", "b.txt")
			},
		},
		{
			name: "turns that change nothing between commits",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.stop()
				r.start(20, 0)
				r.stop()
				r.commit("a.txt")
				r.start(30, 0)
				r.stop()
				r.start(40, 0, "b.txt")
				r.stop()
				r.checkTotal()
				r.commit("b.txt")
			},
		},
		{
			name: "an agent commit in the middle of a turn",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.agentCommit("a.txt")
				r.more(5)
				r.stop()
				r.checkTotal()
				r.start(20, 0, "b.txt")
				r.stop()
				r.commit("b.txt")
			},
		},
		{
			name: "a message whose lines straddle an agent commit",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.agentCommit("a.txt")
				// The message that made the commit writes its next content
				// block after it: same ID, same usage, counted once.
				r.repeatLine(fmt.Sprintf("msg-%d", r.turn), 10)
				r.stop()
				r.checkTotal()
				r.start(20, 0, "b.txt")
				r.stop()
				r.commit("b.txt")
			},
		},
		{
			name: "a subagent that finishes in a later turn",
			run: func(r *tokenTurnRun) {
				r.start(10, 6, "a.txt")
				r.stop()
				r.commit("a.txt", "sub1.txt")
				r.start(20, 0)
				r.stop()
				r.start(30, 4, "b.txt")
				r.stop()
				r.checkTotal()
				r.commit("b.txt", "sub2.txt")
			},
		},
		{
			name: "a transcript rewritten to an older copy",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.stop()
				keep := len(r.s.TranscriptBuilder.messages)
				r.start(20, 0, "b.txt")
				r.stop()
				r.commit("a.txt", "b.txt")
				r.rewind(keep)
				r.start(30, 0, "c.txt")
				r.stop()
				r.checkTotal()
				r.commit("c.txt")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newTokenTurnRun(t)
			tc.run(r)
			r.checkTotal()
			r.verify()
		})
	}
}

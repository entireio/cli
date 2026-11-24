//go:build integration

package integration

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// end fires SessionEnd, as a headless agent does after every prompt.
func (r *tokenTurnRun) end() {
	r.t.Helper()
	require.NoError(r.t, r.env.SimulateSessionEnd(r.s.ID))
}

// resume fires SessionStart for the same session, as a headless agent does
// when it resumes. SessionStart lists every session state.
func (r *tokenTurnRun) resume() {
	r.t.Helper()
	r.env.SimulateSessionStartWithOutput(r.s.ID)
}

// chat runs a turn that changes no files and reports no token usage.
func (r *tokenTurnRun) chat() {
	r.t.Helper()
	r.turn++
	require.NoError(r.t, r.env.SimulateUserPromptSubmitWithTranscriptPath(r.s.ID, r.s.TranscriptPath))
	r.s.CreateTranscript(fmt.Sprintf("turn %d", r.turn), nil)
	r.stop()
}

// TestTokenScope_HeadlessSessionEndedAfterNoChangeTurn covers a headless agent
// that ends its session after every prompt. A turn that changes no files leaves
// an ended state with no pending work, which listing removed as orphaned: the
// turn's tokens were lost and the next checkpoint recounted the transcript from
// line 0, storing the first turn a second time.
func TestTokenScope_HeadlessSessionEndedAfterNoChangeTurn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		run  func(r *tokenTurnRun)
	}{
		{
			name: "no-change turn with tokens",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.stop()
				r.commit("a.txt")
				r.end()
				r.start(20, 0) // no files changed
				r.stop()
				r.end()
				r.resume()
				r.start(30, 0, "c.txt")
				r.stop()
				r.checkTotal()
				r.commit("c.txt")
			},
		},
		{
			name: "no-change turn without tokens",
			run: func(r *tokenTurnRun) {
				r.start(10, 0, "a.txt")
				r.stop()
				r.commit("a.txt")
				r.end()
				r.chat()
				r.end()
				r.resume()
				r.start(30, 0, "c.txt")
				r.stop()
				r.checkTotal()
				r.commit("c.txt")
			},
		},
		{
			// Before any checkpoint the offsets are still 0, so only the
			// pending tokens keep the ended state.
			name: "first turn changes nothing",
			run: func(r *tokenTurnRun) {
				r.start(20, 0) // no files changed
				r.stop()
				r.end()
				r.resume()
				r.start(30, 0, "c.txt")
				r.stop()
				r.checkTotal()
				r.commit("c.txt")
			},
		},
		{
			name: "first turn ran a subagent",
			run: func(r *tokenTurnRun) {
				r.start(10, 5, "a.txt")
				r.stop()
				r.commit("a.txt", "sub1.txt")
				r.end()
				r.start(20, 0) // no files changed
				r.stop()
				r.end()
				r.resume()
				r.start(30, 0, "c.txt")
				r.stop()
				r.checkTotal()
				r.commit("c.txt") // must not store the subagent's tokens again
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newTokenTurnRun(t)
			tc.run(r)
			r.verify()
		})
	}
}

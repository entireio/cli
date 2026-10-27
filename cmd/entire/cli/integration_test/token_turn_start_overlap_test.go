//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// taskNotificationPrompt is the UserPromptSubmit prompt Claude Code sends when
// a background task finishes, also while a turn is still running.
const taskNotificationPrompt = "<task-notification>\n<task-id>bg1</task-id>\n<status>completed</status>\n</task-notification>"

// notify fires the hook Claude Code sends for a background task's notice.
func (r *tokenTurnRun) notify() {
	r.t.Helper()
	require.NoError(r.t, r.env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(r.s.ID, taskNotificationPrompt, r.s.TranscriptPath))
}

// interruptAndPrompt is a real prompt sent after a Ctrl-C, which fires no
// Stop, so it arrives while the interrupted turn is still ACTIVE.
func (r *tokenTurnRun) interruptAndPrompt(prompt string) {
	r.t.Helper()
	r.turn++
	require.NoError(r.t, r.env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(r.s.ID, prompt, r.s.TranscriptPath))
}

func (r *tokenTurnRun) state() *strategy.SessionState {
	r.t.Helper()
	state, err := r.env.GetSessionState(r.s.ID)
	require.NoError(r.t, err)
	require.NotNil(r.t, state)
	return state
}

func (r *tokenTurnRun) prePrompt() []byte {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.env.RepoDir, ".entire", "tmp", "pre-prompt-"+r.s.ID+".json"))
	require.NoError(r.t, err)
	return data
}

// TestTurnStartOverlap_NotificationMidTurnKeepsBaseline: a notification that
// fires while the turn runs must not restart the turn. Before the fix it
// re-baselined the transcript offset and untracked files, so Stop counted
// only the tokens after it and saw a.txt as pre-existing.
func TestTurnStartOverlap_NotificationMidTurnKeepsBaseline(t *testing.T) {
	t.Parallel()
	r := newTokenTurnRun(t)

	r.start(100, 0, "a.txt")
	before := r.state()
	baseline := r.prePrompt()

	r.notify()
	assert.Equal(t, string(baseline), string(r.prePrompt()), "the notification must leave the turn's pre-prompt baseline alone")
	after := r.state()
	assert.Equal(t, before.TurnID, after.TurnID, "the notification must not start a new turn")
	assert.Equal(t, before.LastPrompt, after.LastPrompt, "the notification is not the turn's prompt")
	promptFile, err := os.ReadFile(filepath.Join(r.env.RepoDir, ".entire", "metadata", r.s.ID, "prompt.txt"))
	if err == nil {
		assert.NotContains(t, string(promptFile), "<task-notification>")
	}

	r.more(50)
	r.stop()
	r.checkTotal()
	assert.Contains(t, r.state().FilesTouched, "a.txt", "the turn's step must record the file it wrote")

	r.commit("a.txt")
	r.verify()
}

// TestTurnStartOverlap_NotificationWhileIdleIsANewTurn: Claude Code runs a
// turn for a notification that arrives while the session is idle, so the
// guard must let it through.
func TestTurnStartOverlap_NotificationWhileIdleIsANewTurn(t *testing.T) {
	t.Parallel()
	r := newTokenTurnRun(t)

	r.start(100, 0, "a.txt")
	r.stop()
	firstTurn := r.state().TurnID

	r.notify()
	assert.NotEqual(t, firstTurn, r.state().TurnID, "an idle notification starts a new turn")
	r.more(30)
	r.stop()
	r.checkTotal()

	r.commit("a.txt")
	r.verify()
}

// TestTurnStartOverlap_MidTurnCommitThenNotification: a commit made before the
// notification must stay a turn checkpoint, so turn end finalizes it with the
// full transcript. Before the fix the notification cleared TurnCheckpointIDs.
func TestTurnStartOverlap_MidTurnCommitThenNotification(t *testing.T) {
	t.Parallel()
	r := newTokenTurnRun(t)

	r.start(100, 0, "a.txt")
	r.commit("a.txt")
	turnCheckpoints := r.state().TurnCheckpointIDs
	require.NotEmpty(t, turnCheckpoints, "a mid-turn commit is a turn checkpoint")

	r.notify()
	assert.Equal(t, turnCheckpoints, r.state().TurnCheckpointIDs, "the notification must keep the turn's checkpoints")

	r.more(50)
	r.stop()
	r.checkTotal()
	assert.Empty(t, r.state().TurnCheckpointIDs, "turn end finalizes and clears the turn's checkpoints")
}

// TestTurnStartOverlap_RealPromptWhileActiveStillRecaptures pins the fix's
// scope: a real prompt after a Ctrl-C (no Stop) still starts a fresh turn.
func TestTurnStartOverlap_RealPromptWhileActiveStillRecaptures(t *testing.T) {
	t.Parallel()
	r := newTokenTurnRun(t)

	r.start(100, 0, "a.txt")
	before := r.state()
	baseline := r.prePrompt()

	r.interruptAndPrompt("try again")
	after := r.state()
	assert.NotEqual(t, string(baseline), string(r.prePrompt()), "a real prompt re-baselines the turn")
	assert.NotEqual(t, before.TurnID, after.TurnID, "a real prompt starts a new turn")
	assert.Equal(t, "try again", after.LastPrompt)
}

// TestTurnStartOverlap_InterruptThenNotificationFoldsIntoTurn: after a Ctrl-C
// with the agent still alive, a notification Claude Code delivers as its own
// turn is still dropped, so its work folds into the interrupted turn's
// baseline. Both parts are counted, once.
func TestTurnStartOverlap_InterruptThenNotificationFoldsIntoTurn(t *testing.T) {
	t.Parallel()
	r := newTokenTurnRun(t)

	r.start(100, 0, "a.txt")
	// Ctrl-C: no Stop. The notification then runs as Claude Code's next turn.
	r.notify()
	r.more(50)
	r.stop()
	r.checkTotal()

	r.commit("a.txt")
	r.verify()
}

// TestTurnStartOverlap_NotificationRacingStopIsAKnownGap pins a known gap: a
// notification whose hook reads ACTIVE just before the turn's Stop goes IDLE
// is dropped, and the follow-up turn Claude Code then runs for it has no turn
// start, so its Stop records nothing. Without the guard the same race loses
// that turn too, because Stop deletes the notification's newer pre-prompt file. When Stop
// stops doing that, this test should count the follow-up turn.
func TestTurnStartOverlap_NotificationRacingStopIsAKnownGap(t *testing.T) {
	t.Parallel()
	r := newTokenTurnRun(t)

	// No file edits: a follow-up Stop with no turn start reads the transcript
	// from line 0, and an old edit there would take the step path instead.
	r.start(100, 0)
	r.notify() // the guard reads ACTIVE
	r.stop()   // the turn's own Stop lands after it
	r.checkTotal()

	// The follow-up turn for the notification: no turn start reached Entire.
	appendUsageMessage(r.s, "msg-race-follow-up", 30)
	require.NoError(t, r.s.TranscriptBuilder.WriteToFile(r.s.TranscriptPath))
	r.stop()
	r.checkTotal() // still 100: the follow-up turn's 30 tokens are lost
}

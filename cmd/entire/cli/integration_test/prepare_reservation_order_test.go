//go:build integration

package integration

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// The checkpoint reservation must follow the message write, not precede it: a
// trailer that never reached the message must leave no reservation behind, or
// the next unrelated commit reuses it and post-commit condenses against a
// checkpoint the commit does not name.
func TestPrepareCommitMsg_UnwrittenTrailerLeavesNoReservation(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes; the write cannot be made to fail")
	}
	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()
	prompt := "Create a.txt"
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath))
	env.WriteFile("a.txt", "agent\n")
	sess.CreateTranscript(prompt, []FileChange{{Path: "a.txt", Content: "agent\n"}})
	require.NoError(t, env.SimulateStop(sess.ID, sess.TranscriptPath))
	env.GitAdd("a.txt")

	msgFile := env.commitMsgFile()
	require.NoError(t, os.WriteFile(msgFile, []byte("Add a.txt\n"), 0o444))
	// Human flow (TTY simulated): content detection passes, the prompt defaults
	// to yes, and the write of the stamped message fails on the read-only file.
	if out, err := env.prepareCommitMsgCmd(true, msgFile, "message").CombinedOutput(); err != nil {
		t.Fatalf("prepare-commit-msg: %v\n%s", err, out)
	}
	got, err := os.ReadFile(msgFile)
	require.NoError(t, err)
	require.NotContains(t, string(got), "Entire-Checkpoint:", "precondition: the trailer write must have failed")

	state, err := env.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.True(t, state.PendingCondensationID().IsEmpty(),
		"no trailer was written, so no checkpoint may be reserved on the session")
}

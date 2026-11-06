package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Uses t.Chdir and t.Setenv — do NOT add t.Parallel().

// Prepare stamps a trailer for a session that touched no files on the strength
// of its shape alone: IDLE with a background subagent in flight, or ACTIVE
// mid-turn. The commit can then wait on the TTY prompt or the message editor,
// and meanwhile the subagent finishes without editing anything, or the turn
// ends. Post-commit used to judge the session afresh, find neither shape and
// no tracked files to overlap the commit with, and decline it, leaving the
// trailer naming a checkpoint that is never written. It now keeps prepare's
// decision for the sessions prepare stamped that way.
func TestCommitHooks_SessionStampedMidTaskCondensesAfterItSettles(t *testing.T) {
	for _, tt := range []struct {
		name             string
		tty              bool
		atPrepare        func(*SessionState)
		beforePostCommit func(*SessionState)
	}{
		{
			name:             "its read-only subagent finished",
			atPrepare:        startReadOnlySubagent,
			beforePostCommit: finishSubagent,
		},
		{
			name:             "its read-only subagent finished during the link prompt",
			tty:              true,
			atPrepare:        startReadOnlySubagent,
			beforePostCommit: finishSubagent,
		},
		{
			name: "its turn ended",
			atPrepare: func(st *SessionState) {
				st.Phase = session.PhaseActive
				turnStart := time.Now().Add(-time.Minute)
				st.LastInteractionTime = &turnStart
			},
			beforePostCommit: func(st *SessionState) { st.Phase = session.PhaseIdle },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.tty {
				// Content detection and the link prompt, which answers
				// with its default under test, instead of the fast path.
				t.Setenv(interactive.EnvTestTTY, "1")
			}
			ctx := context.Background()
			dir := setupGitRepo(t)
			t.Chdir(dir)
			s := NewManualCommitStrategy()
			saveReadOnlySession(t, s, dir, "sess-readonly", tt.atPrepare)

			// The user's own change; the session never touched it.
			testutil.WriteFile(t, dir, "user.txt", "written by the user")
			testutil.GitAdd(t, dir, "user.txt")
			msg := prepareMessage(t, "user change\n")
			cps := checkpointIDs(msg)
			require.Len(t, cps, 1, "prepare stamps the session by its shape: %q", msg)

			state, err := s.loadSessionState(ctx, "sess-readonly")
			require.NoError(t, err)
			tt.beforePostCommit(state)
			require.NoError(t, s.saveSessionState(ctx, state))
			require.False(t, condensesWithoutOverlap(state, time.Now()), "sanity: by post-commit the session has settled")

			testutil.GitCommit(t, dir, msg)
			require.NoError(t, NewManualCommitStrategy().PostCommit(ctx))

			state, err = s.loadSessionState(ctx, "sess-readonly")
			require.NoError(t, err)
			assert.Equal(t, cps[0], state.LastCheckpointID.String(), "post-commit writes the checkpoint the trailer names")
		})
	}
}

// Prepare's trust stands in only for a commit no session's tracked files
// claim. Here another session's files claim it, so the session prepare
// trusted is judged as it would have been had post-commit run at once: left
// out by the read-only gate when its subagent edited nothing, and held to
// overlap when the subagent edited a file this commit does not hold, which
// stays pending with its task record for the commit that does.
func TestCommitHooks_TrustedSessionYieldsToTheSessionThatWroteTheCommit(t *testing.T) {
	for _, tt := range []struct {
		name          string
		subagentWrote []string
	}{
		{name: "its subagent edited nothing"},
		{name: "its subagent edited a file not in the commit", subagentWrote: []string{"draft.txt"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(interactive.EnvTestTTY, "1")
			ctx := context.Background()
			dir := setupGitRepo(t)
			t.Chdir(dir)
			s := NewManualCommitStrategy()
			setupSessionWithCheckpointAndFile(t, s, dir, "sess-writer", "feature.txt")
			saveReadOnlySession(t, s, dir, "sess-trusted", startReadOnlySubagent)

			testutil.GitAdd(t, dir, "feature.txt")
			msg := prepareMessage(t, "feature\n")
			cps := checkpointIDs(msg)
			require.Len(t, cps, 1, "%q", msg)

			state, err := s.loadSessionState(ctx, "sess-trusted")
			require.NoError(t, err)
			finishSubagent(state)
			for _, f := range tt.subagentWrote {
				testutil.WriteFile(t, dir, f, "written by the subagent")
			}
			state.FilesTouched = tt.subagentWrote
			require.NoError(t, s.saveSessionState(ctx, state))

			testutil.GitCommit(t, dir, msg)
			require.NoError(t, NewManualCommitStrategy().PostCommit(ctx))

			writer, err := s.loadSessionState(ctx, "sess-writer")
			require.NoError(t, err)
			assert.Equal(t, cps[0], writer.LastCheckpointID.String(), "the session that wrote the commit condenses")
			trusted, err := s.loadSessionState(ctx, "sess-trusted")
			require.NoError(t, err)
			assert.Empty(t, trusted.LastCheckpointID.String(), "the trusted session is not condensed into it")
			assert.ElementsMatch(t, tt.subagentWrote, trusted.FilesTouched, "its own edits stay pending")
			assert.Len(t, trusted.TaskRecords, 1, "its task record waits for the commit that holds its work")
		})
	}
}

// saveReadOnlySession saves an IDLE session homed in dir with a live
// transcript and no tracked files, then lets change set its shape.
func saveReadOnlySession(t *testing.T, s *ManualCommitStrategy, dir, sessionID string, change func(*SessionState)) {
	t.Helper()
	worktreePath, err := paths.WorktreeRoot(context.Background())
	require.NoError(t, err)
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	require.NoError(t, err)
	transcript := filepath.Join(t.TempDir(), "main.jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(testTranscriptPromptResponse), 0o644))
	state := &SessionState{
		SessionID:      sessionID,
		BaseCommit:     testutil.GetHeadHash(t, dir),
		WorktreePath:   worktreePath,
		WorktreeID:     worktreeID,
		StartedAt:      time.Now(),
		Phase:          session.PhaseIdle,
		AgentType:      agent.AgentTypeClaudeCode,
		TranscriptPath: transcript,
	}
	change(state)
	require.NoError(t, s.saveSessionState(context.Background(), state))
}

func startReadOnlySubagent(st *SessionState) {
	st.TaskRecords = []session.TaskRecord{{ToolUseID: "toolu_reviewer", AgentID: "agent-reviewer", StartedAt: time.Now()}}
}

func finishSubagent(st *SessionState) {
	st.TaskRecords[0].CompletedAt = time.Now()
}

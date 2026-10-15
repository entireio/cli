package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/require"
)

const attachFailureTranscript = `{"type":"user","message":{"role":"user","content":"hello"},"uuid":"u1"}
{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}},"uuid":"a1"}
`

// failAttachWrite makes the checkpoint write fail, after writing it when
// land is true.
func failAttachWrite(t *testing.T, land bool) {
	t.Helper()
	orig := writeAttachCheckpoint
	t.Cleanup(func() { writeAttachCheckpoint = orig })
	writeAttachCheckpoint = func(ctx context.Context, store cpkg.PersistentStore, req cpkg.WriteRequest) error {
		if land {
			if err := orig(ctx, store, req); err != nil {
				return err
			}
		}
		return errors.New("injected write failure")
	}
}

// A checkpoint write that fails without storing anything puts the session
// state back as it was, so the tokens attach recorded as counted are counted
// by the session's next checkpoint instead.
func TestAttach_CheckpointWriteFailureRestoresState(t *testing.T) {
	setupAttachTestRepo(t)
	ctx := context.Background()
	sessionID := "test-attach-write-fails"
	setupClaudeTranscript(t, sessionID, attachFailureTranscript)

	store, err := session.NewStateStore(ctx)
	require.NoError(t, err)
	now := time.Now()
	prior := &session.State{
		SessionID:           sessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		Phase:               session.PhaseIdle,
		StartedAt:           now,
		LastInteractionTime: &now,
	}
	prior.SetTokenStart(0)
	require.NoError(t, store.Save(ctx, prior))

	failAttachWrite(t, false)
	var out bytes.Buffer
	err = runAttach(ctx, &out, &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true})
	require.ErrorContains(t, err, "injected write failure")
	require.NotContains(t, err.Error(), "was left as it is")

	got, err := store.Load(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, 0, got.TokenStart(), "token offset put back")
	require.True(t, got.LastCheckpointID.IsEmpty(), "no checkpoint recorded")
	require.Equal(t, session.PhaseIdle, got.Phase)
	require.NotContains(t, out.String(), "Attached session")
}

// With no state before the attach, the undo removes the state file it created
// and nothing else of the session's.
func TestAttach_CheckpointWriteFailureRemovesCreatedState(t *testing.T) {
	setupAttachTestRepo(t)
	ctx := context.Background()
	sessionID := "test-attach-write-fails-new"
	setupClaudeTranscript(t, sessionID, attachFailureTranscript)

	store, err := session.NewStateStore(ctx)
	require.NoError(t, err)
	stateDir := filepath.Join(mustGetwd(t), ".git", session.SessionStateDirName)
	require.NoError(t, os.MkdirAll(stateDir, 0o750))
	hint := filepath.Join(stateDir, sessionID+".model")
	require.NoError(t, os.WriteFile(hint, []byte("claude-test"), 0o600))

	failAttachWrite(t, false)
	var out bytes.Buffer
	err = runAttach(ctx, &out, &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true})
	require.ErrorContains(t, err, "injected write failure")

	got, err := store.Load(ctx, sessionID)
	require.NoError(t, err)
	require.Nil(t, got, "the state attach created is removed")
	require.FileExists(t, hint, "the session's other files are left alone")
}

// A write that stored the session and then failed keeps the state: the
// checkpoint holds the tokens, so putting them back would count them twice.
func TestAttach_CheckpointWriteFailureAfterStoringKeepsState(t *testing.T) {
	setupAttachTestRepo(t)
	ctx := context.Background()
	sessionID := "test-attach-write-lands"
	setupClaudeTranscript(t, sessionID, attachFailureTranscript)

	failAttachWrite(t, true)
	var out bytes.Buffer
	err := runAttach(ctx, &out, &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true})
	require.ErrorContains(t, err, "injected write failure")
	require.ErrorContains(t, err, "was left as it is")

	store, err := session.NewStateStore(ctx)
	require.NoError(t, err)
	got, err := store.Load(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.False(t, got.LastCheckpointID.IsEmpty())
	require.Positive(t, got.TokenStart(), "tokens stay counted")
}

// The undo leaves a state a hook wrote after attach saved its own.
func TestAttachStateUndo_LeavesStateAHookChanged(t *testing.T) {
	setupAttachTestRepo(t)
	ctx := context.Background()
	sessionID := "test-attach-undo-hook"
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")

	undo, err := saveAttachSessionState(ctx, nil, sessionID, agent.AgentTypeClaudeCode,
		"/t.jsonl", "", cpID, transcriptMetadata{}, &agent.TokenUsage{OutputTokens: 9}, 4, 7, attachOptions{}, nil)
	require.NoError(t, err)

	store, err := session.NewStateStore(ctx)
	require.NoError(t, err)
	hooked, err := store.Load(ctx, sessionID)
	require.NoError(t, err)
	hooked.LastCheckpointID = ""
	hooked.Phase = session.PhaseActive
	require.NoError(t, store.Save(ctx, hooked))

	require.ErrorContains(t, undo.restore(ctx), "changed since attach saved it")
	got, err := store.Load(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, session.PhaseActive, got.Phase)
}

// A Stop that adds a turn to the pending usage moves neither the offset nor
// the checkpoint ID; the undo must still leave its update alone.
func TestAttachStateUndo_LeavesPendingUsageAStopAdded(t *testing.T) {
	setupAttachTestRepo(t)
	ctx := context.Background()
	sessionID := "test-attach-undo-stop"
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")

	undo, err := saveAttachSessionState(ctx, nil, sessionID, agent.AgentTypeClaudeCode,
		"/t.jsonl", "", cpID, transcriptMetadata{}, &agent.TokenUsage{OutputTokens: 9}, 4, 7, attachOptions{}, nil)
	require.NoError(t, err)

	store, err := session.NewStateStore(ctx)
	require.NoError(t, err)
	stopped, err := store.Load(ctx, sessionID)
	require.NoError(t, err)
	stopped.CheckpointTokenUsage = &agent.TokenUsage{OutputTokens: 3}
	require.NoError(t, store.Save(ctx, stopped))

	require.ErrorContains(t, undo.restore(ctx), "changed since attach saved it")
	got, err := store.Load(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.CheckpointTokenUsage)
	require.Equal(t, 3, got.CheckpointTokenUsage.OutputTokens)
}

// Under the git-refs primary the checkpoint is a ULID in its own ref, and a
// write that stored nothing is undone there too.
func TestAttach_CheckpointWriteFailureUnderRefsPrimary(t *testing.T) {
	setupAttachTestRepo(t)
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
	ctx := context.Background()

	for _, land := range []bool{false, true} {
		sessionID := "test-attach-refs-write-fails"
		if land {
			sessionID += "-landed"
		}
		t.Run(sessionID, func(t *testing.T) {
			setupClaudeTranscript(t, sessionID, attachFailureTranscript)
			failAttachWrite(t, land)
			var out bytes.Buffer
			err := runAttach(ctx, &out, &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true})
			require.ErrorContains(t, err, "injected write failure")

			store, err := session.NewStateStore(ctx)
			require.NoError(t, err)
			got, err := store.Load(ctx, sessionID)
			require.NoError(t, err)
			if land {
				require.NotNil(t, got, "a stored checkpoint keeps the state")
				require.Positive(t, got.TokenStart())
			} else {
				require.Nil(t, got, "nothing stored, so the state attach created is removed")
			}
		})
	}
}

// A state removed after attach saved it stays removed, and the undo says so.
func TestAttachStateUndo_LeavesARemovedStateRemoved(t *testing.T) {
	setupAttachTestRepo(t)
	ctx := context.Background()
	sessionID := "test-attach-undo-removed"
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")

	store, err := session.NewStateStore(ctx)
	require.NoError(t, err)
	now := time.Now()
	require.NoError(t, store.Save(ctx, &session.State{SessionID: sessionID, StartedAt: now, LastInteractionTime: &now}))

	undo, err := saveAttachSessionState(ctx, nil, sessionID, agent.AgentTypeClaudeCode,
		"/t.jsonl", "", cpID, transcriptMetadata{}, &agent.TokenUsage{OutputTokens: 9}, 4, 7, attachOptions{}, nil)
	require.NoError(t, err)
	require.NoError(t, store.Delete(sessionID))

	require.ErrorContains(t, undo.restore(ctx), "removed since attach saved it")
	got, err := store.Load(ctx, sessionID)
	require.NoError(t, err)
	require.Nil(t, got)
}

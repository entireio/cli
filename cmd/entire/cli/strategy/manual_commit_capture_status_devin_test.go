package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/api/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/go-git/go-git/v6/plumbing"
)

type noPreparerAgent struct {
	agent.Agent
}

func TestDevinCondensationTranscript_CaptureReasons(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("no_transcript_path", func(t *testing.T) {
		t.Parallel()
		state := &SessionState{SessionID: "s", AgentType: devinAgentType, TranscriptPath: ""}
		_, _, err := devinCondensationTranscript(ctx, &stubPreparerAgent{}, state)
		var capErr *captureUnavailableError
		require.ErrorAs(t, err, &capErr)
		assert.Equal(t, "no_transcript_path", capErr.Reason)
	})

	t.Run("no_preparer", func(t *testing.T) {
		t.Parallel()
		state := &SessionState{SessionID: "s", AgentType: devinAgentType, TranscriptPath: filepath.Join(t.TempDir(), "t.jsonl")}
		_, _, err := devinCondensationTranscript(ctx, &noPreparerAgent{}, state)
		var capErr *captureUnavailableError
		require.ErrorAs(t, err, &capErr)
		assert.Equal(t, "no_preparer", capErr.Reason)
	})

	t.Run("prepare_failed", func(t *testing.T) {
		t.Parallel()
		prepareErr := errors.New("devin store unavailable")
		ag := &stubPreparerAgent{prepareErr: prepareErr}
		state := &SessionState{SessionID: "s", AgentType: devinAgentType, TranscriptPath: filepath.Join(t.TempDir(), "t.jsonl")}
		_, _, err := devinCondensationTranscript(ctx, ag, state)
		var capErr *captureUnavailableError
		require.ErrorAs(t, err, &capErr)
		assert.Equal(t, "prepare_failed", capErr.Reason)
		require.ErrorIs(t, err, prepareErr)
		assert.Equal(t, 1, ag.prepareCalls)
	})

	t.Run("snapshot_unreadable", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		state := &SessionState{SessionID: "s", AgentType: devinAgentType, TranscriptPath: dir}
		_, _, err := devinCondensationTranscript(ctx, &stubPreparerAgent{}, state)
		var capErr *captureUnavailableError
		require.ErrorAs(t, err, &capErr)
		assert.Equal(t, "snapshot_unreadable", capErr.Reason)
	})

	t.Run("snapshot_empty", func(t *testing.T) {
		t.Parallel()
		emptyPath := filepath.Join(t.TempDir(), "empty.jsonl")
		require.NoError(t, os.WriteFile(emptyPath, nil, 0o600))
		state := &SessionState{SessionID: "s", AgentType: devinAgentType, TranscriptPath: emptyPath}
		_, _, err := devinCondensationTranscript(ctx, &stubPreparerAgent{}, state)
		var capErr *captureUnavailableError
		require.ErrorAs(t, err, &capErr)
		assert.Equal(t, "snapshot_empty", capErr.Reason)
	})
}

func writeStoredPrompt(t *testing.T, root *os.Root, sessionID, prompt string) {
	t.Helper()
	name, err := storedSessionFileName(sessionID, paths.PromptFileName)
	require.NoError(t, err)
	full := filepath.Join(root.Name(), filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(prompt), 0o600))
}

func TestExtractSessionDataFrom_DevinCaptureUnavailableDegrades(t *testing.T) {
	t.Parallel()
	state, root, _ := devinCondensationFixture(t, "sess-unavail", session.PhaseIdle, devinAgentType)
	state.FilesTouched = []string{"a.go"}
	writeStoredPrompt(t, root, state.SessionID, "fix the bug\n")

	ag := &stubPreparerAgent{prepareErr: errors.New("devin store unavailable")}
	s := NewManualCommitStrategy()
	data, err := s.extractSessionDataFrom(context.Background(), ag, state, root)
	require.NoError(t, err)
	require.NotNil(t, data)
	assert.Equal(t, "unavailable", data.CaptureStatus)
	assert.Equal(t, "prepare_failed", data.CaptureReason)
	assert.Empty(t, data.Transcript)
	assert.Nil(t, data.TokenUsage)
	assert.Empty(t, data.SkillEvents)
	assert.Equal(t, []string{"fix the bug"}, data.Prompts)
	assert.Equal(t, []string{"a.go"}, data.FilesTouched)
	assert.Equal(t, 1, ag.prepareCalls)
}

func TestExtractSessionDataFrom_NonDevinPrepFailureKeepsBestEffort(t *testing.T) {
	t.Parallel()
	state, root, _ := devinCondensationFixture(t, "sess-nondevin", session.PhaseActive, types.AgentType("Mock Work Agent"))
	ag := &stubPreparerAgent{prepareErr: errors.New("transient")}
	s := NewManualCommitStrategy()
	data, err := s.extractSessionDataFrom(context.Background(), ag, state, root)
	require.NoError(t, err)
	require.NotNil(t, data)
	assert.Empty(t, data.CaptureStatus)
	assert.Empty(t, data.CaptureReason)
	assert.NotEmpty(t, data.Transcript)
}

func TestCaptureStatusMetadataJSONRoundTrip(t *testing.T) {
	t.Parallel()
	meta := checkpoint.Metadata{
		CheckpointID:            id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAV"),
		SessionID:               "s1",
		Strategy:                "manual-commit",
		TranscriptCaptureStatus: "unavailable",
		TranscriptCaptureReason: "prepare_failed",
	}
	raw, err := json.Marshal(meta)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"transcript_capture_status":"unavailable"`)
	assert.Contains(t, string(raw), `"transcript_capture_reason":"prepare_failed"`)

	var decoded checkpoint.Metadata
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, "unavailable", decoded.TranscriptCaptureStatus)
	assert.Equal(t, "prepare_failed", decoded.TranscriptCaptureReason)

	var legacy checkpoint.Metadata
	require.NoError(t, json.Unmarshal([]byte(`{"checkpoint_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","session_id":"s1","strategy":"manual-commit","created_at":"2026-01-01T00:00:00Z","checkpoints_count":1,"files_touched":[],"model":""}`), &legacy))
	assert.Empty(t, legacy.TranscriptCaptureStatus)
	assert.Empty(t, legacy.TranscriptCaptureReason)
}

func TestSessionStateCaptureRepairIDsJSONRoundTrip(t *testing.T) {
	t.Parallel()
	state := &SessionState{
		SessionID:                  "s1",
		AgentType:                  devinAgentType,
		CaptureRepairCheckpointIDs: []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV"},
	}
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "capture_repair_checkpoint_ids")
	var decoded SessionState
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, state.CaptureRepairCheckpointIDs, decoded.CaptureRepairCheckpointIDs)
}

func devinRepairFixture(t *testing.T) *finalizeFixture {
	t.Helper()
	f := newFinalizeFixture(t)
	t.Cleanup(osroot.ResetShared)
	devinFinalizeState(f)
	return f
}

func readSessionCaptureMeta(t *testing.T, f *finalizeFixture, sessionID string, cpID id.CheckpointID) *checkpoint.Metadata {
	t.Helper()
	meta, err := f.store.ReadSessionMetadata(context.Background(), cpID, 0)
	require.NoError(t, err)
	require.Equal(t, sessionID, meta.SessionID)
	return meta
}

func TestCondenseSession_DevinCaptureUnavailableWritesCheckpoint(t *testing.T) {
	f := devinRepairFixture(t)
	ctx := context.Background()
	f.restoreUnfetched(t)
	f.state.FilesTouched = []string{"a.go"}

	ag := &finalizeDevinAgent{prepareErr: errors.New("devin store unavailable")}
	registerFinalizeDevinAgent(t, ag)

	s := NewManualCommitStrategy()
	result, err := s.CondenseSession(ctx, f.repo, f.checkpointIDs[0], f.state, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.Skipped)
	assert.True(t, result.CaptureUnavailable)

	meta := readSessionCaptureMeta(t, f, f.state.SessionID, f.checkpointIDs[0])
	assert.Equal(t, "unavailable", meta.TranscriptCaptureStatus)
	assert.Equal(t, "prepare_failed", meta.TranscriptCaptureReason)

	_, err = f.store.ReadSessionContent(ctx, f.checkpointIDs[0], 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no transcript")
}

func TestRepairUnavailableCaptures_WritesRepairedTranscript(t *testing.T) {
	f := devinRepairFixture(t)
	ctx := context.Background()

	extraID := id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAY")
	require.NoError(t, f.store.Write(ctx, checkpoint.Session{
		CheckpointID: extraID,
		SessionID:    f.state.SessionID,
		Strategy:     StrategyNameManualCommit,
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
		Agent:        devinAgentType,
	}))

	f.state.CaptureRepairCheckpointIDs = []string{
		f.checkpointIDs[0].String(),
		extraID.String(),
		"01ARZ3NDEKTSV4RRFFQ69G5FAZ",
	}
	f.state.TurnCheckpointIDs = nil
	f.restoreUnfetched(t)

	ag := &finalizeDevinAgent{freshTranscript: []byte("{\"type\":\"human\",\"message\":{\"content\":\"REPAIRED\"}}\n")}
	registerFinalizeDevinAgent(t, ag)

	s := NewManualCommitStrategy()
	s.checkpointRefFetcher = func(_ context.Context, refName plumbing.ReferenceName) error { return f.restore(refName) }
	s.repairUnavailableCaptures(ctx, f.state)

	assert.Equal(t, []string{"01ARZ3NDEKTSV4RRFFQ69G5FAZ"}, f.state.CaptureRepairCheckpointIDs)

	meta0 := readSessionCaptureMeta(t, f, f.state.SessionID, f.checkpointIDs[0])
	assert.Equal(t, "repaired", meta0.TranscriptCaptureStatus)
	assert.Empty(t, meta0.TranscriptCaptureReason)

	metaExtra := readSessionCaptureMeta(t, f, f.state.SessionID, extraID)
	assert.Equal(t, "repaired", metaExtra.TranscriptCaptureStatus)

	content, err := f.store.ReadSessionContent(ctx, f.checkpointIDs[0], 0)
	require.NoError(t, err)
	assert.Contains(t, string(content.Transcript), "REPAIRED")
}

func TestRepairUnavailableCaptures_KeepsListOnPrepareFailure(t *testing.T) {
	f := devinRepairFixture(t)
	ctx := context.Background()

	f.state.CaptureRepairCheckpointIDs = []string{f.checkpointIDs[0].String()}
	ag := &finalizeDevinAgent{prepareErr: errors.New("devin store unavailable")}
	registerFinalizeDevinAgent(t, ag)

	s := NewManualCommitStrategy()
	s.repairUnavailableCaptures(ctx, f.state)

	assert.Equal(t, []string{f.checkpointIDs[0].String()}, f.state.CaptureRepairCheckpointIDs)
	assert.Equal(t, 1, ag.prepareCalls)
}

func TestFinalizeAllTurnCheckpoints_RemovesRepairedIDsOnSuccess(t *testing.T) {
	f := devinRepairFixture(t)
	ctx := context.Background()

	extraID := id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAY")
	require.NoError(t, f.store.Write(ctx, checkpoint.Session{
		CheckpointID: extraID,
		SessionID:    f.state.SessionID,
		Strategy:     StrategyNameManualCommit,
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
		Agent:        devinAgentType,
	}))

	f.state.CaptureRepairCheckpointIDs = []string{
		f.checkpointIDs[0].String(),
		extraID.String(),
	}

	ag := &finalizeDevinAgent{freshTranscript: []byte("{\"type\":\"human\",\"message\":{\"content\":\"FINALIZED\"}}\n")}
	registerFinalizeDevinAgent(t, ag)

	s := NewManualCommitStrategy()
	s.checkpointRefFetcher = func(_ context.Context, refName plumbing.ReferenceName) error { return f.restore(refName) }
	errCount := s.finalizeAllTurnCheckpoints(ctx, f.state, time.Now().Add(30*time.Second))
	require.Equal(t, 0, errCount)

	assert.Equal(t, []string{extraID.String()}, f.state.CaptureRepairCheckpointIDs)

	meta0 := readSessionCaptureMeta(t, f, f.state.SessionID, f.checkpointIDs[0])
	assert.Empty(t, meta0.TranscriptCaptureStatus)
	assert.Empty(t, meta0.TranscriptCaptureReason)
}


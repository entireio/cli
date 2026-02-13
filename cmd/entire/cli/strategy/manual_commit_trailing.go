package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// handleTrailingTranscript appends post-commit conversation to the prior checkpoint.
// Called from HandleTurnEnd (after SaveChanges) and InitializeSession (on new prompt).
// The call is idempotent: CheckpointTranscriptStart is advanced after each append,
// preventing double-appends across the two call sites. Only appends if:
//   - LastCheckpointID is set (a condensation happened this session)
//   - StepCount == 0 (no new files touched since condensation, meaning SaveChanges
//     did not create a new checkpoint after condensation)
//   - The live transcript has grown beyond CheckpointTranscriptStart
//
// The trailing transcript (explanation, summary, etc.) is appended to the existing
// checkpoint on entire/checkpoints/v1 without modifying FilesTouched or TokenUsage.
func (s *ManualCommitStrategy) handleTrailingTranscript(state *SessionState) error {
	logCtx := logging.WithComponent(context.Background(), "trailing-transcript")

	// Guard: no prior condensation
	if state.LastCheckpointID.IsEmpty() {
		return nil
	}

	// Guard: SaveChanges created a new checkpoint (new files touched)
	if state.StepCount > 0 {
		return nil
	}

	// Guard: transcript path must be available
	if state.TranscriptPath == "" {
		logging.Debug(logCtx, "trailing transcript: no transcript path",
			slog.String("session_id", state.SessionID),
		)
		return nil
	}

	// Read the live transcript to check length
	transcriptData, err := os.ReadFile(state.TranscriptPath)
	if err != nil {
		logging.Debug(logCtx, "trailing transcript: failed to read transcript",
			slog.String("session_id", state.SessionID),
			slog.String("error", err.Error()),
		)
		return nil // Best-effort: don't fail turn-end for this
	}

	transcriptContent := string(transcriptData)
	currentLines := countTranscriptItems(state.AgentType, transcriptContent)

	// Guard: no new content since last condensation
	if currentLines <= state.CheckpointTranscriptStart {
		logging.Debug(logCtx, "trailing transcript: no new content",
			slog.String("session_id", state.SessionID),
			slog.Int("current_lines", currentLines),
			slog.Int("checkpoint_start", state.CheckpointTranscriptStart),
		)
		return nil
	}

	logging.Info(logCtx, "appending trailing transcript to checkpoint",
		slog.String("session_id", state.SessionID),
		slog.String("checkpoint_id", state.LastCheckpointID.String()),
		slog.Int("trailing_lines", currentLines-state.CheckpointTranscriptStart),
	)

	// Extract trailing transcript (from CheckpointTranscriptStart to end)
	var trailingTranscript []byte
	if state.AgentType == agent.AgentTypeGemini {
		// For Gemini, the transcript is a single JSON blob. We can't slice it
		// by line. For now, skip trailing transcript for Gemini.
		logging.Debug(logCtx, "trailing transcript: Gemini transcript slicing not supported",
			slog.String("session_id", state.SessionID),
		)
		return nil
	}
	trailingTranscript = transcript.SliceFromLine(transcriptData, state.CheckpointTranscriptStart)
	if len(trailingTranscript) == 0 {
		return nil
	}

	// Extract prompts from trailing portion
	trailingPrompts := extractUserPrompts(state.AgentType, string(trailingTranscript))

	// Generate updated context from all prompts
	// Read existing prompts and combine with trailing ones
	allPrompts := extractUserPrompts(state.AgentType, transcriptContent)
	updatedContext := generateContextFromPrompts(allPrompts)

	// Get checkpoint store
	store, err := s.getCheckpointStore()
	if err != nil {
		return fmt.Errorf("failed to get checkpoint store: %w", err)
	}

	// Append to existing checkpoint
	if err := store.UpdateCommitted(context.Background(), checkpoint.UpdateCommittedOptions{
		CheckpointID: state.LastCheckpointID,
		SessionID:    state.SessionID,
		Transcript:   trailingTranscript,
		Prompts:      trailingPrompts,
		Context:      updatedContext,
	}); err != nil {
		return fmt.Errorf("failed to append trailing transcript: %w", err)
	}

	// Update state to reflect the new transcript position
	state.CheckpointTranscriptStart = currentLines

	logging.Info(logCtx, "trailing transcript appended successfully",
		slog.String("session_id", state.SessionID),
		slog.String("checkpoint_id", state.LastCheckpointID.String()),
		slog.Int("new_transcript_start", currentLines),
	)

	return nil
}

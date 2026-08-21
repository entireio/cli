package strategy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/transcript/imageextract"
	"github.com/entireio/cli/redact"
)

func transcriptForRecoveryComparison(agentType types.AgentType, transcript redact.RedactedBytes, assets []cpkg.TranscriptAsset) ([]byte, error) {
	codec := imageextract.CodecFor(agentType)
	if codec == nil || len(assets) == 0 {
		return transcript.Bytes(), nil
	}

	byName := make(map[string]agent.CompactedTranscriptAsset, len(assets))
	for _, asset := range assets {
		byName[asset.Name] = agent.CompactedTranscriptAsset{
			Name:      asset.Name,
			MediaType: asset.MediaType,
			Data:      asset.Data,
		}
	}
	reinjected, err := codec.ReinjectImages(transcript.Bytes(), func(name string) (agent.CompactedTranscriptAsset, bool) {
		asset, ok := byName[name]
		return asset, ok
	})
	if err != nil {
		return nil, fmt.Errorf("reinject transcript images: %w", err)
	}
	return reinjected, nil
}

func findInterruptedCondensation(
	ctx context.Context,
	store cpkg.PersistentStore,
	state *SessionState,
	transcript redact.RedactedBytes,
	assets []cpkg.TranscriptAsset,
) (id.CheckpointID, bool, error) {
	expectedTranscript, err := transcriptForRecoveryComparison(state.AgentType, transcript, assets)
	if err != nil {
		return id.EmptyCheckpointID, false, err
	}

	checkpoints, err := store.List(ctx)
	if err != nil {
		return id.EmptyCheckpointID, false, fmt.Errorf("list checkpoints: %w", err)
	}
	var candidateErrors []error
	for _, info := range checkpoints {
		if info.ListedStub || (info.SessionID != state.SessionID && !slices.Contains(info.SessionIDs, state.SessionID)) {
			continue
		}
		summary, readErr := store.Read(ctx, info.CheckpointID)
		if readErr != nil {
			candidateErrors = append(candidateErrors, fmt.Errorf("read checkpoint %s: %w", info.CheckpointID, readErr))
			continue
		}
		if summary == nil {
			candidateErrors = append(candidateErrors, fmt.Errorf("read checkpoint %s: %w", info.CheckpointID, cpkg.ErrCheckpointNotFound))
			continue
		}
		for sessionIndex := range len(summary.Sessions) {
			metadata, metadataErr := store.ReadSessionMetadata(ctx, info.CheckpointID, sessionIndex)
			if metadataErr != nil {
				candidateErrors = append(candidateErrors, fmt.Errorf("read checkpoint %s session %d metadata: %w", info.CheckpointID, sessionIndex, metadataErr))
				continue
			}
			if metadata == nil {
				candidateErrors = append(candidateErrors, fmt.Errorf("read checkpoint %s session %d metadata: %w", info.CheckpointID, sessionIndex, cpkg.ErrCheckpointNotFound))
				continue
			}
			if metadata.SessionID != state.SessionID ||
				metadata.Strategy != StrategyNameManualCommit ||
				metadata.CheckpointTranscriptStart != state.CheckpointTranscriptStart ||
				metadata.TranscriptIdentifierAtStart != state.TranscriptIdentifierAtStart ||
				metadata.CheckpointsCount != checkpointStepCount(state) ||
				metadata.SaveStepCount != state.StepCount ||
				metadata.Agent != state.AgentType ||
				metadata.TurnID != state.TurnID ||
				metadata.Kind != string(state.Kind) ||
				len(metadata.FilesTouched) != 0 {
				continue
			}
			content, contentErr := store.ReadSessionContent(ctx, info.CheckpointID, sessionIndex)
			if contentErr != nil {
				candidateErrors = append(candidateErrors, fmt.Errorf("read checkpoint %s session %d content: %w", info.CheckpointID, sessionIndex, contentErr))
				continue
			}
			if content == nil {
				candidateErrors = append(candidateErrors, fmt.Errorf("read checkpoint %s session %d content: %w", info.CheckpointID, sessionIndex, cpkg.ErrCheckpointNotFound))
				continue
			}
			if bytes.Equal(content.Transcript, expectedTranscript) {
				return info.CheckpointID, true, nil
			}
		}
	}
	if len(candidateErrors) > 0 {
		return id.EmptyCheckpointID, false, errors.Join(candidateErrors...)
	}
	return id.EmptyCheckpointID, false, nil
}

type interruptedCondensationRecovery struct {
	result *CondenseResult
	err    error
	done   bool
}

func recoverInterruptedCondensation(
	ctx, logCtx context.Context,
	enabled bool,
	store cpkg.PersistentStore,
	state *SessionState,
	transcript redact.RedactedBytes,
	assets []cpkg.TranscriptAsset,
	sessionData *ExtractedSessionData,
	transcriptSizeBaseline int64,
) interruptedCondensationRecovery {
	if !enabled {
		return interruptedCondensationRecovery{}
	}
	existingID, found, err := findInterruptedCondensation(ctx, store, state, transcript, assets)
	if err != nil {
		return interruptedCondensationRecovery{
			err:  fmt.Errorf("recover interrupted condensation: %w", err),
			done: true,
		}
	}
	if !found {
		return interruptedCondensationRecovery{}
	}

	state.PromptWindowResetPending = true
	logging.Info(logCtx, "recovered interrupted condensation",
		slog.String("session_id", state.SessionID),
		slog.String("checkpoint_id", existingID.String()),
	)
	return interruptedCondensationRecovery{
		result: &CondenseResult{
			CheckpointID:           existingID,
			SessionID:              state.SessionID,
			CheckpointsCount:       checkpointStepCount(state),
			FilesTouched:           sessionData.FilesTouched,
			Prompts:                sessionData.Prompts,
			TotalTranscriptLines:   sessionData.FullTranscriptLines,
			TranscriptSizeBaseline: transcriptSizeBaseline,
		},
		done: true,
	}
}

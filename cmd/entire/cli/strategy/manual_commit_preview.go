package strategy

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// NextCheckpointPreview describes what one session would contribute to the
// next checkpoint: the work recorded in session state since its last
// checkpoint. Turn ends write no git objects, so until a commit condenses it
// this work has no checkpoint ID of its own.
type NextCheckpointPreview struct {
	SessionID string
	Agent     types.AgentType
	// LastPrompt is the session's most recent prompt, for labelling.
	LastPrompt string
	// LastActivity is the session's last interaction (StartedAt when none).
	LastActivity time.Time
	// Turns is the number of turn-end steps recorded since the last
	// checkpoint (StepCount).
	Turns int
	// FilesTouched lists the files waiting for a commit, sorted.
	FilesTouched []string
	// TaskRecords lists the subagent task records waiting to be materialized.
	TaskRecords []NextCheckpointTaskRecord
	// Prompts are the user prompts since the last checkpoint, read from the
	// session's prompt.txt and, when that is empty, extracted from its
	// transcript after the last checkpoint's offset.
	Prompts []string
}

// NextCheckpointTaskRecord is one subagent task record in a preview.
type NextCheckpointTaskRecord struct {
	ToolUseID    string
	SubagentType string
	Description  string
	Completed    bool
}

// PreviewNextCheckpoint returns, for each session in the current worktree
// that has pending work (State.HasPendingWork), what the next commit's
// checkpoint would carry from it, most recent activity first. Fully condensed
// ended sessions are left out: PostCommit skips them too. It reads session
// state and the session's local metadata only; nothing is written.
func (s *ManualCommitStrategy) PreviewNextCheckpoint(ctx context.Context) ([]NextCheckpointPreview, error) {
	worktreePath, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve worktree root: %w", err)
	}
	sessions, err := s.findSessionsForWorktree(ctx, worktreePath)
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}

	previews := make([]NextCheckpointPreview, 0, len(sessions))
	for _, state := range sessions {
		if state == nil || !state.HasPendingWork() || (state.FullyCondensed && state.Phase == session.PhaseEnded) {
			continue
		}
		previews = append(previews, previewSession(ctx, state))
	}
	sort.SliceStable(previews, func(i, j int) bool {
		return previews[i].LastActivity.After(previews[j].LastActivity)
	})
	return previews, nil
}

func previewSession(ctx context.Context, state *SessionState) NextCheckpointPreview {
	lastActivity := state.StartedAt
	if state.LastInteractionTime != nil {
		lastActivity = *state.LastInteractionTime
	}
	files := slices.Clone(state.FilesTouched)
	sort.Strings(files)
	records := make([]NextCheckpointTaskRecord, 0, len(state.TaskRecords))
	for _, rec := range state.TaskRecords {
		records = append(records, NextCheckpointTaskRecord{
			ToolUseID:    rec.ToolUseID,
			SubagentType: rec.SubagentType,
			Description:  rec.TaskDescription,
			Completed:    !rec.CompletedAt.IsZero(),
		})
	}
	return NextCheckpointPreview{
		SessionID:    state.SessionID,
		Agent:        state.AgentType,
		LastPrompt:   state.LastPrompt,
		LastActivity: lastActivity,
		Turns:        state.StepCount,
		FilesTouched: files,
		TaskRecords:  records,
		Prompts:      previewPrompts(ctx, state),
	}
}

// previewPrompts resolves the prompts since the last checkpoint the way
// condensation does (prompt.txt, then the agent's transcript extractor from
// CheckpointTranscriptStart), without preparing a transcript: a preview must
// not run agent export commands.
func previewPrompts(ctx context.Context, state *SessionState) []string {
	if prompts := readPromptsFromFilesystem(ctx, state.SessionID); len(prompts) > 0 {
		return prompts
	}
	ag, err := agent.GetByAgentType(state.AgentType)
	if err != nil {
		return nil
	}
	var transcript []byte
	var transcriptPath string
	if state.TranscriptPath != "" {
		if resolved, resolveErr := resolveTranscriptPath(state); resolveErr == nil {
			if live, readErr := agent.ReadTranscriptFile(resolved); readErr == nil && len(live) > 0 {
				transcript, transcriptPath = live, resolved
			}
		}
	}
	if len(transcript) == 0 {
		transcript = readStoredTranscript(ctx, state.SessionID)
	}
	return resolveCondensationPrompts(ctx, ag, transcript, transcriptPath, state.CheckpointTranscriptStart)
}

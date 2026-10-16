package claudecode

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

var _ agent.CumulativeTokenSource = (*ClaudeCodeAgent)(nil)

// CumulativeTokenUsage reads the whole transcript, so the message.id dedupe in
// CalculateTokenUsage sees every line of a streamed message: Claude Code repeats
// a message's final usage on each of its content lines, and a window that
// starts mid-message would otherwise count it twice. Children are the
// subagent transcripts the session launched, each read from line 0 and keyed
// by agent ID.
func (c *ClaudeCodeAgent) CumulativeTokenUsage(_ context.Context, in agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage, error) {
	parsed, err := transcript.ParseFromBytes(in.TranscriptData)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse transcript: %w", err)
	}
	total := usageOrNil(CalculateTokenUsage(parsed))
	if in.TranscriptPath == "" || in.SessionID == "" {
		return total, nil, nil
	}

	subagentsDir := paths.SubagentsDir(filepath.Dir(in.TranscriptPath), in.SessionID)
	var children map[string]*agent.TokenUsage
	for agentID, path := range launchedSubagentTranscripts(in.TranscriptData, parsed, subagentsDir) {
		usage, err := CalculateTokenUsageFromFile(path, 0)
		if err != nil || usageOrNil(usage) == nil {
			continue // not written yet, or already cleaned up
		}
		if children == nil {
			children = make(map[string]*agent.TokenUsage)
		}
		children[agentID] = usage
	}
	return total, children, nil
}

// usageOrNil maps a transcript with no API calls to "no data".
func usageOrNil(usage *agent.TokenUsage) *agent.TokenUsage {
	if usage == nil || usage.APICallCount == 0 {
		return nil
	}
	return usage
}

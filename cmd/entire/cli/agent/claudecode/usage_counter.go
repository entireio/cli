package claudecode

import (
	"context"
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

var _ agent.UsageCounterSource = (*ClaudeCodeAgent)(nil)

// openMessageWindow is how many recent messages stay open to later lines.
// Claude Code writes one line per content block of a message, every one with
// the message's usage, and parallel tool calls can put tool results between
// them, so a message's lines are close together but not adjacent.
const openMessageWindow = 64

// NewUsageCounter counts each assistant message once by message ID, keeping
// the line with the most output (its final streaming state). Children are the
// subagent transcripts the session launched, keyed by agent ID, including
// subagents a subagent launched: their launch is in the launching subagent's
// transcript, and they share the session's subagents directory.
func (c *ClaudeCodeAgent) NewUsageCounter() agent.UsageCounter {
	return &usageCounter{Calls: agent.KeyedUsage{Policy: agent.KeepMostOutput, Window: openMessageWindow}}
}

type usageCounter struct {
	Calls agent.KeyedUsage `json:"calls"`
	// Launched maps each subagent a transcript launched to its transcript;
	// RunIDs are the Workflow runs launched, whose agents are found on disk.
	Launched map[string]string `json:"launched,omitempty"`
	RunIDs   []string          `json:"run_ids,omitempty"`
	// Dir is the session's subagents directory; Agents maps each subagent
	// transcript Files returned to its agent ID.
	Dir    string            `json:"dir,omitempty"`
	Agents map[string]string `json:"agents,omitempty"`
}

func (u *usageCounter) Files(in agent.CumulativeTokenInput) []agent.UsageFile {
	files := []agent.UsageFile{{Path: in.TranscriptPath}}
	if in.SessionID == "" {
		return files
	}
	u.Dir = paths.SubagentsDir(filepath.Dir(in.TranscriptPath), in.SessionID)
	children := subagentTranscriptPaths(nil, u.RunIDs, u.Dir)
	for agentID, path := range u.Launched {
		// A Workflow agent's direct location wins only when it exists, as in
		// subagentTranscriptPaths.
		if _, inRun := children[agentID]; inRun && !fileExists(path) {
			continue
		}
		children[agentID] = path
	}
	u.Agents = make(map[string]string, len(children))
	for _, agentID := range slices.Sorted(maps.Keys(children)) {
		u.Agents[children[agentID]] = agentID
		files = append(files, agent.UsageFile{Path: children[agentID]})
	}
	return files
}

func (u *usageCounter) Add(path string, data []byte, _ int) {
	lines, err := transcript.ParseFromBytes(data)
	if err != nil {
		return
	}
	child := u.Agents[path]
	for _, line := range lines {
		if line.Type != envelopeTypeAssistant {
			continue
		}
		var msg messageWithUsage
		// Synthetic entries (API errors) carry an ID but no usage; they are
		// not API calls.
		if json.Unmarshal(line.Message, &msg) != nil || msg.ID == "" || msg.Usage == (messageUsage{}) {
			continue
		}
		u.Calls.Add(child, msg.ID, agent.TokenUsage{
			InputTokens:         msg.Usage.InputTokens,
			CacheCreationTokens: msg.Usage.CacheCreationInputTokens,
			CacheReadTokens:     msg.Usage.CacheReadInputTokens,
			OutputTokens:        msg.Usage.OutputTokens,
			APICallCount:        1,
		})
	}
	if u.Dir == "" {
		return
	}
	for _, runID := range workflowRunIDsFromToolUseResults(data) {
		u.RunIDs = appendWorkflowRunID(u.RunIDs, runID)
	}
	for _, runID := range ExtractWorkflowRunIDs(lines) {
		u.RunIDs = appendWorkflowRunID(u.RunIDs, runID)
	}
	for agentID, launched := range subagentTranscriptPaths(spawnedAgentIDs(data, lines), nil, u.Dir) {
		if u.Launched == nil {
			u.Launched = make(map[string]string)
		}
		u.Launched[agentID] = launched
	}
}

func (u *usageCounter) Reset(path string) { u.Calls.ResetChild(u.Agents[path]) }

func (u *usageCounter) Totals(context.Context, agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage) {
	return u.Calls.Sum(""), u.Calls.Children()
}

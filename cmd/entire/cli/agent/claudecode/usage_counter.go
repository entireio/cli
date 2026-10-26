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
// transcript, and they share the session's subagents directory. A subagent's
// transcript is read whole when it changed and only its total kept, so a
// session with hundreds of subagents keeps a small cursor.
func (c *ClaudeCodeAgent) NewUsageCounter() agent.UsageCounter {
	return &usageCounter{Calls: agent.KeyedUsage{Policy: agent.KeepMostOutput, Window: openMessageWindow}}
}

type usageCounter struct {
	Calls agent.KeyedUsage `json:"calls"`
	// Launched are the subagents a transcript launched, found at
	// <dir>/agent-<id>.jsonl; RunIDs are the Workflow runs launched, whose
	// agents are found on disk.
	Launched []string `json:"launched,omitempty"`
	RunIDs   []string `json:"run_ids,omitempty"`
	// dir is the session's subagents directory and agents maps each subagent
	// transcript Files returned to its agent ID: both rebuilt each reading.
	// runs caches each Workflow run's agents for the reading.
	dir    string
	agents map[string]string
	runs   map[string]map[string]string
}

func (u *usageCounter) Files(in agent.CumulativeTokenInput) []agent.UsageFile {
	files := []agent.UsageFile{{Path: in.TranscriptPath}}
	if in.SessionID == "" {
		return files
	}
	u.dir = paths.SubagentsDir(filepath.Dir(in.TranscriptPath), in.SessionID)
	if u.runs == nil {
		u.runs = make(map[string]map[string]string)
	}
	children := make(map[string]string)
	for _, runID := range u.RunIDs {
		if _, listed := u.runs[runID]; !listed {
			u.runs[runID] = paths.WorkflowRunAgentTranscripts(u.dir, runID)
		}
		paths.MergeWorkflowAgentTranscripts(children, u.runs[runID])
	}
	for _, agentID := range u.Launched {
		path := filepath.Join(u.dir, paths.AgentTranscriptFileName(agentID))
		// A Workflow agent's direct location wins only when it exists, as in
		// subagentTranscriptPaths.
		if _, inRun := children[agentID]; inRun && !fileExists(path) {
			continue
		}
		children[agentID] = path
	}
	u.agents = make(map[string]string, len(children))
	for _, agentID := range slices.Sorted(maps.Keys(children)) {
		u.agents[children[agentID]] = agentID
		files = append(files, agent.UsageFile{Path: children[agentID], Whole: true})
	}
	return files
}

func (u *usageCounter) Add(path string, data []byte) {
	lines, err := transcript.ParseFromBytes(data)
	if err != nil {
		return
	}
	child := u.agents[path]
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
	if child != "" {
		// A subagent's transcript is read whole: keep only its total.
		u.Calls.CloseChild(child)
	}
	if u.dir == "" {
		return
	}
	for _, runID := range workflowRunIDsFromToolUseResults(data) {
		u.RunIDs = appendWorkflowRunID(u.RunIDs, runID)
	}
	for _, runID := range ExtractWorkflowRunIDs(lines) {
		u.RunIDs = appendWorkflowRunID(u.RunIDs, runID)
	}
	for agentID := range spawnedAgentIDs(data, lines) {
		if !slices.Contains(u.Launched, agentID) {
			u.Launched = append(u.Launched, agentID)
		}
	}
}

func (u *usageCounter) Reset(path string) { u.Calls.ResetChild(u.agents[path]) }

func (u *usageCounter) Totals(context.Context, agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage) {
	return u.Calls.Sum(""), u.Calls.Children()
}

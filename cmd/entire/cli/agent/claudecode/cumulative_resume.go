package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

var _ agent.ResumableCumulativeTokenSource = (*ClaudeCodeAgent)(nil)

// openMessageWindow is how many recent message IDs stay open to later lines.
// Claude Code writes one line per content block of a message, every one with
// the message's usage, and parallel tool calls can put tool results between
// them, so a message's lines are close together but not adjacent. Older
// messages are closed and never matched again.
const openMessageWindow = 64

// claudeCursorVersion changes whenever claudeCursor's meaning does, so a
// cursor from another version is ignored and the transcript read from the start.
const claudeCursorVersion = 1

type claudeCursor struct {
	agent.TranscriptCursor

	Version int `json:"v"`
	// Closed is the usage of messages no later line can extend.
	Closed agent.TokenUsage `json:"closed"`
	// Open are the latest messages, oldest first, still matched by ID.
	Open []openMessage `json:"open,omitempty"`
	// Launched maps each subagent the transcript launched to its transcript;
	// RunIDs are the Workflow runs it launched, whose agents are found on disk.
	Launched map[string]string `json:"launched,omitempty"`
	RunIDs   []string          `json:"run_ids,omitempty"`
	// Children caches each subagent's total by its file's size and fingerprint.
	Children map[string]childCursor `json:"children,omitempty"`
}

type openMessage struct {
	ID    string       `json:"id"`
	Usage messageUsage `json:"usage"`
}

type childCursor struct {
	agent.TranscriptCursor

	Path     string            `json:"path"`
	Usage    *agent.TokenUsage `json:"usage,omitempty"`
	Launched map[string]string `json:"launched,omitempty"`
}

// ResumeCumulativeTokenUsage returns what CumulativeTokenUsage returns, reading
// only the lines appended since cursor. A subagent transcript is re-read only
// when it changed.
func (c *ClaudeCodeAgent) ResumeCumulativeTokenUsage(_ context.Context, in agent.CumulativeTokenInput, cursor json.RawMessage) (*agent.TokenUsage, map[string]*agent.TokenUsage, json.RawMessage, error) {
	var cur claudeCursor
	if len(cursor) == 0 || json.Unmarshal(cursor, &cur) != nil || cur.Version != claudeCursorVersion || !cur.Resumes(in.TranscriptData) {
		cur = claudeCursor{Version: claudeCursorVersion}
	}
	end := agent.CompleteLinesEnd(in.TranscriptData, cur.Offset)
	appended := in.TranscriptData[cur.Offset:end]
	parsed, err := transcript.ParseFromBytes(appended)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to parse transcript: %w", err)
	}
	cur.addMessages(parsed)
	cur.TranscriptCursor = agent.NewTranscriptCursor(in.TranscriptData, end)

	var children map[string]*agent.TokenUsage
	if in.TranscriptPath != "" && in.SessionID != "" {
		subagentsDir := paths.SubagentsDir(filepath.Dir(in.TranscriptPath), in.SessionID)
		cur.addLaunched(appended, parsed, subagentsDir)
		children = cur.readChildren(subagentsDir)
	}

	next, err := json.Marshal(cur)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to encode token cursor: %w", err)
	}
	return cur.total(), children, next, nil
}

// addMessages folds each assistant message's usage in, keeping per open
// message the line with the most output (its final streaming state), as
// CalculateTokenUsage does.
func (cur *claudeCursor) addMessages(lines []TranscriptLine) {
	for _, line := range lines {
		if line.Type != envelopeTypeAssistant {
			continue
		}
		var msg messageWithUsage
		if json.Unmarshal(line.Message, &msg) != nil || msg.ID == "" || msg.Usage == (messageUsage{}) {
			continue
		}
		if i := cur.openIndex(msg.ID); i >= 0 {
			if msg.Usage.OutputTokens > cur.Open[i].Usage.OutputTokens {
				cur.Open[i].Usage = msg.Usage
			}
			continue
		}
		cur.Open = append(cur.Open, openMessage(msg))
		if len(cur.Open) > openMessageWindow {
			addMessageUsage(&cur.Closed, cur.Open[0].Usage)
			cur.Open = cur.Open[1:]
		}
	}
}

func (cur *claudeCursor) openIndex(id string) int {
	for i := len(cur.Open) - 1; i >= 0; i-- {
		if cur.Open[i].ID == id {
			return i
		}
	}
	return -1
}

func (cur *claudeCursor) total() *agent.TokenUsage {
	total := cur.Closed
	for _, m := range cur.Open {
		addMessageUsage(&total, m.Usage)
	}
	return usageOrNil(&total)
}

func addMessageUsage(total *agent.TokenUsage, u messageUsage) {
	total.InputTokens += u.InputTokens
	total.CacheCreationTokens += u.CacheCreationInputTokens
	total.CacheReadTokens += u.CacheReadInputTokens
	total.OutputTokens += u.OutputTokens
	total.APICallCount++
}

// addLaunched records the subagents and Workflow runs the appended lines launched.
func (cur *claudeCursor) addLaunched(appended []byte, parsed []TranscriptLine, subagentsDir string) {
	for _, runID := range workflowRunIDsFromToolUseResults(appended) {
		cur.RunIDs = appendWorkflowRunID(cur.RunIDs, runID)
	}
	for _, runID := range ExtractWorkflowRunIDs(parsed) {
		cur.RunIDs = appendWorkflowRunID(cur.RunIDs, runID)
	}
	for agentID, path := range subagentTranscriptPaths(spawnedAgentIDs(appended, parsed), nil, subagentsDir) {
		if cur.Launched == nil {
			cur.Launched = make(map[string]string)
		}
		cur.Launched[agentID] = path
	}
}

// readChildren returns every launched subagent's total, including subagents a
// subagent launched, re-reading only transcripts that changed since the cursor.
func (cur *claudeCursor) readChildren(subagentsDir string) map[string]*agent.TokenUsage {
	queue := subagentTranscriptPaths(nil, cur.RunIDs, subagentsDir)
	for agentID, path := range cur.Launched {
		// A Workflow agent's direct location wins only when it exists, as in
		// subagentTranscriptPaths.
		if _, inRun := queue[agentID]; inRun && !fileExists(path) {
			continue
		}
		queue[agentID] = path
	}
	next := make(map[string]childCursor, len(queue))
	var children map[string]*agent.TokenUsage
	for len(queue) > 0 {
		nested := make(map[string]string)
		for agentID, path := range queue {
			if _, done := next[agentID]; done {
				continue
			}
			child, ok := cur.readChild(agentID, path, subagentsDir)
			if !ok {
				continue // not written yet, or already cleaned up
			}
			next[agentID] = child
			for nestedID, nestedPath := range child.Launched {
				if _, done := next[nestedID]; !done {
					nested[nestedID] = nestedPath
				}
			}
			if child.Usage == nil {
				continue
			}
			if children == nil {
				children = make(map[string]*agent.TokenUsage)
			}
			children[agentID] = child.Usage
		}
		queue = nested
	}
	cur.Children = next
	return children
}

func (cur *claudeCursor) readChild(agentID, path, subagentsDir string) (childCursor, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // path is under the session's subagents dir
	if err != nil {
		return childCursor{}, false
	}
	if cached, ok := cur.Children[agentID]; ok && cached.Path == path && cached.Offset == len(data) && cached.Resumes(data) {
		return cached, true
	}
	lines, err := transcript.ParseFromBytes(data)
	if err != nil {
		return childCursor{}, false
	}
	return childCursor{
		Path:             path,
		TranscriptCursor: agent.NewTranscriptCursor(data, len(data)),
		Usage:            usageOrNil(CalculateTokenUsage(lines)),
		Launched:         launchedSubagentTranscripts(data, lines, subagentsDir),
	}, true
}

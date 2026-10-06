package claudecode

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// TranscriptLine is an alias to the shared transcript.Line type.
type TranscriptLine = transcript.Line

// Type aliases for internal use.
type (
	assistantMessage = transcript.AssistantMessage
	toolInput        = transcript.ToolInput
)

// ExtractModifiedFiles extracts files modified by tool calls from transcript
func ExtractModifiedFiles(lines []TranscriptLine) []string {
	fileSet := make(map[string]bool)
	var files []string

	for _, line := range lines {
		if line.Type != envelopeTypeAssistant {
			continue
		}

		var msg assistantMessage
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}

		for _, block := range msg.Content {
			if block.Type != "tool_use" {
				continue
			}

			// Check if it's a file modification tool
			isModifyTool := false
			for _, name := range FileModificationTools {
				if block.Name == name {
					isModifyTool = true
					break
				}
			}

			if !isModifyTool {
				continue
			}

			var input toolInput
			if err := json.Unmarshal(block.Input, &input); err != nil {
				continue
			}

			file := input.FilePath
			if file == "" {
				file = input.NotebookPath
			}

			if file != "" && !fileSet[file] {
				fileSet[file] = true
				files = append(files, file)
			}
		}
	}

	return files
}

// CalculateTokenUsage calculates token usage from a Claude Code transcript.
// This is specific to Claude/Anthropic's API format where each assistant message
// contains a usage object with input_tokens, output_tokens, and cache tokens.
//
// Due to streaming, multiple transcript rows may share the same message.id.
// We deduplicate by taking the row with the highest output_tokens for each message.id.
func CalculateTokenUsage(transcript []TranscriptLine) *agent.TokenUsage {
	// Map from message.id to the usage with highest output_tokens
	usageByMessageID := make(map[string]messageUsage)

	for _, line := range transcript {
		if line.Type != envelopeTypeAssistant {
			continue
		}

		var msg messageWithUsage
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}

		if msg.ID == "" {
			continue
		}

		// Keep the entry with highest output_tokens (final streaming state)
		existing, exists := usageByMessageID[msg.ID]
		if !exists || msg.Usage.OutputTokens > existing.OutputTokens {
			usageByMessageID[msg.ID] = msg.Usage
		}
	}

	// Sum up all unique messages
	usage := &agent.TokenUsage{
		APICallCount: len(usageByMessageID),
	}
	for _, u := range usageByMessageID {
		usage.InputTokens += u.InputTokens
		usage.CacheCreationTokens += u.CacheCreationInputTokens
		usage.CacheReadTokens += u.CacheReadInputTokens
		usage.OutputTokens += u.OutputTokens
	}

	return usage
}

// CalculateTokenUsageFromFile calculates token usage from a Claude Code transcript file.
// If startLine > 0, only considers lines from startLine onwards.
func CalculateTokenUsageFromFile(path string, startLine int) (*agent.TokenUsage, error) {
	if path == "" {
		return &agent.TokenUsage{}, nil
	}

	lines, err := transcript.ParseFromFileAtLine(path, startLine)
	if err != nil {
		return nil, err //nolint:wrapcheck // caller adds context
	}

	return CalculateTokenUsage(lines), nil
}

// ExtractSpawnedAgentIDs extracts agent IDs from Task tool results in a transcript.
// When a Task tool completes, the tool_result contains "agentId: <id>" in its content.
// Returns a map of agentID -> toolUseID for all spawned agents.
func ExtractSpawnedAgentIDs(transcript []TranscriptLine) map[string]string {
	agentIDs := make(map[string]string)
	forEachToolResultText(transcript, func(toolUseID, text string) {
		// Look for agentId in the text. Drop any ID that isn't path-safe:
		// callers build agent-<id>.jsonl from it and read that file, so this
		// is the choke point that keeps the path inside subagentsDir,
		// independent of extractAgentIDFromText's character handling.
		if agentID := extractAgentIDFromText(text); agentID != "" && validation.ValidateAgentID(agentID) == nil {
			agentIDs[agentID] = toolUseID
		}
	})
	return agentIDs
}

// ExtractWorkflowRunIDs returns the IDs of the Workflow runs a transcript
// launched. A Workflow call's tool_result names its run as "Run ID: <runId>"
// (Claude Code 2.1.291) and none of the agents the run launches; those agents'
// transcripts live under <subagentsDir>/workflows/<runId>/. Run IDs that are
// not path-safe are dropped, as ExtractSpawnedAgentIDs drops agent IDs.
func ExtractWorkflowRunIDs(transcript []TranscriptLine) []string {
	var runIDs []string
	forEachToolResultText(transcript, func(_, text string) {
		if runID := extractIDAfter(text, "Run ID: ", true); runID != "" &&
			validation.ValidateWorkflowRunID(runID) == nil && !slices.Contains(runIDs, runID) {
			runIDs = append(runIDs, runID)
		}
	})
	return runIDs
}

// forEachToolResultText calls fn with the text of every tool_result in the
// transcript's user lines, whether the result content is a string or an array
// of text blocks.
func forEachToolResultText(transcript []TranscriptLine, fn func(toolUseID, text string)) {
	for _, line := range transcript {
		if line.Type != "user" {
			continue
		}

		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}

		// Parse as array of content blocks (tool results)
		var contentBlocks []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(msg.Content, &contentBlocks); err != nil {
			continue
		}

		for _, block := range contentBlocks {
			if block.Type == "tool_result" {
				fn(block.ToolUseID, toolResultText(block.Content))
			}
		}
	}
}

// toolResultText returns a tool_result's content as text: an array of text
// blocks joined line by line, or a plain string.
func toolResultText(content json.RawMessage) string {
	var textBlocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &textBlocks); err == nil {
		var sb strings.Builder
		for _, tb := range textBlocks {
			if tb.Type == "text" {
				sb.WriteString(tb.Text + "\n")
			}
		}
		return sb.String()
	}
	var str string
	if err := json.Unmarshal(content, &str); err == nil {
		return str
	}
	return ""
}

// extractAgentIDFromText extracts an agent ID from text containing "agentId: <id>".
func extractAgentIDFromText(text string) string {
	return extractIDAfter(text, "agentId: ", false)
}

// extractIDAfter extracts the identifier following the first occurrence of
// prefix in text: letters and digits, plus '_' and '-' when withSeparators is
// set. Agent IDs are alphanumeric; run IDs ("wf_e5264e60-494") are not.
func extractIDAfter(text, prefix string, withSeparators bool) string {
	idx := strings.Index(text, prefix)
	if idx == -1 {
		return ""
	}

	start := idx + len(prefix)
	end := start
	for end < len(text) && (text[end] >= 'a' && text[end] <= 'z' ||
		text[end] >= 'A' && text[end] <= 'Z' ||
		text[end] >= '0' && text[end] <= '9' ||
		withSeparators && (text[end] == '_' || text[end] == '-')) {
		end++
	}
	return text[start:end]
}

// CalculateTotalTokenUsage calculates token usage for a turn, including subagents.
// It parses the main transcript bytes from startLine, extracts spawned agent IDs,
// and calculates their token usage from transcript files in subagentsDir.
func (c *ClaudeCodeAgent) ExtractSkillEvents(transcriptData []byte, startLine int) ([]agent.SkillEvent, error) {
	if len(transcriptData) == 0 {
		return nil, nil
	}

	sliced := transcript.SliceFromLine(transcriptData, startLine)
	parsed, err := transcript.ParseFromBytes(sliced)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}

	var events []agent.SkillEvent
	for i, line := range parsed {
		if line.Type != envelopeTypeAssistant {
			continue
		}

		var msg assistantMessage
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}

		for _, block := range msg.Content {
			if block.Type != transcript.ContentTypeToolUse || block.Name != "Skill" {
				continue
			}
			var input toolInput
			if err := json.Unmarshal(block.Input, &input); err != nil {
				continue
			}
			skillName := strings.TrimSpace(input.Skill)
			if skillName == "" {
				continue
			}

			native := map[string]string{"tool_name": "Skill"}
			if block.ID != "" {
				native["tool_use_id"] = block.ID
			}
			events = append(events, agent.SkillEvent{
				ID:        claudeSkillEventID(block.ID, startLine+i),
				EventType: agent.SkillEventTypeToolInvocation,
				Skill: agent.SkillEventSkill{
					Name: skillName,
				},
				Source: agent.SkillEventSource{
					Agent:      string(agent.AgentNameClaudeCode),
					Signal:     agent.SkillSignalClaudeSkillToolUse,
					Confidence: agent.SkillConfidenceExplicit,
				},
				TranscriptAnchor: &agent.SkillEventTranscriptAnchor{
					Unit:      "line",
					Start:     startLine + i,
					End:       startLine + i + 1,
					EntryIDs:  nonEmptyStrings(line.UUID),
					ToolUseID: block.ID,
				},
				Native: native,
				Collapse: agent.SkillEventCollapse{
					Target:           agent.SkillCollapseTargetToolPair,
					Label:            "Skill: " + skillName,
					DefaultCollapsed: true,
				},
			})
		}
	}
	return events, nil
}

func claudeSkillEventID(toolUseID string, line int) string {
	if toolUseID != "" {
		return "claude-skill-" + toolUseID
	}
	return fmt.Sprintf("claude-skill-line-%d", line)
}

func nonEmptyStrings(values ...string) []string {
	var out []string
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func (c *ClaudeCodeAgent) CalculateTotalTokenUsage(transcriptData []byte, startLine int, subagentsDir string) (*agent.TokenUsage, error) {
	if len(transcriptData) == 0 {
		return &agent.TokenUsage{}, nil
	}

	// Slice to the relevant portion and parse
	sliced := transcript.SliceFromLine(transcriptData, startLine)
	parsed, err := transcript.ParseFromBytes(sliced)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}

	// Calculate token usage from parsed transcript
	mainUsage := CalculateTokenUsage(parsed)

	if subagentsDir == "" {
		return mainUsage, nil
	}

	// Extract spawned agent IDs from the FULL transcript (startLine=0), not the
	// sliced portion. A subagent spawned before this checkpoint's startLine can
	// keep writing to its transcript in later turns; scanning only the slice
	// would miss it and undercount subagent token usage (#329).
	//
	// PERF (considered, retained deliberately): this re-parses the full
	// transcript in addition to the sliced parse above — two JSONL parses per
	// call, growing with session length. A single-pass version was rejected as
	// not worth the risk: ParseFromBytes silently drops malformed lines, so a
	// parsed-entry index does not correspond to a raw line number and naively
	// slicing the full parse at startLine would misattribute main-agent usage;
	// doing it safely would mean threading raw-line numbers through the shared
	// transcript parser used by every agent. A cheap line scan for the Task
	// marker instead of a full parse would duplicate ExtractSpawnedAgentIDs'
	// nested tool_result decoding. The common no-subagent case already avoids
	// this cost entirely via the subagentsDir == "" short-circuit above.
	fullParsed, err := transcript.ParseFromBytes(transcriptData)
	if err != nil {
		return nil, fmt.Errorf("failed to parse full transcript: %w", err)
	}
	agentPaths := subagentTranscriptPaths(ExtractSpawnedAgentIDs(fullParsed), ExtractWorkflowRunIDs(fullParsed), subagentsDir)

	// Calculate subagent token usage. This re-reads each subagent transcript from
	// line 0 on every call, so mainUsage.SubagentTokens is a cumulative-since-
	// session-start snapshot — see the CalculateTotalTokenUsage interface contract
	// in cmd/entire/cli/agent for how callers must accumulate it.
	if len(agentPaths) > 0 {
		subagentUsage := &agent.TokenUsage{}
		for _, agentPath := range agentPaths {
			agentUsage, err := CalculateTokenUsageFromFile(agentPath, 0)
			if err != nil {
				// Agent transcript may not exist yet or may have been cleaned up
				continue
			}
			subagentUsage.InputTokens += agentUsage.InputTokens
			subagentUsage.CacheCreationTokens += agentUsage.CacheCreationTokens
			subagentUsage.CacheReadTokens += agentUsage.CacheReadTokens
			subagentUsage.OutputTokens += agentUsage.OutputTokens
			subagentUsage.APICallCount += agentUsage.APICallCount
		}
		if subagentUsage.APICallCount > 0 {
			mainUsage.SubagentTokens = subagentUsage
		}
	}

	return mainUsage, nil
}

// subagentTranscriptPaths maps every subagent the transcript launched to its
// transcript: the agents named by "agentId:" tool results (spawnedAgentIDs),
// at <subagentsDir>/agent-<id>.jsonl, plus the agents of every Workflow run
// the transcript launched (runIDs), under <subagentsDir>/workflows/<runId>/
// (#2685). A Workflow's tool result names its run, never its agents, so those
// are found on disk — but only for runs the transcript names, so a transcript
// prefix (import computes one per turn) counts only the runs launched so far.
// An agent found both ways is read from its direct location when that file
// exists, else from the run directory.
func subagentTranscriptPaths(spawnedAgentIDs map[string]string, runIDs []string, subagentsDir string) map[string]string {
	agentPaths := make(map[string]string)
	for _, runID := range runIDs {
		maps.Copy(agentPaths, paths.WorkflowRunAgentTranscripts(subagentsDir, runID))
	}
	for agentID := range spawnedAgentIDs {
		direct := filepath.Join(subagentsDir, paths.AgentTranscriptFileName(agentID))
		if _, inRun := agentPaths[agentID]; inRun && !fileExists(direct) {
			continue
		}
		agentPaths[agentID] = direct
	}
	return agentPaths
}

// fileExists reports whether path names an existing file.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ExtractAllModifiedFiles extracts files modified by both the main agent and
// any subagents spawned via the Task tool. It parses the main transcript bytes from
// startLine, collects modified files from the main agent, then reads each
// subagent's transcript from subagentsDir to collect their modified files too.
// The result is a deduplicated list of all modified file paths.
func (c *ClaudeCodeAgent) ExtractAllModifiedFiles(transcriptData []byte, startLine int, subagentsDir string) ([]string, error) {
	if len(transcriptData) == 0 {
		return nil, nil
	}

	// Slice to the relevant portion and parse
	sliced := transcript.SliceFromLine(transcriptData, startLine)
	parsed, err := transcript.ParseFromBytes(sliced)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}

	// Collect modified files from main agent
	fileSet := make(map[string]bool)
	var files []string
	for _, f := range ExtractModifiedFiles(parsed) {
		if !fileSet[f] {
			fileSet[f] = true
			files = append(files, f)
		}
	}

	if subagentsDir == "" {
		return files, nil
	}

	// Find spawned subagents from the FULL transcript (startLine=0): a subagent
	// spawned before this checkpoint's startLine may keep modifying files in
	// later turns, and scanning only the slice would miss it (#329). Main-agent
	// file extraction above stays scoped to the slice.
	//
	// PERF: the second full-transcript parse is retained deliberately for the
	// same reasons documented on CalculateTotalTokenUsage above; the common
	// no-subagent case is short-circuited by the subagentsDir == "" guard.
	fullParsed, err := transcript.ParseFromBytes(transcriptData)
	if err != nil {
		return nil, fmt.Errorf("failed to parse full transcript: %w", err)
	}
	agentIDs := ExtractSpawnedAgentIDs(fullParsed)
	for agentID := range agentIDs {
		agentPath := filepath.Join(subagentsDir, paths.AgentTranscriptFileName(agentID))
		agentLines, agentErr := transcript.ParseFromFileAtLine(agentPath, 0)
		if agentErr != nil {
			// Subagent transcript may not exist yet or may have been cleaned up
			continue
		}
		for _, f := range ExtractModifiedFiles(agentLines) {
			if !fileSet[f] {
				fileSet[f] = true
				files = append(files, f)
			}
		}
	}

	return files, nil
}

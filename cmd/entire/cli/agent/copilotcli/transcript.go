package copilotcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// Compile-time interface checks.
var (
	_ agent.TranscriptAnalyzer = (*CopilotCLIAgent)(nil)
	_ agent.TokenCalculator    = (*CopilotCLIAgent)(nil)
)

// copilotEvent is a single line in events.jsonl.
type copilotEvent struct {
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	ParentID  string          `json:"parentId"`
	AgentID   string          `json:"agentId"`

	// Line is the event's 1-based line number in the transcript, so callers
	// can compare it against a line-count offset.
	Line int `json:"-"`
}

const (
	eventTypeUserMessage     = "user.message"
	eventTypeAssistantMsg    = "assistant.message"
	eventTypeToolExecDone    = "tool.execution_complete"
	eventTypeModelChange     = "session.model_change"
	eventTypeSubagentStarted = "subagent.started"
	eventTypeSubagentDone    = "subagent.completed"
)

// userMessageData is the data payload for user.message events.
// IMPORTANT: The field is "content", not "message" — verified from real Copilot JSONL.
type userMessageData struct {
	Content string `json:"content"`
}

// modelChangeData is the data payload for session.model_change events.
// Copilot CLI emits this early in the transcript with the LLM model being used.
type modelChangeData struct {
	NewModel string `json:"newModel"`
}

type toolExecCompleteData struct {
	ToolCallID       string        `json:"toolCallId"`
	ParentToolCallID string        `json:"parentToolCallId"`
	Model            string        `json:"model"`
	ToolTelemetry    toolTelemetry `json:"toolTelemetry"`
}

type toolTelemetry struct {
	Metrics              toolMetrics    `json:"metrics"`
	Properties           toolProperties `json:"properties"`
	RestrictedProperties toolProperties `json:"restrictedProperties"`
}

type toolMetrics struct {
	LinesAdded   int `json:"linesAdded"`
	LinesRemoved int `json:"linesRemoved"`
}

// toolProperties contains string-encoded metadata from tool execution.
// filePaths is a JSON-encoded string array, e.g. "[\"path/to/file.txt\"]".
type toolProperties struct {
	FilePaths string `json:"filePaths"`
}

// parseEventsFromBytes scans JSONL data and returns all parsed events.
// Malformed lines are silently skipped.
func parseEventsFromBytes(data []byte) ([]copilotEvent, error) {
	return parseEventsFromOffset(data, 0)
}

// parseEventsFromOffset scans JSONL data and returns events starting after
// the first startOffset lines. Malformed lines are silently skipped.
func parseEventsFromOffset(data []byte, startOffset int) ([]copilotEvent, error) {
	var events []copilotEvent
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), 10*1024*1024) // 10 MB max line
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		if lineNum <= startOffset {
			continue
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var ev copilotEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // skip malformed lines
		}
		ev.Line = lineNum
		events = append(events, ev)
	}

	if err := scanner.Err(); err != nil {
		return events, fmt.Errorf("transcript scanner error: %w", err)
	}

	return events, nil
}

// extractModifiedFilesFromEvents collects file paths from tool.execution_complete
// events and returns a deduplicated list.
func extractModifiedFilesFromEvents(events []copilotEvent) []string {
	return extractModifiedFilesMatching(events, func(copilotEvent, toolExecCompleteData) bool { return true })
}

func extractModifiedFilesMatching(events []copilotEvent, include func(copilotEvent, toolExecCompleteData) bool) []string {
	seen := make(map[string]bool)
	var files []string

	for i := range events {
		if events[i].Type != eventTypeToolExecDone {
			continue
		}

		var data toolExecCompleteData
		if err := json.Unmarshal(events[i].Data, &data); err != nil {
			continue
		}
		if !include(events[i], data) {
			continue
		}

		// filePaths is a JSON-encoded string array in properties, e.g. "[\"path/to/file\"]"
		filePaths := data.ToolTelemetry.Properties.FilePaths
		if filePaths == "" {
			filePaths = data.ToolTelemetry.RestrictedProperties.FilePaths
		}
		if filePaths == "" {
			continue
		}
		var paths []string
		if err := json.Unmarshal([]byte(filePaths), &paths); err != nil {
			continue
		}
		for _, fp := range paths {
			if fp != "" && !seen[fp] {
				seen[fp] = true
				files = append(files, fp)
			}
		}
	}

	return files
}

type subagentStartedData struct {
	ToolCallID       string `json:"toolCallId"`
	AgentType        string `json:"agentType"`
	AgentName        string `json:"agentName"`
	AgentDescription string `json:"agentDescription"`
}

type subagentEvidence struct {
	AgentID         string
	ToolUseID       string
	SubagentType    string
	TaskDescription string
	Files           []string
}

// extractSubagentEvidence joins current Copilot transcripts on child UUID. For
// older stops without an ID, it accepts only one incomplete start with the same
// agent name. Ambiguous or incomplete identity fails closed.
func extractSubagentEvidence(events []copilotEvent, agentID, agentName string) (subagentEvidence, bool) {
	if agentID == "" && agentName == "" {
		return subagentEvidence{}, false
	}

	completed := make(map[string]bool)
	if agentID == "" {
		for i := range events {
			if events[i].Type != eventTypeSubagentDone {
				continue
			}
			var data subagentStartedData
			if json.Unmarshal(events[i].Data, &data) == nil {
				completed[events[i].AgentID+"\x00"+data.ToolCallID] = true
			}
		}
	}

	matchedAgentID, toolUseID := "", ""
	var started subagentStartedData
	for i := range events {
		if events[i].Type != eventTypeSubagentStarted {
			continue
		}
		var data subagentStartedData
		if json.Unmarshal(events[i].Data, &data) != nil || data.ToolCallID == "" {
			continue
		}
		if agentID != "" && events[i].AgentID != agentID {
			continue
		}
		if agentID == "" && (data.AgentName != agentName || completed[events[i].AgentID+"\x00"+data.ToolCallID]) {
			continue
		}
		if toolUseID != "" && (toolUseID != data.ToolCallID || matchedAgentID != events[i].AgentID) {
			return subagentEvidence{}, false
		}
		matchedAgentID = events[i].AgentID
		toolUseID = data.ToolCallID
		started = data
	}
	if matchedAgentID == "" || toolUseID == "" {
		return subagentEvidence{}, false
	}

	files := extractModifiedFilesMatching(events, func(ev copilotEvent, data toolExecCompleteData) bool {
		return ev.AgentID == matchedAgentID && data.ParentToolCallID == toolUseID
	})
	subagentType := started.AgentType
	if subagentType == "" {
		subagentType = started.AgentName
	}
	return subagentEvidence{
		AgentID:         matchedAgentID,
		ToolUseID:       toolUseID,
		SubagentType:    subagentType,
		TaskDescription: started.AgentDescription,
		Files:           files,
	}, true
}

// extractPromptsFromEvents collects content from user.message events.
func extractPromptsFromEvents(events []copilotEvent) []string {
	var prompts []string

	for i := range events {
		if events[i].Type != eventTypeUserMessage {
			continue
		}

		var data userMessageData
		if err := json.Unmarshal(events[i].Data, &data); err != nil {
			continue
		}

		if data.Content != "" {
			prompts = append(prompts, data.Content)
		}
	}

	return prompts
}

// lastEventField scans events newest-first for entries of eventType,
// returning the first non-empty value extract produces (skipping entries
// whose data doesn't unmarshal).
func lastEventField[T any](events []copilotEvent, eventType string, extract func(T) string) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != eventType {
			continue
		}

		var data T
		if err := json.Unmarshal(events[i].Data, &data); err != nil {
			continue
		}

		if v := extract(data); v != "" {
			return v
		}
	}

	return ""
}

// extractModelFromEvents returns the model from transcript events.
// First checks session.model_change events, then falls back to the model field
// in tool.execution_complete events (Copilot CLI includes model per tool call).
func extractModelFromEvents(events []copilotEvent) string {
	if model := lastEventField(events, eventTypeModelChange,
		func(d modelChangeData) string { return d.NewModel }); model != "" {
		return model
	}
	return lastEventField(events, eventTypeToolExecDone,
		func(d toolExecCompleteData) string { return d.Model })
}

// copilotTokenCounts is the token block Copilot CLI writes both per API call
// (session.usage_record) and per model in the session.shutdown aggregate.
//
// InputTokens is the TOTAL prompt size: it includes CacheReadTokens and
// CacheWriteTokens. Copilot's own billing breakdown (tokenDetails.input) is
// inputTokens - cacheReadTokens - cacheWriteTokens, on every Copilot version
// that writes one (observed 1.0.49 through 1.0.97). ReasoningTokens is a subset
// of OutputTokens (tokenDetails.output equals outputTokens), so it is not read.
type copilotTokenCounts struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
}

// toTokenUsage converts Copilot's counts to Entire's convention, where
// InputTokens is fresh (uncached) input only. The subtraction is clamped so a
// malformed record cannot produce negative input.
func (c copilotTokenCounts) toTokenUsage(apiCalls int) *agent.TokenUsage {
	return &agent.TokenUsage{
		InputTokens:         max(0, c.InputTokens-c.CacheReadTokens-c.CacheWriteTokens),
		CacheCreationTokens: c.CacheWriteTokens,
		CacheReadTokens:     c.CacheReadTokens,
		OutputTokens:        c.OutputTokens,
		APICallCount:        apiCalls,
	}
}

// sessionShutdownData is the data payload for session.shutdown events.
// Contains aggregate model metrics. A resumed session appends one shutdown per
// process run; whether each covers only its run or the whole session so far
// depends on the Copilot release (see extractTokenUsageFromEvents).
// modelMetrics is a JSON object keyed by model name (e.g. "claude-sonnet-4.6"),
// not an array — using map[string] here matches the real Copilot CLI wire format.
type sessionShutdownData struct {
	ModelMetrics map[string]modelMetric `json:"modelMetrics"`
}

// modelMetric contains per-model token usage aggregates.
// The model name is the map key in sessionShutdownData.ModelMetrics, not a field.
type modelMetric struct {
	Requests modelRequests      `json:"requests"`
	Usage    copilotTokenCounts `json:"usage"`
}

type modelRequests struct {
	Count int `json:"count"`
}

// usageRecordData is the data payload for session.usage_record events, which
// Copilot CLI writes once per model API call (main agent and subagents alike),
// immediately before the assistant.message for that call. Observed from
// Copilot CLI 1.0.94; earlier releases do not write it.
type usageRecordData struct {
	Usage struct {
		copilotTokenCounts

		APICallID string `json:"apiCallId"`
	} `json:"usage"`
}

// assistantMessageTokenData is the per-call fallback for Copilot CLI releases
// that carried outputTokens on assistant.message (observed through 1.0.63;
// later releases dropped the field). It has no input or cache counts.
type assistantMessageTokenData struct {
	OutputTokens int    `json:"outputTokens"`
	APICallID    string `json:"apiCallId"`
}

const (
	eventTypeSessionShutdown = "session.shutdown"
	eventTypeUsageRecord     = "session.usage_record"
)

// CalculateTokenUsage computes token usage from the Copilot CLI JSONL transcript.
//
// Per-call usage comes from session.usage_record events, falling back to
// assistant.message outputTokens for releases that predate usage records. For
// full transcripts (fromOffset == 0), session.shutdown aggregates fill in what
// the per-call sources cannot (see extractTokenUsageFromEvents). Sliced
// transcripts never read session.shutdown: it is not checkpoint-scoped, and
// would count every earlier checkpoint's calls again.
//
// Events before fromOffset are still parsed, but only to learn which calls
// were already counted, so a call is never counted again in a later slice.
func (c *CopilotCLIAgent) CalculateTokenUsage(transcriptData []byte, fromOffset int) (*agent.TokenUsage, error) {
	events, err := parseEventsFromOffset(transcriptData, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript for token usage: %w", err)
	}

	return extractTokenUsageFromEvents(events, fromOffset), nil
}

// copilotRun accumulates the per-call usage of the events between two
// session.shutdown events (or the transcript edges).
type copilotRun struct {
	usage    *agent.TokenUsage // usage records, plus outputTokens for calls without one
	messages int               // assistant.message events: one per API call
	records  int               // session.usage_record events
}

// extractTokenUsageFromEvents sums token usage over the events after line
// fromLine, counting every API call from exactly one source. Events at or
// before fromLine are walked too, but only to record which calls they
// counted; this is what makes independently computed checkpoint slices add up
// to the full read.
//
// Per call, the first source in file order wins: a session.usage_record, or
// an assistant.message outputTokens value (older releases), keyed by
// apiCallId. Copilot writes a call's record before its message, so on every
// release observed the record wins. A message without an apiCallId is counted
// only if no usage record has appeared earlier in its run; deciding by what
// came earlier, not by the whole file, keeps a prefix's count unchanged by
// what is appended after it.
//
// For a full read (fromLine == 0), the events are also walked as runs
// separated by session.shutdown. A shutdown's modelMetrics covers either only
// its own run (observed on Copilot CLI 1.0.3) or everything since the session
// started (observed on 1.0.97, where `--resume` appends to the same file).
// Rather than assume one by version, each shutdown is classified by its
// request count, which equals the number of assistant.message events it covers
// on every release observed: the messages since the previous shutdown (per
// run) or since the start (cumulative). Its share of this run is then the
// shutdown itself, or the shutdown minus what earlier runs were counted as.
//
// A run whose every call has a usage record is counted from the records, which
// are exact. Otherwise the shutdown's share stands in for the run, since it
// carries the input and cache counts the older per-call source lacks. A
// shutdown that is unusable, or whose request count matches neither reading,
// is ignored and its run is counted per call. The run after the last shutdown
// is always counted per call.
func extractTokenUsageFromEvents(events []copilotEvent, fromLine int) *agent.TokenUsage {
	preferSessionShutdown := fromLine == 0
	countedCalls := map[string]bool{} // apiCallIds already counted by either source
	runHasRecord := false             // a usage record has appeared in the current run

	total := &agent.TokenUsage{}
	// covered is what has been counted for every run closed by a shutdown so
	// far, and coveredMessages the assistant.message count over the same span.
	covered := &agent.TokenUsage{}
	coveredMessages := 0
	sawShutdown := false
	run := copilotRun{usage: &agent.TokenUsage{}}

	for i := range events {
		counting := events[i].Line > fromLine
		switch events[i].Type {
		case eventTypeUsageRecord:
			var data usageRecordData
			if err := json.Unmarshal(events[i].Data, &data); err != nil {
				continue
			}
			runHasRecord = true
			if id := data.Usage.APICallID; id != "" {
				if countedCalls[id] {
					continue // a repeated record, or a call its message already counted
				}
				countedCalls[id] = true
			}
			if counting {
				run.records++
				run.usage = types.AddTokenUsage(run.usage, data.Usage.toTokenUsage(1))
			}

		case eventTypeAssistantMsg:
			if counting {
				run.messages++
			}
			var data assistantMessageTokenData
			if err := json.Unmarshal(events[i].Data, &data); err != nil || data.OutputTokens <= 0 {
				continue
			}
			if id := data.APICallID; id != "" {
				if countedCalls[id] {
					continue // counted from its session.usage_record
				}
				countedCalls[id] = true
			} else if runHasRecord {
				// Every assistant.message in a record-writing run observed so
				// far (main agent and subagents, 1.0.94-1.0.97) carries an
				// apiCallId, so an unmatched one is assumed covered by a
				// record rather than risk counting the call twice.
				logging.Debug(context.Background(), "copilot-cli: skipping outputTokens on an assistant.message without apiCallId in a run with usage records",
					"outputTokens", data.OutputTokens)
				continue
			}
			if counting {
				run.usage = types.AddTokenUsage(run.usage, &agent.TokenUsage{OutputTokens: data.OutputTokens, APICallCount: 1})
			}

		case eventTypeSessionShutdown:
			runHasRecord = false
			if !preferSessionShutdown {
				continue
			}
			share := shutdownShare(sessionShutdownUsage(events[i].Data), covered, coveredMessages, run.messages, sawShutdown)
			sawShutdown = true
			runUsage := run.usage
			if share != nil && (run.records == 0 || run.records < run.messages) {
				runUsage = share
			}
			total = types.AddTokenUsage(total, runUsage)
			// Track what was actually counted, so a later cumulative
			// shutdown's share telescopes against the same total. When an
			// earlier run could only be counted per call (output only), a
			// later cumulative share also absorbs that run's input and cache
			// counts: misattributed between runs, but the total then equals
			// the cumulative shutdown, which is what a full read returns.
			covered = types.AddTokenUsage(covered, runUsage)
			coveredMessages += run.messages
			run = copilotRun{usage: &agent.TokenUsage{}}
		}
	}
	return types.AddTokenUsage(total, run.usage)
}

// shutdownShare returns the part of a session.shutdown aggregate that belongs to
// the run it closes, or nil when the shutdown cannot be attributed: no usable
// metrics, or a request count that matches neither the run's messages (a
// per-run aggregate) nor every message since the start (a cumulative one).
// The first shutdown reads the same either way, but is still checked against
// its run's messages.
func shutdownShare(shutdown, covered *agent.TokenUsage, coveredMessages, runMessages int, sawShutdown bool) *agent.TokenUsage {
	switch {
	case shutdown == nil:
		return nil
	case shutdown.APICallCount == runMessages:
		return shutdown
	case sawShutdown && shutdown.APICallCount == coveredMessages+runMessages:
		return &agent.TokenUsage{
			InputTokens:         max(0, shutdown.InputTokens-covered.InputTokens),
			CacheCreationTokens: max(0, shutdown.CacheCreationTokens-covered.CacheCreationTokens),
			CacheReadTokens:     max(0, shutdown.CacheReadTokens-covered.CacheReadTokens),
			OutputTokens:        max(0, shutdown.OutputTokens-covered.OutputTokens),
			APICallCount:        runMessages,
		}
	default:
		logging.Debug(context.Background(), "copilot-cli: session.shutdown request count matches neither its run nor the session; counting the run per call",
			"requests", shutdown.APICallCount, "runMessages", runMessages, "sessionMessages", coveredMessages+runMessages)
		return nil
	}
}

// sessionShutdownUsage returns the normalized usage summed across models in a
// session.shutdown payload, or nil when the payload carries no usable metrics.
func sessionShutdownUsage(raw json.RawMessage) *agent.TokenUsage {
	var data sessionShutdownData
	if err := json.Unmarshal(raw, &data); err != nil {
		logging.Debug(context.Background(), "copilot-cli: session.shutdown data unmarshal failed",
			"err", err)
		return nil
	}
	if len(data.ModelMetrics) == 0 {
		return nil
	}
	usage := &agent.TokenUsage{}
	for _, m := range data.ModelMetrics {
		usage = types.AddTokenUsage(usage, m.Usage.toTokenUsage(m.Requests.Count))
	}
	return usage
}

// ExtractModelFromTranscript extracts the LLM model name from a Copilot CLI
// transcript. It prefers session.model_change events (explicit model
// declarations), but falls back to the model field on tool.execution_complete
// events for Copilot CLI versions that do not emit session.model_change.
// Returns the last observed model, or empty string if unavailable.
func ExtractModelFromTranscript(ctx context.Context, transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}

	data, err := os.ReadFile(transcriptPath) //nolint:gosec // Path derived from agent hook input
	if err != nil {
		logging.Debug(ctx, "copilot-cli: failed to read transcript for model extraction",
			"transcriptPath", transcriptPath, "err", err)
		return ""
	}

	events, err := parseEventsFromBytes(data)
	if err != nil {
		logging.Debug(ctx, "copilot-cli: failed to parse transcript for model extraction",
			"transcriptPath", transcriptPath, "err", err)
		return ""
	}

	model := extractModelFromEvents(events)
	if model == "" {
		logging.Debug(ctx, "copilot-cli: no model found in transcript",
			"transcriptPath", transcriptPath, "eventCount", len(events))
	}

	return model
}

// GetTranscriptPosition returns the current line count of a Copilot CLI transcript.
// Copilot CLI uses JSONL format, so position is the number of lines.
// This is a lightweight operation that only counts lines without parsing JSON.
// Uses bufio.Reader to handle arbitrarily long lines (no size limit).
// Returns 0 if the file doesn't exist or is empty.
func (c *CopilotCLIAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}

	file, err := os.Open(path) //nolint:gosec // Path comes from Copilot CLI transcript location
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to open transcript file: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineCount := 0

	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			if readErr == io.EOF {
				if len(line) > 0 {
					lineCount++ // Count final line without trailing newline
				}
				break
			}
			return 0, fmt.Errorf("failed to read transcript: %w", readErr)
		}
		lineCount++
	}

	return lineCount, nil
}

// ExtractModifiedFilesFromOffset extracts files modified since a given line number.
// For Copilot CLI (JSONL format), offset is the starting line number.
// Uses bufio.Reader to handle arbitrarily long lines (no size limit).
// Returns:
//   - files: list of file paths modified by Copilot (from tool.execution_complete events)
//   - currentPosition: total number of lines in the file
//   - error: any error encountered during reading
func (c *CopilotCLIAgent) ExtractModifiedFilesFromOffset(_ context.Context, path string, startOffset int) (files []string, currentPosition int, err error) {
	if path == "" {
		return nil, 0, nil
	}

	file, openErr := os.Open(path) //nolint:gosec // Path comes from Copilot CLI transcript location
	if openErr != nil {
		return nil, 0, fmt.Errorf("failed to open transcript file: %w", openErr)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var events []copilotEvent
	lineNum := 0

	for {
		lineData, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, 0, fmt.Errorf("failed to read transcript: %w", readErr)
		}

		if len(lineData) > 0 {
			lineNum++
			if lineNum > startOffset {
				var ev copilotEvent
				if parseErr := json.Unmarshal(lineData, &ev); parseErr == nil {
					events = append(events, ev)
				}
				// Skip malformed lines silently
			}
		}

		if readErr == io.EOF {
			break
		}
	}

	return extractModifiedFilesFromEvents(events), lineNum, nil
}

// ExtractPrompts extracts user prompts from the transcript starting at the given offset.
func (c *CopilotCLIAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	events, err := parseEventsFromOffset(data, fromOffset)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript events: %w", err)
	}
	return extractPromptsFromEvents(events), nil
}

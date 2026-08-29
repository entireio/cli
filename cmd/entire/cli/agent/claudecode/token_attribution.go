package claudecode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

var _ agent.TokenAttributor = (*ClaudeCodeAgent)(nil)

// contentTypeToolResult is the content block type of a tool's output in a
// user row (the assistant-side counterpart is transcript.ContentTypeToolUse).
const contentTypeToolResult = "tool_result"

// Tool names whose tool_use input carries an attribution-specific field.
const (
	toolNameSkill = "Skill"
	toolNameTask  = "Task"
)

// attributionRow is the row-level envelope of a Claude Code JSONL transcript
// line, keeping the keys transcript.Line drops: the row timestamp, the
// per-call effort, and the harness-stamped attributionSkill. Key casing
// confirmed against a Claude Code 2.1.246 transcript (2026-08-28): `effort`
// and `attributionSkill` sit on every assistant row, top level, beside a
// sibling `attributionPlugin` that types.CallUsage has no field for and so is
// not decoded.
type attributionRow struct {
	Type             string          `json:"type"`
	Timestamp        string          `json:"timestamp"`
	Effort           string          `json:"effort"`
	AttributionSkill string          `json:"attributionSkill"`
	Message          json.RawMessage `json:"message"`
}

// attributionMessage is the message half of a row. Usage is a pointer so a
// row that records no usage (types.CallUsage.UsageUnknown) is told apart from
// one that records zeros. Content is left raw because a user prompt row
// carries a plain string there, not a block array.
type attributionMessage struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Usage   *messageUsage   `json:"usage"`
	Content json.RawMessage `json:"content"`
}

// contentBlockRaw is one content block of either role: tool_use blocks fill
// ID/Name/Input, tool_result blocks fill ToolUseID/Content. It is the struct
// ExtractSpawnedAgentIDs decodes tool results into as well.
type contentBlockRaw struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

// blocks decodes the message content as a block array; a string content (a
// user prompt) or anything else yields nil.
func (m *attributionMessage) blocks() []contentBlockRaw {
	var out []contentBlockRaw
	if err := json.Unmarshal(m.Content, &out); err != nil {
		return nil
	}
	return out
}

// spawnedAgent is a subagent id seen in a Task tool_result, with the tool_use
// that spawned it, in transcript order.
type spawnedAgent struct {
	agentID   string
	toolUseID string
}

// attributionWalk is the single pass over the whole transcript. Labels and
// spawned agents come from every row (the full transcript); calls, consumed
// results and Start/End only from rows at or after startLine.
type attributionWalk struct {
	startLine int
	// labels maps tool_use id → the emitting ref, from every assistant row.
	labels map[string]types.ToolUseRef
	// labelSeq is the transcript order in which each tool_use id was first
	// seen; it orders SubagentRecords deterministically.
	labelSeq map[string]int
	spawned  []spawnedAgent
	spawnIdx map[string]int // agentID → index into spawned

	calls []types.CallUsage
	// callIdx maps message.id → index into calls, for streamed rows.
	callIdx map[string]int
	// pending is the tool results seen since the current last call started;
	// they become the next new call's Consumed.
	pending []types.ToolResultRef
	start   time.Time
	end     time.Time
}

// AttributeTokens implements agent.TokenAttributor for Claude Code: one
// types.CallUsage per assistant message (message.id) from startLine onward.
//
// Mechanics, as coded:
//   - Lines are split on '\n' and every line counts toward the line index —
//     blank and malformed lines included — exactly as
//     transcript.ParseFromFileAtLineWithTotal and transcript.SliceFromLine
//     count, so startLine and CallUsage.Line stay in TokenTranscriptStart's
//     coordinate. Malformed lines are skipped; they are never an error.
//   - Rows sharing message.id are one call: Line/At/Model/Effort/ActiveSkill
//     from the first row, Usage from the row with the highest output_tokens
//     (every field from that row, APICallCount 1), Emitted the union of
//     tool_use blocks in row order. A message none of whose rows record usage
//     has UsageUnknown set and a zero Usage.
//   - ToolUseRef.Detail is transcript.ToolDetail on the decoded input;
//     SkillName is input.skill for Skill; SubagentType/Model are
//     input.subagent_type/input.model for Task.
//   - Consumed for a call is every tool_result block in user rows at or after
//     startLine, after the previous call's first row and before this call's
//     first row; results after the last call are attributed to nothing (the
//     next slice's first call consumes them). Each is labelled through a map
//     built from EVERY assistant row, so a result whose tool_use precedes
//     startLine is still labelled. Bytes is len() of the raw JSON of the
//     block's `content` field as written in the transcript — quotes, escapes
//     and array brackets included; 0 when absent. A tool_use_id found in no
//     assistant row keeps the ref with only ToolUse.ID set (its bytes did
//     enter the context) rather than dropping it.
//   - Start/End are the earliest/latest parsable row timestamps in the slice,
//     whatever the row's type.
//   - Subagents (subagentsDir != "" only): every agentId in a Task tool_result
//     anywhere in the full transcript (same rule as ExtractSpawnedAgentIDs)
//     is read from subagentsDir/agent-<id>.jsonl; a missing or unreadable file
//     is skipped silently, as CalculateTotalTokenUsage does. SubagentType is
//     the spawning ToolUseRef's; Model is the transcript's own model
//     (TokenUsage.Model), never the requested alias; Usage is nil when the
//     file holds no assistant message yet; Start/End are that file's
//     first/last parsable row timestamps. Records are in the order their
//     spawning tool_use appears.
//   - AgentReportedCost stays 0: Claude Code records no dollar cost.
//
// Error contract (agent.TokenAttributor): Claude Code transcripts are JSONL,
// so no document-level parse can fail — the only errors are impossible here
// and the result is never nil. An all-garbage transcript is an empty
// Attribution with a nil error, matching CalculateTokenUsage.
func (c *ClaudeCodeAgent) AttributeTokens(transcriptData []byte, startLine int, subagentsDir string) (*types.Attribution, error) {
	w := newAttributionWalk(startLine)
	w.walk(transcriptData)
	out := &types.Attribution{
		Calls: w.calls,
		Start: w.start,
		End:   w.end,
	}
	if subagentsDir != "" {
		out.Subagents = w.subagentRecords(subagentsDir)
	}
	return out, nil
}

func newAttributionWalk(startLine int) *attributionWalk {
	return &attributionWalk{
		startLine: startLine,
		labels:    make(map[string]types.ToolUseRef),
		labelSeq:  make(map[string]int),
		spawnIdx:  make(map[string]int),
		callIdx:   make(map[string]int),
	}
}

// walk feeds every line of the transcript to visitRow.
func (w *attributionWalk) walk(data []byte) {
	forEachLine(data, w.visitRow)
}

// forEachLine calls visit with each '\n'-delimited line and its 0-based
// index. Every line counts — blank and malformed ones included — and the
// empty tail after a trailing '\n' is not a line, which is how
// transcript.SliceFromLine and ParseFromFileAtLineWithTotal count too. Split
// by hand rather than with bufio.Scanner: a single Claude Code line can carry
// a multi-megabyte tool result.
func forEachLine(data []byte, visit func(line int, raw []byte)) {
	line := 0
	for rest := data; len(rest) > 0; line++ {
		var raw []byte
		if idx := bytes.IndexByte(rest, '\n'); idx >= 0 {
			raw, rest = rest[:idx], rest[idx+1:]
		} else {
			raw, rest = rest, nil
		}
		visit(line, raw)
	}
}

// visitRow decodes one row and dispatches on its envelope type. Rows before
// startLine only contribute labels and spawned-agent ids.
func (w *attributionWalk) visitRow(line int, raw []byte) {
	var row attributionRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return
	}
	inSlice := line >= w.startLine
	if inSlice {
		w.noteTimestamp(row.Timestamp)
	}
	if len(row.Message) == 0 {
		return
	}
	var msg attributionMessage
	if err := json.Unmarshal(row.Message, &msg); err != nil {
		return
	}
	switch row.Type {
	case envelopeTypeAssistant:
		w.visitAssistant(line, inSlice, &row, &msg)
	case transcript.TypeUser:
		w.visitUser(inSlice, &msg)
	}
}

// noteTimestamp widens Start/End with a parsable row timestamp.
func (w *attributionWalk) noteTimestamp(ts string) {
	at := parseRowTimestamp(ts)
	if at.IsZero() {
		return
	}
	if w.start.IsZero() || at.Before(w.start) {
		w.start = at
	}
	if w.end.IsZero() || at.After(w.end) {
		w.end = at
	}
}

// parseRowTimestamp parses a Claude Code row timestamp (RFC 3339, usually
// with milliseconds); zero when absent or malformed.
func parseRowTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}
	}
	return at
}

// visitAssistant registers the row's tool_use labels and, inside the slice,
// folds the row into its call (creating it on the message's first row). A row
// without message.id cannot be grouped and is not a call — the same rule
// CalculateTokenUsage applies — but its labels still count.
func (w *attributionWalk) visitAssistant(line int, inSlice bool, row *attributionRow, msg *attributionMessage) {
	emitted := w.registerEmits(msg.blocks())
	if msg.ID == "" || !inSlice {
		return
	}
	idx, exists := w.callIdx[msg.ID]
	if !exists {
		idx = len(w.calls)
		w.callIdx[msg.ID] = idx
		w.calls = append(w.calls, types.CallUsage{
			UsageUnknown: true,
			Model:        msg.Model,
			Effort:       row.Effort,
			At:           parseRowTimestamp(row.Timestamp),
			Line:         line,
			ActiveSkill:  row.AttributionSkill,
			Consumed:     w.pending,
		})
		w.pending = nil
	}
	call := &w.calls[idx]
	call.Emitted = append(call.Emitted, emitted...)
	if msg.Usage != nil && (call.UsageUnknown || msg.Usage.OutputTokens > call.Usage.OutputTokens) {
		call.Usage = callUsageFrom(msg.Usage)
		call.UsageUnknown = false
	}
}

// registerEmits builds the ToolUseRef for each tool_use block, records it in
// the label map (first sighting wins), and returns the refs in block order.
func (w *attributionWalk) registerEmits(blocks []contentBlockRaw) []types.ToolUseRef {
	var refs []types.ToolUseRef
	for _, b := range blocks {
		if b.Type != transcript.ContentTypeToolUse {
			continue
		}
		ref := toolUseRefFrom(b)
		refs = append(refs, ref)
		if _, seen := w.labels[ref.ID]; !seen {
			w.labelSeq[ref.ID] = len(w.labelSeq)
			w.labels[ref.ID] = ref
		}
	}
	return refs
}

// toolUseRefFrom reduces a tool_use block to its content-free ref. A failed
// input decode still yields the id and tool name.
func toolUseRefFrom(b contentBlockRaw) types.ToolUseRef {
	in := decodeToolInput(b.Input)
	ref := types.ToolUseRef{
		ID:     b.ID,
		Tool:   b.Name,
		Detail: transcript.ToolDetail(b.Name, in),
	}
	switch b.Name {
	case toolNameSkill:
		ref.SkillName = in.Skill
	case toolNameTask:
		ref.SubagentType = in.SubagentType
		ref.Model = in.Model
	}
	return ref
}

// decodeToolInput decodes a tool_use input best-effort: a non-string value
// under a known key makes encoding/json report an UnmarshalTypeError while
// the remaining fields still populate (see transcript.ToolInput), so the
// partial result is returned either way.
func decodeToolInput(raw json.RawMessage) toolInput {
	var in toolInput
	if len(raw) == 0 {
		return in
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return in
	}
	return in
}

// callUsageFrom copies one row's usage into a per-call TokenUsage.
func callUsageFrom(u *messageUsage) types.TokenUsage {
	return types.TokenUsage{
		InputTokens:           u.InputTokens,
		CacheCreationTokens:   u.CacheCreationInputTokens,
		CacheReadTokens:       u.CacheReadInputTokens,
		OutputTokens:          u.OutputTokens,
		APICallCount:          1,
		ThinkingTokens:        u.OutputTokensDetails.ThinkingTokens,
		CacheCreation1hTokens: u.CacheCreation.Ephemeral1hInputTokens,
	}
}

// visitUser records spawned-agent ids from Task results (full transcript)
// and, inside the slice, queues each tool_result for the next call.
func (w *attributionWalk) visitUser(inSlice bool, msg *attributionMessage) {
	for _, b := range msg.blocks() {
		if b.Type != contentTypeToolResult {
			continue
		}
		if agentID := agentIDFromToolResult(b.Content); agentID != "" {
			w.noteSpawn(agentID, b.ToolUseID)
		}
		if !inSlice {
			continue
		}
		ref, ok := w.labels[b.ToolUseID]
		if !ok {
			ref = types.ToolUseRef{ID: b.ToolUseID}
		}
		w.pending = append(w.pending, types.ToolResultRef{ToolUse: ref, Bytes: len(b.Content)})
	}
}

// noteSpawn records agentID → toolUseID, keeping first-seen order and the
// latest tool_use id (as ExtractSpawnedAgentIDs' map does).
func (w *attributionWalk) noteSpawn(agentID, toolUseID string) {
	if i, seen := w.spawnIdx[agentID]; seen {
		w.spawned[i].toolUseID = toolUseID
		return
	}
	w.spawnIdx[agentID] = len(w.spawned)
	w.spawned = append(w.spawned, spawnedAgent{agentID: agentID, toolUseID: toolUseID})
}

// subagentRecords reads each spawned agent's transcript from subagentsDir.
func (w *attributionWalk) subagentRecords(subagentsDir string) []types.SubagentRecord {
	var records []types.SubagentRecord
	for _, s := range w.spawned {
		rec, ok := readSubagentRecord(filepath.Join(subagentsDir, paths.AgentTranscriptFileName(s.agentID)), s.toolUseID)
		if !ok {
			continue
		}
		rec.SubagentType = w.labels[s.toolUseID].SubagentType
		records = append(records, rec)
	}
	sort.SliceStable(records, func(i, j int) bool {
		si, sj := w.labelSeq[records[i].ToolUseID], w.labelSeq[records[j].ToolUseID]
		if si != sj {
			return si < sj
		}
		return records[i].ToolUseID < records[j].ToolUseID
	})
	return records
}

// readSubagentRecord builds the record for one agent-<id>.jsonl: usage and
// model via the same dedupe CalculateTokenUsageFromFile applies, Start/End
// from the file's first/last row timestamps. ok is false when the file cannot
// be read — it may not exist yet or may have been cleaned up.
func readSubagentRecord(path, toolUseID string) (types.SubagentRecord, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // path is subagentsDir + validated agent id
	if err != nil {
		return types.SubagentRecord{}, false
	}
	rec := types.SubagentRecord{ToolUseID: toolUseID}
	lines, err := transcript.ParseFromBytes(data)
	if err == nil {
		usage, model := calculateTokenUsageWithModel(lines)
		if usage.APICallCount > 0 {
			usage.Model = model
			rec.Usage = usage
			rec.Model = model
		}
	}
	rec.Start, rec.End = rowTimestampBounds(data)
	return rec, true
}

// rowTimestampBounds returns the earliest and latest parsable row timestamps
// in a JSONL transcript; zero when none.
func rowTimestampBounds(data []byte) (time.Time, time.Time) {
	w := newAttributionWalk(0)
	forEachLine(data, func(_ int, raw []byte) {
		var row struct {
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal(raw, &row); err == nil {
			w.noteTimestamp(row.Timestamp)
		}
	})
	return w.start, w.end
}

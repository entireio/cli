package pi

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/pi/pijsonl"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

var _ agent.TokenAttributor = (*PiAgent)(nil)

// timeSpan is the earliest and latest timestamp noted so far; zero until the
// first note. (Private copy of the Claude Code attributor's; Task 15a lifts
// it into a shared package.)
type timeSpan struct {
	start, end time.Time
}

// note widens the span to include at; a zero at is ignored.
func (s *timeSpan) note(at time.Time) {
	if at.IsZero() {
		return
	}
	if s.start.IsZero() || at.Before(s.start) {
		s.start = at
	}
	if s.end.IsZero() || at.After(s.end) {
		s.end = at
	}
}

// attributionWalk is the single pass over the whole active branch. Labels,
// the thinking level in force and the queue of tool results come from every
// entry (the full transcript); calls, cost and the embedded timeSpan
// (Start/End) only from entries at or after startLine.
type attributionWalk struct {
	timeSpan

	startLine int
	// level is the thinking level in force: the thinkingLevel of the latest
	// thinking_level_change entry seen, "" before the first.
	level string
	// labels maps tool-call id → the emitting ref, from every assistant
	// message.
	labels map[string]types.ToolUseRef

	calls []types.CallUsage
	// pending is the tool results seen since the last assistant message, in
	// or before the slice; they become the next call's Consumed.
	pending []types.ToolResultRef
	// cost is the sum of usage.cost.total over the calls in the slice.
	cost float64
}

// AttributeTokens implements agent.TokenAttributor for Pi: one
// types.CallUsage per assistant message entry on the active conversation
// branch from startLine onward.
//
// Mechanics, as coded:
//   - The walk is pijsonl.ForEachActiveEntry over the FULL transcript, so
//     active-branch resolution sees every parentId and startLine is applied
//     by physical line index: CallUsage.Line is the 0-based line of the
//     entry, counted as pijsonl.SkipLines counts (every '\n' ends a line;
//     blank and malformed lines count and are skipped) — the coordinate
//     CalculateTokenUsage's fromOffset and GetTranscriptPosition use.
//     Off-branch entries (an abandoned fork, and the session header when the
//     transcript has a tree) contribute nothing, not even a timestamp.
//   - A thinking_level_change entry anywhere sets the level in force; each
//     later assistant message's Effort is that level verbatim ("high",
//     "low", …; "" before any change). A change before startLine still
//     applies to the slice's calls.
//   - One call per assistant message in the slice: Line and At (the entry
//     timestamp, RFC 3339; zero when unparsable), Model = message.model —
//     the bare id Pi records ("gpt-5.5", "claude-sonnet-4-5"), never
//     prefixed with message.provider, which is read but not reported —
//     Usage = input/output/cacheRead/cacheWrite/cacheWrite1h with
//     APICallCount 1, the same arithmetic as CalculateTokenUsage (Pi records
//     no thinking-token count, so ThinkingTokens stays 0). A message without
//     a usage block has UsageUnknown set and a zero Usage; it is still a call
//     and its Emitted refs still label the next call's Consumed. Summing the
//     calls' Usage reproduces CalculateTokenUsage for the same startLine.
//   - Emitted is the message's toolCall content items in order:
//     ToolUseRef{ID, Tool: name, Detail: transcript.ToolDetail(name, args)}
//     with the arguments decoded into transcript.ToolInput (Pi's `command`
//     and `path` keys are ToolInput keys). Pi's core tools carry no skill or
//     subagent, so SkillName/SubagentType/Model stay "". The labels map is
//     built from EVERY assistant message, so a result whose toolCall precedes
//     startLine is still labelled.
//   - Consumed for a call is every active-branch toolResult message after
//     the previous assistant message and before this one, wherever startLine
//     falls — the results are collected from the FULL transcript, so a call's
//     Consumed is the same in every slice that admits it and a result between
//     a pre-slice toolCall and an in-slice call is charged, once, to that
//     call. Each is labelled through the map (a toolCallId found in no
//     assistant message keeps a ref with only ID set — its bytes did enter
//     the context). Bytes is len() of the raw JSON of message.content as
//     written in the transcript. Results after the last call are attributed
//     to nothing.
//   - Start/End are the earliest/latest parsable timestamps of in-slice
//     active-branch entries of any type.
//   - AgentReportedCost is the sum of usage.cost.total over the slice's
//     calls — the dollar figure pi-ai computes from its model registry; 0
//     when no call carries one.
//   - Subagents is always empty and subagentsDir is ignored: Pi's core tool
//     set (bash, read, edit, write, grep, find, ls) spawns no subagents and
//     Pi writes no subagent transcripts.
//
// Error contract (agent.TokenAttributor): Pi transcripts are JSONL, so no
// document-level parse can fail; the only possible error is the scanner's
// (a line over pijsonl.MaxScannerLine), matching CalculateTokenUsage. Empty
// or all-garbage data is an empty Attribution with a nil error.
func (a *PiAgent) AttributeTokens(transcriptData []byte, startLine int, _ string) (*types.Attribution, error) {
	w := &attributionWalk{
		startLine: startLine,
		labels:    make(map[string]types.ToolUseRef),
	}
	if err := pijsonl.ForEachActiveEntry(transcriptData, 0, w.visit); err != nil {
		return nil, fmt.Errorf("attribute tokens: %w", err)
	}
	return &types.Attribution{
		Calls:             w.calls,
		Start:             w.start,
		End:               w.end,
		AgentReportedCost: w.cost,
	}, nil
}

// visit dispatches one active-branch entry. Entries before startLine only
// contribute labels, the thinking level and the tool-result queue.
func (w *attributionWalk) visit(line int, entry pijsonl.Entry) {
	inSlice := line >= w.startLine
	if inSlice {
		w.note(parseEntryTimestamp(entry.Timestamp))
	}
	switch entry.Type {
	case pijsonl.EntryTypeThinkingLevelChange:
		w.level = entry.ThinkingLevel
	case pijsonl.EntryTypeMessage:
		switch entry.Message.Role {
		case pijsonl.RoleAssistant:
			w.visitAssistant(line, inSlice, &entry)
		case pijsonl.RoleToolResult:
			w.visitToolResult(&entry.Message)
		}
	}
}

// parseEntryTimestamp parses a Pi entry timestamp (ISO 8601 with
// milliseconds, e.g. "2026-03-27T21:00:02.000Z"); zero when absent or
// malformed.
func parseEntryTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}
	}
	return at
}

// visitAssistant registers the message's toolCall labels and takes the
// pending tool results; inside the slice it also appends the call they were
// consumed by.
func (w *attributionWalk) visitAssistant(line int, inSlice bool, entry *pijsonl.Entry) {
	emitted := w.registerEmits(entry.Message.Content)
	consumed := w.pending
	w.pending = nil
	if !inSlice {
		return
	}
	call := types.CallUsage{
		UsageUnknown: entry.Message.Usage == nil,
		Model:        entry.Message.Model,
		Effort:       w.level,
		At:           parseEntryTimestamp(entry.Timestamp),
		Line:         line,
		Emitted:      emitted,
		Consumed:     consumed,
	}
	if u := entry.Message.Usage; u != nil {
		call.Usage = types.TokenUsage{
			InputTokens:           u.Input,
			OutputTokens:          u.Output,
			CacheReadTokens:       u.CacheRead,
			CacheCreationTokens:   u.CacheWrite,
			CacheCreation1hTokens: u.CacheWrite1h,
			APICallCount:          1,
		}
		w.cost += u.Cost.Total
	}
	w.calls = append(w.calls, call)
}

// registerEmits builds the ToolUseRef for each toolCall content item, records
// it in the label map (first sighting wins), and returns the refs in item
// order. String content (no items) yields nil.
func (w *attributionWalk) registerEmits(content json.RawMessage) []types.ToolUseRef {
	var items []pijsonl.ContentItem
	if err := json.Unmarshal(content, &items); err != nil {
		return nil
	}
	var refs []types.ToolUseRef
	for _, item := range items {
		if item.Type != pijsonl.ContentTypeToolCall {
			continue
		}
		ref := toolUseRefFrom(item)
		refs = append(refs, ref)
		if _, seen := w.labels[ref.ID]; !seen {
			w.labels[ref.ID] = ref
		}
	}
	return refs
}

// toolUseRefFrom reduces a toolCall item to its content-free ref. The
// arguments are decoded best-effort: a non-string value under a known key
// makes encoding/json report an UnmarshalTypeError while the remaining fields
// still populate, so a failed decode still yields the id, tool name and
// whatever detail was readable.
func toolUseRefFrom(item pijsonl.ContentItem) types.ToolUseRef {
	var in transcript.ToolInput
	_ = json.Unmarshal(item.Arguments, &in) //nolint:errcheck // best-effort partial decode, see doc
	return types.ToolUseRef{
		ID:     item.ID,
		Tool:   item.Name,
		Detail: transcript.ToolDetail(item.Name, in),
	}
}

// visitToolResult queues a tool result for the next assistant message,
// whatever line it is on (see AttributeTokens: Consumed is slice-independent).
func (w *attributionWalk) visitToolResult(msg *pijsonl.Message) {
	ref, ok := w.labels[msg.ToolCallID]
	if !ok {
		ref = types.ToolUseRef{ID: msg.ToolCallID}
	}
	w.pending = append(w.pending, types.ToolResultRef{ToolUse: ref, Bytes: len(msg.Content)})
}

package opencode

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

var _ agent.TokenAttributor = (*OpenCodeAgent)(nil)

// Part type and tool names attribution dispatches on. OpenCode's tool ids are
// lower-case (Tool.define("task"), Tool.define("skill") in the 1.3.13 bundle);
// they are matched case-insensitively anyway, like transcript.ToolDetail.
const (
	partTypeTool = "tool"

	toolNameSkill = "skill"
	toolNameTask  = "task"

	// skillInputName is the skill tool's only parameter: `{name}` (zod
	// schema in the 1.3.13 bundle). transcript.ToolInput has no `name` key,
	// so it is read from the input map directly.
	skillInputName = "name"
)

// timeSpan is the earliest and latest timestamp noted so far; zero until the
// first note.
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

// attributionWalk is the single pass over every message of the export.
// Pending results come from every assistant message; calls, cost and the
// embedded timeSpan (Start/End) only from messages at or after startLine.
type attributionWalk struct {
	timeSpan

	startLine int
	// pending is the tool results of the previous assistant message, whatever
	// its index; they become the next assistant message's Consumed when that
	// message is in the slice, and are dropped otherwise.
	pending []types.ToolResultRef

	calls []types.CallUsage
	cost  float64
}

// AttributeTokens implements agent.TokenAttributor for OpenCode: one
// types.CallUsage per assistant message with index >= startLine.
//
// Mechanics, as coded:
//   - startLine and CallUsage.Line are MESSAGE INDICES into
//     ExportSession.Messages (user messages included), not lines — the
//     coordinate CalculateTokenUsage's fromOffset and SliceFromMessage use,
//     because an export is one JSON document.
//   - Every assistant message in the slice is a call. Usage is Tokens.Input,
//     Tokens.BilledOutput() (reasoning added to output only when the total
//     identity says it sits outside), Tokens.Reasoning as ThinkingTokens,
//     Cache.Read, Cache.Write, APICallCount 1 — the same arithmetic
//     CalculateTokenUsage sums, so the calls add up to its total. A message
//     with no tokens block has UsageUnknown set and a zero Usage; it still
//     counts and its tool parts still label the next call's Consumed.
//   - Model is the bare info.modelID ("claude-opus-4-6"), never prefixed with
//     info.providerID: pricing (tokenreport.WeightsFor) dispatches on the
//     model id's own prefix, and "anthropic/claude-…" would not price. The
//     provider is not carried anywhere in the result. Effort and ActiveSkill
//     stay "": OpenCode records neither per message.
//   - At is info.time.created, an epoch-millisecond value (zero when 0).
//   - Emitted is the message's parts of type "tool", in part order:
//     ID is callID (falling back to the part id), Tool the tool name, Detail
//     transcript.ToolDetail on the state.input map decoded into
//     transcript.ToolInput (OpenCode's `filePath`/`path`/`command`/`url`/
//     `subagent_type` keys are all known to it). SkillName is input.name for
//     the skill tool. For the task tool SubagentType is input.subagent_type
//     and Model is input.model when present — OpenCode's task schema has no
//     such key, so in practice Model is the model the child session actually
//     ran on, state.metadata.model.modelID; Detail stays the bare subagent
//     type either way. A part with no state keeps ID and Tool with an empty
//     Detail.
//   - Consumed: OpenCode stores a tool's result on the SAME part that
//     emitted it (state.output); there is no result message. The result
//     enters the model's context on the NEXT request, so the tool parts of
//     assistant message N are Consumed by the next assistant message after
//     N, whatever its index — a user message between them does not matter.
//     They are consumed whether or not N is in the slice (a slice starting
//     at a user message thus charges the previous turn's results to the
//     first call that read them, and consecutive slices count each result
//     exactly once); the last assistant message's results are consumed by
//     nothing. Bytes is len(state.output) — 0 for a part without state.
//     Each result's ToolUse is the emitting part's own ref, so it is always
//     labelled, even when the emitting message precedes startLine; no
//     separate label map is needed.
//   - Start/End are the earliest/latest of time.created and time.completed
//     over messages in the slice, whatever the role.
//   - Subagents is always empty and subagentsDir is ignored. A task tool
//     part names its child session in state.metadata.sessionId, but that
//     session lives only in OpenCode's own store — the export never includes
//     it and no lifecycle step copies it beside the parent — so there is
//     nothing to read from a subagentsDir yet. The parent's own totals do
//     not include the child's tokens.
//   - AgentReportedCost is Σ info.cost over the slice's assistant messages —
//     OpenCode's own per-message dollar figure, 0 when it records none.
//
// Error contract (agent.TokenAttributor): the export is one JSON document,
// so a malformed one IS an error (wrapped, as CalculateTokenUsage does).
// Empty input is an empty Attribution with a nil error, never a nil result.
func (a *OpenCodeAgent) AttributeTokens(transcriptData []byte, startLine int, _ string) (*types.Attribution, error) {
	session, err := ParseExportSession(transcriptData)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript for token attribution: %w", err)
	}
	out := &types.Attribution{}
	if session == nil {
		return out, nil
	}
	w := &attributionWalk{startLine: startLine}
	for i := range session.Messages {
		w.visitMessage(i, &session.Messages[i])
	}
	out.Calls = w.calls
	out.Start, out.End = w.start, w.end
	out.AgentReportedCost = w.cost
	return out, nil
}

// visitMessage notes the message's timestamps when it is in the slice and
// folds an assistant message into a call (see visitAssistant).
func (w *attributionWalk) visitMessage(index int, msg *ExportMessage) {
	inSlice := index >= w.startLine
	if inSlice {
		w.note(epochMillis(msg.Info.Time.Created))
		w.note(epochMillis(msg.Info.Time.Completed))
	}
	if msg.Info.Role == roleAssistant {
		w.visitAssistant(index, inSlice, msg)
	}
}

// visitAssistant appends the call, inside the slice, with the previous
// assistant message's results as Consumed. The message's own results become
// pending for the next assistant message whether or not it is in the slice.
func (w *attributionWalk) visitAssistant(index int, inSlice bool, msg *ExportMessage) {
	var emitted []types.ToolUseRef
	var results []types.ToolResultRef
	for i := range msg.Parts {
		part := &msg.Parts[i]
		if part.Type != partTypeTool {
			continue
		}
		ref := toolUseRefFrom(part)
		emitted = append(emitted, ref)
		results = append(results, types.ToolResultRef{ToolUse: ref, Bytes: outputBytes(part)})
	}
	if inSlice {
		call := types.CallUsage{
			UsageUnknown: msg.Info.Tokens == nil,
			Model:        msg.Info.ModelID,
			At:           epochMillis(msg.Info.Time.Created),
			Line:         index,
			Emitted:      emitted,
			Consumed:     w.pending,
		}
		if msg.Info.Tokens != nil {
			call.Usage = callUsageFrom(msg.Info.Tokens)
		}
		w.calls = append(w.calls, call)
		w.cost += msg.Info.Cost
	}
	w.pending = results
}

// epochMillis converts OpenCode's millisecond timestamp to UTC time; zero
// when ms is 0 (an absent or in-flight completed time).
func epochMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// callUsageFrom copies one message's tokens block into a per-call
// TokenUsage, field for field as CalculateTokenUsage accumulates them.
func callUsageFrom(t *Tokens) types.TokenUsage {
	return types.TokenUsage{
		InputTokens:         t.Input,
		OutputTokens:        t.BilledOutput(),
		ThinkingTokens:      t.Reasoning,
		CacheReadTokens:     t.Cache.Read,
		CacheCreationTokens: t.Cache.Write,
		APICallCount:        1,
	}
}

// outputBytes is the size of a tool part's result; 0 without state.
func outputBytes(part *Part) int {
	if part.State == nil {
		return 0
	}
	return len(part.State.Output)
}

// toolUseRefFrom reduces a tool part to its content-free ref; see the
// AttributeTokens doc for the per-tool rules.
func toolUseRefFrom(part *Part) types.ToolUseRef {
	ref := types.ToolUseRef{ID: cmp.Or(part.CallID, part.ID), Tool: part.Tool}
	if part.State == nil {
		return ref
	}
	in := decodeToolInput(part.State.Input)
	switch strings.ToLower(part.Tool) {
	case toolNameSkill:
		if name, ok := part.State.Input[skillInputName].(string); ok {
			in.Skill = name
		}
		ref.SkillName = in.Skill
	case toolNameTask:
		ref.SubagentType = in.SubagentType
		ref.Model = in.Model
		if ref.Model == "" && part.State.Metadata != nil && part.State.Metadata.Model != nil {
			ref.Model = part.State.Metadata.Model.ModelID
		}
	}
	ref.Detail = transcript.ToolDetail(part.Tool, in)
	return ref
}

// decodeToolInput maps a part's state.input onto transcript.ToolInput by way
// of JSON, best-effort: a non-string value under a known key leaves that
// field empty while the others still populate, so the partial result is
// returned either way and the errors carry nothing.
func decodeToolInput(input map[string]any) transcript.ToolInput {
	var in transcript.ToolInput
	if len(input) == 0 {
		return in
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return in
	}
	_ = json.Unmarshal(raw, &in) //nolint:errcheck // best-effort partial decode, see doc
	return in
}

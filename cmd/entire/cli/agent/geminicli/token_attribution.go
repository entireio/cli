package geminicli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

var _ agent.TokenAttributor = (*GeminiCLIAgent)(nil)

// argAbsolutePath is a path key Gemini CLI's `read_file` schema has carried;
// the sessions surveyed locally (530 sessions, 2026-08-28) wrote `file_path`
// — which transcript.ToolInput already knows — on 113/113 read_file calls and
// `absolute_path` on none, so this is a defensive fallback for other
// versions, not an observed shape. It is mapped onto ToolInput.FilePath only
// when no known path key is set, and only here — ToolDetail is not taught the
// spelling.
const argAbsolutePath = "absolute_path"

// Gemini CLI tools whose args carry a label under a key transcript.ToolInput
// does not know, so it is read from the args map directly: `activate_skill`
// names its skill under `name`; `delegate_to_agent` names the subagent under
// `agent_name` (its `objective` is user content and is never stored). Names
// are matched case-insensitively, like transcript.ToolDetail.
const (
	toolNameActivateSkill   = "activate_skill"
	toolNameDelegateToAgent = "delegate_to_agent"
	argSkillName            = "name"
	argAgentName            = "agent_name"
)

// attributionSession is the slice of a Gemini session file attribution
// reads: the messages array with the per-message keys the exported
// GeminiMessage drops (timestamp, model, tokens, toolCalls[].result). It is
// decoded separately so GeminiMessage's content normalisation stays
// untouched; the content itself is not needed here.
type attributionSession struct {
	Messages []attributionMessage `json:"messages"`
}

// attributionMessage is one message of the session. Tokens is a pointer so a
// gemini message that records no tokens block (types.CallUsage.UsageUnknown)
// is told apart from one that records zeros.
type attributionMessage struct {
	Type      string                `json:"type"`
	Timestamp string                `json:"timestamp"`
	Model     string                `json:"model"`
	Tokens    *geminiMessageTokens  `json:"tokens"`
	ToolCalls []attributionToolCall `json:"toolCalls"`
}

// attributionToolCall is one entry of a gemini message's toolCalls. Result is
// kept raw: it is the functionResponse array Gemini sent back to the model
// ([{"functionResponse":{"id","name","response":{"output":…}}}]) and only its
// size matters here. Every real call seen carries one — cancelled and errored
// calls included (752/752 toolCalls across 530 local sessions, 2026-08-28) —
// so a missing result is handled defensively, not as a known state.
type attributionToolCall struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Args   map[string]any  `json:"args"`
	Result json.RawMessage `json:"result"`
}

// attributionWalk is the single pass over every message of the session.
// Pending results come from every gemini message; calls and the embedded
// types.TimeSpan (Start/End) only from messages at or after startLine.
type attributionWalk struct {
	types.TimeSpan

	startLine int
	// pending is the tool results of the previous gemini message, whatever
	// its index; they become the next gemini message's Consumed when that
	// message is in the slice, and are dropped otherwise.
	pending []types.ToolResultRef

	calls []types.CallUsage
}

// AttributeTokens implements agent.TokenAttributor for Gemini CLI: one
// types.CallUsage per gemini message with index >= startLine.
//
// Mechanics, as coded:
//   - startLine and CallUsage.Line are MESSAGE INDICES into the session's
//     messages array (user and info messages included), not lines — the
//     coordinate CalculateTokenUsage's fromOffset and SliceFromMessage use,
//     because a Gemini session is one JSON document.
//   - Every message of type "gemini" in the slice is a call; "user" and
//     "info" messages never are. Usage follows CalculateTokenUsage's
//     identities, so the calls add up to its total: InputTokens =
//     tokens.input − tokens.cached + tokens.tool (cached is a subset of
//     input; tool tokens are prompt-side fresh input), OutputTokens =
//     tokens.output + tokens.thoughts (thoughts are reported outside output
//     and billed at the output rate), ThinkingTokens = tokens.thoughts,
//     CacheReadTokens = tokens.cached, CacheCreationTokens 0 (Gemini records
//     no cache writes), APICallCount 1. A gemini message with no tokens block
//     has UsageUnknown set and a zero Usage; it still counts and its
//     toolCalls still label the next call's Consumed.
//   - Model is the message's own `model` key (present on every gemini
//     message in the session shape seen; "" when absent). Effort and
//     ActiveSkill stay "": Gemini records neither.
//   - At is the message `timestamp`, RFC 3339 with milliseconds; zero when
//     absent or unparsable.
//   - Emitted is the message's toolCalls, in order: ID and Tool are the
//     call's id and name, Detail is transcript.ToolDetail on the args map
//     decoded into transcript.ToolInput (`command`, `file_path`, `path`,
//     `pattern` are known to it). The `absolute_path` key older read_file
//     calls used is mapped onto ToolInput.FilePath only when no known path
//     key is set (see argAbsolutePath). SkillName is args.name for
//     `activate_skill` and SubagentType is args.agent_name for
//     `delegate_to_agent`; each is also that call's Detail, and the
//     delegation's `objective` is never stored. Model stays "": Gemini
//     records no per-delegation model. Its web_fetch takes a `prompt` (URLs
//     embedded in prose) rather than a `url`, so its Detail is "" like
//     list_directory's.
//   - Consumed: Gemini stores a tool's result on the SAME toolCall entry
//     that emitted it (`result`, a functionResponse array); there is no
//     result message. The result enters the model's context on the NEXT
//     request, so the toolCalls of gemini message N are Consumed by the next
//     gemini message after N, whatever its index — an info or user message
//     between them does not matter. They are consumed whether or not N is in
//     the slice (a slice starting at a user message thus charges the previous
//     turn's results to the first call that read them, and consecutive
//     slices count each result exactly once); the last gemini message's
//     results are consumed by nothing. Bytes is len() of the `result` JSON
//     compacted (Gemini writes its session files indented, and the model
//     never saw that whitespace); 0 for a toolCall without a result, which
//     no real session has shown (cancelled and errored calls carry one). Each
//     result's ToolUse is the emitting call's own ref, so it is always
//     labelled, even when the emitting message precedes startLine.
//   - Start/End are the earliest/latest parsable timestamps over messages in
//     the slice, whatever their type — info messages included.
//   - Subagents is always empty and subagentsDir is ignored: a
//     `delegate_to_agent` call is labelled (above), but Gemini CLI writes no
//     child transcript for it, so there is nothing to read.
//   - AgentReportedCost stays 0: Gemini records no dollar cost.
//
// Error contract (agent.TokenAttributor): the session is one JSON document,
// so a malformed one IS an error (wrapped, as CalculateTokenUsage does).
// Empty or blank input is an empty Attribution with a nil error, never a nil
// result.
func (g *GeminiCLIAgent) AttributeTokens(transcriptData []byte, startLine int, _ string) (*types.Attribution, error) {
	session, err := parseAttributionSession(transcriptData)
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
	out.Start, out.End = w.Start, w.End
	return out, nil
}

// parseAttributionSession decodes the session document; nil, nil for empty
// or blank input, a wrapped error when the document does not parse.
func parseAttributionSession(data []byte) (*attributionSession, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil //nolint:nilnil // empty transcript is not an error, see AttributeTokens
	}
	var session attributionSession
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}
	return &session, nil
}

// visitMessage notes the message's timestamp when it is in the slice and
// folds a gemini message into a call (see visitGemini); "user" and "info"
// messages contribute their timestamp only.
func (w *attributionWalk) visitMessage(index int, msg *attributionMessage) {
	inSlice := index >= w.startLine
	at := types.ParseTimestamp(msg.Timestamp)
	if inSlice {
		w.Note(at)
	}
	if msg.Type == MessageTypeGemini {
		w.visitGemini(index, inSlice, at, msg)
	}
}

// visitGemini appends the call, inside the slice, with the previous gemini
// message's results as Consumed. The message's own results become pending
// for the next gemini message whether or not it is in the slice.
func (w *attributionWalk) visitGemini(index int, inSlice bool, at time.Time, msg *attributionMessage) {
	var emitted []types.ToolUseRef
	var results []types.ToolResultRef
	for i := range msg.ToolCalls {
		tc := &msg.ToolCalls[i]
		ref := toolUseRefFrom(tc)
		emitted = append(emitted, ref)
		results = append(results, types.ToolResultRef{ToolUse: ref, Bytes: resultBytes(tc.Result)})
	}
	if inSlice {
		call := types.CallUsage{
			UsageUnknown: msg.Tokens == nil,
			Model:        msg.Model,
			At:           at,
			Line:         index,
			Emitted:      emitted,
			Consumed:     w.pending,
		}
		if msg.Tokens != nil {
			call.Usage = callUsageFrom(msg.Tokens)
		}
		w.calls = append(w.calls, call)
	}
	w.pending = results
}

// callUsageFrom converts one message's tokens block into a per-call
// TokenUsage with the same identities CalculateTokenUsage accumulates.
func callUsageFrom(t *geminiMessageTokens) types.TokenUsage {
	return types.TokenUsage{
		InputTokens:     t.Input - t.Cached + t.Tool,
		OutputTokens:    t.Output + t.Thoughts,
		ThinkingTokens:  t.Thoughts,
		CacheReadTokens: t.Cached,
		APICallCount:    1,
	}
}

// resultBytes is the size of a toolCall's result as the model saw it: the
// raw `result` JSON with the file's indentation removed. 0 when absent
// (defensive: every real call carries one); a
// result that cannot be compacted (it decoded, so this does not happen in
// practice) is measured as written.
func resultBytes(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return len(raw)
	}
	return compact.Len()
}

// toolUseRefFrom reduces a toolCall to its content-free ref; see the
// AttributeTokens doc for the per-tool rules.
func toolUseRefFrom(tc *attributionToolCall) types.ToolUseRef {
	in := transcript.ToolInputFromMap(tc.Args)
	if in.AnyFilePath() == "" {
		in.FilePath = transcript.StringArg(tc.Args, argAbsolutePath)
	}
	ref := types.ToolUseRef{ID: tc.ID, Tool: tc.Name}
	switch strings.ToLower(tc.Name) {
	case toolNameActivateSkill:
		in.Skill = transcript.StringArg(tc.Args, argSkillName)
		ref.SkillName = in.Skill
	case toolNameDelegateToAgent:
		in.SubagentType = transcript.StringArg(tc.Args, argAgentName)
		ref.SubagentType = in.SubagentType
	}
	ref.Detail = transcript.ToolDetail(tc.Name, in)
	return ref
}

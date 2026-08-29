package codex

import (
	"cmp"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

var _ agent.TokenAttributor = (*CodexAgent)(nil)

// Rollout envelope and payload type names beyond rolloutLineTypeResponseItem.
// Shapes confirmed against Codex CLI 0.97–0.144 rollouts on disk (2026-08-28).
const (
	rolloutLineTypeEventMsg    = "event_msg"
	rolloutLineTypeTurnContext = "turn_context"
	eventMsgTypeTokenCount     = "token_count"

	responseItemFunctionCall         = "function_call"
	responseItemFunctionCallOutput   = "function_call_output"
	responseItemCustomToolCall       = "custom_tool_call"
	responseItemCustomToolCallOutput = "custom_tool_call_output"

	// toolNameApplyPatch (lifecycle.go) names the custom_tool_call for patches.
	toolNameSpawnAgent = "spawn_agent"
)

// turnContextPayload is the payload for type="turn_context" lines, reduced to
// the model and reasoning effort in force for the turn. `model` and `effort`
// sit at the top level on every row seen; collaboration_mode.settings repeats
// them and is the fallback when the top-level keys are absent.
type turnContextPayload struct {
	Model             string `json:"model"`
	Effort            string `json:"effort"`
	CollaborationMode struct {
		Settings struct {
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoning_effort"`
		} `json:"settings"`
	} `json:"collaboration_mode"`
}

// functionCallArguments is the decoded `arguments` JSON of a function_call,
// reduced to the keys attribution reads. exec_command carries the script under
// `cmd`; the older `shell` tool carried an argv array under `command`;
// spawn_agent names its subagent under `agent_type` (never `subagent_type`).
type functionCallArguments struct {
	Cmd       string          `json:"cmd"`
	Command   json.RawMessage `json:"command"`
	AgentType string          `json:"agent_type"`
}

// attributionWalk is the single pass over the whole rollout. Labels, the
// model/effort state and the pending emits/outputs come from every row (the
// full transcript); calls and the embedded types.TimeSpan (Start/End) only
// from rows at or after startLine.
type attributionWalk struct {
	types.TimeSpan

	startLine int
	// labels maps call_id → the emitting ref, from every tool-call row.
	labels map[string]types.ToolUseRef
	// model and effort are from the latest turn_context seen so far.
	model, effort string
	// prevTotal is the last DISTINCT cumulative total seen — the baseline
	// before the first in-slice call, then the previous call's total.
	prevTotal *tokenUsageData
	// pendingEmitted / pendingConsumed are the tool calls and tool outputs seen
	// since prevTotal, in or before the slice; they belong to the call the
	// next distinct total closes and are dropped when that total precedes
	// startLine.
	pendingEmitted  []types.ToolUseRef
	pendingConsumed []types.ToolResultRef

	calls []types.CallUsage
}

// AttributeTokens implements agent.TokenAttributor for Codex: one
// types.CallUsage per DISTINCT cumulative total in event_msg/token_count rows
// from startLine onward.
//
// Mechanics, as coded:
//   - Lines are split exactly as splitJSONL splits them (blank lines dropped,
//     malformed non-blank lines kept and skipped), and CallUsage.Line is the
//     0-based index in that sequence — the same coordinate CalculateTokenUsage
//     uses, where fromOffset N means "N lines precede the slice": a row is in
//     the slice when its index >= startLine. Malformed lines are never an
//     error.
//   - Codex reports CUMULATIVE totals and often writes the same total twice in
//     a row (e.g. once after the response, once at turn end). A call is each
//     total that differs from the previous distinct total; its Usage is the
//     delta (see tokenUsageDelta: fresh input = Δinput − Δcached clamped at
//     0, CacheReadTokens Δcached, OutputTokens Δoutput, ThinkingTokens
//     Δreasoning clamped at 0, CacheCreationTokens Δcache_write, APICallCount
//     1). A duplicate total is ignored, including a duplicate of the
//     baseline that lands inside the slice. UsageUnknown is never set: a
//     Codex call without a token_count row is simply not seen.
//   - Baseline: the last distinct total at or before startLine, as
//     CalculateTokenUsage. A call with no prior total takes the raw
//     cumulative total as its usage, matching CalculateTokenUsage from 0.
//   - Line/At are the token_count row's index and timestamp — the LAST row of
//     the call's group, not its first as types.CallUsage.Line describes for
//     row-grouped agents: a Codex call has no row of its own before its
//     token_count, and the tool calls that precede it are only known to be
//     the call's once the total closes them. Model and
//     Effort are the latest turn_context's `model` and `effort` (falling back
//     to collaboration_mode.settings.model / .reasoning_effort), tracked
//     across the full transcript so a call whose turn_context precedes
//     startLine is still labelled; both are "" before any turn_context. Model
//     is the bare id ("gpt-5.5"), never prefixed with session_meta's
//     model_provider, so pricing can look it up. ActiveSkill stays "": Codex
//     stamps no skill on its rows.
//   - Emitted is every function_call / custom_tool_call row since the
//     previous distinct total; Consumed is every function_call_output /
//     custom_tool_call_output row since the previous distinct total — both
//     wherever startLine falls: the rows are collected from the FULL
//     transcript, so a call's Emitted and Consumed are the same in every slice
//     that admits it and a row between a pre-slice total and an in-slice total
//     is charged, once, to the in-slice call. In a real rollout the
//     token_count for a response follows the calls it emitted and precedes
//     their outputs, so those outputs fall to the NEXT call — the one that
//     actually read them; an output that lands before its own response's
//     token_count is attributed the same way, to the call at the later total.
//     Rows after the last total are attributed to nothing.
//   - ToolUseRef: ID is call_id, Tool the tool name, Detail is
//     transcript.ToolDetail on a transcript.ToolInput built from the
//     arguments: Command from `cmd`, or from a `command` argv array joined
//     with spaces (lossy; only ever reduced by ToolDetail) — a
//     `<shell> -c|-lc <script>` argv unwrapped to the script first;
//     SubagentType from `agent_type`. ToolUseRef.Model stays "": no Codex
//     build has been seen recording a requested model on spawn_agent, and
//     the subagent's model is that thread's own. For apply_patch
//     (a custom_tool_call whose Input is the patch text) the input is the
//     first `*** Add|Update|Delete File:` path, so Detail is that path; any
//     other custom tool has "". A function_call whose arguments do not decode
//     keeps ID and Tool with an empty Detail. SubagentType is set for
//     spawn_agent. Labels are recorded from EVERY row (first sighting per
//     call_id wins), so an output whose call precedes startLine is still
//     labelled; an output with an unknown call_id keeps a ref with only ID
//     set. Bytes is len() of the raw JSON of the row's `output` field —
//     quotes and escapes included, whether Codex wrote a string or (older
//     builds) an object.
//   - Start/End are the earliest/latest parsable row timestamps in the slice,
//     whatever the row's type.
//   - Subagents is always empty and subagentsDir is ignored. Codex does write
//     subagent threads to disk — a separate rollout file whose session_meta
//     .source is {"subagent":{"thread_spawn":{"parent_thread_id":…}}}, named
//     rollout-<ts>-<agent_id>.jsonl under the sessions date tree, with
//     agent_id reported in the spawn_agent function_call_output — but they
//     live beside the parent in that tree, not in a per-session directory,
//     so there is no subagentsDir to hand this method yet; the parent's own
//     totals do not include them.
//   - AgentReportedCost stays 0: Codex records no dollar cost.
//
// Error contract (agent.TokenAttributor): rollouts are JSONL, so no
// document-level parse can fail; the result is never nil and the error is
// always nil. An empty or all-garbage transcript is an empty Attribution.
func (c *CodexAgent) AttributeTokens(transcriptData []byte, startLine int, _ string) (*types.Attribution, error) {
	w := &attributionWalk{
		startLine: startLine,
		labels:    make(map[string]types.ToolUseRef),
	}
	for i, lineData := range splitJSONL(transcriptData) {
		w.visitRow(i, lineData)
	}
	return &types.Attribution{
		Calls: w.calls,
		Start: w.Start,
		End:   w.End,
	}, nil
}

// visitRow decodes one row and dispatches on its envelope type. Rows before
// startLine only contribute labels, model/effort state, the baseline total and
// the pending emits/outputs.
func (w *attributionWalk) visitRow(line int, raw []byte) {
	var row rolloutLine
	if json.Unmarshal(raw, &row) != nil {
		return
	}
	inSlice := line >= w.startLine
	if inSlice {
		w.Note(types.ParseTimestamp(row.Timestamp))
	}
	switch row.Type {
	case rolloutLineTypeTurnContext:
		w.visitTurnContext(row.Payload)
	case rolloutLineTypeResponseItem:
		w.visitResponseItem(row.Payload)
	case rolloutLineTypeEventMsg:
		if total := lineTokenCountTotal(&row); total != nil {
			w.visitTotal(line, inSlice, row.Timestamp, total)
		}
	}
}

// visitTurnContext updates the model/effort state from a turn_context row; a
// key the row lacks leaves the previous value in place.
func (w *attributionWalk) visitTurnContext(payload json.RawMessage) {
	var tc turnContextPayload
	if json.Unmarshal(payload, &tc) != nil {
		return
	}
	if model := cmp.Or(tc.Model, tc.CollaborationMode.Settings.Model); model != "" {
		w.model = model
	}
	if effort := cmp.Or(tc.Effort, tc.CollaborationMode.Settings.ReasoningEffort); effort != "" {
		w.effort = effort
	}
}

// visitResponseItem registers tool-call labels and queues tool calls as
// pending emits and tool outputs as pending consumed results for the next
// distinct total, whatever line the row is on (see AttributeTokens: Emitted
// and Consumed are slice-independent).
func (w *attributionWalk) visitResponseItem(payload json.RawMessage) {
	var item responseItemPayload
	if json.Unmarshal(payload, &item) != nil {
		return
	}
	switch item.Type {
	case responseItemFunctionCall, responseItemCustomToolCall:
		ref := toolUseRefFrom(&item)
		if _, seen := w.labels[ref.ID]; !seen {
			w.labels[ref.ID] = ref
		}
		w.pendingEmitted = append(w.pendingEmitted, ref)
	case responseItemFunctionCallOutput, responseItemCustomToolCallOutput:
		ref, ok := w.labels[item.CallID]
		if !ok {
			ref = types.ToolUseRef{ID: item.CallID}
		}
		w.pendingConsumed = append(w.pendingConsumed, types.ToolResultRef{ToolUse: ref, Bytes: len(item.Output)})
	}
}

// visitTotal handles one token_count total: a duplicate of the previous
// distinct total is ignored. Every distinct total takes the pending emits and
// outputs and becomes the baseline; inside the slice it also closes the call
// they belong to, before the slice they are dropped with it.
func (w *attributionWalk) visitTotal(line int, inSlice bool, timestamp string, total *tokenUsageData) {
	if w.prevTotal != nil && *total == *w.prevTotal {
		return
	}
	if inSlice {
		w.calls = append(w.calls, types.CallUsage{
			Usage:    tokenUsageDelta(total, w.prevTotal, 1),
			Model:    w.model,
			Effort:   w.effort,
			At:       types.ParseTimestamp(timestamp),
			Line:     line,
			Emitted:  w.pendingEmitted,
			Consumed: w.pendingConsumed,
		})
	}
	w.pendingEmitted, w.pendingConsumed = nil, nil
	w.prevTotal = total
}

// toolUseRefFrom reduces a function_call or custom_tool_call to its
// content-free ref; see the AttributeTokens doc for the per-tool rules.
func toolUseRefFrom(item *responseItemPayload) types.ToolUseRef {
	ref := types.ToolUseRef{ID: item.CallID, Tool: item.Name}
	var in transcript.ToolInput
	switch item.Type {
	case responseItemCustomToolCall:
		if item.Name != toolNameApplyPatch {
			return ref
		}
		if m := applyPatchFileRegex.FindStringSubmatch(item.Input); m != nil {
			in.FilePath = strings.TrimSpace(m[2])
		}
	case responseItemFunctionCall:
		var args functionCallArguments
		if json.Unmarshal([]byte(item.Arguments), &args) != nil {
			return ref
		}
		in.Command = shellCommandFromArguments(&args)
		in.SubagentType = args.AgentType
		if item.Name == toolNameSpawnAgent {
			ref.SubagentType = args.AgentType
		}
	}
	ref.Detail = transcript.ToolDetail(item.Name, in)
	return ref
}

// shellCommandFromArguments returns the shell script a tool call runs: `cmd`
// when present (exec_command), else the `command` argv — the script itself
// when the argv is `<shell> -c|-lc <script> …` (isShellWrapper), the words
// joined with spaces otherwise (lossy; only ever reduced by ToolDetail); ""
// when `cmd` is empty and `command` is not a non-empty array.
func shellCommandFromArguments(args *functionCallArguments) string {
	if args.Cmd != "" {
		return args.Cmd
	}
	var argv []string
	if json.Unmarshal(args.Command, &argv) != nil || len(argv) == 0 {
		return ""
	}
	if isShellWrapper(argv) {
		return argv[2]
	}
	return strings.Join(argv, " ")
}

// isShellWrapper reports whether argv is `<shell> -c|-lc <script> …` with a
// POSIX shell as argv[0] (by basename), so that `git -c core.pager=cat log`
// is not mistaken for a wrapper.
func isShellWrapper(argv []string) bool {
	if len(argv) < 3 || (argv[1] != "-c" && argv[1] != "-lc") {
		return false
	}
	switch filepath.Base(argv[0]) {
	case "sh", "bash", "zsh", "dash":
		return true
	default:
		return false
	}
}

package types

import "time"

// ToolUseRef identifies one tool call an API call emitted.
type ToolUseRef struct {
	// ID is the agent's tool-use id ("toolu_01…", Codex call_id, …).
	ID string
	// Tool is the tool name as the agent names it ("Bash", "exec_command", "read", …).
	Tool string
	// Detail is the drill-down for the report: the command's leading words, a
	// file path, a URL host, a skill name, a subagent type. NEVER the raw
	// command — commands are user content (see agent.ToolInvocation) and must
	// not be stored.
	Detail string
	// SkillName is set when Tool loads a skill.
	SkillName string
	// SubagentType is set when Tool spawns a subagent.
	SubagentType string
	// Model is the model the subagent was asked to run on, when the emit
	// carries it (e.g. Task input.model); "" otherwise.
	Model string
}

// ToolResultRef is a tool result an API call consumed as new input.
type ToolResultRef struct {
	// ToolUse is resolved from the FULL transcript, so a result whose
	// tool_use precedes the slice is still labelled.
	ToolUse ToolUseRef
	// Bytes is the size of the result content; drives proportional
	// attribution of fresh input.
	Bytes int
}

// CallUsage is one API call's own usage and what it emitted/consumed.
type CallUsage struct {
	// Usage is this call only — no subagent tokens; the subset fields
	// (thinking, 1-hour cache write) filled where the agent records them —
	// added to TokenUsage by PR #2155.
	Usage TokenUsage
	// UsageUnknown is true when the agent recorded no usage for this call
	// (OpenCode/Pi assistant messages without a tokens block); Usage is then
	// zero, the call still counts and its Emitted refs still label the next
	// call's Consumed; reports print "N calls with no usage recorded" rather
	// than treating zero as measured.
	UsageUnknown bool
	// Model is the per-call model; "" when the agent does not record it per
	// call (e.g. a Codex call before its first turn_context).
	Model string
	// Effort is the per-call effort where recorded (Claude `effort`, Codex
	// turn_context.effort, Pi thinking level in force); "" otherwise.
	Effort string
	// At is the call timestamp; zero when unknown.
	At time.Time
	// Line is the position of the call's first row, in the same unit as
	// AttributeTokens' startLine (line for JSONL agents, message index for
	// Gemini/OpenCode); used for per-prompt grouping and to match
	// skill_events transcript anchors.
	Line int
	// ActiveSkill is the harness-stamped skill active during this call
	// (Claude attributionSkill); "" otherwise.
	ActiveSkill string
	// Emitted is the tool calls this call made.
	Emitted []ToolUseRef
	// Consumed is the tool results that were NEW input to this call, i.e.
	// emitted by an earlier call.
	Consumed []ToolResultRef
}

// SubagentRecord is a subagent's own usage, discovered from its transcript or
// a committed task record.
type SubagentRecord struct {
	// ToolUseID is the id of the tool call that spawned this subagent,
	// joining it back to the parent's ToolUseRef.
	ToolUseID string
	// SubagentType is the subagent's dispatched type/name.
	SubagentType string
	// Model is the subagent's actual model. Precedence over weaker signals,
	// stated once here: record Model > Usage.Model > the emitting
	// ToolUseRef.Model (which is the *requested* alias, e.g. "haiku", not the
	// actual id).
	Model string
	// Usage is the subagent's total usage; nil when unavailable.
	Usage *TokenUsage
	// Start is the subagent's first timestamp; zero when unknown. A live
	// source fills this from agent-<id>.jsonl's first timestamp; a committed
	// source fills it from task.json's started_at.
	Start time.Time
	// End is the subagent's last timestamp; zero when unknown. End zero on a
	// committed record means the subagent was still in flight when the
	// checkpoint was written. A live source fills this from
	// agent-<id>.jsonl's last timestamp; a committed source fills it from
	// task.json's completed_at.
	End time.Time
}

// Attribution is the result of walking one transcript slice.
type Attribution struct {
	// Calls are the API calls found in the slice, in transcript order.
	Calls []CallUsage
	// Subagents are discovered from the FULL transcript, not just the slice,
	// when subagentsDir != "" (matches the TokenAttributor.AttributeTokens
	// doc); empty when subagentsDir == "".
	Subagents []SubagentRecord
	// Start is the first timestamp seen in the slice (zero when none).
	Start time.Time
	// End is the last timestamp seen in the slice (zero when none).
	End time.Time
	// AgentReportedCost is the provider-computed dollar cost summed over the
	// slice; 0 = not recorded. Only Pi populates it today.
	AgentReportedCost float64
}

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
	// Usage is this call only — no subagent tokens; ThinkingTokens/
	// CacheCreation1hTokens filled where the agent records them.
	Usage TokenUsage
	// Model is the per-call model; "" when the agent does not record it per
	// call (Cursor).
	Model string
	// Effort is the per-call effort where recorded (Claude `effort`, Codex
	// turn_context.effort, Pi thinking level in force); "" otherwise.
	Effort string
	// At is the call timestamp; zero when unknown.
	At time.Time
	// ActiveSkill is the harness-stamped skill active during this call
	// (Claude attributionSkill); "" otherwise.
	ActiveSkill string
	Emitted     []ToolUseRef
	Consumed    []ToolResultRef
}

// SubagentRecord is a subagent's own usage, discovered from its transcript or
// a committed task record.
type SubagentRecord struct {
	ToolUseID    string
	SubagentType string
	Model        string
	Usage        *TokenUsage
}

// Attribution is the result of walking one transcript slice.
type Attribution struct {
	Calls     []CallUsage
	Subagents []SubagentRecord
	// Start is the first timestamp seen in the slice (zero when none).
	Start time.Time
	// End is the last timestamp seen in the slice.
	End time.Time
}

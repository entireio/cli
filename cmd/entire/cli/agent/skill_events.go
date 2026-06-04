package agent

// Skill event types recorded in checkpoint metadata.
const (
	SkillEventTypePromptInvocation = "prompt_invocation"
	SkillEventTypeToolInvocation   = "tool_invocation"
)

// Skill event source signals.
const (
	SkillSignalPiInputSlashCommand = "input_slash_command"
	SkillSignalPromptSlashCommand  = "prompt_slash_command"
	SkillSignalClaudeSkillToolUse  = "skill_tool_use"
)

// Skill event confidence values.
const (
	SkillConfidenceExplicit = "explicit"
)

// Skill event collapse targets.
const (
	SkillCollapseTargetUserMessage = "user_message"
	SkillCollapseTargetToolPair    = "tool_pair"
)

// SkillEvent records a native agent skill signal without rewriting the raw transcript.
// Consumers use TranscriptAnchor/Native to locate the underlying raw event and Collapse
// to decide whether/how to hide verbose skill material by default.
type SkillEvent struct {
	ID        string           `json:"id,omitempty"`
	EventType string           `json:"event_type"`
	Skill     SkillEventSkill  `json:"skill"`
	Source    SkillEventSource `json:"source"`

	TurnID    string `json:"turn_id,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`

	TranscriptAnchor *SkillEventTranscriptAnchor `json:"transcript_anchor,omitempty"`
	Native           map[string]string           `json:"native,omitempty"`
	Collapse         SkillEventCollapse          `json:"collapse"`
}

type SkillEventSkill struct {
	Name string `json:"name"`
}

type SkillEventSource struct {
	Agent      string `json:"agent"`
	Signal     string `json:"signal"`
	Confidence string `json:"confidence"`
}

type SkillEventTranscriptAnchor struct {
	Unit      string   `json:"unit,omitempty"`
	Start     int      `json:"start,omitempty"`
	End       int      `json:"end,omitempty"`
	EntryIDs  []string `json:"entry_ids,omitempty"`
	ToolUseID string   `json:"tool_use_id,omitempty"`
}

type SkillEventCollapse struct {
	Target           string `json:"target"`
	Label            string `json:"label,omitempty"`
	DefaultCollapsed bool   `json:"default_collapsed"`
}

// SkillEventExtractor is implemented by agents that can derive native skill events
// from their transcript format.
type SkillEventExtractor interface {
	ExtractSkillEvents(transcriptData []byte, fromOffset int) ([]SkillEvent, error)
}

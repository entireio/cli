package agent

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var skillSlashCommandPattern = regexp.MustCompile(`^/skill:([A-Za-z0-9][A-Za-z0-9._-]{0,63})(?:\s|$)`)

// SkillEventFromPromptSlashCommand returns a normalized skill event when prompt
// begins with an explicit /skill:<name> command. It intentionally records only
// the slash command token (not the rest of the prompt) so metadata does not copy
// arbitrary user text outside the redacted transcript/prompt files.
func SkillEventFromPromptSlashCommand(agentName, prompt string, timestamp time.Time) (SkillEvent, bool) {
	trimmed := strings.TrimLeft(prompt, " \t\r\n")
	match := skillSlashCommandPattern.FindStringSubmatch(trimmed)
	if match == nil {
		return SkillEvent{}, false
	}

	skillName := strings.TrimSpace(match[1])
	if skillName == "" {
		return SkillEvent{}, false
	}

	command := "/skill:" + skillName
	event := SkillEvent{
		ID:        promptSkillEventID(agentName, skillName, timestamp),
		EventType: SkillEventTypePromptInvocation,
		Skill: SkillEventSkill{
			Name: skillName,
		},
		Source: SkillEventSource{
			Agent:      agentName,
			Signal:     SkillSignalPromptSlashCommand,
			Confidence: SkillConfidenceExplicit,
		},
		Native: map[string]string{
			"command": command,
		},
		Collapse: SkillEventCollapse{
			Target:           SkillCollapseTargetUserMessage,
			Label:            command,
			DefaultCollapsed: true,
		},
	}
	if !timestamp.IsZero() {
		event.Timestamp = timestamp.UTC().Format(time.RFC3339Nano)
	}
	return event, true
}

// AppendPromptSlashCommandSkillEvent adds a generic prompt-invocation skill
// event for explicit /skill:<name> prompts. If an agent adapter already surfaced
// an equivalent prompt skill event (for example Pi's pre-expansion input event),
// the adapter event wins and no generic duplicate is appended.
func AppendPromptSlashCommandSkillEvent(events []SkillEvent, agentName, prompt string, timestamp time.Time) []SkillEvent {
	event, ok := SkillEventFromPromptSlashCommand(agentName, prompt, timestamp)
	if !ok {
		return events
	}
	if hasEquivalentPromptSkillEvent(events, event) {
		return events
	}
	return append(events, event)
}

func promptSkillEventID(agentName, skillName string, timestamp time.Time) string {
	if timestamp.IsZero() {
		return ""
	}
	return fmt.Sprintf("prompt-skill-%s-%s-%s", agentName, skillName, timestamp.UTC().Format(time.RFC3339Nano))
}

func hasEquivalentPromptSkillEvent(events []SkillEvent, candidate SkillEvent) bool {
	candidateCommand := ""
	if candidate.Native != nil {
		candidateCommand = candidate.Native["command"]
	}
	for _, existing := range events {
		if existing.EventType != SkillEventTypePromptInvocation || existing.Skill.Name != candidate.Skill.Name {
			continue
		}
		if existing.ID != "" && candidate.ID != "" && existing.ID == candidate.ID {
			return true
		}
		if candidateCommand != "" && existing.Native != nil && existing.Native["command"] == candidateCommand {
			return true
		}
		if existing.Source.Signal == SkillSignalPiInputSlashCommand || existing.Source.Signal == SkillSignalPromptSlashCommand {
			return true
		}
	}
	return false
}

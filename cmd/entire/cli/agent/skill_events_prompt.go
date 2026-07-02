package agent

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// skillSlashCommandPattern matches a leading "/<command>" token up to the first
// whitespace, so command arguments are never captured.
var skillSlashCommandPattern = regexp.MustCompile(`^/([A-Za-z0-9][A-Za-z0-9._:/-]*)`)

// filesystemRoots reject pasted absolute paths ("/Users/x", "/tmp/y") that would
// otherwise look like commands. Only matched when the root is followed by another
// path segment, so a bare "/dev" stays a command (see isFilesystemPath).
var filesystemRoots = map[string]struct{}{
	"users": {}, "home": {}, "tmp": {}, "usr": {}, "var": {}, "etc": {},
	"opt": {}, "mnt": {}, "private": {}, "volumes": {}, "library": {},
	"applications": {}, "system": {}, "bin": {}, "sbin": {}, "dev": {},
	"proc": {}, "sys": {}, "root": {}, "srv": {}, "run": {}, "boot": {},
	"lib": {}, "media": {}, "network": {}, "cores": {},
}

// SkillEventFromPromptSlashCommand returns a skill event for a prompt beginning
// with a "/<command>" slash command. A recorded prompt only contains a slash
// command that was submitted as a turn, so runtime/UI-only commands (/mcp,
// /model, ...) are naturally absent; pasted filesystem paths are rejected.
//
// Only the command token is stored, never the prompt body. Tool-call skills
// (e.g. Claude Code's Skill tool) are captured separately by SkillEventExtractors
// as "tool_invocation" events.
func SkillEventFromPromptSlashCommand(agentName, prompt string, timestamp time.Time) (SkillEvent, bool) {
	trimmed := strings.TrimLeft(prompt, " \t\r\n")
	match := skillSlashCommandPattern.FindStringSubmatch(trimmed)
	if match == nil {
		return SkillEvent{}, false
	}

	token := strings.Trim(match[1], "/")
	if token == "" || isFilesystemPath(match[1]) {
		return SkillEvent{}, false
	}

	// Normalize Pi's "/skill:<name>" form to the bare skill name so the generic
	// event dedupes against Pi's native input_slash_command event, which records
	// the name without the "skill:" namespace.
	name := token
	if rest, ok := strings.CutPrefix(token, "skill:"); ok {
		if rest == "" {
			return SkillEvent{}, false
		}
		name = rest
	}

	command := "/" + token
	event := SkillEvent{
		ID:        promptSkillEventID(agentName, name, timestamp),
		EventType: SkillEventTypePromptInvocation,
		Skill: SkillEventSkill{
			Name: name,
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

// isFilesystemPath reports whether raw (the captured command token, e.g.
// "Users/alice/x", "dev", "parent/child") is a pasted absolute filesystem path
// rather than a slash command. It matches only when a well-known root segment is
// FOLLOWED by a further path segment, so bare single-token commands that happen
// to collide with a root name (e.g. "/dev", "/run", "/lib") are still treated as
// commands — only "/dev/null", "/Users/alice/...", etc. are rejected.
func isFilesystemPath(raw string) bool {
	first, rest, found := strings.Cut(raw, "/")
	if !found || rest == "" {
		return false
	}
	_, ok := filesystemRoots[strings.ToLower(first)]
	return ok
}

// AppendPromptSlashCommandSkillEvent adds a generic prompt-invocation skill
// event for a "/<command>" prompt. If an agent adapter already surfaced an
// equivalent prompt skill event (for example Pi's pre-expansion input event),
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

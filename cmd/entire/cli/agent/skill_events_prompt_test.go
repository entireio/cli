package agent

import (
	"testing"
	"time"
)

func TestSkillEventFromPromptSlashCommand(t *testing.T) {
	t.Parallel()

	timestamp := time.Date(2026, 5, 25, 12, 34, 56, 0, time.UTC)
	event, ok := SkillEventFromPromptSlashCommand("codex", "  /skill:trigger-analysis inspect the diff", timestamp)
	if !ok {
		t.Fatal("SkillEventFromPromptSlashCommand() ok = false, want true")
	}
	if event.EventType != SkillEventTypePromptInvocation {
		t.Fatalf("EventType = %q, want %q", event.EventType, SkillEventTypePromptInvocation)
	}
	if event.Skill.Name != "trigger-analysis" {
		t.Fatalf("Skill.Name = %q, want trigger-analysis", event.Skill.Name)
	}
	if event.Source.Agent != "codex" || event.Source.Signal != SkillSignalPromptSlashCommand || event.Source.Confidence != SkillConfidenceExplicit {
		t.Fatalf("Source = %+v", event.Source)
	}
	if event.Timestamp != "2026-05-25T12:34:56Z" {
		t.Fatalf("Timestamp = %q", event.Timestamp)
	}
	if event.Native["command"] != "/skill:trigger-analysis" {
		t.Fatalf("Native command = %q", event.Native["command"])
	}
	if event.Collapse.Target != SkillCollapseTargetUserMessage || !event.Collapse.DefaultCollapsed {
		t.Fatalf("Collapse = %+v", event.Collapse)
	}
	if event.Collapse.Label != "/skill:trigger-analysis" {
		t.Fatalf("Collapse label = %q", event.Collapse.Label)
	}
}

func TestSkillEventFromPromptSlashCommand_NonSkillPrompt(t *testing.T) {
	t.Parallel()

	for _, prompt := range []string{"/review", "/skills", "please use /skill:foo", "/skill:", "/skillset:foo"} {
		if event, ok := SkillEventFromPromptSlashCommand("codex", prompt, time.Now()); ok {
			t.Fatalf("SkillEventFromPromptSlashCommand(%q) = %+v, true; want false", prompt, event)
		}
	}
}

func TestAppendPromptSlashCommandSkillEvent_KeepsNativeAdapterEvent(t *testing.T) {
	t.Parallel()

	existing := []SkillEvent{
		{
			ID:        "pi-skill-trigger-analysis-1",
			EventType: SkillEventTypePromptInvocation,
			Skill:     SkillEventSkill{Name: "trigger-analysis"},
			Source: SkillEventSource{
				Agent:      "pi",
				Signal:     SkillSignalPiInputSlashCommand,
				Confidence: SkillConfidenceExplicit,
			},
			Native: map[string]string{"command": "/skill:trigger-analysis"},
		},
	}

	got := AppendPromptSlashCommandSkillEvent(existing, "pi", "/skill:trigger-analysis inspect", time.Now())
	if len(got) != 1 {
		t.Fatalf("AppendPromptSlashCommandSkillEvent len = %d, want 1", len(got))
	}
	if got[0].ID != existing[0].ID {
		t.Fatalf("AppendPromptSlashCommandSkillEvent replaced native event: %+v", got[0])
	}
}

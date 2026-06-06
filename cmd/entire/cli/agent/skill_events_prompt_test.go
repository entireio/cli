package agent

import (
	"testing"
	"time"
)

func TestSkillEventFromPromptSlashCommand(t *testing.T) {
	t.Parallel()

	timestamp := time.Date(2026, 5, 25, 12, 34, 56, 0, time.UTC)
	event, ok := SkillEventFromPromptSlashCommand("codex", "  /goal Complete ENG-623: ship it", timestamp)
	if !ok {
		t.Fatal("SkillEventFromPromptSlashCommand() ok = false, want true")
	}
	if event.EventType != SkillEventTypePromptInvocation {
		t.Fatalf("EventType = %q, want %q", event.EventType, SkillEventTypePromptInvocation)
	}
	if event.Skill.Name != "goal" {
		t.Fatalf("Skill.Name = %q, want goal", event.Skill.Name)
	}
	if event.Source.Agent != "codex" || event.Source.Signal != SkillSignalPromptSlashCommand || event.Source.Confidence != SkillConfidenceExplicit {
		t.Fatalf("Source = %+v", event.Source)
	}
	if event.Timestamp != "2026-05-25T12:34:56Z" {
		t.Fatalf("Timestamp = %q", event.Timestamp)
	}
	if event.Native["command"] != "/goal" {
		t.Fatalf("Native command = %q", event.Native["command"])
	}
	if event.Collapse.Target != SkillCollapseTargetUserMessage || !event.Collapse.DefaultCollapsed {
		t.Fatalf("Collapse = %+v", event.Collapse)
	}
	if event.Collapse.Label != "/goal" {
		t.Fatalf("Collapse label = %q", event.Collapse.Label)
	}
}

func TestSkillEventFromPromptSlashCommand_Variants(t *testing.T) {
	t.Parallel()

	cases := []struct {
		prompt   string
		wantName string // "" means: expect no match
	}{
		{"/review", "review"}, // built-in prompt command — still a skill/prompt
		{"/build-feature implement the thing", "build-feature"},     // custom command with args
		{"/superpowers:brainstorming", "superpowers:brainstorming"}, // plugin-namespaced
		{"/git:commit", "git:commit"},                               // gemini colon namespace
		{"/parent/child do x", "parent/child"},                      // opencode path namespace
		{"/start-ticket https://x/y", "start-ticket"},               // url arg ignored
		{"\t/skill:trigger-analysis inspect", "trigger-analysis"},   // pi form → bare name
		{"/dev", "dev"},                       // bare command colliding with a root name — still a command
		{"/dev implement the feature", "dev"}, // ditto, with args
		{"/Users/alice/notes.md", ""},         // pasted absolute path
		{"/dev/null 2>&1", ""},                // root followed by a path segment
		{"/tmp/output.log read this", ""},     // pasted path with args
		{"please run /review", ""},            // not leading
		{"/ spaced", ""},                      // no command token
		{"/", ""},                             // bare slash
		{"/skill:", ""},                       // empty skill name
		{"do the thing", ""},                  // no slash
	}
	for _, tc := range cases {
		event, ok := SkillEventFromPromptSlashCommand("codex", tc.prompt, time.Time{})
		if tc.wantName == "" {
			if ok {
				t.Errorf("SkillEventFromPromptSlashCommand(%q) = %+v, true; want false", tc.prompt, event)
			}
			continue
		}
		if !ok {
			t.Errorf("SkillEventFromPromptSlashCommand(%q) ok = false, want true", tc.prompt)
			continue
		}
		if event.Skill.Name != tc.wantName {
			t.Errorf("SkillEventFromPromptSlashCommand(%q) name = %q, want %q", tc.prompt, event.Skill.Name, tc.wantName)
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

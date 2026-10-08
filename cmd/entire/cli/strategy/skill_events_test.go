package strategy

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func toolSkillEvent(id, name string) agent.SkillEvent {
	return agent.SkillEvent{
		ID:        id,
		EventType: agent.SkillEventTypeToolInvocation,
		Skill:     agent.SkillEventSkill{Name: name},
		Source: agent.SkillEventSource{
			Agent:      "claude-code",
			Signal:     agent.SkillSignalClaudeSkillToolUse,
			Confidence: agent.SkillConfidenceExplicit,
		},
	}
}

func TestAppendNewSkillEvents_ReturnsOnlyUnrecordedEvents(t *testing.T) {
	t.Parallel()

	existing := toolSkillEvent("claude-skill-tu-1", "entire")
	state := &SessionState{SkillEvents: []agent.SkillEvent{existing}}

	fresh := toolSkillEvent("claude-skill-tu-2", "review")
	appended := appendNewSkillEvents(state, []agent.SkillEvent{existing, fresh})

	if len(appended) != 1 || appended[0].ID != fresh.ID {
		t.Fatalf("appended = %+v, want only %q", appended, fresh.ID)
	}
	if len(state.SkillEvents) != 2 {
		t.Fatalf("state.SkillEvents has %d events, want 2", len(state.SkillEvents))
	}
}

func TestAppendNewSkillEvents_SecondPassIsEmpty(t *testing.T) {
	t.Parallel()

	// Condensation and turn-end finalize re-extract from the full transcript
	// (offset 0) on every run, so a second pass over the same events must
	// append and report nothing — that is what keeps telemetry exactly-once.
	state := &SessionState{}
	events := []agent.SkillEvent{
		toolSkillEvent("claude-skill-tu-1", "entire"),
		toolSkillEvent("claude-skill-tu-2", "review"),
	}

	if first := appendNewSkillEvents(state, events); len(first) != 2 {
		t.Fatalf("first pass appended %d events, want 2", len(first))
	}
	if second := appendNewSkillEvents(state, events); len(second) != 0 {
		t.Fatalf("second pass appended %d events, want 0", len(second))
	}
	if len(state.SkillEvents) != 2 {
		t.Fatalf("state.SkillEvents has %d events, want 2", len(state.SkillEvents))
	}
}

func TestAppendNewSkillEvents_DedupesWithinCandidates(t *testing.T) {
	t.Parallel()

	state := &SessionState{}
	ev := toolSkillEvent("claude-skill-tu-1", "entire")
	appended := appendNewSkillEvents(state, []agent.SkillEvent{ev, ev})

	if len(appended) != 1 {
		t.Fatalf("appended %d events, want 1", len(appended))
	}
	if len(state.SkillEvents) != 1 {
		t.Fatalf("state.SkillEvents has %d events, want 1", len(state.SkillEvents))
	}
}

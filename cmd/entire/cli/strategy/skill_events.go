package strategy

import "github.com/entireio/cli/cmd/entire/cli/agent"

func mergeSkillEvents(groups ...[]agent.SkillEvent) []agent.SkillEvent {
	seen := make(map[string]struct{})
	var out []agent.SkillEvent
	for _, group := range groups {
		for _, ev := range group {
			key := skillEventKey(ev)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, ev)
		}
	}
	return out
}

func skillEventKey(ev agent.SkillEvent) string {
	if ev.ID != "" {
		return "id:" + ev.ID
	}
	key := ev.Source.Agent + "|" + ev.Source.Signal + "|" + ev.EventType + "|" + ev.Skill.Name + "|" + ev.TurnID
	if ev.TranscriptAnchor != nil {
		key += "|" + ev.TranscriptAnchor.ToolUseID
	}
	if ev.Native != nil {
		key += "|" + ev.Native["command"] + "|" + ev.Native["tool_use_id"]
	}
	return key
}

func withSkillEventTurnID(events []agent.SkillEvent, turnID string) []agent.SkillEvent {
	if len(events) == 0 || turnID == "" {
		return events
	}
	out := make([]agent.SkillEvent, len(events))
	copy(out, events)
	for i := range out {
		if out[i].TurnID == "" {
			out[i].TurnID = turnID
		}
	}
	return out
}

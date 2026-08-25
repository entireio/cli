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

// appendNewSkillEvents merges candidates into state.SkillEvents and returns
// the events that were not already recorded. Persisting the merged set into
// session state is what keeps telemetry exactly-once: condensation and the
// turn-end finalize both re-extract skill events from the full transcript
// (offset 0) on every run, so without a durable record each pass would
// re-surface the same events. Callers forward the returned slice to telemetry
// only after their surrounding MutateSessionState saves the state — an
// unsaved append is re-derived (and re-returned) by the next pass.
func appendNewSkillEvents(state *SessionState, candidates []agent.SkillEvent) []agent.SkillEvent {
	if len(candidates) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(state.SkillEvents))
	for _, ev := range state.SkillEvents {
		seen[skillEventKey(ev)] = struct{}{}
	}
	var appended []agent.SkillEvent
	for _, ev := range candidates {
		key := skillEventKey(ev)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		state.SkillEvents = append(state.SkillEvents, ev)
		appended = append(appended, ev)
	}
	return appended
}

// AppendNewSkillEvents records candidates into state.SkillEvents, stamping the
// session's TurnID on events that lack one, and returns those that were not
// already recorded. It is the single dedupe used by every path that adds skill
// events — hook-provided (lifecycle) and transcript-extracted (condensation and
// turn-end finalize) alike — so "already recorded" means the same thing
// everywhere. Exactly-once telemetry rests on that agreement: two paths with
// different notions of duplicate would let one re-report what the other
// recorded.
func AppendNewSkillEvents(state *SessionState, candidates []agent.SkillEvent) []agent.SkillEvent {
	if state == nil || len(candidates) == 0 {
		return nil
	}
	return appendNewSkillEvents(state, withSkillEventTurnID(candidates, state.TurnID))
}

// persistNewSkillEvents records extracted transcript skill events into session
// state and returns both the newly appended events (for telemetry — see
// appendNewSkillEvents for the exactly-once contract) and the full deduped
// checkpoint view (state events first, extracted additions after, matching the
// order mergeSkillEvents always produced here). Transcript-extracted events
// (e.g. Claude Code's Skill tool) only surface on the condense/finalize paths
// — hooks never carry them — which is why persistence lives here rather than
// in the lifecycle handlers.
func persistNewSkillEvents(state *SessionState, extracted []agent.SkillEvent) (newEvents, checkpointView []agent.SkillEvent) {
	newEvents = AppendNewSkillEvents(state, extracted)
	return newEvents, mergeSkillEvents(state.SkillEvents)
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

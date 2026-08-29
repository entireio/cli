package cli

import (
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/tokenreport"
)

// tokenSourceTranscript is the Source value a token report carries when its
// totals were recomputed from a transcript (a checkpoint's stored full.jsonl
// or a live session's transcript file) rather than read from recorded totals.
const tokenSourceTranscript = "transcript"

// resolveTokenAttributor returns the TokenAttributor for agentType. When the
// agent cannot attribute, ok is false and reason says why in the words the
// report's Notes use — "no agent recorded", `agent "X" is not known to this
// CLI`, "per-call attribution is not available for X" — or is "" for an agent
// whose profile records session totals only, where no note is due because
// the profile note already says so.
func resolveTokenAttributor(agentType types.AgentType) (agent.TokenAttributor, string, bool) {
	if agentType == "" {
		return nil, "no agent recorded", false
	}
	ag, err := agent.GetByAgentType(agentType)
	if err != nil {
		return nil, fmt.Sprintf("agent %q is not known to this CLI", agentType), false
	}
	attributor, ok := agent.AsTokenAttributor(ag)
	if !ok {
		if tokenreport.ProfileFor(agentType).TotalsOnly {
			return nil, "", false
		}
		return nil, fmt.Sprintf("per-call attribution is not available for %s", agentType), false
	}
	return attributor, "", true
}

// separateSessionSubagentNote is the "subagent tokens are not included" note
// for agents whose subagents run as separate sessions, so no source — task
// record or subagents directory — can hold their usage: Codex and OpenCode.
// "" for every other agent, whose wording depends on the source.
func separateSessionSubagentNote(agentType types.AgentType) string {
	switch agentType {
	case agent.AgentTypeCodex:
		return "Codex subagent tokens are not included (their rollouts are separate sessions)."
	case agent.AgentTypeOpenCode:
		return "OpenCode subagent tokens are not included (task sessions are separate sessions)."
	default:
		return ""
	}
}

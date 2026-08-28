package tokenreport

import (
	"sort"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Agent type identifiers. These duplicate the string values of the
// agent.AgentType* constants in cmd/entire/cli/agent/registry.go rather than
// importing that package, because tokenreport is constrained to the standard
// library and agent/types only (see doc.go). Keep these in sync with
// registry.go; cmd/entire/cli/tokenreport_agents_test.go (package cli) fails
// if they drift.
const (
	agentClaudeCode     types.AgentType = "Claude Code"
	agentCodex          types.AgentType = "Codex"
	agentOpenCode       types.AgentType = "OpenCode"
	agentGemini         types.AgentType = "Gemini CLI"
	agentPi             types.AgentType = "Pi"
	agentCursor         types.AgentType = "Cursor"
	agentCopilotCLI     types.AgentType = "Copilot CLI"
	agentFactoryAIDroid types.AgentType = "Factory AI Droid"
)

// Effort source vocabulary for AgentProfile.EffortSource. A value states
// where an agent's effort/reasoning-level signal is read from in its
// transcript; the empty string means the agent does not record it.
const (
	effortSourcePerCallField        = "per-call field"
	effortSourceTurnContext         = "turn context"
	effortSourceThinkingLevelEvents = "thinking-level events"
	effortSourceModelName           = "model name"
)

// AgentProfile states what an agent's transcript records, so reports print
// "not recorded" instead of 0 and only offer recommendations whose inputs exist.
type AgentProfile struct {
	// RecordsThinking is true when the transcript records thinking/reasoning
	// token counts.
	RecordsThinking bool
	// RecordsCacheTTL is true when the transcript distinguishes cache writes
	// by TTL tier (e.g. 1h vs 5m ephemeral cache creation).
	RecordsCacheTTL bool
	// RecordsEffort is true when the transcript records an effort/reasoning
	// level signal in some form; see EffortSource for where.
	RecordsEffort bool
	// RecordsModelPerCall is true when the model used is attributable to
	// individual calls (as opposed to only a session-wide or per-model total).
	RecordsModelPerCall bool
	// RecordsToolCalls is true when individual tool invocations are recorded.
	RecordsToolCalls bool
	// RecordsSubagents is true when subagent/task-tool work is recorded in a
	// form that can be aggregated into the report.
	RecordsSubagents bool
	// RecordsCost is true when the transcript records a cost figure directly
	// (as opposed to one derived by the report from token counts and prices).
	RecordsCost bool
	// EffortSource names where RecordsEffort's signal comes from: one of
	// effortSourcePerCallField, effortSourceTurnContext,
	// effortSourceThinkingLevelEvents, effortSourceModelName, or "" when
	// RecordsEffort is false.
	EffortSource string
	// EffortSettingVerified is true only when the printed effort-tuning
	// setting name has been verified against the agent's real config surface.
	// False for every agent in B1 (spec §8.5 open); a setting name may be
	// printed only when this is true.
	EffortSettingVerified bool
	// TotalsOnly is true when the agent's transcript only supports a
	// session-wide usage total, not a per-category breakdown.
	TotalsOnly bool
	// Verified is true when this profile was checked against real committed
	// checkpoints for the agent. False for agents with no checkpoints to
	// verify against (Factory AI Droid) and for unknown agents; Copilot CLI
	// is verified for totals only (see TotalsOnly).
	Verified bool
	// Levers lists agent-specific advice vocabulary for recommendations.
	// Left nil for every agent in B1 (setting names are unverified; see
	// EffortSettingVerified).
	Levers []string
}

// agentProfiles is the per-agent capability table, populated from the
// 2026-08-27 data survey of every agent's committed transcripts. Values here
// are facts about what each agent writes, not aspirational targets — do not
// "improve" a row without a new survey to back it.
var agentProfiles = map[types.AgentType]AgentProfile{
	// Claude Code 2.1.246: usage.output_tokens_details.thinking_tokens,
	// cache_creation.ephemeral_1h_input_tokens, top-level effort, per-call
	// model, tool_use blocks, Task tool subagent records. No cost field.
	agentClaudeCode: {
		RecordsThinking:     true,
		RecordsCacheTTL:     true,
		RecordsEffort:       true,
		RecordsModelPerCall: true,
		RecordsToolCalls:    true,
		RecordsSubagents:    true,
		RecordsCost:         false,
		EffortSource:        effortSourcePerCallField,
		TotalsOnly:          false,
		Verified:            true,
	},
	// Codex: reasoning summaries yield thinking tokens; effort is set once
	// per turn context, not per call; per-call model and tool calls present;
	// spawn_agent events are seen but not aggregated into subagent totals; no
	// cache-TTL breakdown; no cost field.
	agentCodex: {
		RecordsThinking:     true,
		RecordsCacheTTL:     false,
		RecordsEffort:       true,
		RecordsModelPerCall: true,
		RecordsToolCalls:    true,
		RecordsSubagents:    false,
		RecordsCost:         false,
		EffortSource:        effortSourceTurnContext,
		TotalsOnly:          false,
		Verified:            true,
	},
	// OpenCode: thinking tokens and per-call model and tool calls present;
	// task tool subagent records aggregate; no effort signal; a cost field
	// exists on usage but is always 0 in observed transcripts, so it is not
	// treated as recorded; no cache-TTL breakdown.
	agentOpenCode: {
		RecordsThinking:     true,
		RecordsCacheTTL:     false,
		RecordsEffort:       false,
		RecordsModelPerCall: true,
		RecordsToolCalls:    true,
		RecordsSubagents:    true,
		RecordsCost:         false,
		EffortSource:        "",
		TotalsOnly:          false,
		Verified:            true,
	},
	// Gemini CLI: thinking tokens and tool calls present; per-message model
	// field verified on real data; no effort signal, no subagent records, no
	// cache-TTL breakdown, no cost field.
	agentGemini: {
		RecordsThinking:     true,
		RecordsCacheTTL:     false,
		RecordsEffort:       false,
		RecordsModelPerCall: true,
		RecordsToolCalls:    true,
		RecordsSubagents:    false,
		RecordsCost:         false,
		EffortSource:        "",
		TotalsOnly:          false,
		Verified:            true,
	},
	// Pi: no thinking-token field, but cacheWrite1h distinguishes cache TTL;
	// effort is inferred from thinking-level events; per-call model and tool
	// calls present; usage.cost is a recorded cost figure; no subagent
	// records.
	agentPi: {
		RecordsThinking:     false,
		RecordsCacheTTL:     true,
		RecordsEffort:       true,
		RecordsModelPerCall: true,
		RecordsToolCalls:    true,
		RecordsSubagents:    false,
		RecordsCost:         true,
		EffortSource:        effortSourceThinkingLevelEvents,
		TotalsOnly:          false,
		Verified:            true,
	},
	// Cursor: transcript exposes only a session-wide usage total; effort can
	// only be inferred from the model name; no per-call model, tool calls,
	// subagent records, thinking, cache-TTL, or cost field.
	agentCursor: {
		RecordsThinking:     false,
		RecordsCacheTTL:     false,
		RecordsEffort:       true,
		RecordsModelPerCall: false,
		RecordsToolCalls:    false,
		RecordsSubagents:    false,
		RecordsCost:         false,
		EffortSource:        effortSourceModelName,
		TotalsOnly:          true,
		Verified:            true,
	},
	// Copilot CLI: session.shutdown's modelMetrics reports per-model totals
	// (input/output/cache read/cache write tokens; agent/copilotcli/
	// transcript.go:212-280), not per-call data, so RecordsModelPerCall is
	// true only in that coarser per-model sense; otherwise no effort,
	// tool-call, subagent, cache-TTL, or cost recording. Those per-model
	// totals are verified against real transcripts, so Verified is true;
	// with no per-call breakdown available, TotalsOnly is also true.
	agentCopilotCLI: {
		RecordsThinking:     false,
		RecordsCacheTTL:     false,
		RecordsEffort:       false,
		RecordsModelPerCall: true,
		RecordsToolCalls:    false,
		RecordsSubagents:    false,
		RecordsCost:         false,
		EffortSource:        "",
		TotalsOnly:          true,
		Verified:            true,
	},
	// Factory AI Droid: totals-only, and no checkpoints exist to verify any
	// of these facts against, so Verified is false.
	agentFactoryAIDroid: {
		RecordsThinking:     false,
		RecordsCacheTTL:     false,
		RecordsEffort:       false,
		RecordsModelPerCall: false,
		RecordsToolCalls:    false,
		RecordsSubagents:    false,
		RecordsCost:         false,
		EffortSource:        "",
		TotalsOnly:          true,
		Verified:            false,
	},
}

// KnownAgents returns the agent types tokenreport has a capability profile
// for, sorted for deterministic output. Used by tests to guard against drift
// from the agent.AgentType* registry constants tokenreport cannot import.
func KnownAgents() []types.AgentType {
	agents := make([]types.AgentType, 0, len(agentProfiles))
	for a := range agentProfiles {
		agents = append(agents, a)
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i] < agents[j] })
	return agents
}

// ProfileFor returns the capability profile for agent, or the zero
// AgentProfile (Verified false) for an agent with no known profile.
func ProfileFor(agent types.AgentType) AgentProfile {
	return agentProfiles[agent]
}

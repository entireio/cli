package tokenreport

import (
	"maps"
	"slices"

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

// EffortSource identifies where an agent's effort/reasoning-level signal is
// read from in its transcript. The zero value ("") means no such signal is
// available at all — distinct from a signal that exists but is display-only
// (see EffortSourceModelName), which AgentProfile.RecordsEffort governs.
type EffortSource string

// Effort source vocabulary for AgentProfile.EffortSource.
const (
	// EffortSourcePerCallField means effort is recorded as its own field on
	// each call, usable directly by effort-tuning rules.
	EffortSourcePerCallField EffortSource = "per-call field"
	// EffortSourceTurnContext means effort is set once per turn rather than
	// per call.
	EffortSourceTurnContext EffortSource = "turn context"
	// EffortSourceThinkingLevelEvents means effort is inferred from
	// thinking-level events rather than an explicit field.
	EffortSourceThinkingLevelEvents EffortSource = "thinking-level events"
	// EffortSourceModelName means only the model name hints at effort. This
	// is display-only: a model name is not a per-call field that effort
	// rules can act on, so an agent using only this source still has
	// RecordsEffort false.
	EffortSourceModelName EffortSource = "model name"
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
	// level signal usable by effort rules; see EffortSource for where. A
	// display-only hint (EffortSourceModelName) does not count.
	RecordsEffort bool
	// RecordsModelPerCall is true when the model used is attributable to
	// individual calls. It is also true when only per-model totals are
	// recorded (Copilot CLI) rather than nothing at all — check TotalsOnly
	// to know whether that granularity is per-call or per-model.
	RecordsModelPerCall bool
	// RecordsToolCalls is true when individual tool invocations are recorded.
	RecordsToolCalls bool
	// RecordsSubagents is true when subagent/task-tool work is recorded in a
	// form that can be aggregated into the report.
	RecordsSubagents bool
	// RecordsCost is true when the transcript records a cost figure directly
	// (as opposed to one derived by the report from token counts and prices).
	RecordsCost bool
	// EffortSource names where an effort/reasoning-level signal, if any, is
	// read from. This may be set even when RecordsEffort is false: Cursor
	// sets it to EffortSourceModelName as a display hint even though a
	// model name is not a field effort rules can act on. The zero value
	// means no signal is available at all.
	EffortSource EffortSource
	// EffortSettingVerified is true only when the printed effort-tuning
	// setting name has been verified against the agent's real config surface.
	// False for every agent in B1 (spec §8.5 open); a setting name may be
	// printed only when this is true.
	EffortSettingVerified bool
	// TotalsOnly is true when the agent's transcript only supports a
	// session-wide usage total, not a per-category breakdown. It is also
	// the value ProfileFor uses for an agent with no known profile, since
	// totals-only is the safe assumption for an unrecognized transcript
	// shape.
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
	// Claude Code: usage.output_tokens_details.thinking_tokens has been
	// recorded since ~2026-08-11; cache_creation.ephemeral_1h_input_tokens,
	// top-level effort, per-call model, tool_use blocks, and Task tool
	// subagent records are all present. No cost field.
	agentClaudeCode: {
		RecordsThinking:     true,
		RecordsCacheTTL:     true,
		RecordsEffort:       true,
		RecordsModelPerCall: true,
		RecordsToolCalls:    true,
		RecordsSubagents:    true,
		RecordsCost:         false,
		EffortSource:        EffortSourcePerCallField,
		TotalsOnly:          false,
		Verified:            true,
	},
	// Codex: reasoning_output_tokens yields thinking token counts; effort is
	// set once per turn context, not per call; per-call model and tool
	// calls present; spawn_agent events are seen but not aggregated into
	// subagent totals; no cache-TTL breakdown; no cost field.
	agentCodex: {
		RecordsThinking:     true,
		RecordsCacheTTL:     false,
		RecordsEffort:       true,
		RecordsModelPerCall: true,
		RecordsToolCalls:    true,
		RecordsSubagents:    false,
		RecordsCost:         false,
		EffortSource:        EffortSourceTurnContext,
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
	// field verified on real data (the Gemini parser reads `model` from
	// Task 14 onward); no effort signal, no subagent records, no
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
		EffortSource:        EffortSourceThinkingLevelEvents,
		TotalsOnly:          false,
		Verified:            true,
	},
	// Cursor: transcript exposes only a session-wide usage total; the model
	// name is a display-only hint, not a per-call effort field, so
	// RecordsEffort is false even though EffortSource names it; no per-call
	// model, tool calls, subagent records, thinking, cache-TTL, or cost
	// field.
	agentCursor: {
		RecordsThinking:     false,
		RecordsCacheTTL:     false,
		RecordsEffort:       false,
		RecordsModelPerCall: false,
		RecordsToolCalls:    false,
		RecordsSubagents:    false,
		RecordsCost:         false,
		EffortSource:        EffortSourceModelName,
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
// for, sorted for deterministic output.
func KnownAgents() []types.AgentType {
	return slices.Sorted(maps.Keys(agentProfiles))
}

// ProfileFor returns the capability profile for agent. An agent with no
// known profile gets the safe default AgentProfile{TotalsOnly: true} —
// totals only, no breakdown, Verified false — rather than a bare zero
// value, so an unrecognized agent's report degrades to a totals view
// instead of implying that nothing at all was recorded.
func ProfileFor(agent types.AgentType) AgentProfile {
	if profile, ok := agentProfiles[agent]; ok {
		return profile
	}
	return AgentProfile{TotalsOnly: true}
}

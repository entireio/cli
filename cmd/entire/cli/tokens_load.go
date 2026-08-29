package cli

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
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

// sessionTokenAnalysis is what one session contributes to the report.
type sessionTokenAnalysis struct {
	meta *checkpoint.Metadata
	// attribution is the session's per-call usage; nil when the session was
	// not attributed (no attributor, no transcript, zero calls, parse error).
	attribution *types.Attribution
	// legacy is true for a checkpoint without token_usage_version: the
	// breakdown covers the whole stored transcript while the totals stay the
	// committed token_usage, which has no per-call counterpart.
	legacy bool
	// recomputed is true when usage was summed from the attributed calls and
	// subagent records rather than read from the committed token_usage.
	recomputed bool
	// usage is the session's flattened total; nil when nothing was recorded.
	usage *types.TokenUsage
	// subagent is the flattened subagent part of usage; nil when none.
	subagent *types.TokenUsage
	// attributed is the contributor table; zero when not attributed.
	attributed tokenreport.Attributed
	costParts  []tokenreport.CostShares
	duration   time.Duration
	// calls is the parent session's API calls with recorded usage: Σ
	// APICallCount over the attributed calls, or the committed count minus
	// the subagent count on the metadata path. Subagent calls are never
	// included, so the long_session replay gate sees the same figure on
	// every path.
	calls             int
	unknownUsageCalls int
	efforts           map[string]int
	models            map[string]int
	agentReportedCost float64
	// unmatchedSubagentRefs counts subagent tool calls with no task record.
	unmatchedSubagentRefs int
	// transcriptThinking and transcriptCacheWrite1h are the thinking and
	// 1-hour cache-write counts summed over a legacy session's attributed
	// calls — subset figures the committed token_usage predates. They are
	// surfaced beside the committed totals, never added into them.
	transcriptThinking     int
	transcriptCacheWrite1h int
	notes                  []string
}

// applySkillEventAnchors labels skill loads the attributor could not name:
// a tool-use ref whose ID matches a skill event's transcript anchor takes
// that event's skill name. The harness-stamped ActiveSkill on each call
// stays the first source; this is the second.
func applySkillEventAnchors(attribution *types.Attribution, events []types.SkillEvent) {
	names := make(map[string]string)
	for _, e := range events {
		if e.TranscriptAnchor != nil && e.TranscriptAnchor.ToolUseID != "" && e.Skill.Name != "" {
			names[e.TranscriptAnchor.ToolUseID] = e.Skill.Name
		}
	}
	if len(names) == 0 {
		return
	}
	label := func(ref *types.ToolUseRef) {
		if ref.SkillName != "" || ref.SubagentType != "" || ref.Tool == "" {
			return
		}
		if name, ok := names[ref.ID]; ok {
			ref.SkillName = name
			if ref.Detail == "" {
				ref.Detail = name
			}
		}
	}
	for i := range attribution.Calls {
		call := &attribution.Calls[i]
		for j := range call.Emitted {
			label(&call.Emitted[j])
		}
		for j := range call.Consumed {
			label(&call.Consumed[j].ToolUse)
		}
	}
}

// finishSessionTokenAnalysis computes a session's totals, cost parts,
// duration, call and effort counts. An attributed version-2 session is
// recomputed from its calls plus its subagent records; a legacy session
// keeps its breakdown but takes totals, calls and class shares from its
// committed token_usage (see sessionTokenAnalysis.legacy); any other session
// uses its committed token_usage, priced with the session model's base
// ratios.
func finishSessionTokenAnalysis(a *sessionTokenAnalysis) {
	if a.attribution == nil {
		a.finishFromMetadata()
		return
	}
	attr := a.attribution
	if a.legacy {
		a.finishLegacyFromTranscript()
		return
	}
	a.recomputed = true
	var usage *types.TokenUsage
	for i := range attr.Calls {
		call := &attr.Calls[i]
		if call.UsageUnknown {
			a.unknownUsageCalls++
		}
		u := call.Usage
		u.Model = ""
		usage = types.AddTokenUsage(usage, &u)
		a.calls += call.Usage.APICallCount
		if call.Effort != "" {
			a.efforts[call.Effort]++
		}
		if call.Model != "" {
			a.models[call.Model]++
		}
		if w, ok := callWeights(call); ok {
			// A per-call usage block records its TTL split: 0 means all 5m.
			a.costParts = append(a.costParts, tokenreport.ComputeCostSharesKnownTTL(&call.Usage, w))
		}
	}
	for i := range attr.Subagents {
		rec := &attr.Subagents[i]
		if rec.Usage == nil {
			continue
		}
		u := *rec.Usage
		u.Model = ""
		a.subagent = types.AddTokenUsage(a.subagent, &u)
		model := rec.Model
		if model == "" {
			model = rec.Usage.Model
		}
		if w, _, ok := tokenreport.WeightsFor(model); ok {
			a.costParts = append(a.costParts, tokenreport.ComputeCostSharesKnownTTL(rec.Usage, w))
		}
	}
	a.usage = types.AddTokenUsage(usage, a.subagent)
	a.attributed = tokenreport.Attribute(attr, nil)
	a.agentReportedCost = attr.AgentReportedCost
	a.unmatchedSubagentRefs = countUnmatchedSubagentRefs(attr)
	a.duration = attributionDuration(attr, a.meta)
}

// attributionDuration is the transcript's span (End − Start), falling back
// to the hook-reported session duration when the slice has no timestamps.
func attributionDuration(attr *types.Attribution, meta *checkpoint.Metadata) time.Duration {
	if !attr.Start.IsZero() && !attr.End.IsZero() && attr.End.After(attr.Start) {
		return attr.End.Sub(attr.Start)
	}
	return metadataDuration(meta)
}

// finishFromMetadata fills a session's totals from its committed
// token_usage, which already includes any subagent usage. The duration is
// the hook-reported one unless the transcript already supplied it.
func (a *sessionTokenAnalysis) finishFromMetadata() {
	if a.duration == 0 {
		a.duration = metadataDuration(a.meta)
	}
	if a.meta == nil || a.meta.TokenUsage == nil {
		return
	}
	flat := flattenTokenUsage(a.meta.TokenUsage)
	flat.Model = ""
	a.usage = flat
	a.subagent = flattenTokenUsage(a.meta.TokenUsage.SubagentTokens)
	a.calls = flat.APICallCount
	if a.subagent != nil {
		a.calls = max(flat.APICallCount-a.subagent.APICallCount, 0)
	}
	if a.meta.Model != "" {
		a.models[a.meta.Model] += max(flat.APICallCount, 1)
		if w, _, ok := tokenreport.WeightsFor(a.meta.Model); ok {
			a.costParts = append(a.costParts, tokenreport.ComputeCostShares(flat, w))
		}
	}
}

// callWeights returns the price ratios for one call at the long-context tier
// its total input puts it in; false for an unknown or unrecorded model.
func callWeights(call *types.CallUsage) (tokenreport.Weights, bool) {
	if _, _, ok := tokenreport.WeightsFor(call.Model); !ok {
		return tokenreport.Weights{}, false
	}
	u := &call.Usage
	return tokenreport.WeightsForCall(call.Model, u.InputTokens+u.CacheReadTokens+u.CacheCreationTokens), true
}

// countUnmatchedSubagentRefs counts the distinct subagent tool calls seen in
// the window — emitted, or consumed from before it — that no SubagentRecord
// accounts for: their tokens are not in the report.
func countUnmatchedSubagentRefs(a *types.Attribution) int {
	recorded := make(map[string]bool, len(a.Subagents))
	for _, rec := range a.Subagents {
		recorded[rec.ToolUseID] = true
	}
	unmatched := make(map[string]bool)
	note := func(ref *types.ToolUseRef) {
		if ref.SubagentType != "" && !recorded[ref.ID] {
			unmatched[ref.ID] = true
		}
	}
	for i := range a.Calls {
		call := &a.Calls[i]
		for j := range call.Emitted {
			note(&call.Emitted[j])
		}
		for j := range call.Consumed {
			note(&call.Consumed[j].ToolUse)
		}
	}
	return len(unmatched)
}

// metadataDuration is the hook-reported session duration, or 0.
func metadataDuration(meta *checkpoint.Metadata) time.Duration {
	if meta == nil || meta.SessionMetrics == nil || meta.SessionMetrics.DurationMs <= 0 {
		return 0
	}
	return time.Duration(meta.SessionMetrics.DurationMs) * time.Millisecond
}

// assembleTokenReportView merges the per-session analyses into the view the
// renderer prints: summed usage, merged contributors (Report.Sessions counts
// the attributed sessions merged), cost shares summed by units, the modal
// model and effort, summed duration and calls.
func assembleTokenReportView(analyses []sessionTokenAnalysis, metas []*checkpoint.Metadata) tokenReportView {
	var view tokenReportView
	var usage, subagent *types.TokenUsage
	var perSession []tokenreport.Attributed
	var costParts []tokenreport.CostShares
	efforts, models := make(map[string]int), make(map[string]int)
	for i := range analyses {
		a := &analyses[i]
		usage = types.AddTokenUsage(usage, a.usage)
		subagent = types.AddTokenUsage(subagent, a.subagent)
		if a.attribution != nil {
			perSession = append(perSession, a.attributed)
			view.Attributed = true
		}
		costParts = append(costParts, a.costParts...)
		view.Report.Duration += a.duration
		view.Report.Calls += a.calls
		view.UnknownUsageCalls += a.unknownUsageCalls
		view.AgentReportedCost += a.agentReportedCost
		for k, n := range a.efforts {
			efforts[k] += n
		}
		for k, n := range a.models {
			models[k] += n
		}
	}
	view.Report.Agent = reportAgent(metas)
	view.Report.Profile = tokenreport.ProfileFor(view.Report.Agent)
	view.Report.Model = modalKey(models)
	view.Report.Effort, view.EffortCalls = modalKeyCount(efforts)
	if usage != nil {
		view.HasUsage = tokenVolume(usage) > 0 || usage.APICallCount > 0
		usage.SubagentTokens = nil
		view.Report.Usage = *usage
	}
	if subagent != nil {
		subagent.SubagentTokens = nil
		view.Subagent = *subagent
	}
	view.Report.Sessions = len(perSession)
	if len(perSession) == 1 {
		view.Report.Attributed = perSession[0]
	} else if len(perSession) > 1 {
		view.Report.Attributed = tokenreport.MergeContributors(perSession)
	}
	view.Report.Cost = tokenreport.SumCostShares(costParts...)
	return view
}

// reportAgent is the agent whose gates and profile the report uses: the one
// agent of a single-agent checkpoint, else the agent of the most sessions
// (ties: first seen).
func reportAgent(metas []*checkpoint.Metadata) types.AgentType {
	counts := make(map[string]int)
	var order []string
	for _, m := range metas {
		if m == nil || m.Agent == "" {
			continue
		}
		key := string(m.Agent)
		if counts[key] == 0 {
			order = append(order, key)
		}
		counts[key]++
	}
	best := ""
	for _, key := range order {
		if best == "" || counts[key] > counts[best] {
			best = key
		}
	}
	return types.AgentType(best)
}

// modalKey returns the key with the highest count (ties: lexically first),
// or "" for an empty map.
func modalKey(counts map[string]int) string {
	k, _ := modalKeyCount(counts)
	return k
}

// modalKeyCount returns the key with the highest count and that count.
func modalKeyCount(counts map[string]int) (string, int) {
	best, bestN := "", 0
	for _, k := range slices.Sorted(maps.Keys(counts)) {
		if n := counts[k]; n > bestN {
			best, bestN = k, n
		}
	}
	return best, bestN
}

// countTokenSources counts the sessions whose totals were recomputed from
// their transcripts and those that fell back to committed token_usage.
func countTokenSources(analyses []sessionTokenAnalysis) (recomputed, fallback int) {
	for i := range analyses {
		switch {
		case analyses[i].recomputed:
			recomputed++
		case analyses[i].usage != nil:
			fallback++
		}
	}
	return recomputed, fallback
}

// pluralHaveHas picks "has"/"have" for n.
func pluralHaveHas(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}

// tokenPluralSuffix is "s" unless count is 1.
func tokenPluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

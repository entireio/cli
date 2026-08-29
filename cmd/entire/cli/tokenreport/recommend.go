package tokenreport

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Cause identifies why a Recommendation fired. The string values are the
// `cause` field of `--json` output and the keys Plan B2's pattern ledger
// counts recurrences by — do not rename.
type Cause string

// Causes a session-kind recommendation can name; see Recommend for the gate
// behind each.
const (
	// CauseLongSession is a long session, or one that spent most of its cost
	// replaying its own context (the two are the same complaint: context
	// replay grows with session length).
	CauseLongSession Cause = "long_session"
	// CauseContextGrowth is cache writes being the largest cost class with
	// one contributor row responsible for a large share — tool output read
	// back into context.
	CauseContextGrowth Cause = "context_growth"
	// CauseSubagentModel is a subagent row with a large cost share that ran
	// on the session's own model rather than a smaller one.
	CauseSubagentModel Cause = "subagent_model"
	// CauseThinking is thinking taking at least half of output tokens on an
	// agent that records an effort level.
	CauseThinking Cause = "thinking"
	// CauseCacheMiss is fresh (uncached) input taking a large share of cost
	// on a provider where that is avoidable.
	CauseCacheMiss Cause = "cache_miss"
	// CauseRepeatedSkill is one skill loaded more than once in a session.
	CauseRepeatedSkill Cause = "repeated_skill"
)

// RecommendationKindSession is the Recommendation.Kind of every
// recommendation this package produces in B1: an observation about the one
// session or checkpoint in the report. Plan B2 adds the "pattern" kind.
const RecommendationKindSession = "session"

// maxRecommendations caps how many recommendations Recommend returns.
const maxRecommendations = 2

// Citation names one contributor row — and optionally one of its details —
// whose figures a Recommendation's Text quotes. Kind, Label and Skill are the
// row's identity (the same three fields Attribute merges rows on); Detail is
// a Detail.Detail value when a detail sub-row's figures are quoted, "" when
// only the row's own figures are.
type Citation struct {
	// Kind is the cited row's Kind.
	Kind ContributorKind `json:"kind"`
	// Label is the cited row's Label.
	Label string `json:"label"`
	// Skill is the cited row's Skill annotation, "" when none.
	Skill string `json:"skill,omitempty"`
	// Detail is the cited detail's Detail, "" when no detail is quoted.
	Detail string `json:"detail,omitempty"`
}

// Recommendation is one or two plain sentences a colleague would add after
// reading the breakdown. Every figure in Text is one the renderer prints
// from the same Report (a contributor or detail row, a usage class, a cost
// share, the duration, a call count), formatted with the same Format*
// functions, so a reader can find each number in a row above.
type Recommendation struct {
	// Kind is RecommendationKindSession in B1.
	Kind string `json:"kind"`
	// Text is the sentence(s). Names (model, command, skill, effort value)
	// are set in backticks; numbers are printed as digits, never spelled out.
	Text string `json:"text"`
	// Cause is why it fired.
	Cause Cause `json:"cause"`
	// Cited lists every contributor row (and detail) whose figures Text
	// quotes — one entry per row, Detail set when that row's detail sub-row
	// is quoted too. Class-level figures (a usage class, a cost class share,
	// the duration, the call count) need no citation. The renderer must
	// print every cited row (and cited detail) even when it falls below
	// MaxRenderedRows/MaxRenderedDetails, so that every figure in Text is
	// visible. Nil when the Text quotes no row.
	Cited []Citation `json:"cited,omitempty"`
	// Memory is the general instruction an agent could store verbatim.
	// Pattern kind only (Plan B2); always empty here.
	Memory string `json:"memory,omitempty"`
	// Seen is the number of sessions in the profile window where the cause
	// recurred. Pattern kind only; always 0 here.
	Seen int `json:"seen,omitempty"`
	// Of is the number of sessions in the profile window. Pattern kind only;
	// always 0 here.
	Of int `json:"of,omitempty"`
}

// Gates are the thresholds each cause must clear before it fires. GatesFor
// supplies the calibrated per-agent values; tests read thresholds from the
// same Gates so fixtures cannot drift from the table.
type Gates struct {
	// LongSessionDuration fires CauseLongSession when Report.Duration is at
	// least this long. 0 disables the duration arm (an unrecorded duration
	// is 0 and never fires it).
	LongSessionDuration time.Duration
	// LongSessionCacheReadShare fires CauseLongSession when the cache-read
	// share of cost is at least this and the report has at least
	// LongSessionMinCalls calls.
	LongSessionCacheReadShare float64
	// LongSessionMinCalls is the call count the cache-read arm also needs.
	LongSessionMinCalls int
	// ContextGrowthRowShare is the cost share one contributor row must reach
	// for CauseContextGrowth, given cache write is the largest cost class.
	ContextGrowthRowShare float64
	// SubagentModelShare is the cost share a KindSubagent row on the
	// session's model must reach for CauseSubagentModel.
	SubagentModelShare float64
	// ThinkingShare is the fraction of output tokens that must be thinking
	// for CauseThinking.
	ThinkingShare float64
	// CacheMissShare is the fresh-input share of cost for CauseCacheMiss.
	CacheMissShare float64
	// RepeatedSkillMinLoads is how many times one skill must be loaded for
	// CauseRepeatedSkill.
	RepeatedSkillMinLoads int
}

// Gate values calibrated on real transcripts — grounding spec §7
// (docs/superpowers/specs/2026-08-27-token-report-agent-grounding.md,
// re-run 2026-08-28: Claude 150 sessions, Codex 150, OpenCode 113, Gemini 6,
// Pi 179). Each session-kind gate is set so it fires on a clear minority
// (≤ ~35%) of that agent's sessions:
//
//   - long_session: D = 8h for Claude Code (4h fired 40%, p75 duration 6.2h;
//     8h fires 31%), 4h for every other agent (Codex 30%, Pi 28% — entirely
//     the cache-read arm). Cache-read arm: ≥ 70% of cost and ≥ 20 calls.
//   - context_growth: cache write largest and one row ≥ 25% (Claude 32%,
//     Pi 16%; n/a where the provider has no cache-write charge).
//   - subagent_model: 15% (spec §3.6).
//   - thinking: 50% of output (Codex 17%; Claude expected ~5%).
//   - cache_miss: 45% Codex (18%), 40% OpenCode (~10%), 70% Gemini (~33%,
//     n=6 provisional); other agents take the Codex value (Pi on OpenAI fires
//     5% at 45%). Anthropic-priced reports never fire (see cacheMissEligible).
//   - repeated_skill: 2 loads (spec §3.6).
const (
	longSessionDurationClaudeCode = 8 * time.Hour
	longSessionDurationDefault    = 4 * time.Hour
	longSessionCacheReadShare     = 0.70
	longSessionMinCalls           = 20
	contextGrowthRowShare         = 0.25
	subagentModelShare            = 0.15
	thinkingShare                 = 0.50
	cacheMissShareCodex           = 0.45
	cacheMissShareOpenCode        = 0.40
	cacheMissShareGemini          = 0.70
	repeatedSkillMinLoads         = 2
)

// GatesFor returns the calibrated Gates for agent. Only two thresholds vary
// by agent: LongSessionDuration (8h for Claude Code, 4h otherwise) and
// CacheMissShare (45% Codex, 40% OpenCode, 70% Gemini CLI, 45% otherwise).
// An agent not in the table gets the "others" values.
func GatesFor(agent types.AgentType) Gates {
	g := Gates{
		LongSessionDuration:       longSessionDurationDefault,
		LongSessionCacheReadShare: longSessionCacheReadShare,
		LongSessionMinCalls:       longSessionMinCalls,
		ContextGrowthRowShare:     contextGrowthRowShare,
		SubagentModelShare:        subagentModelShare,
		ThinkingShare:             thinkingShare,
		CacheMissShare:            cacheMissShareCodex,
		RepeatedSkillMinLoads:     repeatedSkillMinLoads,
	}
	switch agent {
	case agentClaudeCode:
		g.LongSessionDuration = longSessionDurationClaudeCode
	case agentOpenCode:
		g.CacheMissShare = cacheMissShareOpenCode
	case agentGemini:
		g.CacheMissShare = cacheMissShareGemini
	default:
		// Codex, Pi, Cursor, Copilot CLI, Factory AI Droid and unknown
		// agents take the defaults above.
	}
	return g
}

// Report is the finished report Recommend reads: everything the renderer
// prints, in the values it prints them from, so a sentence can quote the
// same figures.
type Report struct {
	// Agent selects the Gates (Recommend) and, with Profile, what the
	// transcript records.
	Agent types.AgentType
	// Profile is ProfileFor(Agent), or a caller-adjusted profile. Only
	// RecordsEffort, EffortSettingVerified and Levers are read here.
	Profile AgentProfile
	// Model is the session's (parent) model, "" when unrecorded. A
	// KindSubagent row whose Model equals it, or is empty, counts as running
	// on the session's model.
	Model string
	// Effort is the dominant effort value seen across the report's calls —
	// the CallUsage.Effort carried by the most calls, "" when no call records
	// one. The caller computes it; it is quoted verbatim (in backticks) in
	// the thinking sentence, so pass the agent's own value ("high"), not a
	// paraphrase.
	Effort string
	// Usage is the flattened usage of the whole report (subagents folded in).
	Usage types.TokenUsage
	// Cost is the cost-share view of Usage. Units == 0 means volume-only
	// (unpriced model): no class-share gate fires and no class share is
	// quoted.
	Cost CostShares
	// Attributed is the contributor table, in the order the renderer prints
	// it. PricedUnits == 0 means every row share is 0: no row-share gate
	// fires and no row share is quoted.
	Attributed Attributed
	// Duration is the report's wall-clock span (Attribution.End − Start, or
	// the checkpoint's SessionMetrics); 0 when unrecorded, which disables
	// the duration arm of long_session.
	Duration time.Duration
	// Calls is the number of API calls. 0 falls back to Usage.APICallCount,
	// which is what callers normally have; the field exists so a caller with
	// a better count (distinct calls after deduplication) can pass it.
	Calls int
}

// Recommend returns at most two session-kind recommendations for r, gated by
// GatesFor(r.Agent). Each cause is evaluated once; those that fire are
// ordered by the cost share they address, descending (ties keep the order
// long_session, context_growth, subagent_model, thinking, cache_miss,
// repeated_skill), and the top two are returned. The addressed share is: the
// cache-read share (long_session), the qualifying row's share
// (context_growth, subagent_model, repeated_skill), Cost.Thinking (thinking)
// and Cost.Input (cache_miss). When nothing fires the result is nil — there
// is no "looks fine" recommendation.
func Recommend(r Report) []Recommendation {
	return RecommendWithGates(r, GatesFor(r.Agent))
}

// RecommendWithGates is Recommend with explicit Gates, for tests and callers
// that tune thresholds.
func RecommendWithGates(r Report, g Gates) []Recommendation {
	rules := []func(*Report, Gates) (Recommendation, float64, bool){
		recommendLongSession,
		recommendContextGrowth,
		recommendSubagentModel,
		recommendThinking,
		recommendCacheMiss,
		recommendRepeatedSkill,
	}
	var fired []candidate
	for order, rule := range rules {
		if rec, share, ok := rule(&r, g); ok {
			fired = append(fired, candidate{rec: rec, share: share, order: order})
		}
	}
	slices.SortStableFunc(fired, func(a, b candidate) int {
		if c := cmp.Compare(b.share, a.share); c != 0 {
			return c
		}
		return cmp.Compare(a.order, b.order)
	})
	var out []Recommendation
	for i := 0; i < len(fired) && i < maxRecommendations; i++ {
		out = append(out, fired[i].rec)
	}
	return out
}

// candidate is a fired recommendation with the share it addresses and its
// table order, for ranking.
type candidate struct {
	rec   Recommendation
	share float64
	order int
}

// sessionRecommendation builds a session-kind Recommendation citing rows.
func sessionRecommendation(cause Cause, text string, cited ...Citation) Recommendation {
	return Recommendation{Kind: RecommendationKindSession, Text: text, Cause: cause, Cited: cited}
}

// cite builds the Citation for row c, naming detail d when non-nil.
func cite(c *Contributor, d *Detail) Citation {
	ct := Citation{Kind: c.Kind, Label: c.Label, Skill: c.Skill}
	if d != nil {
		ct.Detail = d.Detail
	}
	return ct
}

// calls is the report's API call count: Calls, else Usage.APICallCount.
func (r *Report) calls() int {
	return cmp.Or(r.Calls, r.Usage.APICallCount)
}

// costPriced reports whether class shares (Cost) are meaningful.
func (r *Report) costPriced() bool {
	return r.Cost.Units > 0
}

// rowsPriced reports whether contributor and detail shares are meaningful.
func (r *Report) rowsPriced() bool {
	return r.Attributed.PricedUnits > 0
}

// ofCost renders a share as "23% of cost", or "" when the share is not
// priced so callers can drop the clause.
func ofCost(priced bool, share float64) string {
	if !priced {
		return ""
	}
	return FormatPercent(share) + " of cost"
}

// withOfCost appends ", 23% of cost" to figures when the fragment is
// non-empty: "140.2k tokens, 27% of cost".
func withOfCost(figures, ofCostFragment string) string {
	if ofCostFragment == "" {
		return figures
	}
	return figures + ", " + ofCostFragment
}

// selfDetail returns the Detail of c named after the row itself, or nil. A
// skill or subagent ref always carries its own name as Detail (see
// Attribute), so a KindSkill/KindSubagent row has one such detail and its
// Calls is the number of loads/runs — the figure the renderer prints as that
// detail's call count, which is why a sentence quoting it cites the detail.
// Usage.APICallCount is not used for this: it is the row's share of the
// emitting calls' API-call counts, split across every ref a call emitted,
// so it undercounts a Skill call emitted alongside other tools. A row with
// no such detail (an attributor that stamps no Detail) has no run count.
func selfDetail(c *Contributor) *Detail {
	for i := range c.Details {
		if c.Details[i].Detail == c.Label {
			return &c.Details[i]
		}
	}
	return nil
}

// rowFigures renders a row's tokens and share, "140.2k tokens, 27% of cost"
// or "140.2k tokens" when unpriced.
func rowFigures(r *Report, c *Contributor) string {
	return withOfCost(FormatTokenCount(volume(&c.Usage))+" tokens", ofCost(r.rowsPriced(), c.CostShare))
}

// detailFigures renders a detail's calls, tokens and share: "9 calls,
// 140.2k tokens, 27%" (the share is omitted when unpriced).
func detailFigures(r *Report, d *Detail) string {
	s := strconv.Itoa(d.Calls) + " calls, " + FormatTokenCount(d.Tokens) + " tokens"
	if r.rowsPriced() {
		s += ", " + FormatPercent(d.CostShare)
	}
	return s
}

// largestRow returns the row with the greatest key among those that keep
// passes, or nil. Ties keep the earlier row (the table is sorted by cost
// share, so that is the more expensive one).
func largestRow(rows []Contributor, keep func(*Contributor) bool, key func(*Contributor) float64) *Contributor {
	var best *Contributor
	for i := range rows {
		c := &rows[i]
		if !keep(c) {
			continue
		}
		if best == nil || key(c) > key(best) {
			best = c
		}
	}
	return best
}

// rowShare is a largestRow key: the row's cost share.
func rowShare(c *Contributor) float64 { return c.CostShare }

// recommendLongSession fires when the duration arm or the cache-read arm of
// Gates trips. The duration arm reads Report.Duration; the cache-read arm
// needs priced cost, a cache-read share ≥ LongSessionCacheReadShare and
// ≥ LongSessionMinCalls calls. Quotes class-level figures only (no
// citation). Addressed share: Cost.CacheRead.
func recommendLongSession(r *Report, g Gates) (Recommendation, float64, bool) {
	byDuration := g.LongSessionDuration > 0 && r.Duration >= g.LongSessionDuration
	byReplay := r.costPriced() && r.Cost.CacheRead >= g.LongSessionCacheReadShare && r.calls() >= g.LongSessionMinCalls
	if !byDuration && !byReplay {
		return Recommendation{}, 0, false
	}
	var text string
	if byDuration {
		text = "This session ran " + FormatDuration(r.Duration)
		if r.Usage.CacheReadTokens > 0 {
			text += "; re-reading its own context on every call took " + FormatTokenCount(r.Usage.CacheReadTokens) + " cache-read tokens"
			if share := ofCost(r.costPriced(), r.Cost.CacheRead); share != "" {
				text += " (" + share + ")"
			}
		}
		text += ". Work this long is cheaper as several shorter sessions."
	} else {
		text = "Most of this session's cost (" + FormatPercent(r.Cost.CacheRead) + ") was re-reading its own context: " +
			strconv.Itoa(r.calls()) + " calls replayed " + FormatTokenCount(r.Usage.CacheReadTokens) +
			" cache-read tokens. Shorter sessions replay less context on each call."
	}
	return sessionRecommendation(CauseLongSession, text), r.Cost.CacheRead, true
}

// recommendContextGrowth fires when cache write is strictly the largest cost
// class and the largest row that can carry cache writes (tool, skill,
// subagent, prompt — text and replay rows carry only output and cache reads)
// has a share ≥ ContextGrowthRowShare. The sentence names Details[0] when
// the row has one; the row (and that detail) are cited. Addressed share:
// that row's.
func recommendContextGrowth(r *Report, g Gates) (Recommendation, float64, bool) {
	if !r.costPriced() || !cacheWriteLargest(&r.Cost) {
		return Recommendation{}, 0, false
	}
	row := largestRow(r.Attributed.Contributors, carriesCacheWrites, rowShare)
	if row == nil || row.CostShare < g.ContextGrowthRowShare {
		return Recommendation{}, 0, false
	}
	text := FormatPercent(row.CostShare) + " of the cost was " + contextGrowthSubject(row)
	var d *Detail
	if len(row.Details) > 0 {
		d = &row.Details[0]
		text += ", mostly `" + d.Detail + "` (" + detailFigures(r, d) + ")"
	}
	text += ". " + contextGrowthAdvice(row.Kind)
	return sessionRecommendation(CauseContextGrowth, text, cite(row, d)), row.CostShare, true
}

// cacheWriteLargest reports whether cache write is strictly the largest of
// the four cost classes; a tie does not count.
func cacheWriteLargest(cs *CostShares) bool {
	return cs.CacheWrite > cs.Input && cs.CacheWrite > cs.CacheRead && cs.CacheWrite > cs.Output
}

// carriesCacheWrites reports whether rows of c's kind can hold fresh input
// and cache writes (see Attribute: consumed results and the prompt row).
func carriesCacheWrites(c *Contributor) bool {
	switch c.Kind {
	case KindTool, KindSkill, KindSubagent, KindPrompt:
		return true
	case KindText, KindReplay:
		return false
	}
	return false
}

// contextGrowthSubject names what a row's cache writes were, by kind:
// "Bash output (during systematic-debugging) read back into context".
func contextGrowthSubject(c *Contributor) string {
	switch c.Kind {
	case KindTool:
		label := c.Label
		if c.Skill != "" {
			label += " output (during " + c.Skill + ")"
		} else {
			label += " output"
		}
		return label + " read back into context"
	case KindSkill:
		return "loading the `" + c.Label + "` skill into context"
	case KindSubagent:
		return c.Label + " subagent work written into context"
	case KindPrompt:
		return "prompt and system context written into the cache"
	case KindText, KindReplay:
		return ""
	}
	return ""
}

// contextGrowthAdvice is the closing sentence for a context_growth row of
// kind.
func contextGrowthAdvice(kind ContributorKind) string {
	switch kind {
	case KindTool:
		return "Narrower commands or trimmed output would have avoided most of it."
	case KindSkill:
		return "A slimmer skill would have avoided most of it."
	case KindSubagent:
		return "Shorter subagent briefs and results would have avoided most of it."
	case KindPrompt:
		return "Shorter prompts and less injected context would have avoided most of it."
	case KindText, KindReplay:
		return ""
	}
	return ""
}

// recommendSubagentModel fires when the largest KindSubagent row that ran on
// the session's model (row Model equal to Report.Model, or empty) has a
// share ≥ SubagentModelShare. The run count is selfDetail's Calls, printed
// only when that detail exists, in which case the detail is cited along
// with the row. Addressed share: that row's.
func recommendSubagentModel(r *Report, g Gates) (Recommendation, float64, bool) {
	onParentModel := func(c *Contributor) bool {
		return c.Kind == KindSubagent && (c.Model == "" || c.Model == r.Model)
	}
	row := largestRow(r.Attributed.Contributors, onParentModel, rowShare)
	if row == nil || row.CostShare <= 0 || row.CostShare < g.SubagentModelShare {
		return Recommendation{}, 0, false
	}
	subject := row.Label + " subagents"
	if row.Label == LabelUnknownSubagent {
		subject = "Unlabelled subagents"
	}
	text := subject + " ran"
	d := selfDetail(row)
	if d != nil && d.Calls > 0 {
		text += " " + strconv.Itoa(d.Calls) + " times"
	} else {
		d = nil
	}
	if model := cmp.Or(row.Model, r.Model); model != "" {
		text += " on `" + model + "`"
	} else {
		text += " on the session's own model"
	}
	text += " (" + rowFigures(r, row) + "); delegated work like this often runs well on a smaller model."
	return sessionRecommendation(CauseSubagentModel, text, cite(row, d)), row.CostShare, true
}

// recommendThinking fires when the agent records effort and thinking tokens
// are ≥ ThinkingShare of output tokens. The sentence quotes the share of
// output the renderer prints ("57% of output"), the two token counts, the
// cost share when priced, and the effort value when known — class-level
// figures only, so nothing is cited. It names a setting only when
// Profile.EffortSettingVerified and Profile.Levers has one (Levers[0]);
// otherwise it stops at "a lower effort setting". Addressed share:
// Cost.Thinking (0 when volume-only, so it ranks last).
func recommendThinking(r *Report, g Gates) (Recommendation, float64, bool) {
	out, thinking := r.Usage.OutputTokens, r.Usage.ThinkingTokens
	if !r.Profile.RecordsEffort || out <= 0 || thinking <= 0 {
		return Recommendation{}, 0, false
	}
	ratio := float64(thinking) / float64(out)
	if ratio < g.ThinkingShare {
		return Recommendation{}, 0, false
	}
	counts := FormatTokenCount(thinking) + " of " + FormatTokenCount(out)
	text := "Thinking took " + FormatPercent(ratio) + " of output tokens (" + withOfCost(counts, ofCost(r.costPriced(), r.Cost.Thinking)) + ")"
	if r.Effort != "" {
		text += " at effort `" + r.Effort + "`"
	}
	if r.Profile.EffortSettingVerified && len(r.Profile.Levers) > 0 {
		text += "; lowering `" + r.Profile.Levers[0] + "` is usually enough for most work."
	} else {
		text += "; a lower effort setting is usually enough for most work."
	}
	return sessionRecommendation(CauseThinking, text), r.Cost.Thinking, true
}

// recommendCacheMiss fires when the report is priced on a provider where
// fresh input is avoidable (cacheMissEligible) and Cost.Input ≥
// CacheMissShare. It names the KindTool row with the most fresh input tokens
// when there is one, and that row's Details[0] when it has one, citing what
// it names. Addressed share: Cost.Input.
func recommendCacheMiss(r *Report, g Gates) (Recommendation, float64, bool) {
	if !r.costPriced() || !cacheMissEligible(r.Cost.Provider) || r.Cost.Input < g.CacheMissShare {
		return Recommendation{}, 0, false
	}
	text := FormatPercent(r.Cost.Input) + " of the cost was uncached input"
	isTool := func(c *Contributor) bool { return c.Kind == KindTool && c.Usage.InputTokens > 0 }
	freshInput := func(c *Contributor) float64 { return float64(c.Usage.InputTokens) }
	row := largestRow(r.Attributed.Contributors, isTool, freshInput)
	if row == nil {
		text += " — context that arrived fresh on each call instead of from the cache."
		return sessionRecommendation(CauseCacheMiss, text), r.Cost.Input, true
	}
	text += " — tool output that arrived fresh on each call — mostly " + row.Label + " results"
	var d *Detail
	if len(row.Details) > 0 {
		d = &row.Details[0]
		text += ", led by `" + d.Detail + "` (" + detailFigures(r, d) + ")."
	} else {
		text += " (" + rowFigures(r, row) + ")."
	}
	return sessionRecommendation(CauseCacheMiss, text, cite(row, d)), r.Cost.Input, true
}

// cacheMissEligible reports whether cache_miss can fire for a report priced
// on p: OpenAI and Google, whose sessions carry a real fresh-input share
// (grounding §7 medians 29%, 31%, 62%). Anthropic-priced sessions run ≈0%
// fresh input (median 0%, p90 3%), so a high share there is an artefact,
// not advice; a "" Provider (mixed or unknown) is not eligible either.
func cacheMissEligible(p Provider) bool {
	switch p {
	case ProviderOpenAI, ProviderGoogle:
		return true
	case ProviderAnthropic:
		return false
	}
	return false
}

// recommendRepeatedSkill fires when a KindSkill row's selfDetail counts at
// least RepeatedSkillMinLoads loads; with several, the most-loaded row wins
// (ties: higher share). The row and its self detail are cited. Addressed
// share: that row's.
func recommendRepeatedSkill(r *Report, g Gates) (Recommendation, float64, bool) {
	var row *Contributor
	var loads *Detail
	for i := range r.Attributed.Contributors {
		c := &r.Attributed.Contributors[i]
		if c.Kind != KindSkill {
			continue
		}
		d := selfDetail(c)
		if d == nil {
			continue
		}
		if loads == nil || d.Calls > loads.Calls || (d.Calls == loads.Calls && c.CostShare > row.CostShare) {
			row, loads = c, d
		}
	}
	if row == nil || loads.Calls < g.RepeatedSkillMinLoads {
		return Recommendation{}, 0, false
	}
	text := fmt.Sprintf("`%s` was loaded %d times (%s); once per session is enough.", row.Label, loads.Calls, rowFigures(r, row))
	return sessionRecommendation(CauseRepeatedSkill, text, cite(row, loads)), row.CostShare, true
}

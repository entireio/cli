package tokenreport

import (
	"cmp"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// ContributorKind says what kind of thing a Contributor row is, so a renderer
// can prefix the Label ("Skill: <name> (loaded)", "Subagent: <type>") and a
// recommendation can pick rows by kind rather than by display string.
type ContributorKind string

// Contributor kinds.
const (
	// KindTool is a tool the assistant called; Label is the tool name as the
	// agent names it ("Bash", "exec_command"), or LabelEarlierResults.
	KindTool ContributorKind = "tool"
	// KindSkill is a skill's own load cost — the Skill tool call and its
	// result; Label is the bare skill name.
	KindSkill ContributorKind = "skill"
	// KindSubagent is a subagent's cost — the parent's share of spawning it
	// plus the subagent's own usage; Label is the bare subagent type.
	KindSubagent ContributorKind = "subagent"
	// KindText is output the assistant produced without calling a tool;
	// Label is LabelAssistantText.
	KindText ContributorKind = "text"
	// KindReplay is cache-read input — context replayed from the prompt
	// cache; Label is LabelContextReplay.
	KindReplay ContributorKind = "replay"
	// KindPrompt is fresh input and cache writes on a call that consumed no
	// tool result — the user's prompt and (re-)injected system context;
	// Label is LabelPromptContext.
	KindPrompt ContributorKind = "prompt"
)

// Labels of the synthetic contributors. Rows of KindSkill and KindSubagent
// keep the bare skill name / subagent type as Label; the renderer prefixes
// them by Kind ("Skill: <name> (loaded)", "Subagent: <type>"). These are the
// only display strings this package defines for attribution.
const (
	// LabelAssistantText is the KindText row.
	LabelAssistantText = "Assistant text"
	// LabelContextReplay is the KindReplay row.
	LabelContextReplay = "Context replay"
	// LabelPromptContext is the KindPrompt row.
	LabelPromptContext = "Prompt & system context"
	// LabelEarlierResults is the KindTool row for tool results whose
	// tool_use precedes the transcript window and so has no tool name.
	LabelEarlierResults = "Earlier tool results"
)

// Contributor sources.
const (
	// SourceTranscript marks a row that has at least one token, or one call,
	// attributed from the transcript's own calls.
	SourceTranscript = "transcript"
	// SourceTaskRecord marks a row built solely from subagent records whose
	// spawning tool_use is not in the window (an orphan task record).
	SourceTaskRecord = "task_record"
)

// maxDetails is how many drill-down rows a Contributor keeps.
const maxDetails = 3

// Detail is one drill-down row under a Contributor: a ToolUseRef.Detail
// (command's leading words, file path, host, skill name, subagent type) with
// what it cost.
type Detail struct {
	// Detail is the ToolUseRef.Detail value; never empty.
	Detail string `json:"detail"`
	// Calls is the number of distinct tool calls with this detail: every
	// emitted ref, plus every consumed ref whose ID was not emitted inside
	// the window (its call happened earlier). A consumed ref with an empty
	// ID is assumed to have been emitted in the window and is not counted.
	Calls int `json:"calls"`
	// Tokens is the sum of the four token classes (input, cache write, cache
	// read, output) attributed to this detail.
	Tokens int `json:"tokens"`
	// CostShare is this detail's cost units over Attributed.PricedUnits —
	// the same denominator as Contributor.CostShare, so sub-rows compare
	// directly with rows. 0 when nothing is priced.
	CostShare float64 `json:"cost_share"`
}

// Contributor is one row of the "where the tokens went" table.
type Contributor struct {
	// Kind classifies the row; see ContributorKind.
	Kind ContributorKind `json:"kind"`
	// Label is the tool name, skill name, subagent type, or one of the
	// Label* constants, by Kind.
	Label string `json:"label"`
	// Skill is the harness-stamped skill that was active on the calls whose
	// tokens landed here (CallUsage.ActiveSkill); set only on KindTool and
	// KindText rows. It is part of the row's identity: Bash during a skill
	// is a different row from plain Bash.
	Skill string `json:"skill,omitempty"`
	// Model is the model behind the tokens: the calls' model for transcript
	// rows, the subagent's model for KindSubagent (SubagentRecord.Model, then
	// its Usage.Model, then the requested alias on the emitting ref); "" when
	// the row merges different models or none was recorded.
	Model string `json:"model,omitempty"`
	// Usage is the tokens attributed to this row. Its Model field is always
	// empty (see Model); APICallCount is the calls attributed here (a call's
	// count rides with its output).
	Usage types.TokenUsage `json:"usage"`
	// CostShare is this row's cost units over Attributed.PricedUnits; 0 when
	// nothing is priced or the row's calls were all on unpriced models.
	CostShare float64 `json:"cost_share"`
	// Source is SourceTranscript or SourceTaskRecord.
	Source string `json:"source"`
	// Details are the top maxDetails drill-down rows by Tokens (ties: more
	// Calls, then Detail ascending); nil when no ref carried a Detail.
	Details []Detail `json:"details,omitempty"`
}

// Attributed is the contributor table for one transcript slice, or for
// several merged with MergeContributors.
type Attributed struct {
	// Contributors is sorted by CostShare descending, then volume (the four
	// token classes) descending, then Label, Kind, Skill ascending. Never
	// nil.
	Contributors []Contributor `json:"contributors"`
	// Unpriced lists the recorded model names that have no verified price
	// ratios (WeightsFor ok=false), deduplicated in order of first
	// appearance. Their tokens count toward volume only. An unrecorded model
	// ("") is likewise unpriced but has no name to list.
	Unpriced []string `json:"unpriced_models,omitempty"`
	// PricedUnits is the total cost units over every priced call and
	// subagent record — the denominator of every CostShare.
	PricedUnits float64 `json:"priced_units"`
}

// Attribute turns one transcript slice's per-call usage into contributors.
// Every one of TokenUsage's seven numeric fields is conserved: Σ over the
// returned Contributors equals Σ over a.Calls[].Usage plus Σ over
// a.Subagents[].Usage. Rules, per call in order:
//
//   - Output. OutputTokens, ThinkingTokens and APICallCount go to the tools
//     the call Emitted, split equally with LargestRemainder (ties to the
//     earlier ref); ThinkingTokens and the non-thinking remainder are split
//     separately and re-added so each row's thinking stays ≤ its output. A
//     call that emitted nothing puts them on LabelAssistantText.
//   - Fresh input and cache writes. InputTokens, CacheCreationTokens and
//     CacheCreation1hTokens go to the results the call Consumed — the next
//     call pays for a tool's result — proportionally to Bytes (equally when
//     every Bytes is 0), 1h and 5m writes split separately as above. A call
//     that consumed nothing — the first call, and any later call that starts
//     a new user turn — puts them on LabelPromptContext.
//   - Cache reads. CacheReadTokens of every call go to LabelContextReplay.
//   - Targets. A ref with SkillName → KindSkill/<name>; with SubagentType →
//     KindSubagent/<type> (never keyed on the tool's name — "Agent" and the
//     legacy "Task" both land here); with an empty Tool (its tool_use
//     precedes the window) → KindTool/LabelEarlierResults; otherwise
//     KindTool/<Tool>. KindTool (named tools only) and KindText rows carry
//     the attributing call's ActiveSkill as Skill, so a tool run during a
//     skill is its own row; skill, subagent, replay, prompt and
//     earlier-results rows are never annotated.
//   - Subagents. A record whose ToolUseID matches a subagent ref (one with
//     SubagentType) emitted in the window adds its whole Usage — cache reads
//     included, they are the subagent's own replay — to that ref's
//     KindSubagent row, whose Model is record.Model, else
//     record.Usage.Model, else the ref's requested alias. Any other record
//     is its own KindSubagent row labelled record.SubagentType with Source
//     SourceTaskRecord; a row that also received transcript tokens keeps
//     SourceTranscript. Records are absorbed even when Usage is nil, so a
//     spawned subagent with no usage yet still shows with 0.
//   - Rows. Same Kind+Label+Skill merge with types.AddTokenUsage, whose
//     model rule gives Model (kept when equal, "" when mixed). A ref-driven
//     row (tool, skill, subagent) exists even when its tokens are zero — a
//     UsageUnknown call's tools still show with 0 — while a synthetic row
//     (text, replay, prompt) is created only when something non-zero lands
//     on it.
//   - Cost. With w nil, each call is priced with WeightsForCall(call.Model,
//     input+cacheRead+cacheWrite) when WeightsFor knows the model; an
//     unknown model's calls contribute volume only and the name goes to
//     Unpriced. With w non-nil every call and record is priced with w. Each
//     attributed piece is priced separately with ComputeCostShares, so a
//     call that mixes 5m and 1h cache writes under a Family that prices them
//     differently may leave a piece's writes unpriced (CacheWriteUnpriced);
//     real sessions are all-1h or all-5m and are priced exactly.
//
// A nil a, or one with no Calls, yields an empty Attributed even when
// Subagents is non-empty.
func Attribute(a *types.Attribution, w *Weights) Attributed {
	acc := newAccumulator()
	if a == nil || len(a.Calls) == 0 {
		return acc.result()
	}
	at := newAttributor(acc, w, a.Subagents)
	for i := range a.Calls {
		at.attributeCall(&a.Calls[i])
	}
	at.absorbSubagents(a.Subagents)
	return acc.result()
}

// MergeContributors merges the contributor tables of several sessions (of
// one checkpoint) into one: same Kind+Label+Skill rows are summed with
// types.AddTokenUsage (Model kept when equal, "" when mixed; Source stays
// SourceTranscript if any part was), PricedUnits are summed and every
// CostShare is recomputed over the combined total, Unpriced is the ordered
// union, and Details with the same Detail are summed and re-topped to
// maxDetails. Only the details each session kept survive, so a merged
// detail's Calls and Tokens are lower bounds when a session had more than
// maxDetails. Nil or empty input yields an empty Attributed.
func MergeContributors(perSession []Attributed) Attributed {
	acc := newAccumulator()
	for _, s := range perSession {
		acc.pricedUnits += s.PricedUnits
		for _, m := range s.Unpriced {
			acc.noteUnpriced(m)
		}
		for _, c := range s.Contributors {
			u := c.Usage
			u.Model = c.Model
			row := acc.row(contribKey{kind: c.Kind, label: c.Label, skill: c.Skill})
			row.absorb(&u, c.CostShare*s.PricedUnits, c.Source)
			for _, d := range c.Details {
				row.addDetail(d.Detail, d.Calls, d.Tokens, d.CostShare*s.PricedUnits)
			}
		}
	}
	return acc.result()
}

// LargestRemainder apportions budget across counts in proportion to each
// count's share of total, using the largest-remainder (Hamilton) method so
// the result sums to budget exactly instead of drifting through independent
// flooring (three equal thirds of 100 → 34/33/33, not 33/33/33). Each entry
// gets floor(count×budget/total); the leftover units go one each to the
// largest fractional remainders, ties broken by lower index. Callers pass
// total == Σcounts with non-negative counts. Returns all zeros when total or
// budget is non-positive.
func LargestRemainder(counts []int, total, budget int) []int {
	out := make([]int, len(counts))
	if total <= 0 || budget <= 0 {
		return out
	}
	allocated := 0
	order := make([]int, len(counts))
	for i, c := range counts {
		out[i] = c * budget / total
		allocated += out[i]
		order[i] = i
	}
	leftover := budget - allocated
	if leftover <= 0 {
		return out
	}
	remainder := func(i int) int { return (counts[i] * budget) % total }
	slices.SortStableFunc(order, func(a, b int) int {
		if c := cmp.Compare(remainder(b), remainder(a)); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	for i := 0; i < leftover && i < len(order); i++ {
		out[order[i]]++
	}
	return out
}

// splitWithSubset apportions whole and its subset across counts so that each
// entry's subset stays ≤ its whole: the subset and the remainder
// (whole−subset) are apportioned separately with LargestRemainder and the
// two re-added. When subset is not within [0, whole] (corrupt input) the two
// are apportioned independently instead, which still conserves both sums.
func splitWithSubset(counts []int, total, whole, subset int) (wholes, subsets []int) {
	subsets = LargestRemainder(counts, total, subset)
	if subset < 0 || subset > whole {
		return LargestRemainder(counts, total, whole), subsets
	}
	wholes = LargestRemainder(counts, total, whole-subset)
	for i := range wholes {
		wholes[i] += subsets[i]
	}
	return wholes, subsets
}

// contribKey is a Contributor's identity.
type contribKey struct {
	kind  ContributorKind
	label string
	skill string
}

// detailAcc accumulates one Detail.
type detailAcc struct {
	calls  int
	tokens int
	units  float64
}

// contribAcc accumulates one Contributor.
type contribAcc struct {
	usage   *types.TokenUsage
	units   float64
	source  string
	details map[string]*detailAcc
}

// absorb adds u (whose Model field takes part in the merge), its cost units,
// and source to the row.
func (r *contribAcc) absorb(u *types.TokenUsage, units float64, source string) {
	r.usage = types.AddTokenUsage(r.usage, u)
	r.units += units
	if r.source == "" || source == SourceTranscript {
		r.source = source
	}
}

// addDetail adds to the detail row named detail; an empty detail is ignored.
func (r *contribAcc) addDetail(detail string, calls, tokens int, units float64) {
	if detail == "" {
		return
	}
	d := r.details[detail]
	if d == nil {
		d = &detailAcc{}
		r.details[detail] = d
	}
	d.calls += calls
	d.tokens += tokens
	d.units += units
}

// topDetails returns the row's details sorted by tokens desc, calls desc,
// name asc, truncated to maxDetails; nil when there are none.
func (r *contribAcc) topDetails(pricedUnits float64) []Detail {
	if len(r.details) == 0 {
		return nil
	}
	out := make([]Detail, 0, len(r.details))
	for name, d := range r.details {
		out = append(out, Detail{Detail: name, Calls: d.calls, Tokens: d.tokens, CostShare: share(d.units, pricedUnits)})
	}
	slices.SortStableFunc(out, func(a, b Detail) int {
		if c := cmp.Compare(b.Tokens, a.Tokens); c != 0 {
			return c
		}
		if c := cmp.Compare(b.Calls, a.Calls); c != 0 {
			return c
		}
		return strings.Compare(a.Detail, b.Detail)
	})
	if len(out) > maxDetails {
		out = out[:maxDetails]
	}
	return out
}

// accumulator collects rows, priced units and unpriced models, and turns
// them into a sorted Attributed.
type accumulator struct {
	rows        map[contribKey]*contribAcc
	pricedUnits float64
	unpriced    []string
}

func newAccumulator() *accumulator {
	return &accumulator{rows: make(map[contribKey]*contribAcc)}
}

// row returns the accumulator for key, creating it.
func (acc *accumulator) row(key contribKey) *contribAcc {
	r := acc.rows[key]
	if r == nil {
		r = &contribAcc{details: make(map[string]*detailAcc)}
		acc.rows[key] = r
	}
	return r
}

// noteUnpriced records a model with no price ratios, once, in order of first
// appearance; an unrecorded model ("") has no name to list.
func (acc *accumulator) noteUnpriced(model string) {
	if model == "" || slices.Contains(acc.unpriced, model) {
		return
	}
	acc.unpriced = append(acc.unpriced, model)
}

// result builds the sorted contributor table.
func (acc *accumulator) result() Attributed {
	out := Attributed{Contributors: make([]Contributor, 0, len(acc.rows)), Unpriced: acc.unpriced, PricedUnits: acc.pricedUnits}
	for key, r := range acc.rows {
		c := Contributor{Kind: key.kind, Label: key.label, Skill: key.skill, Source: r.source, CostShare: share(r.units, acc.pricedUnits)}
		if r.usage != nil {
			c.Usage = *r.usage
			c.Model = c.Usage.Model
			c.Usage.Model = ""
		}
		c.Details = r.topDetails(acc.pricedUnits)
		out.Contributors = append(out.Contributors, c)
	}
	slices.SortStableFunc(out.Contributors, compareContributors)
	return out
}

// compareContributors orders by CostShare desc, volume desc, then Label,
// Kind and Skill asc.
func compareContributors(a, b Contributor) int {
	if c := cmp.Compare(b.CostShare, a.CostShare); c != 0 {
		return c
	}
	if c := cmp.Compare(volume(&b.Usage), volume(&a.Usage)); c != 0 {
		return c
	}
	if c := strings.Compare(a.Label, b.Label); c != 0 {
		return c
	}
	if c := strings.Compare(string(a.Kind), string(b.Kind)); c != 0 {
		return c
	}
	return strings.Compare(a.Skill, b.Skill)
}

// share divides units by total, 0 when total is not positive.
func share(units, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return units / total
}

// volume is the four token classes summed — the size of a usage regardless
// of price. Subset fields and APICallCount are not part of it.
func volume(u *types.TokenUsage) int {
	return u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens + u.OutputTokens
}

// isZero reports whether every one of the seven numeric fields is 0.
func isZero(u *types.TokenUsage) bool {
	return volume(u) == 0 && u.APICallCount == 0 && u.ThinkingTokens == 0 && u.CacheCreation1hTokens == 0
}

// isSynthetic reports whether rows of kind exist only when tokens land on
// them (text, replay, prompt), as opposed to ref-driven rows that exist as
// soon as a ref names them.
func isSynthetic(kind ContributorKind) bool {
	switch kind {
	case KindText, KindReplay, KindPrompt:
		return true
	case KindTool, KindSkill, KindSubagent:
		return false
	}
	return false
}

// piece is one slice of a call's usage headed for one row.
type piece struct {
	key    contribKey
	usage  types.TokenUsage
	detail string
	calls  int
}

// attributor walks one Attribution's calls into an accumulator.
type attributor struct {
	acc      *accumulator
	override *Weights
	// records indexes a.Subagents by ToolUseID (first record wins) so a
	// subagent ref's row can carry the record's model from its first piece.
	records map[string]*types.SubagentRecord
	// emitted maps every in-window emitted tool_use id to its ref; it labels
	// matching subagent records and tells a consumed ref's call apart from
	// one made before the window.
	emitted map[string]types.ToolUseRef
}

func newAttributor(acc *accumulator, w *Weights, records []types.SubagentRecord) *attributor {
	at := &attributor{acc: acc, override: w, records: make(map[string]*types.SubagentRecord), emitted: make(map[string]types.ToolUseRef)}
	for i := range records {
		if _, seen := at.records[records[i].ToolUseID]; !seen {
			at.records[records[i].ToolUseID] = &records[i]
		}
	}
	return at
}

// weightsFor returns the price ratios for a call or record on model with
// usage u, and whether it is priced; an unpriced model is noted.
func (at *attributor) weightsFor(model string, u *types.TokenUsage) (Weights, bool) {
	if at.override != nil {
		return *at.override, true
	}
	if _, _, ok := WeightsFor(model); !ok {
		at.acc.noteUnpriced(model)
		return Weights{}, false
	}
	return WeightsForCall(model, u.InputTokens+u.CacheReadTokens+u.CacheCreationTokens), true
}

// attributeCall splits one call into pieces and adds each to its row.
func (at *attributor) attributeCall(call *types.CallUsage) {
	w, priced := at.weightsFor(call.Model, &call.Usage)
	pieces := at.outputPieces(call)
	pieces = append(pieces, at.inputPieces(call)...)
	pieces = append(pieces, piece{
		key:   contribKey{kind: KindReplay, label: LabelContextReplay},
		usage: types.TokenUsage{CacheReadTokens: call.Usage.CacheReadTokens, Model: call.Model},
	})
	for i := range pieces {
		at.add(&pieces[i], w, priced, SourceTranscript)
	}
}

// add prices one piece and folds it into its row. A synthetic row is not
// created for an all-zero piece; a ref-driven row is.
func (at *attributor) add(p *piece, w Weights, priced bool, source string) {
	if isSynthetic(p.key.kind) && isZero(&p.usage) {
		return
	}
	var units float64
	if priced {
		units = ComputeCostShares(&p.usage, w).Units
		at.acc.pricedUnits += units
	}
	row := at.acc.row(p.key)
	row.absorb(&p.usage, units, source)
	row.addDetail(p.detail, p.calls, volume(&p.usage), units)
}

// outputPieces splits a call's output, thinking and call count across the
// refs it emitted, or onto Assistant text.
func (at *attributor) outputPieces(call *types.CallUsage) []piece {
	u := &call.Usage
	if len(call.Emitted) == 0 {
		return []piece{{
			key:   contribKey{kind: KindText, label: LabelAssistantText, skill: call.ActiveSkill},
			usage: types.TokenUsage{OutputTokens: u.OutputTokens, ThinkingTokens: u.ThinkingTokens, APICallCount: u.APICallCount, Model: call.Model},
		}}
	}
	n := len(call.Emitted)
	ones := make([]int, n)
	for i := range ones {
		ones[i] = 1
	}
	outputs, thinking := splitWithSubset(ones, n, u.OutputTokens, u.ThinkingTokens)
	calls := LargestRemainder(ones, n, u.APICallCount)
	pieces := make([]piece, 0, n)
	for i, ref := range call.Emitted {
		if ref.ID != "" {
			at.emitted[ref.ID] = ref
		}
		pieces = append(pieces, piece{
			key:    keyFor(&ref, call.ActiveSkill),
			usage:  types.TokenUsage{OutputTokens: outputs[i], ThinkingTokens: thinking[i], APICallCount: calls[i], Model: at.modelFor(&ref, call.Model)},
			detail: ref.Detail,
			calls:  1,
		})
	}
	return pieces
}

// inputPieces splits a call's fresh input and cache writes across the
// results it consumed, by Bytes, or onto Prompt & system context.
func (at *attributor) inputPieces(call *types.CallUsage) []piece {
	u := &call.Usage
	if len(call.Consumed) == 0 {
		return []piece{{
			key:   contribKey{kind: KindPrompt, label: LabelPromptContext},
			usage: types.TokenUsage{InputTokens: u.InputTokens, CacheCreationTokens: u.CacheCreationTokens, CacheCreation1hTokens: u.CacheCreation1hTokens, Model: call.Model},
		}}
	}
	counts, total := resultWeights(call.Consumed)
	inputs := LargestRemainder(counts, total, u.InputTokens)
	writes, oneHour := splitWithSubset(counts, total, u.CacheCreationTokens, u.CacheCreation1hTokens)
	pieces := make([]piece, 0, len(call.Consumed))
	for i, res := range call.Consumed {
		ref := res.ToolUse
		calls := 0
		if _, seen := at.emitted[ref.ID]; ref.ID != "" && !seen {
			// Emitted before the window: this is the only sighting of the call.
			calls = 1
			at.emitted[ref.ID] = ref
		}
		pieces = append(pieces, piece{
			key:    keyFor(&ref, call.ActiveSkill),
			usage:  types.TokenUsage{InputTokens: inputs[i], CacheCreationTokens: writes[i], CacheCreation1hTokens: oneHour[i], Model: at.modelFor(&ref, call.Model)},
			detail: ref.Detail,
			calls:  calls,
		})
	}
	return pieces
}

// resultWeights returns each consumed result's Bytes and their sum, or all
// ones when every Bytes is 0 so the split is equal.
func resultWeights(consumed []types.ToolResultRef) (counts []int, total int) {
	counts = make([]int, len(consumed))
	for i, res := range consumed {
		counts[i] = max(res.Bytes, 0)
		total += counts[i]
	}
	if total == 0 {
		for i := range counts {
			counts[i] = 1
		}
		total = len(counts)
	}
	return counts, total
}

// keyFor is the row a ref's tokens land on; see Attribute for the rules.
func keyFor(ref *types.ToolUseRef, activeSkill string) contribKey {
	switch {
	case ref.Tool == "":
		return contribKey{kind: KindTool, label: LabelEarlierResults}
	case ref.SkillName != "":
		return contribKey{kind: KindSkill, label: ref.SkillName}
	case ref.SubagentType != "":
		return contribKey{kind: KindSubagent, label: ref.SubagentType}
	default:
		return contribKey{kind: KindTool, label: ref.Tool, skill: activeSkill}
	}
}

// modelFor is the Model a piece for ref carries: the subagent's resolved
// model for a subagent ref, otherwise the call's.
func (at *attributor) modelFor(ref *types.ToolUseRef, callModel string) string {
	if ref.SubagentType == "" {
		return callModel
	}
	return subagentModel(at.records[ref.ID], ref.Model)
}

// subagentModel applies the SubagentRecord precedence: record.Model, then
// record.Usage.Model, then the emitting ref's requested alias. rec may be
// nil.
func subagentModel(rec *types.SubagentRecord, refModel string) string {
	if rec != nil {
		if rec.Model != "" {
			return rec.Model
		}
		if rec.Usage != nil && rec.Usage.Model != "" {
			return rec.Usage.Model
		}
	}
	return refModel
}

// absorbSubagents adds each record's usage to its subagent row: the emitting
// ref's row when the spawning tool_use is in the window, else an orphan row
// with Source SourceTaskRecord.
func (at *attributor) absorbSubagents(records []types.SubagentRecord) {
	for i := range records {
		rec := &records[i]
		ref, inWindow := at.emitted[rec.ToolUseID]
		var key contribKey
		source := SourceTaskRecord
		if inWindow && ref.SubagentType != "" {
			key = keyFor(&ref, "")
			source = SourceTranscript
		} else {
			key = contribKey{kind: KindSubagent, label: rec.SubagentType}
		}
		var usage types.TokenUsage
		if rec.Usage != nil {
			usage = *rec.Usage
		}
		usage.Model = subagentModel(rec, ref.Model)
		w, priced := at.weightsFor(usage.Model, &usage)
		at.add(&piece{key: key, usage: usage}, w, priced, source)
	}
}

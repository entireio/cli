package types

import (
	"slices"
	"strings"
)

// TokenUsage represents aggregated token usage for a checkpoint.
// This is agent-agnostic and can be populated by any agent that tracks token usage.
type TokenUsage struct {
	// InputTokens is the number of input tokens (fresh, not from cache)
	InputTokens int `json:"input_tokens"`
	// CacheCreationTokens is tokens written to cache (billable at cache write rate)
	CacheCreationTokens int `json:"cache_creation_tokens"`
	// CacheReadTokens is tokens read from cache (discounted rate)
	CacheReadTokens int `json:"cache_read_tokens"`
	// OutputTokens is the number of output tokens generated
	OutputTokens int `json:"output_tokens"`
	// APICallCount is the number of API calls made
	APICallCount int `json:"api_call_count"`

	// Model is the model that produced every token in this entry. Set on
	// subagent entries only (the session metadata's model covers the top
	// level); empty when unrecorded or when more than one model contributed,
	// in which case ByModel holds the split.
	Model string `json:"model,omitempty"`
	// ByModel splits this entry's counts by model when more than one model
	// (an unrecorded model counts as one, under "") contributed. Entries are
	// flat, sorted by Model, and their counts sum to this entry's.
	ByModel []TokenUsage `json:"by_model,omitempty"`

	// SubagentTokens contains token usage from spawned subagents (if any)
	SubagentTokens *TokenUsage `json:"subagent_tokens,omitempty"`
	// SubagentTokensComplete says whether the outer result has exact child coverage.
	SubagentTokensComplete *bool `json:"subagent_tokens_complete,omitempty"`
}

// WithClearedSubagentTokens returns an independent usage result with child
// totals removed and an explicit coverage marker. A nil usage becomes a
// marker-only result so unavailable coverage persists.
func WithClearedSubagentTokens(usage *TokenUsage, complete bool) *TokenUsage {
	if usage == nil {
		usage = &TokenUsage{}
	}
	cleared := *usage
	cleared.SubagentTokens = nil
	cleared.SubagentTokensComplete = &complete
	return &cleared
}

// MaxSubagentDepth caps how deep a SubagentTokens chain is walked. Real chains
// are depth 1 — an agent reports one aggregate for all its subagents — so this is
// insurance against a malformed or hostile chain, not a real limit.
//
// It matters because token usage is read back from per-session metadata.json blobs
// on the shared checkpoint branch, which anyone with push access can author. The
// depth is not a stack-overflow risk (encoding/json caps nesting at 10000), but an
// unbounded chain reaching the root CheckpointSummary is a write amplification
// vector: the summary is re-marshalled with indentation, which is O(depth²) in
// output size, so a ~10k-deep chain in a 200KB session blob expands to a ~700MB
// root blob that then gets written and pushed.
const MaxSubagentDepth = 16

// MaxModelsPerUsage caps how many ByModel entries one usage entry may carry.
// Real splits name a handful of models; a longer list read back from a pushed
// blob is treated as malformed (see modelBreakdown) rather than carried
// forward into every summary that sums it.
const MaxModelsPerUsage = 32

// AddTokenUsage returns the sum of a and b, recursing into subagent usage.
// Either operand may be nil (treated as zero); the result is nil only when both
// are. Neither input is mutated. Subagent chains deeper than MaxSubagentDepth are
// truncated.
func AddTokenUsage(a, b *TokenUsage) *TokenUsage {
	return addTokenUsageAtDepth(a, b, 0)
}

func addTokenUsageAtDepth(a, b *TokenUsage, depth int) *TokenUsage {
	if a == nil && b == nil {
		return nil
	}
	sum := &TokenUsage{}
	var aSub, bSub *TokenUsage
	if a != nil {
		sum.InputTokens = a.InputTokens
		sum.CacheCreationTokens = a.CacheCreationTokens
		sum.CacheReadTokens = a.CacheReadTokens
		sum.OutputTokens = a.OutputTokens
		sum.APICallCount = a.APICallCount
		aSub = a.SubagentTokens
	}
	if b != nil {
		sum.InputTokens += b.InputTokens
		sum.CacheCreationTokens += b.CacheCreationTokens
		sum.CacheReadTokens += b.CacheReadTokens
		sum.OutputTokens += b.OutputTokens
		sum.APICallCount += b.APICallCount
		bSub = b.SubagentTokens
	}
	sum.setModels(append(modelBreakdown(a), modelBreakdown(b)...))
	if depth >= MaxSubagentDepth {
		if depth == 0 {
			sum.SubagentTokensComplete = tokenCompleteness(a, b)
		}
		return sum
	}
	sum.SubagentTokens = addTokenUsageAtDepth(aSub, bSub, depth+1)
	if depth == 0 {
		sum.SubagentTokensComplete = tokenCompleteness(a, b)
	}
	return sum
}

func tokenCompleteness(a, b *TokenUsage) *bool {
	seenComplete := false
	seenUnknown := false
	for _, usage := range []*TokenUsage{a, b} {
		if usage == nil {
			continue
		}
		if usage.SubagentTokensComplete == nil {
			seenUnknown = true
			continue
		}
		if !*usage.SubagentTokensComplete {
			incomplete := false
			return &incomplete
		}
		seenComplete = true
	}
	if seenUnknown {
		return nil
	}
	if seenComplete {
		complete := true
		return &complete
	}
	return nil
}

// SubtractTokenUsage returns a-b, recursing into subagent usage and clamping
// every field at zero (a nil operand is treated as zero). Neither input is
// mutated. Used to rescope a cumulative-since-session-start snapshot (e.g.
// subagent token usage, which is always re-read from the start of each
// subagent transcript) down to a delta since a previously captured baseline.
func SubtractTokenUsage(a, b *TokenUsage) *TokenUsage {
	return subtractTokenUsageAtDepth(a, b, 0)
}

func subtractTokenUsageAtDepth(a, b *TokenUsage, depth int) *TokenUsage {
	if a == nil {
		return nil
	}
	if b == nil {
		b = &TokenUsage{}
	}
	diff := subtractCounts(a, b)
	if models, ok := subtractModels(modelBreakdown(a), modelBreakdown(b)); ok {
		diff.setModels(models)
		if diff.Model == "" && len(diff.ByModel) == 0 {
			// Nothing left to attribute: keep a's label on the empty delta.
			diff.Model = a.Model
		}
	}
	diff.SubagentTokens = subtractTokenUsageAtDepth(a.SubagentTokens, b.SubagentTokens, depth+1)
	if depth == 0 && a.SubagentTokensComplete != nil {
		complete := *a.SubagentTokensComplete
		diff.SubagentTokensComplete = &complete
	}
	return diff
}

// clampSubtract returns a-b, floored at zero so a stale or racy baseline
// never produces a negative delta.
func clampSubtract(a, b int) int {
	if a < b {
		return 0
	}
	return a - b
}

// LabelModel returns usage with model recorded as the model of all its
// tokens, for usage whose source names its model only out of band (a hook
// payload, a session settings file). Usage that already records its own model
// or split, a nil usage, or an empty model is returned unchanged; the input is
// never mutated.
func LabelModel(usage *TokenUsage, model string) *TokenUsage {
	if usage == nil || model == "" || usage.Model != "" || len(usage.ByModel) > 0 {
		return usage
	}
	labelled := *usage
	labelled.Model = model
	return &labelled
}

// subtractCounts returns a-b for the count fields only, clamping each at zero.
func subtractCounts(a, b *TokenUsage) *TokenUsage {
	return &TokenUsage{
		InputTokens:         clampSubtract(a.InputTokens, b.InputTokens),
		CacheCreationTokens: clampSubtract(a.CacheCreationTokens, b.CacheCreationTokens),
		CacheReadTokens:     clampSubtract(a.CacheReadTokens, b.CacheReadTokens),
		OutputTokens:        clampSubtract(a.OutputTokens, b.OutputTokens),
		APICallCount:        clampSubtract(a.APICallCount, b.APICallCount),
	}
}

// modelBreakdown returns u's per-model counts: ByModel when set, otherwise u's
// own counts under u.Model. Nested fields are dropped so breakdown entries
// stay flat whatever the input carried. Usage is read back from pushed blobs,
// so a split that cannot be trusted (see validSplit) is reduced to u's own
// counts under no model rather than letting it misattribute tokens.
func modelBreakdown(u *TokenUsage) []TokenUsage {
	if u == nil {
		return nil
	}
	if len(u.ByModel) == 0 {
		return []TokenUsage{countsOnly(u, u.Model)}
	}
	if !validSplit(u) {
		return []TokenUsage{countsOnly(u, "")}
	}
	out := make([]TokenUsage, 0, len(u.ByModel))
	for i := range u.ByModel {
		out = append(out, countsOnly(&u.ByModel[i], u.ByModel[i].Model))
	}
	return out
}

// validSplit reports whether u.ByModel is a split this package could have
// written: no Model beside it, at most MaxModelsPerUsage entries, and
// non-negative counts that sum exactly to u's. Remaining counts are checked
// per entry, so no sum can overflow.
func validSplit(u *TokenUsage) bool {
	if u.Model != "" || len(u.ByModel) > MaxModelsPerUsage {
		return false
	}
	remaining := countsOnly(u, "")
	for i := range u.ByModel {
		e := &u.ByModel[i]
		for _, f := range [][2]*int{
			{&remaining.InputTokens, &e.InputTokens},
			{&remaining.CacheCreationTokens, &e.CacheCreationTokens},
			{&remaining.CacheReadTokens, &e.CacheReadTokens},
			{&remaining.OutputTokens, &e.OutputTokens},
			{&remaining.APICallCount, &e.APICallCount},
		} {
			left, spent := f[0], *f[1]
			if spent < 0 || spent > *left {
				return false
			}
			*left -= spent
		}
	}
	return !remaining.hasCounts()
}

func countsOnly(u *TokenUsage, model string) TokenUsage {
	return TokenUsage{
		InputTokens:         u.InputTokens,
		CacheCreationTokens: u.CacheCreationTokens,
		CacheReadTokens:     u.CacheReadTokens,
		OutputTokens:        u.OutputTokens,
		APICallCount:        u.APICallCount,
		Model:               model,
	}
}

// setModels records entries (possibly repeating a model) on u: one model sets
// Model, several set ByModel (merged per model, sorted) and clear Model. An
// entry with no counts spent nothing, so it cannot make a sum mixed; it only
// supplies the label when nothing was spent at all.
func (u *TokenUsage) setModels(entries []TokenUsage) {
	merged := make(map[string]*TokenUsage, len(entries))
	label := ""
	for i := range entries {
		e := &entries[i]
		if !e.hasCounts() {
			if label == "" {
				label = e.Model
			}
			continue
		}
		if m, ok := merged[e.Model]; ok {
			m.InputTokens += e.InputTokens
			m.CacheCreationTokens += e.CacheCreationTokens
			m.CacheReadTokens += e.CacheReadTokens
			m.OutputTokens += e.OutputTokens
			m.APICallCount += e.APICallCount
			continue
		}
		c := countsOnly(e, e.Model)
		merged[e.Model] = &c
	}
	u.Model, u.ByModel = "", nil
	switch len(merged) {
	case 0:
		u.Model = label
		return
	case 1:
		for model := range merged {
			u.Model = model
		}
		return
	}
	u.ByModel = make([]TokenUsage, 0, len(merged))
	for _, m := range merged {
		u.ByModel = append(u.ByModel, *m)
	}
	slices.SortFunc(u.ByModel, func(x, y TokenUsage) int { return strings.Compare(x.Model, y.Model) })
}

// subtractModels returns a-b per model, dropping models with nothing left.
// When a model shrank (b holds more of it than a), the excess is taken from
// a's remaining models in order, so the result sums to the clamped total
// difference rather than overstating it. When b spent tokens on a model a
// does not have (an unrecorded "" baseline written before models were
// recorded, or a child whose transcript went away), nothing says which of a's
// models the difference belongs to, so ok is false and the caller leaves the
// delta unlabelled.
func subtractModels(a, b []TokenUsage) (diff []TokenUsage, ok bool) {
	byModel := make(map[string]TokenUsage, len(b))
	for _, e := range b {
		byModel[e.Model] = sumCounts(byModel[e.Model], e)
	}
	matched := make(map[string]bool, len(a))
	for _, e := range a {
		matched[e.Model] = true
	}
	for model, e := range byModel {
		if !matched[model] && e.hasCounts() {
			return nil, false
		}
	}
	out := make([]TokenUsage, 0, len(a))
	var excess TokenUsage
	for _, e := range a {
		base := byModel[e.Model]
		delete(byModel, e.Model) // a repeated model takes its baseline once
		d := *subtractCounts(&e, &base)
		excess = sumCounts(excess, *subtractCounts(&base, &e))
		d.Model = e.Model
		out = append(out, d)
	}
	kept := out[:0]
	for _, d := range out {
		d = drain(d, &excess)
		if d.hasCounts() {
			kept = append(kept, d)
		}
	}
	return kept, true
}

// drain takes as much of excess from d as d holds, field by field.
func drain(d TokenUsage, excess *TokenUsage) TokenUsage {
	take := func(have, owed *int) {
		n := min(*have, *owed)
		*have -= n
		*owed -= n
	}
	take(&d.InputTokens, &excess.InputTokens)
	take(&d.CacheCreationTokens, &excess.CacheCreationTokens)
	take(&d.CacheReadTokens, &excess.CacheReadTokens)
	take(&d.OutputTokens, &excess.OutputTokens)
	take(&d.APICallCount, &excess.APICallCount)
	return d
}

func sumCounts(a, b TokenUsage) TokenUsage {
	a.InputTokens += b.InputTokens
	a.CacheCreationTokens += b.CacheCreationTokens
	a.CacheReadTokens += b.CacheReadTokens
	a.OutputTokens += b.OutputTokens
	a.APICallCount += b.APICallCount
	return a
}

func (u *TokenUsage) hasCounts() bool {
	return u.InputTokens != 0 || u.CacheCreationTokens != 0 || u.CacheReadTokens != 0 ||
		u.OutputTokens != 0 || u.APICallCount != 0
}

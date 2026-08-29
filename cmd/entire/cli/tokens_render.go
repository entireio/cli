package cli

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/entireio/cli/cmd/entire/cli/tokenreport"
)

// tokenReportView is the body of a token report that does not depend on
// where the report came from — a committed checkpoint or a live session. The
// shared writers in this file render it (breakdown, usage table,
// recommendations, notes, agent brief) and the shared --json fields are
// derived from it, so `checkpoint tokens` and `session tokens` print the
// same shapes from the same values.
type tokenReportView struct {
	// Report carries Agent, Profile, Model, Effort, Usage, Cost, Attributed,
	// Duration and Calls in exactly the values tokenreport.Recommend read, so
	// every figure a recommendation quotes is printed from the same value.
	Report tokenreport.Report
	// HasUsage is false when nothing recorded any usage: no attributed call,
	// no committed token_usage. The usage table then prints "not recorded".
	HasUsage bool
	// EffortCalls is how many calls carried Report.Effort; 0 when no call
	// recorded an effort.
	EffortCalls int
	// Subagent is the subagent part of Report.Usage, flattened; zero when no
	// subagent usage is known.
	Subagent types.TokenUsage
	// Attributed is true when Report.Attributed was computed from a
	// transcript. False means the breakdown is unavailable, not empty.
	Attributed bool
	// UnknownUsageCalls counts calls whose usage the agent did not record;
	// they are in Report.Attributed with zero tokens and not in Calls.
	UnknownUsageCalls int
	// Recommendations are tokenreport.Recommend's sentences for Report.
	Recommendations []tokenreport.Recommendation
	// AgentReportedCost is the provider-computed dollar cost the agent
	// recorded (Pi, OpenCode); 0 when not recorded.
	AgentReportedCost float64
	// Legacy is non-nil for a checkpoint written before token_usage_version 2.
	Legacy *tokenLegacyInfo
	// Limitations are the caller's Notes lines: attribution failures,
	// unreadable metadata, unmatched subagent records. tokenReportNotes adds
	// the pricing and profile notes derived from Report.
	Limitations []string
}

// tokenLegacyInfo describes a legacy checkpoint's token scope, for the JSON
// `legacy` object and the header label.
type tokenLegacyInfo struct {
	// Cumulative is true when the checkpoint's token_usage may be the
	// session's running total rather than this checkpoint's delta
	// (tokenreport.ScopeLegacyFromStart).
	Cumulative bool `json:"cumulative"`
	// ThinkingRecorded is true when the committed usage carries a thinking
	// token count.
	ThinkingRecorded bool `json:"thinking_recorded"`
	// CacheTTLRecorded is true when the committed usage carries the 1-hour
	// cache-write split.
	CacheTTLRecorded bool `json:"cache_ttl_recorded"`
}

// tokenUsageJSON is the `tokens` object of a token report's --json output.
type tokenUsageJSON struct {
	Total                 int `json:"total"`
	Input                 int `json:"input"`
	CacheRead             int `json:"cache_read"`
	CacheWrite            int `json:"cache_write"`
	Output                int `json:"output"`
	APICalls              int `json:"api_calls"`
	SubagentTotal         int `json:"subagent_total,omitempty"`
	ThinkingTokens        int `json:"thinking_tokens,omitempty"`
	CacheCreation1hTokens int `json:"cache_creation_1h_tokens,omitempty"`
}

// tokenCostJSON is the `cost` object of a token report's --json output.
type tokenCostJSON struct {
	Provider tokenreport.Provider `json:"provider,omitempty"`
	Family   tokenreport.Family   `json:"family,omitempty"`
	// Weights are the price ratios the shares were computed with; omitted
	// when the report mixes model families or nothing was priced.
	Weights *tokenreport.Weights   `json:"weights,omitempty"`
	Shares  tokenreport.CostShares `json:"shares"`
}

// tokenEffortJSON is the `effort` object of a token report's --json output.
type tokenEffortJSON struct {
	Value string `json:"value"`
	Calls int    `json:"calls"`
}

// tokenRecommendationJSON is one entry of `recommendations` in --json output.
// ID and Message duplicate Cause and Text under the previous schema's keys so
// existing consumers keep working; only that schema's severity and signals
// are gone.
type tokenRecommendationJSON struct {
	Kind    tokenreport.RecommendationKind `json:"kind"`
	Text    string                         `json:"text"`
	Cause   tokenreport.Cause              `json:"cause"`
	Cited   []tokenreport.Citation         `json:"cited,omitempty"`
	Memory  string                         `json:"memory,omitempty"`
	Seen    int                            `json:"seen,omitempty"`
	Of      int                            `json:"of,omitempty"`
	ID      string                         `json:"id"`
	Message string                         `json:"message"`
}

// Display strings shared by the token report writers.
const (
	tokenTableShareHeader   = "est. cost share" //nolint:gosec // G101: a table header, not a credential
	tokenTableUnpriced      = "—"
	tokenNotRecorded        = "not recorded"
	tokenUsageNotRecorded   = "(usage not recorded)" //nolint:gosec // G101: a table note, not a credential
	tokenLabelContextReplay = tokenreport.LabelContextReplay + " (cache read)"
	tokenLabelCacheWrite    = "Cache write"
	tokenRecommendationWrap = 78
	tokenDetailMaxRunes     = 40
)

// tokenVolume is the four token classes summed — the size of a usage
// regardless of price; a nil u is 0.
func tokenVolume(u *types.TokenUsage) int {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens + u.OutputTokens
}

// tokenUsageJSONFor builds the `tokens` object from a view; nil when the view
// has no usage.
func tokenUsageJSONFor(v *tokenReportView) *tokenUsageJSON {
	if !v.HasUsage {
		return nil
	}
	u := &v.Report.Usage
	return &tokenUsageJSON{
		Total:                 tokenVolume(u),
		Input:                 u.InputTokens,
		CacheRead:             u.CacheReadTokens,
		CacheWrite:            u.CacheCreationTokens,
		Output:                u.OutputTokens,
		APICalls:              u.APICallCount,
		SubagentTotal:         tokenVolume(&v.Subagent),
		ThinkingTokens:        u.ThinkingTokens,
		CacheCreation1hTokens: u.CacheCreation1hTokens,
	}
}

// tokenCostJSONFor builds the `cost` object from a view; nil when nothing was
// priced.
func tokenCostJSONFor(v *tokenReportView) *tokenCostJSON {
	if v.Report.Cost.Units <= 0 {
		return nil
	}
	out := &tokenCostJSON{Provider: v.Report.Cost.Provider, Family: v.Report.Cost.Family, Shares: v.Report.Cost}
	if w, ok := tokenCostWeights(v); ok {
		out.Weights = &w
	}
	return out
}

// tokenEffortJSONFor builds the `effort` object; nil when no effort was seen
// or the agent's profile does not record one.
func tokenEffortJSONFor(v *tokenReportView) *tokenEffortJSON {
	if !tokenEffortShown(v) {
		return nil
	}
	return &tokenEffortJSON{Value: v.Report.Effort, Calls: v.EffortCalls}
}

// tokenRecommendationsJSONFor converts the view's recommendations.
func tokenRecommendationsJSONFor(recs []tokenreport.Recommendation) []tokenRecommendationJSON {
	if len(recs) == 0 {
		return nil
	}
	out := make([]tokenRecommendationJSON, 0, len(recs))
	for _, r := range recs {
		out = append(out, tokenRecommendationJSON{
			Kind: r.Kind, Text: r.Text, Cause: r.Cause, Cited: r.Cited,
			Memory: r.Memory, Seen: r.Seen, Of: r.Of,
			ID: string(r.Cause), Message: r.Text,
		})
	}
	return out
}

// tokenEffortShown reports whether the header prints an Effort entry: the
// agent's profile records effort and at least one call carried one.
func tokenEffortShown(v *tokenReportView) bool {
	return v.Report.Profile.RecordsEffort && v.Report.Effort != ""
}

// tokenCostWeights returns the single set of price ratios the view's cost
// shares were computed with: the report model's ratios when they belong to
// the family the shares name. False when nothing was priced, the report
// mixes families, or the parent model is unknown.
func tokenCostWeights(v *tokenReportView) (tokenreport.Weights, bool) {
	if v.Report.Cost.Units <= 0 || v.Report.Cost.Family == "" {
		return tokenreport.Weights{}, false
	}
	w, f, ok := tokenreport.WeightsFor(v.Report.Model)
	if !ok || f != v.Report.Cost.Family {
		return tokenreport.Weights{}, false
	}
	return w, true
}

// tokenDurationLine is the "Duration:" header value: the duration, the call
// count and the total volume joined with " · ", each part saying "not
// recorded" when it is.
func tokenDurationLine(v *tokenReportView) string {
	duration := tokenNotRecorded
	if v.Report.Duration > 0 {
		duration = tokenreport.FormatDuration(v.Report.Duration)
	}
	if !v.HasUsage {
		return duration + " · token usage " + tokenNotRecorded
	}
	calls := v.Report.Calls
	if calls == 0 {
		calls = v.Report.Usage.APICallCount
	}
	return duration + " · " + formatAPICalls(calls) + " · " + tokenreport.FormatTokenCount(tokenVolume(&v.Report.Usage)) + " tokens"
}

// tokenEffortHeader is the "Effort: high (43 calls)" header entry, or "".
func tokenEffortHeader(v *tokenReportView) string {
	if !tokenEffortShown(v) {
		return ""
	}
	return fmt.Sprintf("Effort: %s (%s)", v.Report.Effort, formatCallCount(v.EffortCalls))
}

// formatCallCount renders "1 call" / "N calls".
func formatCallCount(n int) string {
	if n == 1 {
		return "1 call"
	}
	return strconv.Itoa(n) + " calls"
}

// writeTokenReportBody prints the shared body: the breakdown (when
// attributed), the usage table, the recommendations and the notes. The
// caller prints the header first.
func writeTokenReportBody(w io.Writer, v *tokenReportView) {
	if v.Attributed {
		writeTokenWhereItWent(w, v)
	}
	writeTokenUsageTable(w, v)
	writeTokenRecommendationSentences(w, v.Recommendations)
	writeTokenNotes(w, tokenReportNotes(v))
}

// tokenTableLine is one printed line of the breakdown or usage table.
type tokenTableLine struct {
	indent int
	label  string
	calls  string
	tokens string
	share  string
	note   string
}

// writeTokenTable prints a two-column-plus-share table: the header, a dim
// rule, then each line with the label left-aligned, calls and tokens
// right-aligned and the share right-aligned under the share header.
func writeTokenTable(w io.Writer, title string, lines []tokenTableLine) {
	labelWidth := utf8.RuneCountInString(title)
	callsWidth, tokensWidth := 0, len("tokens")
	for _, l := range lines {
		labelWidth = max(labelWidth, l.indent+utf8.RuneCountInString(l.label))
		callsWidth = max(callsWidth, len(l.calls))
		tokensWidth = max(tokensWidth, len(l.tokens))
	}
	sty := newStatusStyles(w)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s  %s  %s\n", tokenPadRight(title, labelWidth+callsWidth), tokenPadLeft("tokens", tokensWidth), tokenTableShareHeader)
	fmt.Fprintln(w, sty.render(sty.dim, strings.Repeat("─", labelWidth+callsWidth+tokensWidth+len(tokenTableShareHeader)+4)))
	for _, l := range lines {
		label := strings.Repeat(" ", l.indent) + l.label
		line := tokenPadRight(label, labelWidth) + "  " + tokenPadLeft(l.calls, callsWidth) + "  " + tokenPadLeft(l.tokens, tokensWidth) + "  " + tokenPadLeft(l.share, 4)
		if l.note != "" {
			line += "   " + l.note
		}
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
}

// padRight pads s with spaces to width runes.
func tokenPadRight(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// padLeft left-pads s with spaces to width runes.
func tokenPadLeft(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return strings.Repeat(" ", width-n) + s
	}
	return s
}

// writeTokenWhereItWent prints the contributor table. The top
// tokenreport.MaxRenderedRows rows are printed in table order, details under
// the top tokenreport.MaxRenderedDetails of them; every row (and detail) a
// recommendation cites is printed too, after the top block, so each quoted
// figure is on the page. Rows beyond that are counted in the
// "(N smaller items omitted)" line.
func writeTokenWhereItWent(w io.Writer, v *tokenReportView) {
	rows, omitted := selectContributorRows(&v.Report.Attributed, v.Recommendations)
	priced := v.Report.Attributed.PricedUnits > 0
	var lines []tokenTableLine
	for _, r := range rows {
		lines = append(lines, contributorLine(r.contributor, priced))
		for _, d := range r.details {
			lines = append(lines, tokenTableLine{
				indent: 4,
				label:  stringutil.TruncateRunes(d.Detail, tokenDetailMaxRunes, "…"),
				calls:  formatCallCount(d.Calls),
				tokens: tokenreport.FormatTokenCount(d.Tokens),
				share:  tokenShare(d.CostShare, priced),
			})
		}
	}
	if len(lines) == 0 {
		return
	}
	writeTokenTable(w, "Where it went", lines)
	if omitted > 0 {
		fmt.Fprintf(w, "  (%d smaller item%s omitted)\n", omitted, tokenPluralSuffix(omitted))
	}
}

// renderedContributor is one contributor chosen for printing with the
// details to print beneath it.
type renderedContributor struct {
	contributor *tokenreport.Contributor
	details     []tokenreport.Detail
}

// selectContributorRows applies the cutoffs and citations described on
// writeTokenWhereItWent and returns the rows to print, in print order, and
// how many were left out.
func selectContributorRows(a *tokenreport.Attributed, recs []tokenreport.Recommendation) ([]renderedContributor, int) {
	var out, cited []renderedContributor
	for i := range a.Contributors {
		c := &a.Contributors[i]
		rowCited, citedDetails := citationsFor(c, recs)
		switch {
		case i < tokenreport.MaxRenderedRows && i < tokenreport.MaxRenderedDetails:
			out = append(out, renderedContributor{contributor: c, details: c.Details})
		case i < tokenreport.MaxRenderedRows:
			out = append(out, renderedContributor{contributor: c, details: citedDetails})
		case rowCited:
			cited = append(cited, renderedContributor{contributor: c, details: citedDetails})
		}
	}
	out = append(out, cited...)
	return out, len(a.Contributors) - len(out)
}

// citationsFor reports whether any recommendation cites row c and returns the
// details of c those citations name, in c.Details order.
func citationsFor(c *tokenreport.Contributor, recs []tokenreport.Recommendation) (bool, []tokenreport.Detail) {
	cited := false
	named := make(map[string]bool)
	for _, r := range recs {
		for _, ct := range r.Cited {
			if ct.Kind != c.Kind || ct.Label != c.Label || ct.Skill != c.Skill {
				continue
			}
			cited = true
			if ct.Detail != "" {
				named[ct.Detail] = true
			}
		}
	}
	var details []tokenreport.Detail
	for _, d := range c.Details {
		if named[d.Detail] {
			details = append(details, d)
		}
	}
	return cited, details
}

// contributorLine renders one contributor row: the label by kind (see
// contributorLabel), its four-class volume, its share, and a
// "(usage not recorded)" note for a ref-driven row (tool, skill, subagent)
// with nothing attributed — the agent recorded no usage for its calls.
func contributorLine(c *tokenreport.Contributor, priced bool) tokenTableLine {
	volume := tokenVolume(&c.Usage)
	line := tokenTableLine{indent: 2, label: contributorLabel(c), tokens: tokenreport.FormatTokenCount(volume), share: tokenShare(c.CostShare, priced)}
	if volume == 0 && c.Usage.APICallCount == 0 && isRefDrivenKind(c.Kind) {
		line.note = tokenUsageNotRecorded
	}
	return line
}

// isRefDrivenKind reports whether rows of kind exist because a tool call
// named them (and so can legitimately carry zero tokens).
func isRefDrivenKind(kind tokenreport.ContributorKind) bool {
	switch kind {
	case tokenreport.KindTool, tokenreport.KindSkill, tokenreport.KindSubagent:
		return true
	case tokenreport.KindText, tokenreport.KindReplay, tokenreport.KindPrompt:
		return false
	}
	return false
}

// contributorLabel is a row's display label: "Skill: <name> (loaded)",
// "Subagent: <type>", "Context replay (cache read)", the tool name or
// LabelAssistantText — the last two suffixed " · during <skill>" when the
// row carries a Skill annotation.
func contributorLabel(c *tokenreport.Contributor) string {
	var label string
	switch c.Kind {
	case tokenreport.KindSkill:
		return "Skill: " + c.Label + " (loaded)"
	case tokenreport.KindSubagent:
		return "Subagent: " + c.Label
	case tokenreport.KindReplay:
		return tokenLabelContextReplay
	case tokenreport.KindPrompt:
		return c.Label
	case tokenreport.KindTool, tokenreport.KindText:
		label = c.Label
	}
	if c.Skill != "" {
		label += " · during " + c.Skill
	}
	return label
}

// tokenShare renders a share for the share column: FormatPercent when the
// table is priced, "—" otherwise.
func tokenShare(share float64, priced bool) string {
	if !priced {
		return tokenTableUnpriced
	}
	return tokenreport.FormatPercent(share)
}

// writeTokenUsageTable prints the four token classes with their cost shares
// and price ratios, the thinking and subagent subset rows, and the total.
// Every value comes from Report.Usage and Report.Cost, the values
// tokenreport.Recommend quotes.
func writeTokenUsageTable(w io.Writer, v *tokenReportView) {
	if !v.HasUsage {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Usage")
		fmt.Fprintln(w, "  Token usage: "+tokenNotRecorded)
		return
	}
	u := &v.Report.Usage
	cs := &v.Report.Cost
	priced := cs.Units > 0
	weights, haveWeights := tokenCostWeights(v)
	ratio := func(r float64) string {
		if !haveWeights {
			return ""
		}
		return "(" + strconv.FormatFloat(r, 'f', -1, 64) + "×)"
	}
	lines := []tokenTableLine{
		{indent: 2, label: "Input (fresh)", tokens: tokenreport.FormatTokenCount(u.InputTokens), share: tokenShare(cs.Input, priced)},
	}
	lines = append(lines, cacheWriteLines(v, weights, haveWeights)...)
	lines = append(lines,
		tokenTableLine{indent: 2, label: "Cache read", tokens: tokenreport.FormatTokenCount(u.CacheReadTokens), share: tokenShare(cs.CacheRead, priced), note: ratio(weights.CacheRead)},
		tokenTableLine{indent: 2, label: "Output", tokens: tokenreport.FormatTokenCount(u.OutputTokens), share: tokenShare(cs.Output, priced), note: ratio(weights.Output)},
		thinkingLine(v),
		tokenTableLine{indent: 2, label: "Total", tokens: tokenreport.FormatTokenCount(tokenVolume(u))},
	)
	if sub := tokenVolume(&v.Subagent); sub > 0 {
		lines = append(lines, tokenTableLine{indent: 4, label: "of which subagents", tokens: tokenreport.FormatTokenCount(sub)})
	}
	writeTokenTable(w, "Usage", lines)
}

// cacheWriteLines renders the cache-write row by what the usage recorded:
// "Cache write, 1-hour" when every write carried the 1-hour TTL, "Cache
// write, 5-minute" when the agent records TTLs and none did, plain "Cache
// write" (with an "of which 1-hour" sub-row when mixed) otherwise. The share
// is "—" when the TTL is unknown and the provider prices the two TTLs
// differently (Cost.CacheWriteUnpriced).
func cacheWriteLines(v *tokenReportView, weights tokenreport.Weights, haveWeights bool) []tokenTableLine {
	u := &v.Report.Usage
	cs := &v.Report.Cost
	priced := cs.Units > 0 && !cs.CacheWriteUnpriced
	line := tokenTableLine{indent: 2, label: tokenLabelCacheWrite, tokens: tokenreport.FormatTokenCount(u.CacheCreationTokens), share: tokenShare(cs.CacheWrite, priced)}
	ratio := func(r float64) string {
		if !haveWeights || r == 0 {
			return ""
		}
		return "(" + strconv.FormatFloat(r, 'f', -1, 64) + "×)"
	}
	switch {
	case u.CacheCreationTokens == 0:
		return []tokenTableLine{line}
	case u.CacheCreation1hTokens == u.CacheCreationTokens:
		line.label += ", 1-hour"
		line.note = ratio(weights.CacheWrite1h)
		return []tokenTableLine{line}
	case u.CacheCreation1hTokens == 0:
		if v.Report.Profile.RecordsCacheTTL && haveWeights && weights.CacheWrite5m != weights.CacheWrite1h && !cs.CacheWriteUnpriced {
			line.label += ", 5-minute"
		}
		if !cs.CacheWriteUnpriced {
			line.note = ratio(weights.CacheWrite5m)
		}
		return []tokenTableLine{line}
	default:
		return []tokenTableLine{line, {indent: 4, label: "of which 1-hour", tokens: tokenreport.FormatTokenCount(u.CacheCreation1hTokens), note: ratio(weights.CacheWrite1h)}}
	}
}

// thinkingLine renders "of which thinking" with the thinking share of cost
// and of output, or "not recorded" when the agent does not record thinking.
func thinkingLine(v *tokenReportView) tokenTableLine {
	u := &v.Report.Usage
	line := tokenTableLine{indent: 4, label: "of which thinking"}
	if !v.Report.Profile.RecordsThinking && u.ThinkingTokens == 0 {
		line.tokens = tokenNotRecorded
		return line
	}
	line.tokens = tokenreport.FormatTokenCount(u.ThinkingTokens)
	line.share = tokenShare(v.Report.Cost.Thinking, v.Report.Cost.Units > 0)
	if u.OutputTokens > 0 {
		line.note = tokenreport.FormatPercent(float64(u.ThinkingTokens)/float64(u.OutputTokens)) + " of output"
	}
	return line
}

// writeTokenRecommendationSentences prints the Recommendations section: each
// sentence wrapped at tokenRecommendationWrap columns and indented two
// spaces. Nothing is printed when no recommendation fired.
func writeTokenRecommendationSentences(w io.Writer, recs []tokenreport.Recommendation) {
	if len(recs) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Recommendations")
	for i, r := range recs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		for _, line := range strings.Split(wrapText(r.Text, tokenRecommendationWrap-2), "\n") {
			fmt.Fprintln(w, "  "+line)
		}
	}
}

// writeTokenNotes prints the Notes section; nothing when there are none.
func writeTokenNotes(w io.Writer, notes []string) {
	if len(notes) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Notes")
	for _, n := range notes {
		fmt.Fprintf(w, "  - %s\n", n)
	}
}

// tokenReportNotes is the full Notes list for a view (also the JSON
// `limitations`): the price-ratio note, one line per unpriced model, the
// unknown-TTL line, the agent's totals-only caveat, the calls-without-usage
// count, then the caller's Limitations in order.
func tokenReportNotes(v *tokenReportView) []string {
	var notes []string
	if v.HasUsage {
		notes = append(notes, tokenPricingNotes(v)...)
		if profileNote := tokenProfileNote(v); profileNote != "" {
			notes = append(notes, profileNote)
		}
	}
	if v.UnknownUsageCalls > 0 {
		notes = append(notes, formatCallCount(v.UnknownUsageCalls)+" with no usage recorded")
	}
	return append(notes, v.Limitations...)
}

// tokenPricingNotes explains how the shares were priced: the ratio table
// used, the models that could not be priced, and an unpriced cache-write TTL.
func tokenPricingNotes(v *tokenReportView) []string {
	var notes []string
	cs := &v.Report.Cost
	if cs.Units > 0 {
		if w, ok := tokenCostWeights(v); ok {
			notes = append(notes, "Cost shares use "+tokenProviderName(w.Provider)+" list-price ratios ("+tokenRatioList(w)+"), not your plan's rates.")
		} else {
			notes = append(notes, "Cost shares mix list-price ratios from more than one model family, not your plan's rates.")
		}
	}
	unpriced := v.Report.Attributed.Unpriced
	if cs.Units == 0 && v.Report.Model != "" {
		if _, _, ok := tokenreport.WeightsFor(v.Report.Model); !ok && !slices.Contains(unpriced, v.Report.Model) {
			unpriced = append(append([]string(nil), unpriced...), v.Report.Model)
		}
	}
	for _, m := range unpriced {
		notes = append(notes, "no verified price ratios for `"+m+"`; its tokens count toward volume only")
	}
	if cs.CacheWriteUnpriced {
		notes = append(notes, "cache-write TTL not recorded; not priced")
	}
	return notes
}

// tokenRatioList renders a Weights row as "input 1×, 5m write 1.25×, 1h
// write 2×, cache read 0.1×, output 5×"; a zero cache-write ratio is
// rendered as "no cache-write charge".
func tokenRatioList(w tokenreport.Weights) string {
	f := func(r float64) string { return strconv.FormatFloat(r, 'f', -1, 64) + "×" }
	parts := []string{"input " + f(w.Input)}
	switch {
	case w.CacheWrite5m == 0 && w.CacheWrite1h == 0:
		parts = append(parts, "no cache-write charge")
	case w.CacheWrite5m == w.CacheWrite1h:
		parts = append(parts, "cache write "+f(w.CacheWrite5m))
	default:
		parts = append(parts, "5m write "+f(w.CacheWrite5m), "1h write "+f(w.CacheWrite1h))
	}
	return strings.Join(append(parts, "cache read "+f(w.CacheRead), "output "+f(w.Output)), ", ")
}

// tokenProviderName is a Provider's display name.
func tokenProviderName(p tokenreport.Provider) string {
	switch p {
	case tokenreport.ProviderAnthropic:
		return "Anthropic"
	case tokenreport.ProviderOpenAI:
		return "OpenAI"
	case tokenreport.ProviderGoogle:
		return "Google"
	}
	return string(p)
}

// tokenProfileNote is the caveat for an agent whose transcript records only
// session totals: the breakdown is not available and, for an agent with no
// verified profile, not verified either. "" for agents with a breakdown.
func tokenProfileNote(v *tokenReportView) string {
	p := v.Report.Profile
	if !p.TotalsOnly {
		return ""
	}
	agentName := string(v.Report.Agent)
	if agentName == "" {
		agentName = unknownPlaceholder
	}
	if p.Verified {
		return agentName + " records session totals only; the per-call breakdown is not verified for this agent."
	}
	return "no verified capability profile for " + agentName + "; totals shown, breakdown not verified."
}

// writeTokenAgentBrief prints the compact next-step view both commands offer
// with --agent-brief: the identity line, a one-line usage summary with the
// duration, the top recommendation's text as the next best action, and one
// Signals line per fired cause.
func writeTokenAgentBrief(w io.Writer, title, idLabel, id string, v *tokenReportView) {
	fmt.Fprintln(w, title)
	fmt.Fprintf(w, "%s: %s\n", idLabel, id)
	fmt.Fprintln(w)
	fmt.Fprintln(w, tokenAgentBriefUsageLine(v))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Next best action:")
	fmt.Fprintln(w, tokenAgentBriefNextAction(v))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Signals:")
	if !v.HasUsage {
		fmt.Fprintln(w, "- token usage "+tokenNotRecorded)
		return
	}
	if len(v.Recommendations) == 0 {
		fmt.Fprintln(w, "- none: no token recommendation fired")
		return
	}
	for _, r := range v.Recommendations {
		fmt.Fprintf(w, "- %s\n", r.Cause)
	}
}

// tokenAgentBriefUsageLine is the brief's usage summary: total volume, call
// count, duration and the cache-read share of cost (of volume when unpriced).
func tokenAgentBriefUsageLine(v *tokenReportView) string {
	if !v.HasUsage {
		return "Token usage: unavailable."
	}
	u := &v.Report.Usage
	parts := []string{tokenreport.FormatTokenCount(tokenVolume(u)) + " total", formatAPICalls(v.Report.Calls)}
	if v.Report.Duration > 0 {
		parts = append(parts, tokenreport.FormatDuration(v.Report.Duration))
	} else {
		parts = append(parts, "duration "+tokenNotRecorded)
	}
	switch {
	case u.CacheReadTokens > 0 && v.Report.Cost.Units > 0:
		parts = append(parts, "cache read "+tokenreport.FormatPercent(v.Report.Cost.CacheRead)+" of cost")
	case u.CacheReadTokens > 0:
		parts = append(parts, "cache read "+tokenreport.FormatPercent(float64(u.CacheReadTokens)/float64(tokenVolume(u)))+" of volume")
	}
	return "Token usage: " + strings.Join(parts, "; ") + "."
}

// tokenAgentBriefNextAction is the brief's next best action: the top
// recommendation's text, or a "continue" line when none fired.
func tokenAgentBriefNextAction(v *tokenReportView) string {
	if !v.HasUsage {
		return "Token usage is not recorded here; continue with the task and recheck once a newer checkpoint captures usage."
	}
	if len(v.Recommendations) > 0 {
		return v.Recommendations[0].Text
	}
	return "Continue normally; no token recommendation fired for this report."
}

// tokenDurationSeconds is a duration in whole seconds for JSON; 0 when
// unrecorded.
func tokenDurationSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(d / time.Second)
}

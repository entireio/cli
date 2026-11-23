package review

import (
	"encoding/json"
	"strings"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// trailFindingsJSONInstructions is the trail-mode output contract. Reviewers
// and the judge end with this JSON line so posting reads their findings
// structurally instead of interpreting prose. The example line must parse
// with splitTrailJSON; a test enforces that.
const trailFindingsJSONInstructions = `Final line (required, read by a program):
End your output with exactly one line holding only this JSON object, with no code fence and nothing after it:
{"summary":"one-sentence verdict","comments":[{"severity":"high","body":"bug; impact; fix","location":{"granularity":"line","file_path":"path/to/file.go","start_line":42}}]}
- Add one comment per actionable finding reported above, with the same severity: high, medium, or low.
- location.granularity is line (with start_line), range (with start_line and end_line), file, or whole_change.
- If nothing is actionable, use an empty array: "comments":[].
- This line is required even where other instructions say to output nothing else.`

// maxTrailJSONLines bounds the backward scan for a pretty-printed JSON block.
const maxTrailJSONLines = 200

// TrailComment is one finding in the trail JSON contract.
type TrailComment struct {
	Severity   string        `json:"severity"`
	Confidence *float64      `json:"confidence,omitempty"`
	Body       string        `json:"body"`
	Location   TrailLocation `json:"location"`
}

// TrailLocation anchors a TrailComment to the change.
type TrailLocation struct {
	Granularity string `json:"granularity"`
	FilePath    string `json:"file_path,omitempty"`
	StartLine   int    `json:"start_line,omitempty"`
	EndLine     int    `json:"end_line,omitempty"`
}

// TrailReport is what a trail-mode run posts: structured comments from
// reviews that honored the JSON contract, and the prose, with any contract
// line removed, of those that did not. At least one of the two is set.
type TrailReport struct {
	Comments []TrailComment
	Prose    string
	// Scope is what the findings may anchor to; see TrailScope.
	Scope TrailScope
	// HeadSHA is the commit the review ran on.
	HeadSHA string
}

// trailReportFor decides what a trail-mode run posts. It returns post=false
// only when the JSON contract reports no findings and nothing in the prose
// contradicts that; every malformed or contradictory case posts, so a
// misbehaving model costs an extra finding rather than a lost one. A posted
// report is empty only when the output had nothing readable. When a judge
// ran, its verdict decides: it is asked to drop unsupported claims.
func trailReportFor(summary reviewtypes.RunSummary, aggregateOutput string) (TrailReport, bool) {
	if verdict := strings.TrimSpace(aggregateOutput); verdict != "" {
		prose, comments, ok := splitTrailJSON(verdict)
		if !contractCoversProse(prose, comments, ok) {
			if prose == "" {
				// A JSON-only reply without a summary has no prose of its own.
				prose = combinedReviewNarratives(usableAgentRuns(summary))
			}
			return TrailReport{Prose: prose}, true
		}
		return TrailReport{Comments: comments}, len(comments) > 0
	}
	var merged []TrailComment
	var unstructured []reviewtypes.AgentRun
	for _, run := range usableAgentRuns(summary) {
		earlier, final := reportParts(run.Buffer)
		prose, comments, ok := splitTrailJSON(final)
		if !contractCoversProse(prose, comments, ok) || earlierReportsFindings(earlier, comments) {
			unstructured = append(unstructured, run)
			continue
		}
		merged = append(merged, comments...)
	}
	report := TrailReport{Comments: merged, Prose: combinedReviewNarratives(unstructured)}
	return report, len(merged) > 0 || len(unstructured) > 0
}

// trailReportOutcome is what a finished run's report holds for a trail post.
type trailReportOutcome int

const (
	// reportToPost holds findings or prose to post.
	reportToPost trailReportOutcome = iota
	// reportNothingToReport is a review that found nothing to report.
	reportNothingToReport
	// reportNoOutput is a review whose reviewers and judge left no output.
	reportNoOutput
	// reportUnreadable is output in which no report could be read.
	reportUnreadable
)

// readTrailReport is trailReportFor with the reason a report posts
// nothing: there is nothing to report, the review left no output, or none
// of its output could be read.
func readTrailReport(summary reviewtypes.RunSummary, aggregateOutput string) (TrailReport, trailReportOutcome) {
	report, post := trailReportFor(summary, aggregateOutput)
	switch {
	case !post && strings.TrimSpace(aggregateOutput) == "" && len(usableAgentRuns(summary)) == 0:
		return report, reportNoOutput
	case !post:
		return report, reportNothingToReport
	case len(report.Comments) == 0 && strings.TrimSpace(report.Prose) == "":
		return report, reportUnreadable
	}
	return report, reportToPost
}

// readLocalReport is readTrailReport for a run that did not ask for the
// trail JSON line, such as a local profile's. Without the line, a trailing
// JSON block is the reviewer's own text, so the report is prose: the
// judge's verdict when one ran, otherwise each reviewer's final report
// (finalReportText). Nothing says such a review is clean, so a report that
// is a clean verdict (cleanVerdict) is nothing to report, and one
// reviewer's clean verdict is left out of the others' findings.
func readLocalReport(summary reviewtypes.RunSummary, aggregateOutput string) (TrailReport, trailReportOutcome) {
	if verdict := strings.TrimSpace(aggregateOutput); verdict != "" {
		if cleanVerdict(verdict) {
			return TrailReport{}, reportNothingToReport
		}
		return TrailReport{Prose: verdict}, reportToPost
	}
	runs := usableAgentRuns(summary)
	if len(runs) == 0 {
		return TrailReport{}, reportNoOutput
	}
	var b strings.Builder
	for _, run := range runs {
		text := finalReportText(run.Buffer)
		if cleanVerdict(text) {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("## " + run.Name + "\n\n" + text)
	}
	if b.Len() == 0 {
		return TrailReport{}, reportNothingToReport
	}
	return TrailReport{Prose: b.String()}, reportToPost
}

// finalReportText is a reviewer's report in a run without the trail JSON
// line. The output format asks a report to open with a verdict line, so
// when the text after the last tool call does, that text is the report, and
// exploration narration such as "I'll check:\n- retry.go" written before it
// is left out. Otherwise, or when the earlier text has a severity marker or
// a feedback verdict, which narration does not, all of the text is the
// report, so findings written before a last tool call are kept.
func finalReportText(buf []reviewtypes.Event) string {
	for i := len(buf) - 1; i >= 0; i-- {
		if !isTextBoundary(buf, i) {
			continue
		}
		final := joinAssistantText(buf[i+1:])
		if final == "" {
			continue
		}
		if opensWithVerdict(final) && !reportsFindings(joinAssistantText(buf[:i]), false) {
			return final
		}
		break
	}
	return joinAssistantText(buf)
}

// opensWithVerdict reports whether report's first line is a verdict the
// output format asks for: approve, approve with nits, or request changes.
func opensWithVerdict(report string) bool {
	first, _, _ := strings.Cut(strings.TrimSpace(report), "\n")
	verdict := verdictText(first)
	return strings.HasPrefix(verdict, "approve") || strings.HasPrefix(verdict, "request changes") ||
		strings.HasPrefix(verdict, "request_changes")
}

// cleanVerdict reports whether a report without the trail JSON line says
// the change is clean: it is only an approve verdict line, which is what the
// output format asks a clean review to write, and the verdict is not
// "approve with nits" or another one that says there is feedback. Anything
// else, such as an approval followed by a finding written as a plain
// paragraph, is a finding.
func cleanVerdict(report string) bool {
	first, rest, _ := strings.Cut(strings.TrimSpace(report), "\n")
	if strings.TrimSpace(rest) != "" {
		return false
	}
	verdict := verdictText(first)
	return strings.HasPrefix(verdict, "approve") && !proseReportsFindings(verdict)
}

// verdictEmphasis removes the Markdown emphasis a verdict line may carry
// anywhere in it, as in "Verdict: **approve** with nits".
var verdictEmphasis = strings.NewReplacer("**", "", "__", "", "*", "", "`", "")

// verdictText is line in lower case without Markdown emphasis, a heading or
// quote marker, or a "Verdict:" label.
func verdictText(line string) string {
	text := strings.ToLower(verdictEmphasis.Replace(line))
	text = strings.TrimLeft(text, "#>_ ")
	if after, found := strings.CutPrefix(text, "verdict"); found {
		text = strings.TrimLeft(after, ":-—– ")
	}
	return text
}

// contractCoversProse reports whether a valid contract can be posted in place
// of its prose: the prose reports no finding the comments leave out. The
// prose normally repeats every comment, so it is posted instead of, not
// alongside, an incomplete contract.
func contractCoversProse(prose string, comments []TrailComment, ok bool) bool {
	if !ok {
		return false
	}
	if len(comments) == 0 {
		return !proseReportsFindings(prose)
	}
	return markedFindingCount(prose) <= len(comments)
}

// earlierReportsFindings reports whether text borrowed from before a
// JSON-only final message contradicts its contract. That text is often
// exploration narration, whose bullets ("I'll check:\n- retry.go") are not
// findings, so only severity markers and a feedback verdict count.
func earlierReportsFindings(earlier string, comments []TrailComment) bool {
	if len(comments) == 0 {
		return reportsFindings(earlier, false)
	}
	return markedFindingCount(earlier) > len(comments)
}

// markedFindingCount counts the lines, list items included, that open with a
// severity marker: the finding shape the output format asks for.
func markedFindingCount(prose string) int {
	count := 0
	for _, line := range strings.Split(prose, "\n") {
		trimmed := strings.TrimSpace(line)
		if item, ok := ListItemText(trimmed); ok {
			trimmed = item
		}
		if StartsWithSeverityMarker(trimmed) {
			count++
		}
	}
	return count
}

// reportText returns a reviewer's report as one text: see reportParts.
func reportText(buf []reviewtypes.Event) string {
	earlier, final := reportParts(buf)
	if earlier == "" {
		return final
	}
	return earlier + "\n\n" + final
}

// reportParts finds a reviewer's report: final is the last stretch of text
// between tool calls that ends with a valid contract line, or "" if none
// does. It is found by the contract line rather than by position, so a tool
// call after the report does not hide it, and earlier exploration narration,
// whose bullets ("I'll check:\n- retry.go") are not findings, is left out. A
// tool call inside a message, where the next text continues it as a delta,
// is not a boundary. When final holds only the contract line, earlier is the
// stretch before it, which may hold a report written before a last tool call
// or may be narration.
func reportParts(buf []reviewtypes.Event) (earlier, final string) {
	end := len(buf)
	for i := len(buf) - 1; i >= -1; i-- {
		if i >= 0 && !isTextBoundary(buf, i) {
			continue
		}
		text := joinAssistantText(buf[i+1 : end])
		prose, payload, found := splitTrailJSONPayload(text)
		if _, ok := decodeTrailComments(payload["comments"]); !found || !ok {
			end = i
			continue
		}
		if prose == "" && i >= 0 {
			start := i - 1
			for start >= 0 && !isTextBoundary(buf, start) {
				start--
			}
			if before := joinAssistantText(buf[start+1 : i]); strings.TrimSpace(before) != "" {
				return before, text
			}
		}
		return "", text
	}
	return "", ""
}

// isTextBoundary reports whether buf[i] is a tool call that separates two
// stretches of text rather than interrupting a message.
func isTextBoundary(buf []reviewtypes.Event, i int) bool {
	_, ok := buf[i].(reviewtypes.ToolCall)
	return ok && !continuesMessage(buf[i+1:])
}

// continuesMessage reports whether the first text in buf continues the
// message before it.
func continuesMessage(buf []reviewtypes.Event) bool {
	for _, ev := range buf {
		if at, ok := ev.(reviewtypes.AssistantText); ok {
			return at.Delta
		}
	}
	return false
}

// splitTrailJSON separates a trailing contract-shaped JSON object (one line,
// a fenced block, or a pretty-printed block, with a "summary" or "comments"
// key) from the prose before it, so a malformed contract is never posted as
// finding text. Other trailing JSON, such as a config snippet quoted as
// evidence, stays in the prose. A JSON-only reply contributes its summary as
// the prose instead. ok reports a valid contract: a "comments" array.
// Entries without a body are dropped.
func splitTrailJSON(text string) (prose string, comments []TrailComment, ok bool) {
	prose, payload, found := splitTrailJSONPayload(text)
	if !found {
		return prose, nil, false
	}
	if prose == "" {
		var summary string
		if json.Unmarshal(payload["summary"], &summary) == nil {
			prose = strings.TrimSpace(summary)
		}
	}
	comments, ok = decodeTrailComments(payload["comments"])
	return prose, comments, ok
}

// splitTrailJSONPayload finds a trailing contract-shaped JSON object and
// returns the prose before it and its decoded keys. found is false, with
// the whole text as prose, when there is none.
func splitTrailJSONPayload(text string) (prose string, payload map[string]json.RawMessage, found bool) {
	text = strings.TrimSpace(text)
	lines := strings.Split(text, "\n")
	end := len(lines)
	fenced := end > 1 && strings.TrimSpace(lines[end-1]) == "```"
	if fenced {
		end--
	}
	for start := end - 1; start >= 0 && start >= end-maxTrailJSONLines; start-- {
		if !strings.HasPrefix(strings.TrimSpace(lines[start]), "{") {
			continue
		}
		payload = nil
		if err := json.Unmarshal([]byte(strings.Join(lines[start:end], "\n")), &payload); err != nil {
			continue
		}
		_, hasSummary := payload["summary"]
		_, hasComments := payload["comments"]
		if !hasSummary && !hasComments {
			break
		}
		proseEnd := start
		if fenced && proseEnd > 0 && strings.HasPrefix(strings.TrimSpace(lines[proseEnd-1]), "```") {
			proseEnd--
		}
		return strings.TrimSpace(strings.Join(lines[:proseEnd], "\n")), payload, true
	}
	return text, nil, false
}

func decodeTrailComments(raw json.RawMessage) ([]TrailComment, bool) {
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		return nil, false
	}
	var decoded []TrailComment
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, false
	}
	comments := []TrailComment{}
	for _, comment := range decoded {
		if strings.TrimSpace(comment.Body) != "" {
			comments = append(comments, comment)
		}
	}
	return comments, true
}

// stripTrailJSON removes a valid trailing contract line for display. Only a
// run that asked for the line has one; elsewhere a trailing JSON block is the
// reviewer's own text and stays.
func stripTrailJSON(text string, trailFindingsJSON bool) string {
	if !trailFindingsJSON {
		return text
	}
	if prose, _, ok := splitTrailJSON(text); ok {
		return prose
	}
	return text
}

// proseReportsFindings reports whether prose contradicts an empty comments
// array: a list item, severity marker, or feedback label such as "Nits:" at
// any indentation, or a verdict, labeled or not, that itself says there is
// feedback. These are the shapes the output format asks for, so a clean
// review has none of them.
func proseReportsFindings(prose string) bool {
	return reportsFindings(prose, true)
}

// reportsFindings reports whether prose holds a finding signal. Severity
// markers and feedback verdicts always count; plain list items and feedback
// labels count only when includeWeak is set.
func reportsFindings(prose string, includeWeak bool) bool {
	for _, line := range strings.Split(prose, "\n") {
		trimmed := strings.TrimSpace(line)
		if item, ok := ListItemText(trimmed); ok {
			if includeWeak {
				return true
			}
			trimmed = item
		}
		lower := strings.ToLower(trimMarkdownEmphasis(trimmed))
		if after, found := strings.CutPrefix(lower, "verdict"); found {
			lower = trimMarkdownEmphasis(strings.TrimLeft(after, ":-—– "))
		}
		if StartsWithSeverityMarker(lower) {
			return true
		}
		prefixes := []string{
			"request changes", "request_changes", "changes requested", "approve with nits",
			"needs changes", "needs work", "changes needed",
		}
		if includeWeak {
			prefixes = append(prefixes, "nits:", "issues:", "concerns:", "suggestions:", "findings:", "problems:", "blocking:")
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(lower, prefix) {
				return true
			}
		}
	}
	return false
}

func trimMarkdownEmphasis(s string) string {
	return strings.TrimLeft(s, "#>*_` ")
}

// StartsWithSeverityMarker reports whether line, after leading Markdown
// emphasis, opens with a severity tag: bracketed ("[high]"), delimited
// ("high —", "p2:"), closed by emphasis ("**High** ..."), or alone (a "### High"
// heading). A word that merely starts with a marker ("Highlights") is not one.
func StartsWithSeverityMarker(line string) bool {
	lower := strings.ToLower(trimMarkdownEmphasis(strings.TrimSpace(line)))
	for _, marker := range []string{"critical", "high", "medium", "low", "p0", "p1", "p2", "p3", "nit"} {
		if strings.HasPrefix(lower, "["+marker+"]") {
			return true
		}
		rest, found := strings.CutPrefix(lower, marker)
		if !found {
			continue
		}
		if strings.TrimSpace(rest) == "" {
			return true
		}
		for _, suffix := range []string{" —", " –", " -", ":", "*", "_"} {
			if strings.HasPrefix(rest, suffix) {
				return true
			}
		}
	}
	return false
}

// ListItemText returns the text of a Markdown list item ("- ", "* ", "+ ",
// "• ", "1. ", "1) ") and whether trimmed, a line without surrounding space,
// is one.
func ListItemText(trimmed string) (string, bool) {
	for _, bullet := range []string{"- ", "* ", "+ ", "• "} {
		if item, found := strings.CutPrefix(trimmed, bullet); found {
			return strings.TrimSpace(item), true
		}
	}
	digits := len(trimmed) - len(strings.TrimLeft(trimmed, "0123456789"))
	if digits == 0 {
		return "", false
	}
	for _, delim := range []string{". ", ") "} {
		if item, found := strings.CutPrefix(trimmed[digits:], delim); found {
			return strings.TrimSpace(item), true
		}
	}
	return "", false
}

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
	Granularity  string `json:"granularity"`
	FilePath     string `json:"file_path,omitempty"`
	StartLine    int    `json:"start_line,omitempty"`
	EndLine      int    `json:"end_line,omitempty"`
	SelectedText string `json:"selected_text,omitempty"`
}

// TrailReport is what a trail-mode run posts: structured comments from
// reviews that honored the JSON contract, and the prose, with any contract
// line removed, of those that did not. At least one of the two is set.
type TrailReport struct {
	Comments []TrailComment
	Prose    string
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

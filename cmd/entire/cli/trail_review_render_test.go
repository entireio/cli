package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
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

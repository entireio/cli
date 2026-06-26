package investigate

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRenderFindingsHTML_RendersFindingsBody(t *testing.T) {
	t.Parallel()

	m := LocalManifest{
		RunID:           "abcdef012345",
		Topic:           "why is the build slow",
		StartedAt:       time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		FindingsContent: "## Findings\n\nThe answer is npm install.\n",
	}

	out, err := RenderFindingsHTML(m)
	if err != nil {
		t.Fatalf("RenderFindingsHTML: %v", err)
	}

	if !strings.Contains(out, "<!doctype html>") && !strings.Contains(out, "<!DOCTYPE html>") {
		t.Errorf("expected a complete HTML document, got: %q", out)
	}
	// Self-contained: no external (CDN / network) references.
	if strings.Contains(out, "http://") || strings.Contains(out, "https://") {
		t.Errorf("expected no external references, got a URL in: %q", out)
	}
	// Prompt is the page title.
	if !strings.Contains(out, "why is the build slow") {
		t.Errorf("expected the prompt as the page title, got: %q", out)
	}
	if !strings.Contains(out, "<h2") {
		t.Errorf("expected markdown heading rendered to <h2>, got: %q", out)
	}
	if !strings.Contains(out, "The answer is npm install.") {
		t.Errorf("expected findings prose, got: %q", out)
	}
}

func TestRenderFindingsHTML_OmitsAgentMetadata(t *testing.T) {
	t.Parallel()

	m := LocalManifest{
		RunID:           "abcdef012345",
		Topic:           "pipeline question",
		Agents:          []string{"claude-code", "codex"},
		Outcome:         "quorum",
		StancesByAgent:  map[string]string{"claude-code": "agree", "codex": "disagree"},
		StartedAt:       time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		EndedAt:         time.Date(2026, 6, 23, 9, 10, 0, 0, time.UTC),
		FindingsContent: "## Result\n\nA clean explanation with no agent names.\n",
	}

	out, err := RenderFindingsHTML(m)
	if err != nil {
		t.Fatalf("RenderFindingsHTML: %v", err)
	}

	for _, banned := range []string{"claude-code", "codex", "quorum", "disagree", "Last stance"} {
		if strings.Contains(out, banned) {
			t.Errorf("expected no agent/outcome metadata, but found %q in: %q", banned, out)
		}
	}
}

func TestRenderFindingsHTML_BuildsTOCWithHeadingLinks(t *testing.T) {
	t.Parallel()

	m := LocalManifest{
		RunID:           "abcdef012345",
		Topic:           "build hang",
		StartedAt:       time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		FindingsContent: "## Background\n\nbody\n\n## Evidence\n\nmore\n",
	}

	out, err := RenderFindingsHTML(m)
	if err != nil {
		t.Fatalf("RenderFindingsHTML: %v", err)
	}

	// Headings get anchor ids and the TOC links to them.
	if !strings.Contains(out, `id="background"`) || !strings.Contains(out, `id="evidence"`) {
		t.Errorf("expected slugged heading ids, got: %q", out)
	}
	if !strings.Contains(out, `href="#background"`) || !strings.Contains(out, `href="#evidence"`) {
		t.Errorf("expected TOC links to headings, got: %q", out)
	}
}

func TestRenderFindingsHTML_IsInteractive(t *testing.T) {
	t.Parallel()

	m := LocalManifest{
		RunID:           "abcdef012345",
		Topic:           "interactivity",
		StartedAt:       time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		FindingsContent: "## Section\n\nbody\n",
	}

	out, err := RenderFindingsHTML(m)
	if err != nil {
		t.Fatalf("RenderFindingsHTML: %v", err)
	}

	// Inline behavior — no external scripts.
	if !strings.Contains(out, "<script>") {
		t.Errorf("expected inline interactivity, got no <script>: %q", out)
	}
	// In-page search control.
	if !strings.Contains(out, `type="search"`) {
		t.Errorf("expected a search input, got: %q", out)
	}
	// Theme toggle control.
	if !strings.Contains(out, "data-theme-toggle") {
		t.Errorf("expected a theme toggle control, got: %q", out)
	}
}

func TestRenderFindingsHTML_RendersRenderableMermaidAsDiagramPre(t *testing.T) {
	t.Parallel()

	body := "## Flow\n\n```mermaid\nflowchart LR\n  A[Producer] --> B[Consumer]\n```\n"
	m := LocalManifest{
		RunID:           "abcdef012345",
		Topic:           "pipeline",
		StartedAt:       time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		FindingsContent: body,
	}

	out, err := RenderFindingsHTML(m)
	if err != nil {
		t.Fatalf("RenderFindingsHTML: %v", err)
	}

	if !strings.Contains(out, `<pre class="diagram">`) {
		t.Errorf("expected a diagram <pre> block, got: %q", out)
	}
	if !strings.Contains(out, "Producer") || !strings.Contains(out, "Consumer") {
		t.Errorf("expected diagram to contain node labels, got: %q", out)
	}
	if strings.Contains(out, "flowchart LR") {
		t.Errorf("expected mermaid source to be replaced by the diagram, got: %q", out)
	}
}

func TestRenderFindingsHTML_EscapesHTML(t *testing.T) {
	t.Parallel()

	m := LocalManifest{
		RunID:           "abcdef012345",
		Topic:           "<script>alert('xss')</script>",
		StartedAt:       time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		FindingsContent: "Inline danger: <img src=x onerror=alert(1)>\n",
	}

	out, err := RenderFindingsHTML(m)
	if err != nil {
		t.Fatalf("RenderFindingsHTML: %v", err)
	}

	if strings.Contains(out, "<script>alert") {
		t.Errorf("topic was not escaped, got: %q", out)
	}
	if strings.Contains(out, "<img src=x onerror") {
		t.Errorf("raw HTML in findings was not neutralized, got: %q", out)
	}
}

func TestRenderFindingsHTML_HeroShowsTitleNotTLDR(t *testing.T) {
	t.Parallel()

	m := LocalManifest{
		RunID:           "abcdef012345",
		Topic:           "Why does the build hang?",
		StartedAt:       time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		FindingsContent: "## Summary\n\nThe root cause is an orphaned cache lock.\n\n## Detail\n\nmore\n",
	}

	out, err := RenderFindingsHTML(m)
	if err != nil {
		t.Fatalf("RenderFindingsHTML: %v", err)
	}
	// The hero shows the prompt as the title.
	if !strings.Contains(out, `class="intro"`) || !strings.Contains(out, "Why does the build hang?") {
		t.Errorf("expected the prompt in the hero, got: %q", out)
	}
	// The TLDR/Summary section is removed entirely — not shown as a standfirst.
	if strings.Contains(out, "The root cause is an orphaned cache lock.") {
		t.Errorf("TLDR should be removed, but found it in: %q", out)
	}
	// Other sections still render.
	if !strings.Contains(out, `id="detail"`) {
		t.Errorf("expected the Detail section, got: %q", out)
	}
}

func TestRenderFindingsHTML_ListsReferencedFiles(t *testing.T) {
	t.Parallel()

	body := "## Cause\n\nThe bug is in `internal/cache/lock.go:42`, `internal/cache/lock.go:90`, and also `pkg/install/run.go`.\n"
	m := LocalManifest{
		RunID:           "abcdef012345",
		Topic:           "files",
		StartedAt:       time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		FindingsContent: body,
	}

	out, err := RenderFindingsHTML(m)
	if err != nil {
		t.Fatalf("RenderFindingsHTML: %v", err)
	}
	if !strings.Contains(out, `id="referenced-files"`) {
		t.Errorf("expected a referenced-files section, got: %q", out)
	}
	// Refs are grouped by file: the path appears once, its lines listed beside it.
	if !strings.Contains(out, `class="file-path">internal/cache/lock.go</span>`) {
		t.Errorf("expected grouped file path, got: %q", out)
	}
	if !strings.Contains(out, `class="file-lines">42, 90</span>`) {
		t.Errorf("expected grouped line list, got: %q", out)
	}
	if strings.Count(out, `class="file-path">internal/cache/lock.go</span>`) != 1 {
		t.Errorf("grouped file path should appear once in the files list, got: %q", out)
	}
	if !strings.Contains(out, "pkg/install/run.go") {
		t.Errorf("expected second file ref, got: %q", out)
	}
	// The files section sits at the very bottom (after all parsed sections).
	if idx := strings.Index(out, `id="referenced-files"`); idx != -1 {
		if cause := strings.Index(out, `id="cause"`); cause == -1 || cause > idx {
			t.Errorf("referenced-files should come after content sections")
		}
	}
}

func TestRenderFindingsHTML_RendersCallouts(t *testing.T) {
	t.Parallel()

	body := "## Notes\n\n> [!WARNING]\n> Do not interrupt the install.\n\n**Fix:** wrap the install in a trap.\n"
	m := LocalManifest{
		RunID:           "abcdef012345",
		Topic:           "callouts",
		StartedAt:       time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		FindingsContent: body,
	}

	out, err := RenderFindingsHTML(m)
	if err != nil {
		t.Fatalf("RenderFindingsHTML: %v", err)
	}
	if !strings.Contains(out, "callout-warning") {
		t.Errorf("expected a warning callout from the GitHub alert, got: %q", out)
	}
	if !strings.Contains(out, "callout-success") {
		t.Errorf("expected a success callout from the **Fix:** lead-in, got: %q", out)
	}
}

func TestRenderFindingsHTML_ShowsReadingStats(t *testing.T) {
	t.Parallel()

	m := LocalManifest{
		RunID:           "abcdef012345",
		Topic:           "stats",
		StartedAt:       time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		FindingsContent: "## A\n\nword word word\n\n## B\n\nmore\n",
	}

	out, err := RenderFindingsHTML(m)
	if err != nil {
		t.Fatalf("RenderFindingsHTML: %v", err)
	}
	if !strings.Contains(out, "min read") {
		t.Errorf("expected a reading-time stat, got: %q", out)
	}
	if !strings.Contains(out, "2 sections") {
		t.Errorf("expected a section count, got: %q", out)
	}
}

func TestRenderFindingsHTML_HasReadingAids(t *testing.T) {
	t.Parallel()

	m := LocalManifest{
		RunID:           "abcdef012345",
		Topic:           "aids",
		StartedAt:       time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		FindingsContent: "## Section\n\nbody\n",
	}

	out, err := RenderFindingsHTML(m)
	if err != nil {
		t.Fatalf("RenderFindingsHTML: %v", err)
	}
	if !strings.Contains(out, "data-print") {
		t.Errorf("expected a print/PDF control, got: %q", out)
	}
	if !strings.Contains(out, "window.print") {
		t.Errorf("expected print wiring, got: %q", out)
	}
	if !strings.Contains(out, `id="to-top"`) {
		t.Errorf("expected a back-to-top control, got: %q", out)
	}
	if !strings.Contains(out, "@media print") {
		t.Errorf("expected a print stylesheet, got: %q", out)
	}
}

func TestOrderedFindingsMarkdown(t *testing.T) {
	t.Parallel()

	body := "## TLDR\n\nthe gist\n\n## Question\n\n<untrusted source=\"x\">\nissue text\n</untrusted>\n\n" +
		"## Prior work\n\nsearched\n\n## Approach\n\nread code\n\n## Findings\n\nbug in `pkg/run.go:10`\n\n## Conclusion\n\nship it\n"

	out := orderedFindingsMarkdown(body)

	// TLDR and Question (raw issue) are dropped.
	if strings.Contains(out, "the gist") || strings.Contains(out, "issue text") || strings.Contains(out, "## Question") {
		t.Errorf("TLDR/Question should be dropped, got:\n%s", out)
	}
	// Sections appear in HTML order: Conclusion, Findings, Prior work, Approach.
	order := []string{"## Conclusion", "## Findings", "## Prior work", "## Approach", "## Referenced files"}
	last := -1
	for _, want := range order {
		at := strings.Index(out, want)
		if at == -1 {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
		if at < last {
			t.Errorf("%q out of order in:\n%s", want, out)
		}
		last = at
	}
	// Referenced files list present.
	if !strings.Contains(out, "`pkg/run.go` 10") {
		t.Errorf("expected grouped referenced file, got:\n%s", out)
	}
}

func TestOrderedFindingsMarkdown_NoSectionsUnchanged(t *testing.T) {
	t.Parallel()

	body := "just some freeform notes\n\nno headings here\n"
	if got := orderedFindingsMarkdown(body); got != body {
		t.Errorf("freeform findings should pass through unchanged, got: %q", got)
	}
}

func TestGroupFileRefs(t *testing.T) {
	t.Parallel()

	got := groupFileRefs([]string{
		"handler.go:418", "handler.go:418-420",
		"internal/handler/handler.go:856", "handler.go:120",
		"client.go:10", "pkg/run.go",
	})

	// handler.go (bare) merges into internal/handler/handler.go (unique basename);
	// client.go stays bare (no matching full path). Sorted by path, lines by start.
	want := []fileRef{
		{path: "client.go", lines: []string{"10"}},
		{path: "internal/handler/handler.go", lines: []string{"120", "418", "418-420", "856"}},
		{path: "pkg/run.go", lines: nil},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d groups, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].path != want[i].path {
			t.Errorf("group %d path = %q, want %q", i, got[i].path, want[i].path)
		}
		if strings.Join(got[i].lines, ",") != strings.Join(want[i].lines, ",") {
			t.Errorf("group %d (%s) lines = %v, want %v", i, got[i].path, got[i].lines, want[i].lines)
		}
	}
}

func TestRenderFindingsHTML_OrdersProcessSectionsLast(t *testing.T) {
	t.Parallel()

	// Authored in template order; Approach and Prior work must sink below
	// Findings and Conclusion in the rendered output.
	m := LocalManifest{
		RunID:     "abcdef012345",
		Topic:     "ordering",
		StartedAt: time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		FindingsContent: "## Prior work\n\nsearched X\n\n## Approach\n\nread Y\n\n" +
			"## Findings\n\nthe bug is real\n\n## Conclusion\n\nship the fix\n",
	}

	out, err := RenderFindingsHTML(m)
	if err != nil {
		t.Fatalf("RenderFindingsHTML: %v", err)
	}
	order := []string{`id="conclusion"`, `id="findings"`, `id="prior-work"`, `id="approach"`}
	last := -1
	for _, want := range order {
		at := strings.Index(out, want)
		if at == -1 {
			t.Fatalf("missing section %s in: %q", want, out)
		}
		if at < last {
			t.Errorf("section %s out of order (expected conclusion, findings, prior-work, approach)", want)
		}
		last = at
	}
}

func TestRenderFindingsHTML_DropsEmptyTemplateSections(t *testing.T) {
	t.Parallel()

	// A section containing only an authoring comment must not render.
	m := LocalManifest{
		RunID:     "abcdef012345",
		Topic:     "empties",
		StartedAt: time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		FindingsContent: "## Approach\n\n<!-- how the investigation was run -->\n\n" +
			"## Findings\n\nthe real finding\n",
	}

	out, err := RenderFindingsHTML(m)
	if err != nil {
		t.Fatalf("RenderFindingsHTML: %v", err)
	}
	if strings.Contains(out, `id="approach"`) {
		t.Errorf("empty Approach section should be dropped, got: %q", out)
	}
	if !strings.Contains(out, `id="findings"`) {
		t.Errorf("non-empty Findings section should render, got: %q", out)
	}
}

func TestRenderFindingsHTML_RemovesQuestionSection(t *testing.T) {
	t.Parallel()

	// The Question section holds the raw issue body (with its own headings
	// inside <untrusted>). The whole section — including that issue content —
	// must be dropped.
	m := LocalManifest{
		RunID:     "abcdef012345",
		Topic:     "untrusted",
		StartedAt: time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		FindingsContent: "## Question\n\n<untrusted source=\"issue-body\">\n" +
			"## What happened?\n\nit broke\n\n## Steps to reproduce\n\nrun it\n</untrusted>\n\n" +
			"## Findings\n\nthe real finding\n",
	}

	out, err := RenderFindingsHTML(m)
	if err != nil {
		t.Fatalf("RenderFindingsHTML: %v", err)
	}
	if strings.Contains(out, `id="question"`) {
		t.Errorf("Question section should be removed, got: %q", out)
	}
	if strings.Contains(out, "it broke") {
		t.Errorf("issue content should be gone with the Question section, got: %q", out)
	}
	// Other sections still render.
	if !strings.Contains(out, `id="findings"`) || !strings.Contains(out, "the real finding") {
		t.Errorf("expected the Findings section to survive, got: %q", out)
	}
}

func TestRenderFindingsHTML_TildeFenceIsNotASection(t *testing.T) {
	t.Parallel()

	// A `##`-looking line inside a ~~~ code fence (valid CommonMark) must not
	// be treated as a section boundary — it is code, not a heading.
	m := LocalManifest{
		RunID:     "abcdef012345",
		Topic:     "tilde fences",
		StartedAt: time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		FindingsContent: "## Findings\n\nExample config:\n\n~~~\n" +
			"## not-a-real-heading\nkey = value\n~~~\n\nReal prose after the fence.\n",
	}

	out, err := RenderFindingsHTML(m)
	if err != nil {
		t.Fatalf("RenderFindingsHTML: %v", err)
	}
	if strings.Contains(out, `id="not-a-real-heading"`) {
		t.Errorf("a ## line inside a ~~~ fence must not become a section, got: %q", out)
	}
	if !strings.Contains(out, `id="findings"`) {
		t.Errorf("expected the real Findings section, got: %q", out)
	}
	if !strings.Contains(out, "Real prose after the fence.") {
		t.Errorf("expected post-fence prose to render inside Findings, got: %q", out)
	}
}

func TestRenderFindingsHTML_EmptyBodyReturnsSentinel(t *testing.T) {
	t.Parallel()

	m := LocalManifest{
		RunID:       "abcdef012345",
		Topic:       "lost run",
		StartedAt:   time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC),
		FindingsDoc: "/tmp/this/does/not/exist.md",
	}

	_, err := RenderFindingsHTML(m)
	if !errors.Is(err, errNoFindingsContent) {
		t.Errorf("expected errNoFindingsContent for empty body, got: %v", err)
	}
}

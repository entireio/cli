package investigate

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/yuin/goldmark"

	"github.com/entireio/cli/cmd/entire/cli/investigate/flowchart"
)

// errNoFindingsContent signals that a manifest has no findings body to render —
// neither embedded content nor a readable on-disk findings file. Callers map
// this to the same soft "No findings content available" notice the terminal
// `show` path emits, rather than treating it as a hard failure.
var errNoFindingsContent = errors.New("investigate: no findings content")

// wordsPerMinute is the assumed reading speed for the reading-time estimate.
const wordsPerMinute = 200

// pageDoc is the data assembled into the findings HTML page.
type pageDoc struct {
	Title    string
	Intro    string // pre-rendered hero HTML
	Content  string // pre-rendered section HTML
	TOC      []tocEntry
	Minutes  int
	Sections int
}

// RenderFindingsHTML builds a self-contained, interactive HTML document from a
// saved investigation's findings.
//
// Findings follow a fixed template (TLDR, Question, Prior work, Approach,
// Findings, Conclusion, …). The renderer is template-aware: it splits the body
// on the canonical section headings, folds the raw issue body and its noise
// headings into a single section, drops sections that are empty (only authoring
// comments), drops the TLDR and Question sections entirely (the hero shows the
// prompt as the title, not the TLDR), and reorders so the useful findings come
// first and process/context sections (Approach, Prior work, the original issue)
// sink to the bottom. Findings with no recognizable sections fall back to a
// straight markdown render.
//
// Markdown is converted with goldmark in default (safe) mode — raw HTML in
// agent-authored findings is NOT passed through. Renderable Mermaid flowcharts
// become Unicode box-outlines in <pre>. The page shows findings only (no agent
// / outcome / stance metadata) and references no external assets. An empty body
// returns errNoFindingsContent.
func RenderFindingsHTML(m LocalManifest) (string, error) {
	body := findingsBody(m)
	if body == "" {
		return "", errNoFindingsContent
	}

	// parseSections also strips the TLDR and Question sections; the TLDR is
	// intentionally not surfaced (no hero standfirst).
	secs := parseSections(body)

	content, toc, err := renderSections(secs)
	if err != nil {
		return "", err
	}
	// Fallback: no recognizable sections → straight render of the whole body.
	if content == "" {
		generic, gerr := renderFindingsContent(body)
		if gerr != nil {
			return "", gerr
		}
		generic, toc = injectHeadingIDs(applyCallouts(generic))
		content = generic
	}

	// Referenced files are their own collapsible section at the very bottom.
	if files := extractFileRefs(body); len(files) > 0 {
		fhtml, fentry := buildFilesSection(files)
		content += fhtml
		toc = append(toc, fentry)
	}

	doc := pageDoc{
		Title:    m.Topic,
		Intro:    buildIntro(m.Topic),
		Content:  content,
		TOC:      toc,
		Minutes:  readingMinutes(body),
		Sections: len(toc),
	}
	return assembleHTMLDoc(doc), nil
}

// buildFilesSection renders the referenced-files list as a collapsible section
// for the bottom of the page, plus its table-of-contents entry. References are
// grouped by file so each file appears once with its line numbers beside it.
func buildFilesSection(refs []string) (string, tocEntry) {
	groups := groupFileRefs(refs)
	var b strings.Builder
	b.WriteString(`<section class="sec"><h2 class="sec-h" id="referenced-files">Referenced files</h2><div class="sec-body"><ul class="file-list">`)
	for _, g := range groups {
		fmt.Fprintf(&b, `<li><span class="file-path">%s</span>`, html.EscapeString(g.path))
		if len(g.lines) > 0 {
			fmt.Fprintf(&b, ` <span class="file-lines">%s</span>`, html.EscapeString(strings.Join(g.lines, ", ")))
		}
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul></div></section>`)
	return b.String(), tocEntry{Level: 2, Text: "Referenced files", ID: "referenced-files"}
}

// fileRef is one file path with the distinct line specs referenced in it.
type fileRef struct {
	path  string
	lines []string
}

var lineSpecRe = regexp.MustCompile(`^\d+(-\d+)?$`)

// groupFileRefs collapses raw `path:line` reference tokens into one entry per
// file, with sorted unique line specs. A bare filename (no slash) is merged
// into a full path when exactly one referenced path shares its basename, so
// `handler.go` and `internal/handler/handler.go` become a single row.
func groupFileRefs(refs []string) []fileRef {
	lines := map[string]map[string]struct{}{}
	add := func(p, line string) {
		if lines[p] == nil {
			lines[p] = map[string]struct{}{}
		}
		if line != "" {
			lines[p][line] = struct{}{}
		}
	}
	for _, r := range refs {
		p, line := r, ""
		if i := strings.LastIndex(r, ":"); i >= 0 && lineSpecRe.MatchString(r[i+1:]) {
			p, line = r[:i], r[i+1:]
		}
		add(p, line)
	}

	for bare := range lines {
		if strings.Contains(bare, "/") {
			continue
		}
		match, n := "", 0
		for full := range lines {
			if full != bare && strings.Contains(full, "/") && path.Base(full) == bare {
				match, n = full, n+1
			}
		}
		if n == 1 {
			for l := range lines[bare] {
				lines[match][l] = struct{}{}
			}
			delete(lines, bare)
		}
	}

	out := make([]fileRef, 0, len(lines))
	for p, set := range lines {
		ls := make([]string, 0, len(set))
		for l := range set {
			ls = append(ls, l)
		}
		sort.Slice(ls, func(i, j int) bool {
			if a, b := leadingInt(ls[i]), leadingInt(ls[j]); a != b {
				return a < b
			}
			return ls[i] < ls[j]
		})
		out = append(out, fileRef{path: p, lines: ls})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// leadingInt parses the leading run of digits in s (0 if none).
func leadingInt(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// orderedFindingsMarkdown rebuilds the findings markdown in the same shape the
// HTML view uses, so the terminal `show` output matches it: the TLDR and the
// raw-issue Question section are dropped, empty (comment-only) sections are
// removed, sections are reordered (Conclusion first … Approach last), and a
// Referenced files list is appended. Findings with no recognizable sections are
// returned unchanged.
func orderedFindingsMarkdown(body string) string {
	secs := parseSections(body)
	if len(secs) == 0 {
		return body
	}
	var b strings.Builder
	for _, s := range secs {
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", s.title, strings.TrimSpace(s.body.String()))
	}
	if files := extractFileRefs(body); len(files) > 0 {
		b.WriteString("## Referenced files\n\n")
		for _, g := range groupFileRefs(files) {
			if len(g.lines) > 0 {
				fmt.Fprintf(&b, "- `%s` %s\n", g.path, strings.Join(g.lines, ", "))
			} else {
				fmt.Fprintf(&b, "- `%s`\n", g.path)
			}
		}
	}
	return b.String()
}

// section is one parsed findings section: a canonical/slug key, its display
// title, and the accumulated markdown body.
type section struct {
	key   string
	title string
	body  strings.Builder
}

// canonicalKeys maps a normalized section title to a stable key. Titles not in
// this map keep their own slug key and sort into the middle by document order.
var canonicalKeys = map[string]string{
	"tldr": "tldr", "tl;dr": "tldr", "summary": "tldr",
	"question":                   "question",
	"prior work":                 "prior-work",
	"system under investigation": "system",
	"approach":                   "approach",
	"findings":                   "findings",
	"unknowns / assumptions":     "unknowns",
	"unknowns":                   "unknowns",
	"unknowns/assumptions":       "unknowns",
	"assumptions":                "unknowns",
	"conclusion":                 "conclusion",
	"comments":                   "comments",
}

// sectionRank sets display order (lower = higher on the page). Conclusion
// leads, then the supporting findings; process sections (prior work, approach)
// sink to the bottom. Unknown sections take the middle rank and keep document
// order via a stable sort. The Question (raw issue) section is dropped entirely
// in parseSections.
var sectionRank = map[string]int{
	"conclusion": 5, "system": 10, "findings": 20, "unknowns": 40,
	"comments": 65, "prior-work": 70, "approach": 80,
}

const unknownRank = 50

var (
	sectionHeadingRe = regexp.MustCompile(`^##[ \t]+(.+?)[ \t]*$`)
	htmlCommentRe    = regexp.MustCompile(`(?s)<!--.*?-->`)
)

// parseSections splits the findings body into the template sections in display
// order, dropping the TLDR and raw-issue Question sections and any empty
// (comment-only) sections.
//
// Section boundaries are level-2 headings whose title matches a canonical
// template section. Everything else — level-1/3 headings, the raw issue body
// inside <untrusted> blocks, authoring comments — folds into the current
// section's content, so the issue's own headings never pollute the outline.
func parseSections(body string) []*section {
	order := []*section{}
	byKey := map[string]*section{}
	// Content before the first canonical heading (scaffold title/metadata) lands
	// in this preamble, which is never added to order and so is discarded.
	preamble := &section{key: "__preamble__"}
	cur := preamble

	start := func(title string) {
		key := canonicalKeyFor(title)
		if s, ok := byKey[key]; ok {
			s.body.WriteString("\n")
			cur = s
			return
		}
		s := &section{key: key, title: title}
		byKey[key] = s
		order = append(order, s)
		cur = s
	}

	// fenceMarker holds the marker that opened the current code fence ("```"
	// or "~~~"); empty when not inside a fence. CommonMark accepts both, and a
	// fence is closed only by its own marker, so a ``` line cannot close a ~~~
	// block. While fenced, `##`-looking lines are code, not section headings.
	var fenceMarker string
	var inUntrusted, inComment bool
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case inComment:
			cur.body.WriteString(line + "\n")
			if strings.Contains(line, "-->") {
				inComment = false
			}
		case inUntrusted:
			if strings.Contains(t, "</untrusted") {
				inUntrusted = false
			} else {
				cur.body.WriteString(line + "\n")
			}
		case fenceMarker != "":
			cur.body.WriteString(line + "\n")
			if strings.HasPrefix(t, fenceMarker) {
				fenceMarker = ""
			}
		case strings.HasPrefix(t, "<untrusted"):
			inUntrusted = true // drop the tag line itself
		case strings.HasPrefix(t, "<!--") && !strings.Contains(t, "-->"):
			inComment = true
			cur.body.WriteString(line + "\n")
		case strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~"):
			fenceMarker = "```"
			if strings.HasPrefix(t, "~~~") {
				fenceMarker = "~~~"
			}
			cur.body.WriteString(line + "\n")
		case sectionHeadingRe.MatchString(line):
			start(sectionHeadingRe.FindStringSubmatch(line)[1])
		default:
			cur.body.WriteString(line + "\n")
		}
	}

	var secs []*section
	for _, s := range order {
		clean := strings.TrimSpace(htmlCommentRe.ReplaceAllString(s.body.String(), ""))
		if clean == "" {
			continue
		}
		// The TLDR and the raw-issue Question section are dropped entirely.
		if s.key == "tldr" || s.key == "question" {
			continue
		}
		s.body.Reset()
		s.body.WriteString(clean)
		secs = append(secs, s)
	}

	sort.SliceStable(secs, func(i, j int) bool {
		return rankOf(secs[i].key) < rankOf(secs[j].key)
	})
	return secs
}

// canonicalKeyFor maps a section title to its canonical key, or a slug key for
// unknown sections.
func canonicalKeyFor(title string) string {
	n := strings.ToLower(strings.TrimSpace(title))
	n = strings.TrimRight(n, ":：")
	n = strings.Join(strings.Fields(n), " ")
	if k, ok := canonicalKeys[n]; ok {
		return k
	}
	return "x:" + slugify(title)
}

func rankOf(key string) int {
	if r, ok := sectionRank[key]; ok {
		return r
	}
	return unknownRank
}

// renderSections renders each section to HTML and builds the table of contents.
// Each section is a collapsible block with a slugged heading id.
func renderSections(secs []*section) (string, []tocEntry, error) {
	if len(secs) == 0 {
		return "", nil, nil
	}
	seen := map[string]int{}
	var content strings.Builder
	var toc []tocEntry
	for _, s := range secs {
		inner, err := renderFindingsContent(s.body.String())
		if err != nil {
			return "", nil, err
		}
		inner = applyCallouts(inner)
		id := uniqueSlug(s.title, seen)
		toc = append(toc, tocEntry{Level: 2, Text: s.title, ID: id})
		fmt.Fprintf(&content, `<section class="sec"><h2 class="sec-h" id=%q>%s</h2><div class="sec-body">%s</div></section>`,
			id, html.EscapeString(s.title), inner)
	}
	return content.String(), toc, nil
}

// findingsMD is the shared goldmark renderer (default, safe config). Reused
// across every section so a fresh parser/renderer isn't built per call.
var findingsMD = goldmark.New()

// renderFindingsContent converts markdown to HTML: prose through goldmark,
// renderable Mermaid flowcharts as escaped box-outlines in <pre class="diagram">.
func renderFindingsContent(body string) (string, error) {
	var content strings.Builder
	for _, seg := range flowchart.SplitRenderable(body) {
		if seg.Diagram != "" {
			content.WriteString(`<pre class="diagram">`)
			content.WriteString(html.EscapeString(seg.Diagram))
			content.WriteString("</pre>\n")
			continue
		}
		var buf bytes.Buffer
		if err := findingsMD.Convert([]byte(seg.Markdown), &buf); err != nil {
			return "", fmt.Errorf("render findings markdown: %w", err)
		}
		content.Write(buf.Bytes())
	}
	return content.String(), nil
}

// buildIntro renders the hero: an eyebrow and the prompt as the question.
// Returns "" when there is no prompt.
func buildIntro(topic string) string {
	if strings.TrimSpace(topic) == "" {
		return ""
	}
	return `<section class="intro"><p class="eyebrow">Investigated</p>` +
		fmt.Sprintf(`<h1 class="intro-q">%s</h1>`, html.EscapeString(topic)) +
		`</section>`
}

// tocEntry is one heading in the generated table of contents.
type tocEntry struct {
	Level int
	Text  string
	ID    string
}

var (
	headingRe = regexp.MustCompile(`(?s)<h([1-6])>(.*?)</h[1-6]>`)
	tagRe     = regexp.MustCompile(`<[^>]+>`)
)

// injectHeadingIDs adds a unique slug id to every heading in content and
// returns the rewritten content plus the table-of-contents entries in document
// order. Used only by the fallback (no recognizable sections) path.
func injectHeadingIDs(content string) (string, []tocEntry) {
	seen := map[string]int{}
	var toc []tocEntry
	out := headingRe.ReplaceAllStringFunc(content, func(match string) string {
		sub := headingRe.FindStringSubmatch(match)
		level := int(sub[1][0] - '0')
		inner := sub[2]
		label := html.UnescapeString(strings.TrimSpace(tagRe.ReplaceAllString(inner, "")))
		id := uniqueSlug(label, seen)
		toc = append(toc, tocEntry{Level: level, Text: label, ID: id})
		return fmt.Sprintf("<h%d id=%q>%s</h%d>", level, id, inner, level)
	})
	return out, toc
}

// uniqueSlug returns a URL-fragment-safe slug for text, disambiguating repeats
// with a numeric suffix so every id is unique within the document.
func uniqueSlug(text string, seen map[string]int) string {
	base := slugify(text)
	if base == "" {
		base = "section"
	}
	n := seen[base]
	seen[base]++
	if n == 0 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, n)
}

// slugify lowercases text and collapses every run of non-alphanumeric
// characters into a single hyphen, trimming leading/trailing hyphens. Shares
// the package's slugRE with SlugifyTopic (which adds a length cap + fallback).
func slugify(text string) string {
	return strings.Trim(slugRE.ReplaceAllString(strings.ToLower(text), "-"), "-")
}

var (
	codeSpanRe = regexp.MustCompile("`([^`\n]+)`")
	fileLikeRe = regexp.MustCompile(`^[\w@~][\w./@+-]*\.[A-Za-z][A-Za-z0-9]{0,5}(:\d+(-\d+)?)?$`)
	bareRefRe  = regexp.MustCompile(`[\w./~@-]+\.[A-Za-z]{1,6}:\d+(-\d+)?`)
)

// knownFileExt is the set of extensions that mark a dotted token as a file even
// when it has no directory, so identifiers like `consumer.worker` or
// `h.poller.Poll` are not mistaken for files.
var knownFileExt = map[string]bool{
	"go": true, "mod": true, "sum": true, "ts": true, "tsx": true, "js": true,
	"jsx": true, "mjs": true, "cjs": true, "py": true, "rb": true, "rs": true,
	"java": true, "kt": true, "c": true, "h": true, "cc": true, "cpp": true,
	"hpp": true, "cs": true, "php": true, "swift": true, "scala": true,
	"sql": true, "proto": true, "graphql": true, "gql": true, "sh": true,
	"bash": true, "zsh": true, "ps1": true, "yaml": true, "yml": true,
	"json": true, "toml": true, "ini": true, "cfg": true, "conf": true,
	"env": true, "md": true, "mdx": true, "txt": true, "html": true,
	"css": true, "scss": true, "xml": true, "svg": true, "lock": true,
	"gradle": true, "tf": true, "tfvars": true, "dockerfile": true,
}

// looksLikeFile reports whether tok is a file reference: a path (contains a
// slash) or a dotted name with a recognized file extension. A trailing
// `:line` / `:start-end` is ignored.
func looksLikeFile(tok string) bool {
	if !fileLikeRe.MatchString(tok) {
		return false
	}
	if strings.Contains(tok, "/") {
		return true
	}
	base := tok
	if i := strings.LastIndex(base, ":"); i >= 0 && lineSpecRe.MatchString(base[i+1:]) {
		base = base[:i]
	}
	ext := base[strings.LastIndex(base, ".")+1:]
	return knownFileExt[strings.ToLower(ext)]
}

// extractFileRefs scans the findings body for file-path references — code spans
// that look like paths and bare `path.ext:line` mentions — returning a sorted,
// deduplicated list for the "Referenced files" section.
func extractFileRefs(body string) []string {
	set := map[string]struct{}{}
	for _, m := range codeSpanRe.FindAllStringSubmatch(body, -1) {
		tok := strings.TrimSpace(m[1])
		if looksLikeFile(tok) {
			set[tok] = struct{}{}
		}
	}
	for _, tok := range bareRefRe.FindAllString(body, -1) {
		if looksLikeFile(tok) {
			set[tok] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for tok := range set {
		out = append(out, tok)
	}
	sort.Strings(out)
	return out
}

var (
	alertRe   = regexp.MustCompile(`(?is)<blockquote>\s*<p>\s*\[!(note|tip|important|warning|caution)\]\s*(.*?)</blockquote>`)
	leadInRe  = regexp.MustCompile(`(?i)<p>(<strong>(root cause|evidence|fix|conclusion|hypothesis|recommendation|impact|note|warning|caution|tip|important)[:：]</strong>)`)
	calloutSv = map[string]string{
		"warning": "warning", "caution": "warning", "impact": "warning",
		"fix": "success", "recommendation": "success", "conclusion": "success",
	}
)

func calloutSeverity(kind string) string {
	if s, ok := calloutSv[strings.ToLower(kind)]; ok {
		return s
	}
	return "info"
}

// applyCallouts rewrites GitHub-style alert blockquotes (`> [!WARNING]`) and
// bold lead-in paragraphs (`**Fix:** …`) into styled callout boxes.
func applyCallouts(content string) string {
	content = alertRe.ReplaceAllStringFunc(content, func(match string) string {
		sub := alertRe.FindStringSubmatch(match)
		kind := strings.ToLower(sub[1])
		title := strings.ToUpper(kind[:1]) + kind[1:]
		return fmt.Sprintf(
			`<div class="callout callout-%s"><p class="callout-title">%s</p><p>%s</div>`,
			calloutSeverity(kind), title, sub[2])
	})
	content = leadInRe.ReplaceAllStringFunc(content, func(match string) string {
		sub := leadInRe.FindStringSubmatch(match)
		return fmt.Sprintf(`<p class="callout callout-%s">%s`, calloutSeverity(sub[2]), sub[1])
	})
	return content
}

// readingMinutes estimates reading time from the body word count (min 1).
func readingMinutes(body string) int {
	m := (len(strings.Fields(body)) + wordsPerMinute - 1) / wordsPerMinute
	if m < 1 {
		return 1
	}
	return m
}

// buildSidebar renders the table-of-contents navigation. Returns "" when there
// are no entries (single-column layout).
func buildSidebar(toc []tocEntry) string {
	if len(toc) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<aside class="sidebar"><nav class="toc" aria-label="Findings contents"><div class="panel-title">Contents</div>`)
	for _, e := range toc {
		fmt.Fprintf(&b, `<a class="toc-link lvl-%d" href="#%s">%s</a>`,
			e.Level, e.ID, html.EscapeString(e.Text))
	}
	b.WriteString(`</nav></aside>`)
	return b.String()
}

// assembleHTMLDoc wraps the rendered findings in the full interactive page.
func assembleHTMLDoc(d pageDoc) string {
	title := d.Title
	if title == "" {
		title = "Investigation findings"
	}
	escTitle := html.EscapeString(title)
	sidebar := buildSidebar(d.TOC)
	layoutClass := "layout"
	if sidebar == "" {
		layoutClass = "layout no-sidebar"
	}

	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"en\">\n<head>\n")
	b.WriteString(`<meta charset="utf-8">` + "\n")
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">` + "\n")
	fmt.Fprintf(&b, "<title>%s</title>\n", escTitle)
	b.WriteString("<style>\n" + findingsCSS + "</style>\n")
	b.WriteString("</head>\n<body>\n")

	b.WriteString(`<header class="topbar">`)
	fmt.Fprintf(&b, `<span class="doc-title" title=%q>%s</span>`, escTitle, escTitle)
	fmt.Fprintf(&b, `<span class="stats">%d min read · %s</span>`, d.Minutes, sectionLabel(d.Sections))
	b.WriteString(`<div class="controls">`)
	b.WriteString(`<input type="search" id="filter" placeholder="Filter findings…" aria-label="Filter findings" autocomplete="off">`)
	b.WriteString(`<button type="button" data-print aria-label="Print or save as PDF" title="Print / save as PDF">⎙</button>`)
	b.WriteString(`<button type="button" data-theme-toggle aria-label="Switch colour theme" title="Switch colour theme">●</button>`)
	b.WriteString(`</div></header>`)

	fmt.Fprintf(&b, `<div class="%s">`, layoutClass)
	b.WriteString(sidebar)
	b.WriteString(`<main class="findings" id="findings-root">` + "\n")
	b.WriteString(d.Intro)
	b.WriteString(d.Content)
	b.WriteString("\n</main></div>\n")
	b.WriteString(`<p class="empty-hint" id="empty-hint" hidden>No findings match your filter.</p>` + "\n")
	b.WriteString(`<button type="button" id="to-top" aria-label="Back to top" title="Back to top" hidden>↑</button>` + "\n")

	b.WriteString("<script>\n" + findingsJS + "</script>\n")
	b.WriteString("</body>\n</html>\n")
	return b.String()
}

func sectionLabel(n int) string {
	if n == 1 {
		return "1 section"
	}
	return fmt.Sprintf("%d sections", n)
}

// findingsCSS is the inline stylesheet for the generated page. Self-contained
// (no @import, no external fonts) so the document renders offline.
const findingsCSS = `
:root {
  --bg: #fbfcfd; --fg: #1b2330; --muted: #5b6675; --muted-strong: #3a4452;
  --border: #e4e8ef; --rule: #eef1f5; --surface: #f3f6fa; --surface-2: #eaeef4;
  --accent: #4f46e5; --accent-soft: #ecebfd;
  --mark: #ffe89a; --shadow: rgba(27,35,48,0.07);
  --info: #4f46e5; --info-bg: #f1f0fe; --warn: #9a6700; --warn-bg: #fdf6d8;
  --ok: #1a7f4b; --ok-bg: #e6f6ed;
  --mono: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace;
}
[data-theme="dark"] {
  --bg: #0f1216; --fg: #dce3ec; --muted: #8b95a4; --muted-strong: #aab4c2;
  --border: #262d38; --rule: #1c222b; --surface: #171c23; --surface-2: #1f262f;
  --accent: #8b85f5; --accent-soft: #211f3a;
  --mark: #5c4a00; --shadow: rgba(0,0,0,0.45);
  --info: #8b85f5; --info-bg: #1a1830; --warn: #d8b34a; --warn-bg: #2a2206;
  --ok: #46c07e; --ok-bg: #0e2618;
}
@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) {
    --bg: #0f1216; --fg: #dce3ec; --muted: #8b95a4; --muted-strong: #aab4c2;
    --border: #262d38; --rule: #1c222b; --surface: #171c23; --surface-2: #1f262f;
    --accent: #8b85f5; --accent-soft: #211f3a;
    --mark: #5c4a00; --shadow: rgba(0,0,0,0.45);
    --info: #8b85f5; --info-bg: #1a1830; --warn: #d8b34a; --warn-bg: #2a2206;
    --ok: #46c07e; --ok-bg: #0e2618;
  }
}
* { box-sizing: border-box; }
html { scroll-behavior: smooth; }
body {
  margin: 0; color: var(--fg); background: var(--bg);
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
  font-size: 17px; line-height: 1.72; -webkit-font-smoothing: antialiased;
}
.eyebrow, .panel-title, .stats, .callout-title, .doc-title {
  font-family: var(--mono); text-transform: uppercase; letter-spacing: 0.11em;
}
.topbar {
  position: sticky; top: 0; z-index: 20;
  display: flex; align-items: center; gap: 1rem;
  padding: 0.65rem 1.5rem;
  background: color-mix(in srgb, var(--bg) 86%, transparent);
  backdrop-filter: saturate(150%) blur(10px);
  border-bottom: 1px solid var(--border);
}
.doc-title {
  font-size: 0.78rem; font-weight: 600; color: var(--muted);
  margin: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; min-width: 0; flex: 1;
}
.stats { color: var(--muted); font-size: 0.66rem; white-space: nowrap; }
.controls { display: flex; align-items: center; gap: 0.5rem; }
#filter {
  width: 13rem; max-width: 34vw;
  padding: 0.4rem 0.8rem; border: 1px solid var(--border); border-radius: 8px;
  background: var(--bg); color: var(--fg); font-size: 0.85rem;
}
#filter:focus { outline: 2px solid var(--accent); outline-offset: 0; border-color: transparent; }
[data-print], [data-theme-toggle] {
  width: 2rem; height: 2rem; border-radius: 8px; cursor: pointer;
  border: 1px solid var(--border); background: var(--bg); color: var(--muted);
  font-size: 0.85rem; line-height: 1;
}
[data-print]:hover, [data-theme-toggle]:hover { color: var(--accent); border-color: var(--accent); }
.layout {
  display: grid; grid-template-columns: minmax(180px, 230px) minmax(0, 1fr);
  gap: 3.5rem; max-width: 1080px; margin: 0 auto; padding: 3rem 2rem 7rem;
}
.layout.no-sidebar { grid-template-columns: minmax(0, 46rem); justify-content: center; }
.sidebar { position: sticky; top: 4.5rem; align-self: start; max-height: calc(100vh - 6rem); overflow-y: auto; }
.panel-title { font-size: 0.66rem; color: var(--muted); margin-bottom: 0.7rem; font-weight: 600; }
.toc { display: flex; flex-direction: column; gap: 0.05rem; font-size: 0.86rem; line-height: 1.4; }
.toc-link {
  color: var(--muted); text-decoration: none; padding: 0.28rem 0.7rem;
  border-left: 2px solid var(--rule); transition: color 0.1s, border-color 0.1s;
}
.toc-link:hover { color: var(--fg); }
.toc-link.active { color: var(--accent); border-left-color: var(--accent); font-weight: 600; }
.file-list { list-style: none; margin: 0; padding: 0; font-family: var(--mono); font-size: 0.84rem; }
.file-list li { padding: 0.2rem 0; color: var(--fg); word-break: break-all; }
.file-path { color: var(--fg); }
.file-lines { color: var(--muted); }
.findings { min-width: 0; max-width: 46rem; }
.findings > :first-child { margin-top: 0; }
/* Hero */
.intro { margin: 0 0 1rem; padding-bottom: 2rem; border-bottom: 1px solid var(--border); }
.eyebrow { font-size: 0.72rem; font-weight: 600; color: var(--accent); margin: 0 0 0.9rem; }
.intro-q { font-size: 2rem; font-weight: 700; line-height: 1.18; letter-spacing: -0.018em; margin: 0; }
/* Sections */
.sec { border-top: 1px solid var(--rule); }
.sec:first-of-type { border-top: 0; }
.sec-h {
  font-size: 1.3rem; font-weight: 680; line-height: 1.3; letter-spacing: -0.01em;
  margin: 2.4rem 0 1rem; scroll-margin-top: 5rem; cursor: pointer; position: relative;
}
.sec-h::before {
  content: ""; position: absolute; left: -1.5rem; top: 0.5em;
  width: 0.5rem; height: 0.5rem; border-right: 2px solid var(--accent); border-bottom: 2px solid var(--accent);
  transform: rotate(45deg); transition: transform 0.15s ease; opacity: 0.8;
}
.sec.collapsed .sec-h::before { transform: rotate(-45deg); }
.sec.collapsed .sec-body { display: none; }
.anchor { opacity: 0; text-decoration: none; color: var(--muted); margin-left: 0.5rem; font-weight: 400; font-size: 0.7em; }
.sec-h:hover .anchor { opacity: 1; }
/* Content headings inside a section (e.g. raw issue body) are subdued. */
.sec-body h1, .sec-body h2, .sec-body h3, .sec-body h4 {
  font-size: 1rem; font-weight: 650; line-height: 1.35; margin: 1.5rem 0 0.5rem; color: var(--muted-strong);
}
.findings p { margin: 0 0 1.15rem; }
.findings li { margin: 0.3rem 0; }
.findings ul, .findings ol { padding-left: 1.4rem; }
.findings a { color: var(--accent); text-underline-offset: 2px; }
.findings code {
  font-family: var(--mono); background: var(--surface-2); padding: 0.12em 0.4em;
  border-radius: 5px; font-size: 0.85em;
}
.findings pre {
  position: relative; background: var(--surface); border: 1px solid var(--border);
  padding: 1rem 1.1rem; border-radius: 10px; overflow-x: auto; margin: 1.4rem 0;
}
.findings pre code { background: none; padding: 0; font-size: 0.84rem; }
.findings pre.diagram { font-family: var(--mono); line-height: 1.25; white-space: pre; font-size: 0.8rem; color: var(--muted-strong); }
.copy-btn {
  position: absolute; top: 0.5rem; right: 0.5rem; padding: 0.2rem 0.55rem;
  font-family: var(--mono); font-size: 0.66rem; border: 1px solid var(--border); border-radius: 6px;
  background: var(--bg); color: var(--muted); cursor: pointer; opacity: 0; transition: opacity 0.12s;
}
.findings pre:hover .copy-btn { opacity: 1; }
.copy-btn:hover { color: var(--accent); border-color: var(--accent); }
.callout {
  margin: 1.5rem 0; padding: 0.9rem 1.1rem; border-left: 3px solid var(--info);
  background: var(--info-bg); border-radius: 0 8px 8px 0;
}
.callout-title { margin: 0 0 0.35rem; font-weight: 600; font-size: 0.7rem; color: var(--info); }
.callout > :last-child { margin-bottom: 0; }
.callout-warning { border-left-color: var(--warn); background: var(--warn-bg); }
.callout-warning .callout-title { color: var(--warn); }
.callout-success { border-left-color: var(--ok); background: var(--ok-bg); }
.callout-success .callout-title { color: var(--ok); }
.findings blockquote {
  margin: 1.4rem 0; padding: 0.3rem 1.1rem; color: var(--muted); border-left: 3px solid var(--border);
}
.findings table { border-collapse: collapse; width: 100%; margin: 1.4rem 0; font-size: 0.92rem; }
.findings th, .findings td { border: 1px solid var(--border); padding: 0.5rem 0.8rem; text-align: left; }
.findings th { background: var(--surface); font-family: var(--mono); font-size: 0.78rem; }
.findings img { max-width: 100%; }
mark.hl { background: var(--mark); color: var(--fg); border-radius: 3px; padding: 0 0.1em; }
.sec.filtered-out { display: none !important; }
.empty-hint { text-align: center; color: var(--muted); margin: 3rem auto; }
#to-top {
  position: fixed; bottom: 1.5rem; right: 1.5rem; width: 2.6rem; height: 2.6rem;
  border-radius: 10px; border: 1px solid var(--border); background: var(--bg);
  color: var(--fg); font-size: 1rem; cursor: pointer; box-shadow: 0 4px 14px var(--shadow); z-index: 30;
}
#to-top:hover { color: var(--accent); border-color: var(--accent); }
:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
@media (max-width: 800px) {
  body { font-size: 16px; }
  .layout { grid-template-columns: minmax(0, 1fr); gap: 1.5rem; padding: 1.5rem 1.25rem 4rem; }
  .sidebar { position: static; max-height: none; border-bottom: 1px solid var(--border); padding-bottom: 1.25rem; }
  .stats { display: none; }
  #filter { width: 8rem; }
  .intro-q { font-size: 1.6rem; }
  .sec-h::before { left: -1.15rem; }
}
@media print {
  .topbar, .sidebar, #to-top, .copy-btn, .anchor { display: none !important; }
  .layout { display: block; max-width: none; padding: 0; }
  .findings { max-width: none; font-size: 11pt; }
  .sec.collapsed .sec-body { display: block !important; }
  .sec-h::before { display: none; }
  .findings pre, .callout, .intro, blockquote, table, img, .sec { break-inside: avoid; }
  a { color: inherit; text-decoration: underline; }
}
@media (prefers-reduced-motion: reduce) {
  html { scroll-behavior: auto; }
  * { transition: none !important; }
}
`

// findingsJS powers the page interactivity: theme toggle (persisted),
// collapsible sections, scroll-spy TOC, in-page filter with highlighting,
// copy-to-clipboard, heading anchors, back-to-top, print/PDF, and keyboard
// shortcuts. Vanilla JS, no external dependencies.
const findingsJS = `
(function () {
  var root = document.documentElement;
  var THEME_KEY = "entire-findings-theme";

  function applyTheme(t) {
    if (t === "light" || t === "dark") root.setAttribute("data-theme", t);
    else root.removeAttribute("data-theme");
  }
  try { applyTheme(localStorage.getItem(THEME_KEY)); } catch (e) {}
  var toggle = document.querySelector("[data-theme-toggle]");
  if (toggle) {
    toggle.addEventListener("click", function () {
      var cur = root.getAttribute("data-theme") || "auto";
      var next = cur === "auto" ? "light" : cur === "light" ? "dark" : "auto";
      applyTheme(next);
      try {
        if (next === "auto") localStorage.removeItem(THEME_KEY);
        else localStorage.setItem(THEME_KEY, next);
      } catch (e) {}
    });
  }

  var findings = document.getElementById("findings-root");
  if (!findings) return;
  var sections = Array.prototype.slice.call(findings.querySelectorAll(".sec"));

  // Collapsible sections (click the heading; ignore clicks on the anchor link).
  sections.forEach(function (sec) {
    var h = sec.querySelector(".sec-h");
    if (!h) return;
    h.addEventListener("click", function (ev) {
      if (ev.target.closest("a")) return;
      sec.classList.toggle("collapsed");
    });
  });

  // Anchor links on section headings (hover to reveal, click copies a link).
  findings.querySelectorAll(".sec-h[id]").forEach(function (h) {
    var a = document.createElement("a");
    a.className = "anchor"; a.href = "#" + h.id; a.textContent = "#";
    a.setAttribute("aria-label", "Link to this section");
    a.addEventListener("click", function () {
      var url = location.href.split("#")[0] + "#" + h.id;
      if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(url).catch(function () {});
    });
    h.appendChild(a);
  });

  // Scroll-spy: highlight the TOC link for the heading nearest the top of the
  // viewport. Computed from scroll position (rather than only reacting to
  // intersection events) so the active link stays correct even when no heading
  // sits in a thin observation band — deep inside a long section, or at the
  // very top/bottom of the page. Hidden (filtered-out) headings are skipped.
  var links = {};
  document.querySelectorAll(".toc-link").forEach(function (a) { links[a.getAttribute("href").slice(1)] = a; });
  var spyHeadings = Array.prototype.slice.call(findings.querySelectorAll(".sec-h[id]"));
  if (spyHeadings.length && Object.keys(links).length) {
    var activeLink = null;
    function setActive(link) {
      if (link === activeLink) return;
      if (activeLink) activeLink.classList.remove("active");
      if (link) link.classList.add("active");
      activeLink = link;
    }
    function syncSpy() {
      var threshold = 80; // just below the sticky top bar
      var current = null, firstVisible = null;
      for (var i = 0; i < spyHeadings.length; i++) {
        var h = spyHeadings[i];
        if (!h.getClientRects().length) continue; // skip hidden headings
        if (!firstVisible) firstVisible = h;
        if (h.getBoundingClientRect().top <= threshold) current = h;
      }
      if (!current) current = firstVisible; // above the first heading → highlight it
      setActive(current ? (links[current.id] || null) : null);
    }
    var spyTicking = false;
    function onSpyScroll() {
      if (spyTicking) return;
      spyTicking = true;
      window.requestAnimationFrame(function () { syncSpy(); spyTicking = false; });
    }
    window.addEventListener("scroll", onSpyScroll, { passive: true });
    window.addEventListener("resize", onSpyScroll);
    syncSpy();
  }

  // Copy buttons on code blocks (skip rendered diagrams).
  findings.querySelectorAll("pre > code").forEach(function (code) {
    var pre = code.parentNode;
    if (pre.classList.contains("diagram")) return;
    var btn = document.createElement("button");
    btn.type = "button"; btn.className = "copy-btn"; btn.textContent = "Copy";
    btn.addEventListener("click", function () {
      var text = code.textContent;
      var done = function () { btn.textContent = "Copied"; setTimeout(function () { btn.textContent = "Copy"; }, 1200); };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, function () { fallbackCopy(text); done(); });
      } else { fallbackCopy(text); done(); }
    });
    pre.appendChild(btn);
  });
  function fallbackCopy(text) {
    var ta = document.createElement("textarea");
    ta.value = text; ta.style.position = "fixed"; ta.style.opacity = "0";
    document.body.appendChild(ta); ta.select();
    try { document.execCommand("copy"); } catch (e) {}
    document.body.removeChild(ta);
  }

  // In-page filter: show matching sections, highlight matches.
  function clearMarks(el) {
    el.querySelectorAll("mark.hl").forEach(function (m) {
      var parent = m.parentNode;
      parent.replaceChild(document.createTextNode(m.textContent), m);
      parent.normalize();
    });
  }
  function highlight(el, term) {
    var walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT, {
      acceptNode: function (n) {
        if (!n.nodeValue.toLowerCase().includes(term)) return NodeFilter.FILTER_REJECT;
        if (n.parentNode && n.parentNode.closest("pre")) return NodeFilter.FILTER_REJECT;
        return NodeFilter.FILTER_ACCEPT;
      }
    });
    var targets = [];
    while (walker.nextNode()) targets.push(walker.currentNode);
    targets.forEach(function (node) {
      var frag = document.createDocumentFragment();
      var lower = node.nodeValue.toLowerCase();
      var i = 0, idx;
      while ((idx = lower.indexOf(term, i)) !== -1) {
        if (idx > i) frag.appendChild(document.createTextNode(node.nodeValue.slice(i, idx)));
        var mk = document.createElement("mark");
        mk.className = "hl";
        mk.textContent = node.nodeValue.slice(idx, idx + term.length);
        frag.appendChild(mk);
        i = idx + term.length;
      }
      if (i < node.nodeValue.length) frag.appendChild(document.createTextNode(node.nodeValue.slice(i)));
      node.parentNode.replaceChild(frag, node);
    });
  }
  var filter = document.getElementById("filter");
  var emptyHint = document.getElementById("empty-hint");
  function runFilter() {
    var term = filter.value.trim().toLowerCase();
    sections.forEach(function (sec) { clearMarks(sec); });
    if (!term) {
      sections.forEach(function (sec) { sec.classList.remove("filtered-out"); });
      if (emptyHint) emptyHint.hidden = true;
      return;
    }
    var anyVisible = false;
    sections.forEach(function (sec) {
      var match = sec.textContent.toLowerCase().includes(term);
      sec.classList.toggle("filtered-out", !match);
      if (match) { anyVisible = true; sec.classList.remove("collapsed"); highlight(sec, term); }
    });
    if (emptyHint) emptyHint.hidden = anyVisible;
  }
  if (filter) filter.addEventListener("input", runFilter);

  // Print / save as PDF (expand sections first).
  var printBtn = document.querySelector("[data-print]");
  if (printBtn) printBtn.addEventListener("click", function () { window.print(); });
  window.addEventListener("beforeprint", function () {
    sections.forEach(function (sec) { sec.classList.remove("collapsed"); });
  });

  // Back to top.
  var toTop = document.getElementById("to-top");
  if (toTop) {
    window.addEventListener("scroll", function () { toTop.hidden = window.scrollY < 600; });
    toTop.addEventListener("click", function () { window.scrollTo({ top: 0, behavior: "smooth" }); });
  }

  // Keyboard: "/" focuses filter, Esc clears it.
  document.addEventListener("keydown", function (e) {
    if (e.key === "/" && filter && document.activeElement !== filter) {
      e.preventDefault(); filter.focus();
    } else if (e.key === "Escape" && document.activeElement === filter) {
      filter.value = ""; runFilter(); filter.blur();
    }
  });
})();
`

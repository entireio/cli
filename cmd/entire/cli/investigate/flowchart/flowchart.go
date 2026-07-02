// Package flowchart renders Mermaid flowcharts as top-down Unicode box
// diagrams for terminal display.
//
// Investigation findings are authored in Mermaid (which renders natively on
// GitHub and in docs), but the terminal renderer (glamour) shows a
// ```mermaid block as raw source. This package converts a flowchart into
// boxes and arrows flowing top-to-bottom.
//
// Top-down (rather than left-to-right) layout is used because real
// investigation diagrams have enough nodes that a horizontal layout
// overflows any terminal width: flowing downward, width grows only at forks
// (siblings sit side-by-side), never with chain length. The diagram must be
// printed OUTSIDE the markdown renderer — glamour word-wraps content and
// would corrupt the alignment.
//
// The renderer builds a spanning forest from the flowchart's edges: most
// flows are a chain or a success/failure fork, which render as boxes joined
// by arrows. Edges that can't be tree edges — back-edges (retry loops),
// fan-in (two arrows into one node), cross-links — are rendered as "↪"
// references to the already-shown node, so cyclic and converging diagrams
// still render instead of falling back. Subgraphs are treated as transparent
// grouping. It falls back (ok=false) only when the input isn't a flowchart
// it can parse at all (non-flowchart diagram types, `&` multi-edge
// shorthand, or unrecognized syntax), so the caller can show the raw Mermaid
// source.
package flowchart

import (
	"regexp"
	"strings"

	"github.com/mattn/go-runewidth"
)

// outEdge is a directed link target with its optional `-->|label|`.
type outEdge struct {
	to    string
	label string
}

// linkRe matches a Mermaid link operator with an optional `|label|`. The
// surrounding whitespace is consumed so the text between matches is exactly
// the node tokens. Inline-label syntax (`A -- text --> B`) is intentionally
// unsupported and causes a parse failure → raw fallback.
var linkRe = regexp.MustCompile(`\s*(?:-->|---|==>|===|-\.->|-\.-)\s*(?:\|"?([^|"]*)"?\|)?\s*`)

// nodeRe matches a single node token: an id followed by an optional shape
// wrapper. We extract the id and the inner label regardless of shape.
var nodeRe = regexp.MustCompile(`^([A-Za-z0-9_]+)\s*(\[\[.*\]\]|\(\(.*\)\)|\[.*\]|\(.*\)|\{.*\}|>.*\])?$`)

// headerRe matches the `flowchart`/`graph` declaration line.
var headerRe = regexp.MustCompile(`^(?:flowchart|graph)\b`)

// brRe matches Mermaid line breaks in labels (`<br>`, `<br/>`, `<br />`).
var brRe = regexp.MustCompile(`(?i)<br\s*/?>`)

// Render renders src as a top-down box diagram and returns ok=true,
// or returns ok=false when src isn't a parseable flowchart. On ok=false the
// caller should show the raw Mermaid source.
func Render(src string) (string, bool) {
	order, labels, edges, ok := parse(src)
	if !ok {
		return "", false
	}
	return renderForest(order, labels, edges), true
}

// parse reads the flowchart body into the node ids in declaration order, a
// label per id, and the edges. It returns ok=false for any construct outside
// the supported subset (`&` multi-edge, non-flowchart diagrams, or any
// unrecognized non-blank line). Subgraph grouping is skipped (transparent).
func parse(src string) ([]string, map[string]string, []outEdgeWithFrom, bool) {
	labels := map[string]string{}
	var order []string
	var edges []outEdgeWithFrom
	sawFlowchart := false

	ensure := func(id, label string) {
		cur, exists := labels[id]
		if !exists {
			labels[id] = label
			order = append(order, id)
			return
		}
		// A later bracketed reference defines the label; bare references
		// (label == id) never overwrite a real label.
		if label != id && cur == id {
			labels[id] = label
		}
	}

	for _, line := range logicalLines(src) {
		if line == "" {
			continue
		}
		if headerRe.MatchString(line) {
			sawFlowchart = true
			continue
		}
		if isIgnorableDirective(line) {
			continue
		}
		if isBailDirective(line) || hasStructuralAmp(line) {
			return nil, nil, nil, false
		}

		lineNodes, lineEdges, ok := parseLine(line)
		if !ok {
			return nil, nil, nil, false
		}
		for _, n := range lineNodes {
			ensure(n.id, n.label)
		}
		for _, e := range lineEdges {
			ensure(e.from, e.from)
			ensure(e.to, e.to)
			edges = append(edges, e)
		}
	}

	// Every parsed structural line declares at least one node, so an empty
	// label map means no content lines were seen.
	if !sawFlowchart || len(labels) == 0 {
		return nil, nil, nil, false
	}
	return order, labels, edges, true
}

// outEdgeWithFrom is an edge as parsed, before the forest assigns roles.
type outEdgeWithFrom struct {
	from  string
	to    string
	label string
}

type nodeDecl struct{ id, label string }

// parseLine parses one structural line into the node declarations it makes
// and the edges it forms. A line is either a bare node declaration
// (`A[Label]`) or a chain of nodes joined by links (`A --> B --> C`).
func parseLine(line string) ([]nodeDecl, []outEdgeWithFrom, bool) {
	locs := linkRe.FindAllStringSubmatchIndex(line, -1)
	if len(locs) == 0 {
		id, label, ok := parseNodeToken(line)
		if !ok {
			return nil, nil, false
		}
		return []nodeDecl{{id, label}}, nil, true
	}

	var tokens []nodeDecl
	var edgeLabels []string
	prev := 0
	for _, loc := range locs {
		id, label, ok := parseNodeToken(line[prev:loc[0]])
		if !ok {
			return nil, nil, false
		}
		tokens = append(tokens, nodeDecl{id, label})
		el := ""
		if loc[2] >= 0 {
			el = cleanEdgeLabel(line[loc[2]:loc[3]])
		}
		edgeLabels = append(edgeLabels, el)
		prev = loc[1]
	}
	id, label, ok := parseNodeToken(line[prev:])
	if !ok {
		return nil, nil, false
	}
	tokens = append(tokens, nodeDecl{id, label})

	edges := make([]outEdgeWithFrom, 0, len(tokens)-1)
	for i := 0; i+1 < len(tokens); i++ {
		edges = append(edges, outEdgeWithFrom{from: tokens[i].id, to: tokens[i+1].id, label: edgeLabels[i]})
	}
	return tokens, edges, true
}

// parseNodeToken extracts the id and label from a single node token, peeling
// the shape wrapper and surrounding quotes. Returns ok=false on anything that
// isn't a lone node reference.
func parseNodeToken(s string) (id, label string, ok bool) {
	s = strings.TrimSpace(s)
	m := nodeRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	id = m[1]
	label = id
	if shape := m[2]; shape != "" {
		inner := strings.TrimSpace(unwrapShape(shape))
		inner = strings.TrimSpace(strings.Trim(inner, `"`))
		if inner != "" {
			label = inner
		}
	}
	return id, label, true
}

// unwrapShape strips the outer bracket pair(s) from a shape wrapper, leaving
// the inner label text.
func unwrapShape(shape string) string {
	for _, pair := range []struct{ open, close string }{
		{"[[", "]]"}, {"((", "))"}, {"[", "]"}, {"(", ")"}, {"{", "}"}, {">", "]"},
	} {
		if strings.HasPrefix(shape, pair.open) && strings.HasSuffix(shape, pair.close) {
			return strings.TrimSuffix(strings.TrimPrefix(shape, pair.open), pair.close)
		}
	}
	return shape
}

// forest is the spanning structure used to render: tree children per node
// (recursed into) and reference edges per node (back/fan-in/cross links shown
// as "↪" without recursion), plus the roots to render top-level.
type forest struct {
	labels map[string]string
	tree   map[string][]outEdge
	refs   map[string][]outEdge
	roots  []string
}

// buildForest turns the parsed edges into a spanning forest. Roots are the
// in-degree-0 nodes in declaration order; if a component has none (a pure
// cycle), its first-declared node is used. A DFS in declaration order assigns
// each node's first incoming edge as a tree edge and every later edge into an
// already-visited node as a reference edge.
func buildForest(order []string, labels map[string]string, edges []outEdgeWithFrom) forest {
	adj := map[string][]outEdge{}
	indeg := map[string]int{}
	for _, id := range order {
		indeg[id] = 0
	}
	for _, e := range edges {
		adj[e.from] = append(adj[e.from], outEdge{to: e.to, label: e.label})
		indeg[e.to]++
	}

	f := forest{
		labels: labels,
		tree:   map[string][]outEdge{},
		refs:   map[string][]outEdge{},
	}
	visited := map[string]bool{}

	var dfs func(u string)
	dfs = func(u string) {
		for _, oe := range adj[u] {
			if visited[oe.to] {
				f.refs[u] = append(f.refs[u], oe)
				continue
			}
			visited[oe.to] = true
			f.tree[u] = append(f.tree[u], oe)
			dfs(oe.to)
		}
	}

	visit := func(root string) {
		visited[root] = true
		f.roots = append(f.roots, root)
		dfs(root)
	}

	for _, id := range order {
		if indeg[id] == 0 && !visited[id] {
			visit(id)
		}
	}
	// Any remaining unvisited node belongs to a component with no entry point
	// (e.g. an isolated cycle); root it at its first-declared node.
	for _, id := range order {
		if !visited[id] {
			visit(id)
		}
	}
	return f
}

// cellShadow marks the second display column of a double-width rune in a row
// grid, so slice indices keep matching display columns. rowString drops it.
const cellShadow = '\x00'

// rowString converts a row grid to its display string, dropping cellShadow
// placeholders left behind by double-width runes.
func rowString(row []rune) string {
	var b strings.Builder
	for _, r := range row {
		if r != cellShadow {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// block is a rendered rectangle of text plus the column at which connector
// lines attach (the anchor). Lines are space-padded as built; trailing
// whitespace is trimmed at the very end.
type block struct {
	lines  []string
	width  int
	anchor int
}

// shifted returns the block moved n columns right.
func (b block) shifted(n int) block {
	if n <= 0 {
		return b
	}
	pad := strings.Repeat(" ", n)
	lines := make([]string, len(b.lines))
	for i, l := range b.lines {
		lines[i] = pad + l
	}
	return block{lines: lines, width: b.width + n, anchor: b.anchor + n}
}

// newBoxBlock draws labelLines inside a box. The anchor is the box's center
// column, where vertical connectors attach.
func newBoxBlock(labelLines []string) block {
	inner := 0
	for _, l := range labelLines {
		inner = max(inner, runewidth.StringWidth(l))
	}
	bar := strings.Repeat("─", inner+2)
	lines := make([]string, 0, len(labelLines)+2)
	lines = append(lines, "┌"+bar+"┐")
	for _, l := range labelLines {
		gap := inner - runewidth.StringWidth(l)
		lines = append(lines, "│ "+l+strings.Repeat(" ", gap)+" │")
	}
	lines = append(lines, "└"+bar+"┘")
	w := inner + 4
	return block{lines: lines, width: w, anchor: w / 2}
}

// refBlock is the one-line stand-in for a reference edge target: the node is
// already drawn elsewhere, so the edge just points back at it by label.
func refBlock(f forest, target string) block {
	txt := "↪ " + strings.Join(splitLabel(f.labels[target], target), " — ")
	return block{lines: []string{txt}, width: runewidth.StringWidth(txt), anchor: 0}
}

// item is one outgoing edge of a node prepared for layout: its edge label and
// the rendered subtree (or refBlock) it leads to.
type item struct {
	label string
	blk   block
	ref   bool
}

// vstack places top above bottom with their anchors aligned in one column.
func vstack(top, bottom block) block {
	t := top.shifted(max(0, bottom.anchor-top.anchor))
	b := bottom.shifted(max(0, top.anchor-bottom.anchor))
	lines := make([]string, 0, len(t.lines)+len(b.lines))
	lines = append(lines, t.lines...)
	lines = append(lines, b.lines...)
	return block{lines: lines, width: max(t.width, b.width), anchor: t.anchor}
}

// composeChildren lays the child subtrees side-by-side and draws the
// connector rows above them: a distributor bar (for >1 child) splitting the
// parent's spine across the children, a label row (│ label per child, dashed
// ╎ for reference edges), and an arrow row (▼ per tree child). The returned
// block's anchor is where the parent's spine should meet the distributor.
func composeChildren(items []item) block {
	const gap = 3
	x := 0
	starts := make([]int, len(items))
	anchors := make([]int, len(items))
	height := 0
	for i, it := range items {
		starts[i] = x
		anchors[i] = x + it.blk.anchor
		// Reserve room for the connector label so it can't run into the
		// next sibling's column.
		labelEnd := anchors[i] + 2 + runewidth.StringWidth(it.label)
		x = max(starts[i]+it.blk.width, labelEnd) + gap
		height = max(height, len(it.blk.lines))
	}
	width := x - gap

	newRow := func() []rune {
		r := make([]rune, width)
		for i := range r {
			r[i] = ' '
		}
		return r
	}
	put := func(row []rune, col int, s string) {
		for _, r := range s {
			w := runewidth.RuneWidth(r)
			if col >= 0 && col < width {
				row[col] = r
				// A double-width rune (CJK, emoji) covers the next display
				// column too; shadow that cell so slice indices keep matching
				// display columns. Shadows are dropped by rowString.
				if w == 2 && col+1 < width {
					row[col+1] = cellShadow
				}
			}
			col += w
		}
	}

	first, last := anchors[0], anchors[len(items)-1]
	anchor := (first + last) / 2

	var out []string
	if len(items) > 1 {
		row := newRow()
		for c := first; c <= last; c++ {
			row[c] = '─'
		}
		for i := range items {
			switch i {
			case 0:
				row[anchors[i]] = '┌'
			case len(items) - 1:
				row[anchors[i]] = '┐'
			default:
				row[anchors[i]] = '┬'
			}
		}
		if row[anchor] == '─' {
			row[anchor] = '┴'
		} else {
			row[anchor] = '┼'
		}
		out = append(out, rowString(row))
	}

	labelRow := newRow()
	for i, it := range items {
		bar := "│"
		if it.ref {
			bar = "╎"
		}
		txt := bar
		if it.label != "" {
			txt += " " + it.label
		}
		put(labelRow, anchors[i], txt)
	}
	out = append(out, rowString(labelRow))

	arrowRow := newRow()
	for i, it := range items {
		if it.ref {
			arrowRow[anchors[i]] = '╎'
		} else {
			arrowRow[anchors[i]] = '▼'
		}
	}
	out = append(out, rowString(arrowRow))

	for r := range height {
		row := newRow()
		for i, it := range items {
			if r < len(it.blk.lines) {
				put(row, starts[i], it.blk.lines[r])
			}
		}
		out = append(out, rowString(row))
	}
	return block{lines: out, width: width, anchor: anchor}
}

// blockFor renders node u's box with all its outgoing edges below it: tree
// children recurse into full subtrees; reference edges become one-line "↪"
// pointers at the already-rendered node.
func blockFor(f forest, u string) block {
	box := newBoxBlock(splitLabel(f.labels[u], u))
	var items []item
	for _, oe := range f.tree[u] {
		items = append(items, item{label: oe.label, blk: blockFor(f, oe.to)})
	}
	for _, oe := range f.refs[u] {
		items = append(items, item{label: oe.label, blk: refBlock(f, oe.to), ref: true})
	}
	if len(items) == 0 {
		return box
	}
	return vstack(box, composeChildren(items))
}

// renderForest draws each root's subtree as a top-down box diagram; multiple
// roots are separated by a blank line.
func renderForest(order []string, labels map[string]string, edges []outEdgeWithFrom) string {
	f := buildForest(order, labels, edges)
	var parts []string
	for _, root := range f.roots {
		blk := blockFor(f, root)
		lines := make([]string, len(blk.lines))
		for i, l := range blk.lines {
			lines[i] = strings.TrimRight(l, " ")
		}
		parts = append(parts, strings.Join(lines, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

// logicalLines splits src into comment-stripped, trimmed logical lines,
// merging physical lines whose brackets or quotes are still open. This
// repairs node labels that got wrapped across lines on paste — e.g. a
// `["…long label…` continued on the next physical line — which a strictly
// line-based parser would otherwise reject. Continuation lines are joined
// with a single space.
func logicalLines(src string) []string {
	var out []string
	buf := ""
	for raw := range strings.SplitSeq(src, "\n") {
		seg := strings.TrimSpace(stripComment(raw))
		switch {
		case buf == "":
			buf = seg
		case seg == "":
			// blank physical line inside an open token: keep the open buffer.
		default:
			buf += " " + seg
		}
		if balanced(buf) {
			out = append(out, buf)
			buf = ""
		}
	}
	if buf != "" {
		out = append(out, buf)
	}
	return out
}

// balanced reports whether s has no open bracket or quote — i.e. it is a
// complete logical line. Brackets inside double quotes are ignored.
func balanced(s string) bool {
	depth := 0
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case inQuote:
			// brackets inside quoted labels don't affect nesting
		case r == '[' || r == '(' || r == '{':
			depth++
		case r == ']' || r == ')' || r == '}':
			depth--
		}
	}
	return depth <= 0 && !inQuote
}

// stripComment drops Mermaid comment lines. Mermaid comments must occupy
// their own line (`%% like this`); `%%` appearing mid-line — e.g. inside a
// quoted label like `A["50%% done"]` — is content, not a comment, so only
// whole lines starting with `%%` are removed.
func stripComment(line string) string {
	if strings.HasPrefix(strings.TrimSpace(line), "%%") {
		return ""
	}
	return line
}

// hasStructuralAmp reports whether line contains a `&` outside quotes, shape
// brackets, and `|…|` edge labels — Mermaid's multi-edge shorthand
// (`A --> B & C`), which we don't support. A `&` inside a label (`A[R&D]`,
// `-->|Q&A|`) is plain content.
func hasStructuralAmp(line string) bool {
	depth := 0
	inQuote := false
	inPipe := false
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
		case inQuote:
			// content inside quoted labels is never structural
		case r == '|':
			inPipe = !inPipe
		case inPipe:
			// content inside |…| edge labels is never structural
		case r == '[' || r == '(' || r == '{':
			depth++
		case r == ']' || r == ')' || r == '}':
			depth--
		case r == '&' && depth == 0:
			return true
		}
	}
	return false
}

// isIgnorableDirective reports lines we can safely skip without affecting the
// structure: styling/interaction directives, and subgraph grouping (which we
// render transparently — the inner nodes and edges still parse normally).
func isIgnorableDirective(line string) bool {
	if line == "end" || line == "subgraph" {
		return true // subgraph grouping is rendered transparently
	}
	for _, p := range []string{"classDef", "class ", "style ", "linkStyle", "click ", "direction ", "subgraph "} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

// isBailDirective reports non-flowchart diagram types we cannot render.
func isBailDirective(line string) bool {
	for _, p := range []string{"sequenceDiagram", "stateDiagram", "erDiagram", "gantt", "pie", "journey", "classDiagram", "mindmap", "timeline"} {
		if line == p || strings.HasPrefix(line, p+" ") {
			return true
		}
	}
	return false
}

// splitLabel splits a node label on Mermaid line breaks into display lines,
// falling back to the id when empty.
func splitLabel(label, id string) []string {
	var out []string
	for _, p := range brRe.Split(label, -1) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{id}
	}
	return out
}

// cleanEdgeLabel flattens an edge label to a single line (line breaks and
// runs of whitespace collapse to single spaces) and trims surrounding quotes.
func cleanEdgeLabel(s string) string {
	s = strings.Join(strings.Fields(brRe.ReplaceAllString(s, " ")), " ")
	return strings.Trim(s, `"`)
}

var mermaidBlockRe = regexp.MustCompile("(?s)```mermaid[ \t]*\r?\n(.*?)\r?\n```")

// Segment is a piece of findings content produced by SplitRenderable: either
// Markdown to be rendered by the caller's markdown renderer, or a Diagram —
// a pre-rendered ASCII diagram that must be printed verbatim (NOT through the
// markdown renderer, which would word-wrap and corrupt its alignment).
// Exactly one field is non-empty.
type Segment struct {
	Markdown string
	Diagram  string
}

// SplitRenderable splits findings markdown into segments, replacing each
// ```mermaid block that is a renderable flowchart with a Diagram segment.
// Blocks it cannot render are left inside the surrounding Markdown segment, so
// the raw Mermaid survives for the markdown renderer (and for GitHub/doc
// rendering).
func SplitRenderable(md string) []Segment {
	var segs []Segment
	last := 0
	for _, loc := range mermaidBlockRe.FindAllStringSubmatchIndex(md, -1) {
		ascii, ok := Render(md[loc[2]:loc[3]])
		if !ok {
			continue // leave this block in the surrounding markdown
		}
		if pre := md[last:loc[0]]; pre != "" {
			segs = append(segs, Segment{Markdown: pre})
		}
		segs = append(segs, Segment{Diagram: ascii})
		last = loc[1]
	}
	if rest := md[last:]; rest != "" {
		segs = append(segs, Segment{Markdown: rest})
	}
	return segs
}

package flowchart

import (
	"strings"
	"testing"
)

// lineOf returns the index of the first line containing sub, or -1.
func lineOf(out, sub string) int {
	for i, l := range strings.Split(out, "\n") {
		if strings.Contains(l, sub) {
			return i
		}
	}
	return -1
}

// renderable cases: a single rooted tree (chain or branch). We assert the
// labels appear top-to-bottom in DFS order and edge labels show in brackets.
func TestRender_RenderableTrees(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		src       string
		wantOrder []string // labels expected top-to-bottom
		wantSub   []string // other substrings expected somewhere
	}{
		{
			name:      "two node bracket labels",
			src:       "flowchart LR\n  A[Producer] --> B[Consumer]",
			wantOrder: []string{"Producer", "Consumer"},
		},
		{
			name:      "edge label on the connector",
			src:       "flowchart LR\n  A[Producer] -->|enqueue| B[Consumer]",
			wantOrder: []string{"Producer", "Consumer"},
			wantSub:   []string{"│ enqueue"},
		},
		{
			name: "multi node, order follows the path not declaration",
			src: "flowchart LR\n" +
				"  C[Consumer] --> R[Retries]\n" +
				"  P[Producer] --> I[Input]\n" +
				"  I --> C\n",
			wantOrder: []string{"Producer", "Input", "Consumer", "Retries"},
		},
		{
			name:      "chained on one line",
			src:       "flowchart LR\n  A --> B --> C",
			wantOrder: []string{"A", "B", "C"},
		},
		{
			name:      "rounded and diamond shapes",
			src:       "flowchart LR\n  A(Producer) --> B{Decision}",
			wantOrder: []string{"Producer", "Decision"},
		},
		{
			name:      "single node only",
			src:       "flowchart LR\n  A[Solo]",
			wantOrder: []string{"Solo"},
		},
		{
			name:      "quoted label with special chars",
			src:       "flowchart LR\n  A[\"sh -c 'exec entire'\"] --> B[Done]",
			wantOrder: []string{"sh -c 'exec entire'", "Done"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out, ok := Render(tc.src)
			if !ok {
				t.Fatalf("expected renderable, got ok=false for:\n%s", tc.src)
			}
			prev := -1
			for _, label := range tc.wantOrder {
				idx := strings.Index(out, label)
				if idx < 0 {
					t.Fatalf("label %q missing from output:\n%s", label, out)
				}
				if idx <= prev {
					t.Errorf("label %q out of order in output:\n%s", label, out)
				}
				prev = idx
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(out, sub) {
					t.Errorf("expected %q in output:\n%s", sub, out)
				}
			}
		})
	}
}

// A chain renders as boxes stacked top-down joined by ▼ arrows, one box per
// node, in path order.
func TestRender_VerticalFlow(t *testing.T) {
	t.Parallel()

	out, ok := Render("flowchart LR\n A[Root] --> B[Mid] --> C[Leaf]")
	if !ok {
		t.Fatalf("expected renderable")
	}
	if got := strings.Count(out, "┌"); got != 3 {
		t.Errorf("expected 3 boxes, got %d top-left corners:\n%s", got, out)
	}
	if got := strings.Count(out, "▼"); got != 2 {
		t.Errorf("expected 2 arrows for a 3-node chain, got %d:\n%s", got, out)
	}
	if lineOf(out, "Root") >= lineOf(out, "Mid") || lineOf(out, "Mid") >= lineOf(out, "Leaf") {
		t.Errorf("expected Root above Mid above Leaf:\n%s", out)
	}
}

// A fork renders both branches side-by-side under a ┴ distributor bar, each
// with its own labeled connector and ▼ arrow. Sibling boxes share rows.
func TestRender_Branches(t *testing.T) {
	t.Parallel()

	src := "flowchart LR\n" +
		"  R[Read stdin] -->|EOF| OK[Parse event]\n" +
		"  R -.->|no EOF| HANG[Hangs]\n"
	out, ok := Render(src)
	if !ok {
		t.Fatalf("expected renderable")
	}
	for _, want := range []string{"Read stdin", "Parse event", "Hangs", "│ EOF", "│ no EOF", "┴", "▼"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in branch output:\n%s", want, out)
		}
	}
	if lineOf(out, "Parse event") != lineOf(out, "Hangs") {
		t.Errorf("expected sibling branches side-by-side on the same line:\n%s", out)
	}
	if got := strings.Count(out, "▼"); got != 2 {
		t.Errorf("expected one arrow per branch, got %d:\n%s", got, out)
	}
}

// A back-edge (retry loop) renders the forward flow as a tree and the looping
// edge as a "↪" reference, rather than falling back to raw Mermaid.
func TestRender_CycleBecomesReference(t *testing.T) {
	t.Parallel()

	src := "flowchart LR\n" +
		"  A[Start] --> B[Work]\n" +
		"  B --> C[Done]\n" +
		"  C -->|retry| A\n"
	out, ok := Render(src)
	if !ok {
		t.Fatalf("expected cycle to render via reference, got fallback")
	}
	if !strings.Contains(out, "↪") {
		t.Errorf("expected a ↪ reference for the back-edge, got:\n%s", out)
	}
	if !strings.Contains(out, "╎ retry") {
		t.Errorf("expected the back-edge label on a dashed connector, got:\n%s", out)
	}
	// All three nodes appear, and Start is the root (flush left).
	for _, n := range []string{"Start", "Work", "Done"} {
		if !strings.Contains(out, n) {
			t.Errorf("missing node %q:\n%s", n, out)
		}
	}
}

// A subgraph wrapper is transparent: the inner nodes and edges still render.
func TestRender_SubgraphIsTransparent(t *testing.T) {
	t.Parallel()

	src := "flowchart LR\n" +
		"  subgraph grp [Group]\n" +
		"    A[Inner]\n" +
		"  end\n" +
		"  A --> B[Outer]\n"
	out, ok := Render(src)
	if !ok {
		t.Fatalf("expected subgraph to render transparently, got fallback")
	}
	if !strings.Contains(out, "Inner") || !strings.Contains(out, "Outer") {
		t.Errorf("expected inner+outer nodes, got:\n%s", out)
	}
}

// Fan-in (two arrows into one node) renders the second arrival as a reference.
func TestRender_FanInBecomesReference(t *testing.T) {
	t.Parallel()

	out, ok := Render("flowchart LR\n A --> C\n B --> C")
	if !ok {
		t.Fatalf("expected fan-in to render, got fallback")
	}
	if !strings.Contains(out, "↪") {
		t.Errorf("expected a ↪ reference for the converging edge, got:\n%s", out)
	}
}

// Multi-line (<br/>) labels render as stacked lines inside one box.
func TestRender_MultiLineLabels(t *testing.T) {
	t.Parallel()

	out, ok := Render("flowchart LR\n  A[\"first line<br/>second line\"] --> B[End]")
	if !ok {
		t.Fatalf("expected renderable")
	}
	first, second := lineOf(out, "first line"), lineOf(out, "second line")
	if first < 0 || second < 0 {
		t.Fatalf("expected both label lines present, got:\n%s", out)
	}
	if second != first+1 {
		t.Errorf("expected label lines stacked in one box (rows %d,%d):\n%s", first, second, out)
	}
	// Two boxes: the multi-line node and End.
	if got := strings.Count(out, "┌"); got != 2 {
		t.Errorf("expected 2 boxes, got %d:\n%s", got, out)
	}
}

// A node label wrapped across physical lines (a copy-paste artifact) is
// rejoined into one logical line rather than failing to parse.
func TestRender_WrappedLabel(t *testing.T) {
	t.Parallel()

	// The bracketed label is split mid-token across two lines.
	src := "flowchart LR\n" +
		"  A --> R[\"io.ReadAll stdin<br/>event.go:157<br/>NO timeout / NO tty\n" +
		"  guard\"]\n" +
		"  R --> B[Done]\n"
	out, ok := Render(src)
	if !ok {
		t.Fatalf("expected wrapped label to rejoin and render, got fallback")
	}
	if !strings.Contains(out, "NO timeout / NO tty guard") {
		t.Errorf("expected the wrapped label rejoined with a space, got:\n%s", out)
	}
	if !strings.Contains(out, "Done") {
		t.Errorf("expected the edge after the wrapped node to render, got:\n%s", out)
	}
}

// An & inside a label or quoted edge label is content; only the structural
// multi-edge shorthand (`A --> B & C`) forces fallback.
func TestRender_AmpersandInLabel(t *testing.T) {
	t.Parallel()

	out, ok := Render("flowchart LR\n  A[R&D team] -->|Q&A pass| B[Ship]")
	if !ok {
		t.Fatalf("expected & inside labels to render, got fallback")
	}
	if !strings.Contains(out, "R&D team") || !strings.Contains(out, "Q&A pass") {
		t.Errorf("expected & labels preserved, got:\n%s", out)
	}
}

// Mermaid comments occupy whole lines; %% inside a label is content.
func TestRender_PercentHandling(t *testing.T) {
	t.Parallel()

	src := "flowchart LR\n" +
		"  %% this whole line is a comment\n" +
		"  A[\"50%% done\"] --> B[Finish]\n"
	out, ok := Render(src)
	if !ok {
		t.Fatalf("expected %%%% in label to render, got fallback")
	}
	if !strings.Contains(out, "50%% done") {
		t.Errorf("expected label with %%%% preserved, got:\n%s", out)
	}
	if strings.Contains(out, "comment") {
		t.Errorf("expected comment line dropped, got:\n%s", out)
	}
}

// Double-width runes (CJK) must not skew sibling alignment: the rendered
// rows contain no shadow placeholders, and sibling boxes still share rows.
func TestRender_WideRunes(t *testing.T) {
	t.Parallel()

	src := "flowchart LR\n" +
		"  A[日本語のラベル] -->|はい| B[完了]\n" +
		"  A -->|no| C[Retry]\n"
	out, ok := Render(src)
	if !ok {
		t.Fatalf("expected CJK labels to render")
	}
	if strings.ContainsRune(out, '\x00') {
		t.Errorf("expected no shadow placeholders in output:\n%s", out)
	}
	if lineOf(out, "完了") != lineOf(out, "Retry") {
		t.Errorf("expected sibling boxes on the same row despite wide runes:\n%s", out)
	}
}

// not-renderable cases must return ok=false so the caller can fall back to
// the raw mermaid source. We never render a misleading partial diagram.
func TestRender_FallsBack(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  string
	}{
		{name: "ampersand multi target", src: "flowchart LR\n A --> B & C"},
		{name: "unrecognized line", src: "flowchart LR\n A --> B\n this is not valid"},
		{name: "empty", src: ""},
		{name: "header only", src: "flowchart LR"},
		{name: "not a flowchart", src: "sequenceDiagram\n Alice->>Bob: hi"},
		{name: "class diagram", src: "classDiagram\n Animal <|-- Duck"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if out, ok := Render(tc.src); ok {
				t.Errorf("expected ok=false (fallback) for %q, got:\n%s", tc.name, out)
			}
		})
	}
}

func TestSplitRenderable(t *testing.T) {
	t.Parallel()

	t.Run("renderable block becomes a diagram segment", func(t *testing.T) {
		t.Parallel()

		md := "## System\n\n```mermaid\nflowchart LR\n A[Producer] --> B[Consumer]\n```\n\nrest\n"
		segs := SplitRenderable(md)
		diagrams, markdown := 0, 0
		var diagram string
		for _, s := range segs {
			if s.Diagram != "" {
				diagrams++
				diagram = s.Diagram
			}
			if s.Markdown != "" {
				markdown++
			}
		}
		if diagrams != 1 {
			t.Fatalf("expected exactly one diagram segment, got %d:\n%#v", diagrams, segs)
		}
		if markdown < 1 {
			t.Errorf("expected surrounding markdown segment(s), got %d", markdown)
		}
		if strings.Contains(diagram, "```") {
			t.Errorf("diagram segment must not contain a fence:\n%s", diagram)
		}
		if !strings.Contains(diagram, "Producer") || !strings.Contains(diagram, "Consumer") {
			t.Errorf("diagram missing labels:\n%s", diagram)
		}
		joined := joinMarkdown(segs)
		if !strings.Contains(joined, "## System") || !strings.Contains(joined, "rest") {
			t.Errorf("expected surrounding markdown preserved, got:\n%s", joined)
		}
	})

	t.Run("unrenderable block stays in markdown", func(t *testing.T) {
		t.Parallel()

		md := "```mermaid\nsequenceDiagram\n Alice->>Bob: hi\n```\n"
		segs := SplitRenderable(md)
		if len(segs) != 1 || segs[0].Diagram != "" || segs[0].Markdown != md {
			t.Errorf("expected single untouched markdown segment, got:\n%#v", segs)
		}
	})

	t.Run("non-mermaid content is one markdown segment", func(t *testing.T) {
		t.Parallel()

		md := "# Title\n\nSome text and a ```go\nfunc x(){}\n``` block.\n"
		segs := SplitRenderable(md)
		if len(segs) != 1 || segs[0].Markdown != md {
			t.Errorf("expected single markdown segment, got:\n%#v", segs)
		}
	})

	t.Run("multiple blocks handled independently", func(t *testing.T) {
		t.Parallel()

		md := "```mermaid\nflowchart LR\n A[X] --> B[Y]\n```\n\nmid\n\n```mermaid\nsequenceDiagram\n P->>Q: ping\n```\n"
		segs := SplitRenderable(md)
		diagrams := 0
		for _, s := range segs {
			if s.Diagram != "" {
				diagrams++
			}
		}
		// first (flowchart) → diagram; second (sequence) → stays markdown.
		if diagrams != 1 {
			t.Errorf("expected exactly one diagram segment, got %d:\n%#v", diagrams, segs)
		}
		if !strings.Contains(joinMarkdown(segs), "```mermaid") {
			t.Errorf("expected the unrenderable mermaid block to remain:\n%#v", segs)
		}
	})
}

func joinMarkdown(segs []Segment) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.Markdown)
	}
	return b.String()
}

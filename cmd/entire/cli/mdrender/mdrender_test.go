package mdrender_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/mdrender"
)

// TestRender_DarkBackgroundProducesAnsi verifies the renderer emits ANSI
// escape sequences (i.e. actually styled output) when explicitly asked.
// We don't pin specific colors — glamour version bumps shift exact codes —
// just that something terminal-styled comes out.
func TestRender_DarkBackgroundProducesAnsi(t *testing.T) {
	t.Parallel()

	out, err := mdrender.Render("# Hello\n\nworld", 80, true)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("expected ANSI escape codes in output, got plain string: %q", out)
	}
	if !strings.Contains(out, "Hello") {
		t.Errorf("expected heading text in output, got: %q", out)
	}
	if !strings.Contains(out, "world") {
		t.Errorf("expected body text in output, got: %q", out)
	}
}

// TestRender_LightBackgroundAlsoProducesAnsi verifies the light palette
// also produces styled output (regression: a misconfigured StyleConfig
// could degrade light mode to plain text).
func TestRender_LightBackgroundAlsoProducesAnsi(t *testing.T) {
	t.Parallel()

	out, err := mdrender.Render("# Hello", 80, false)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("expected ANSI escape codes for light bg, got: %q", out)
	}
}

func TestRender_CodeBlockDoesNotPanic(t *testing.T) {
	t.Parallel()

	const md = "```go\nfunc main() {\n\tprintln(\"ok\")\n}\n```\n"
	cases := []struct {
		name string
		dark bool
	}{
		{name: "dark", dark: true},
		{name: "light", dark: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out, err := mdrender.Render(md, 80, tc.dark)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if !strings.Contains(out, "func") {
				t.Errorf("expected rendered code block to contain code, got: %q", out)
			}
			if !strings.Contains(out, "\x1b[") {
				t.Errorf("expected rendered code block to contain ANSI styling, got: %q", out)
			}
		})
	}
}

// TestRenderForWriter_NonTTYReturnsRawMarkdown verifies the TTY-aware path
// passes markdown through unchanged when w is a *bytes.Buffer (not a TTY).
// This is the path entire review uses when stdout is redirected, so the
// output stays grep-friendly.
func TestRenderForWriter_NonTTYReturnsRawMarkdown(t *testing.T) {
	t.Parallel()

	const md = "# Heading\n\n- item one\n- item two\n"
	out, err := mdrender.RenderForWriter(&bytes.Buffer{}, md)
	if err != nil {
		t.Fatalf("RenderForWriter: %v", err)
	}
	if out != md {
		t.Errorf("expected raw markdown for non-TTY writer, got:\n%q\nwant:\n%q", out, md)
	}
}

// TestRenderForWriter_NoColorEnvForcesRaw verifies NO_COLOR=1 also disables
// rendering. Note: this test sets NO_COLOR via t.Setenv so it cannot use
// t.Parallel — process-level env mutation would race other tests.
func TestRenderForWriter_NoColorEnvForcesRaw(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	const md = "# Heading"
	out, err := mdrender.RenderForWriter(&bytes.Buffer{}, md)
	if err != nil {
		t.Fatalf("RenderForWriter: %v", err)
	}
	if out != md {
		t.Errorf("expected raw markdown when NO_COLOR set, got: %q", out)
	}
}

// TestRender_EmptyInputDoesNotPanic verifies the renderer handles edge cases
// (empty string, whitespace-only) without erroring.
func TestRender_EmptyInputDoesNotPanic(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "   ", "\n\n"} {
		out, err := mdrender.Render(input, 80, true)
		if err != nil {
			t.Errorf("Render(%q): %v", input, err)
		}
		_ = out // result content irrelevant; not panicking is the assertion
	}
}

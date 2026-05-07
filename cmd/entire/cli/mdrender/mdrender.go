// Package mdrender renders markdown to terminal-styled output using the
// shared entire CLI palette (orange H1, cyan H2, indigo H3, plus chroma
// syntax highlighting). Used by `entire dispatch`, `entire review`, and
// any other command that prints LLM-generated markdown to the terminal.
//
// Two entry points:
//   - Render: pure function — caller supplies width and background hint.
//   - RenderForWriter: TTY-aware — renders to a terminal writer with
//     auto-detected width; passes raw markdown through when w is not a
//     terminal (so redirected output is grep-friendly, not full of ANSI).
package mdrender

import (
	"fmt"
	"io"
	"os"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	glamourstyles "charm.land/glamour/v2/styles"
	"github.com/muesli/termenv"
	"golang.org/x/term"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

// DefaultTerminalWidth caps glamour word-wrap when no real terminal width
// is available. Matches the cap used by status_style.getTerminalWidth.
const DefaultTerminalWidth = 80

// Render produces a glamour-styled string from markdown using the entire
// CLI palette. width is the word-wrap target; darkBackground selects the
// dark or light palette variant.
//
// Errors only on glamour renderer construction or render failure — both
// of which indicate a malformed StyleConfig (programmer error) rather
// than a runtime condition. Renderer panics are recovered and returned as
// errors so callers can fall back to raw markdown instead of crashing.
func Render(markdown string, width int, darkBackground bool) (rendered string, err error) {
	defer func() {
		if r := recover(); r != nil {
			rendered = ""
			err = fmt.Errorf("render markdown panic: %v", r)
		}
	}()

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(stylesForBackground(darkBackground)),
		glamour.WithWordWrap(width),
		glamour.WithPreservedNewLines(),
	)
	if err != nil {
		return "", fmt.Errorf("initialize markdown renderer: %w", err)
	}
	rendered, err = renderer.Render(markdown)
	if err != nil {
		return "", fmt.Errorf("render markdown: %w", err)
	}
	return rendered, nil
}

// RenderForWriter renders markdown when w is a terminal writer, and returns
// the input unchanged otherwise. NO_COLOR=1 also disables rendering so
// pipelines that grep through redirected output work without unwrapping
// ANSI escape sequences.
//
// Width is auto-detected from w (capped at 80); background palette is
// detected via termenv.HasDarkBackground.
func RenderForWriter(w io.Writer, markdown string) (string, error) {
	if !shouldRender(w) {
		return markdown, nil
	}
	return Render(markdown, terminalWidth(w), termenv.HasDarkBackground())
}

// shouldRender returns true if w is a terminal writer and NO_COLOR is unset.
func shouldRender(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return interactive.IsTerminalWriter(w)
}

// terminalWidth returns the writer's terminal width capped at 80.
// Falls back to stdout/stderr probing, then DefaultTerminalWidth.
func terminalWidth(w io.Writer) int {
	if f, ok := w.(*os.File); ok {
		if width, _, err := term.GetSize(int(f.Fd())); err == nil && width > 0 { //nolint:gosec // G115: uintptr->int is safe for fd
			return min(width, DefaultTerminalWidth)
		}
	}
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		if f == nil {
			continue
		}
		if width, _, err := term.GetSize(int(f.Fd())); err == nil && width > 0 { //nolint:gosec // G115: uintptr->int is safe for fd
			return min(width, DefaultTerminalWidth)
		}
	}
	return DefaultTerminalWidth
}

// stylesForBackground returns the entire CLI's glamour StyleConfig.
//
// Palette:
//   - H1: orange (#fb923c) — agent name, top-level section
//   - H2: cyan (#22d3ee) — secondary headings, links
//   - H3: indigo (#818cf8) — tertiary headings, enumerations, keywords
//   - List items: orange
//   - Inline code: orange
//   - Code-block chroma: indigo keywords, cyan function names, amber literals
func stylesForBackground(darkBackground bool) ansi.StyleConfig {
	var styles ansi.StyleConfig
	if darkBackground {
		styles = glamourstyles.DarkStyleConfig
	} else {
		styles = glamourstyles.LightStyleConfig
	}

	if darkBackground {
		styles.Document.Color = strPtr("252")
		styles.Heading.Color = strPtr("252")
		styles.Code.BackgroundColor = strPtr("236")
		styles.CodeBlock.Color = strPtr("252")
	} else {
		styles.Document.Color = strPtr("234")
		styles.Heading.Color = strPtr("234")
		styles.Code.BackgroundColor = strPtr("254")
		styles.CodeBlock.Color = strPtr("242")
	}
	styles.Heading.Bold = boolPtrV(true)

	styles.H1.Prefix = "# "
	styles.H1.Suffix = ""
	styles.H1.Color = strPtr("#fb923c")
	styles.H1.BackgroundColor = nil
	styles.H1.Bold = boolPtrV(true)

	styles.H2.Color = strPtr("#22d3ee")
	styles.H2.Bold = boolPtrV(true)
	styles.H3.Color = strPtr("#818cf8")
	styles.H3.Bold = boolPtrV(true)
	styles.H4.Color = strPtr("252")
	styles.H4.Bold = boolPtrV(true)
	styles.H5.Color = strPtr("245")
	styles.H5.Bold = boolPtrV(true)
	styles.H6.Color = strPtr("245")
	styles.H6.Bold = boolPtrV(false)

	styles.HorizontalRule.Color = strPtr("240")
	styles.Item.Color = strPtr("#fb923c")
	styles.Enumeration.Color = strPtr("#818cf8")
	styles.BlockQuote.Color = strPtr("245")

	styles.Link.Color = strPtr("#22d3ee")
	styles.Link.Underline = boolPtrV(true)
	styles.LinkText.Color = strPtr("#818cf8")
	styles.LinkText.Bold = boolPtrV(true)

	styles.Code.Color = strPtr("#fb923c")
	styles.CodeBlock.Chroma = chromaForBackground(darkBackground)

	styles.Table.Color = strPtr("245")
	styles.Table.CenterSeparator = strPtr(" ")
	styles.Table.ColumnSeparator = strPtr(" ")
	styles.Table.RowSeparator = strPtr("-")

	return styles
}

// chromaForBackground returns the syntax-highlighting palette for code
// blocks. Dark and light backgrounds use distinct text colors but share
// the same accent colors for keywords/functions/literals.
func chromaForBackground(darkBackground bool) *ansi.Chroma {
	textColor := "#2A2A2A"
	commentColor := "#8D8D8D"
	punctColor := "#7A7A7A"
	bgColor := "#E4E4E4"
	if darkBackground {
		textColor = "#D0D0D0"
		commentColor = "#8A8A8A"
		punctColor = "#808080"
		bgColor = "#303030"
	}
	return &ansi.Chroma{
		Text:            ansi.StylePrimitive{Color: strPtr(textColor)},
		Error:           ansi.StylePrimitive{Color: strPtr(textColor)},
		Comment:         ansi.StylePrimitive{Color: strPtr(commentColor), Italic: boolPtrV(true)},
		Keyword:         ansi.StylePrimitive{Color: strPtr("#818cf8"), Bold: boolPtrV(true)},
		KeywordReserved: ansi.StylePrimitive{Color: strPtr("#818cf8"), Bold: boolPtrV(true)},
		Name:            ansi.StylePrimitive{Color: strPtr(textColor)},
		NameFunction:    ansi.StylePrimitive{Color: strPtr("#22d3ee")},
		NameBuiltin:     ansi.StylePrimitive{Color: strPtr("#818cf8")},
		Literal:         ansi.StylePrimitive{Color: strPtr("#fbbf24")},
		LiteralString:   ansi.StylePrimitive{Color: strPtr("#fbbf24")},
		LiteralNumber:   ansi.StylePrimitive{Color: strPtr("#fbbf24")},
		Operator:        ansi.StylePrimitive{Color: strPtr(punctColor)},
		Punctuation:     ansi.StylePrimitive{Color: strPtr(punctColor)},
		GenericDeleted:  ansi.StylePrimitive{Color: strPtr("#EF4444")},
		GenericInserted: ansi.StylePrimitive{Color: strPtr("#22C55E")},
		Background:      ansi.StylePrimitive{BackgroundColor: strPtr(bgColor)},
	}
}

func strPtr(v string) *string { return &v }
func boolPtrV(v bool) *bool   { return &v }

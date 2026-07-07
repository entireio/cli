// Package mdrender renders markdown to terminal-styled output using the
// shared entire CLI base16 palette (magenta H1, cyan H2, blue H3, plus chroma
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
	"github.com/entireio/cli/cmd/entire/cli/palette"
)

// DefaultTerminalWidth caps glamour word-wrap when no real terminal width
// is available. Matches the cap used by status_style.getTerminalWidth.
const DefaultTerminalWidth = 80

// MaxRenderBytes caps glamour input: its render cost is super-linear (~6s at
// 2MB, minutes beyond), so above this we return raw markdown unchanged.
const MaxRenderBytes = 256 * 1024

// Render produces a glamour-styled string from markdown using the entire
// CLI palette. width is the word-wrap target; darkBackground selects the
// dark or light palette variant.
//
// Errors only on glamour renderer construction or render failure — both
// of which indicate a malformed StyleConfig (programmer error) rather
// than a runtime condition. Renderer panics are recovered and returned as
// errors so callers can fall back to raw markdown instead of crashing.
func Render(markdown string, width int, darkBackground bool) (rendered string, err error) {
	if len(markdown) > MaxRenderBytes {
		return markdown, nil
	}

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

// shouldRender returns true when styled output is appropriate for w
// (terminal writer, NO_COLOR unset, no legacy console) — see
// interactive.ShouldStyle.
func shouldRender(w io.Writer) bool {
	return interactive.ShouldStyle(w)
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

// stylesForBackground returns the entire CLI's glamour StyleConfig, using
// only base16 (ANSI 0–15) colors.
//
// Palette:
//   - H1: magenta — agent name, top-level section
//   - H2: cyan — secondary headings, links
//   - H3: blue — tertiary headings, enumerations, keywords
//   - List items: magenta
//   - Inline code: magenta
//   - Code-block chroma: hex palette (see chromaForBackground — glamour's
//     chroma parser requires hex, so this is the one non-base16 exception)
//
// Body/heading text is left unset so it uses the terminal's default
// foreground, which inverts with the background (dark text on light, light on
// dark). Accent colors are ANSI slots the terminal already remaps per theme,
// so the StyleConfig itself no longer needs a dark/light branch — only the
// chroma block does.
func stylesForBackground(darkBackground bool) ansi.StyleConfig {
	var styles ansi.StyleConfig
	if darkBackground {
		styles = glamourstyles.DarkStyleConfig
	} else {
		styles = glamourstyles.LightStyleConfig
	}

	// Body text: leave colors unset so glamour uses the terminal's default
	// foreground (which inverts with the background) instead of a pinned slot.
	styles.Document.Color = nil
	styles.Heading.Color = nil
	styles.Code.BackgroundColor = nil // use terminal default background
	styles.CodeBlock.Color = nil
	styles.Heading.Bold = boolPtrV(true)

	styles.H1.Prefix = "# "
	styles.H1.Suffix = ""
	styles.H1.Color = strPtr(palette.Accent)
	styles.H1.BackgroundColor = nil
	styles.H1.Bold = boolPtrV(true)

	styles.H2.Color = strPtr(palette.Cyan)
	styles.H2.Bold = boolPtrV(true)
	styles.H3.Color = strPtr(palette.Blue)
	styles.H3.Bold = boolPtrV(true)
	styles.H4.Color = nil // default fg (inverts with terminal theme)
	styles.H4.Bold = boolPtrV(true)
	styles.H5.Color = strPtr(palette.Muted)
	styles.H5.Bold = boolPtrV(true)
	styles.H6.Color = strPtr(palette.Muted)
	styles.H6.Bold = boolPtrV(false)

	styles.HorizontalRule.Color = strPtr(palette.Muted)
	styles.Item.Color = strPtr(palette.Accent)
	styles.Enumeration.Color = strPtr(palette.Blue)
	styles.BlockQuote.Color = strPtr(palette.Muted)

	styles.Link.Color = strPtr(palette.Cyan)
	styles.Link.Underline = boolPtrV(true)
	styles.LinkText.Color = strPtr(palette.Blue)
	styles.LinkText.Bold = boolPtrV(true)

	styles.Code.Color = strPtr(palette.Accent)
	styles.CodeBlock.Chroma = chromaForBackground(darkBackground)

	styles.Table.Color = strPtr(palette.Muted)
	styles.Table.CenterSeparator = strPtr(" ")
	styles.Table.ColumnSeparator = strPtr(" ")
	styles.Table.RowSeparator = strPtr("-")

	return styles
}

// chromaForBackground returns the syntax-highlighting palette for code blocks.
//
// NOTE: unlike the rest of mdrender, the chroma block must use hex colors.
// glamour parses these through the chroma library's color parser, which only
// accepts hex (#rrggbb) — bare ANSI palette indices like "5" are rejected at
// render time (panic: unknown style element). So code-block syntax colors are
// the one place the CLI can't express its palette in base16; we keep the hex
// values closest in hue to the base16 accents used elsewhere (blue keywords,
// cyan functions, amber/yellow literals, red/green diff markers).
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

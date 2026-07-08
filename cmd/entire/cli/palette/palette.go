// Package palette is the single source of truth for terminal colors used
// across the Entire CLI. Every color is a base16 (ANSI 0–15) slot so the UI
// respects the user's terminal theme and stays internally consistent.
//
// Colors are plain string constants (not lipgloss.Color values) so this package
// has zero dependencies and can be imported by any other package — cli, recap,
// mdrender, search, etc. — without import cycles. Callers wrap as needed, e.g.
// lipgloss.Color(palette.Accent).
//
// Use the bright variants (8–15) and lipgloss Faint(true) ("dim") for visual
// hierarchy rather than reaching for extended 256-color codes.
package palette

// Base16 ANSI slots.
const (
	Black         = "0"
	Red           = "1"
	Green         = "2"
	Yellow        = "3"
	Blue          = "4"
	Magenta       = "5"
	Cyan          = "6"
	White         = "7"
	BrightBlack   = "8"
	BrightRed     = "9"
	BrightGreen   = "10"
	BrightYellow  = "11"
	BrightBlue    = "12"
	BrightMagenta = "13"
	BrightCyan    = "14"
	BrightWhite   = "15"
)

// Semantic aliases — prefer these in styles; reserve the raw slot names for
// one-off uses where no semantic meaning applies.
//
// There is deliberately no "primary text" alias: primary/body text should be
// left unstyled (no Foreground) so it uses the terminal's default foreground,
// which inverts with the background. Pinning a slot like White ("7") makes text
// disappear on light terminals.
const (
	Accent  = Magenta       // primary brand accent (was orange)
	Accent2 = BrightMagenta // secondary accent (detail framing, team)
	Muted   = BrightBlack   // all dim/secondary text & borders (was 8/240/241/243/245)
	Success = Green
	Error   = Red
	Warning = Yellow
	Info    = Cyan
)

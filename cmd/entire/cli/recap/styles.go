package recap

import "github.com/charmbracelet/lipgloss"

// Palette colors. Reuse the same lipgloss.Color values as search_tui.go,
// status_style.go, and PR #999's activity_render.go so the recap view feels
// like it belongs to the CLI rather than a foreign body.
const (
	colorAccent    = "214" // amber — panel titles, agent names
	colorMuted     = "8"   // dark gray — labels, units
	colorHelpKey   = "245" // slightly brighter gray for key cues
	colorHelpSep   = "241" // dim gray for separators in help line
	colorBorder    = "243" // panel borders
	colorAdd       = "2"   // green — push / additions
	colorDel       = "1"   // red — destructive
	colorWarn      = "3"   // yellow — clean
	colorInfo      = "6"   // cyan — resume
	colorNeutral   = "7"   // light gray — default body
	colorAccentDim = "240" // dim amber for faded accents
)

// Styles holds pre-built lipgloss styles by semantic role. Declaring
// roles (not colors) lets rendering code say `styles.accent.Render(name)`
// without caring which Pantone chip is active — and lets us retune the
// whole palette in one place.
type Styles struct {
	bold   lipgloss.Style
	dim    lipgloss.Style
	label  lipgloss.Style
	value  lipgloss.Style
	unit   lipgloss.Style
	title  lipgloss.Style
	accent lipgloss.Style
	border lipgloss.Style
	add    lipgloss.Style
	del    lipgloss.Style
	warn   lipgloss.Style
	info   lipgloss.Style
	muted  lipgloss.Style
	help   lipgloss.Style
	key    lipgloss.Style

	// hintResume/Commit/Push/Clean map the ActionHint values to styles so
	// the session list renders consistent colors without a switch at each
	// call site.
	hintResume lipgloss.Style
	hintCommit lipgloss.Style
	hintPush   lipgloss.Style
	hintClean  lipgloss.Style
}

// NewStyles returns a style set. When useColor is false (piped stdout,
// ACCESSIBLE=1), every style becomes a no-op so rendered output contains
// no ANSI escapes.
func NewStyles(useColor bool) Styles {
	if !useColor {
		return Styles{} // all zero-value lipgloss.Style → plain text
	}
	return Styles{
		bold:       lipgloss.NewStyle().Bold(true),
		dim:        lipgloss.NewStyle().Faint(true),
		label:      lipgloss.NewStyle().Foreground(lipgloss.Color(colorMuted)).Bold(true),
		value:      lipgloss.NewStyle().Bold(true),
		unit:       lipgloss.NewStyle().Foreground(lipgloss.Color(colorMuted)),
		title:      lipgloss.NewStyle().Foreground(lipgloss.Color(colorAccent)).Bold(true),
		accent:     lipgloss.NewStyle().Foreground(lipgloss.Color(colorAccent)),
		border:     lipgloss.NewStyle().Foreground(lipgloss.Color(colorBorder)),
		add:        lipgloss.NewStyle().Foreground(lipgloss.Color(colorAdd)),
		del:        lipgloss.NewStyle().Foreground(lipgloss.Color(colorDel)),
		warn:       lipgloss.NewStyle().Foreground(lipgloss.Color(colorWarn)),
		info:       lipgloss.NewStyle().Foreground(lipgloss.Color(colorInfo)),
		muted:      lipgloss.NewStyle().Foreground(lipgloss.Color(colorMuted)),
		help:       lipgloss.NewStyle().Foreground(lipgloss.Color(colorHelpSep)),
		key:        lipgloss.NewStyle().Foreground(lipgloss.Color(colorHelpKey)).Bold(true),
		hintResume: lipgloss.NewStyle().Foreground(lipgloss.Color(colorInfo)),
		hintCommit: lipgloss.NewStyle().Foreground(lipgloss.Color(colorAccent)),
		hintPush:   lipgloss.NewStyle().Foreground(lipgloss.Color(colorAdd)),
		hintClean:  lipgloss.NewStyle().Foreground(lipgloss.Color(colorWarn)),
	}
}

// styleForHint returns the style that renders a given ActionHint. Returns a
// zero Style (plain) for ActionNone so callers don't have to handle nil.
func (s Styles) styleForHint(h ActionHint) lipgloss.Style {
	switch h {
	case ActionResume:
		return s.hintResume
	case ActionCommit:
		return s.hintCommit
	case ActionPush:
		return s.hintPush
	case ActionClean:
		return s.hintClean
	case ActionNone:
		return lipgloss.Style{}
	}
	return lipgloss.Style{}
}

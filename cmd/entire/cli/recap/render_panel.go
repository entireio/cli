package recap

import (
	"github.com/charmbracelet/lipgloss"
)

// renderPanel wraps body in a bordered block titled at the top-left corner.
// Width is the outer width of the panel (including borders). When useColor
// is false in styles, the border renders as plain ASCII characters instead
// of Unicode corners so piped output stays readable.
//
// Body is passed through verbatim — it may contain ANSI-styled text, blank
// lines, or internal spacing; we don't second-guess its content. Callers
// responsible for fitting body to `width - 4` (2 border + 2 padding).
func renderPanel(title, body string, width int, styles Styles) string {
	if width < 10 {
		width = 10 // don't draw a panel narrower than its minimal title chrome
	}
	border := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(colorBorder)).
		Padding(0, 2).
		Width(width - 2) // subtract border chars; lipgloss handles the rest
	if title == "" {
		return border.Render(body)
	}
	titled := styles.title.Render(title) + "\n\n" + body
	return border.Render(titled)
}

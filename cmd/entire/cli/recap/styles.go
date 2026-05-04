package recap

import (
	"io"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

const (
	colorAccent = "214"
	colorMuted  = "8"
	colorBorder = "243"
	colorInfo   = "6"
	colorTeam   = "170"

	colorActivityEmpty = "240"
	colorActivityLow   = "6"
	colorActivityMid   = "214"
)

type staticStyles struct {
	accent        lipgloss.Style
	activityEmpty lipgloss.Style
	activityHigh  lipgloss.Style
	activityLow   lipgloss.Style
	activityMid   lipgloss.Style
	border        lipgloss.Style
	info          lipgloss.Style
	muted         lipgloss.Style
	team          lipgloss.Style
	title         lipgloss.Style
	value         lipgloss.Style
}

func newStaticStyles(useColor bool) staticStyles {
	if !useColor {
		return staticStyles{}
	}
	renderer := lipgloss.NewRenderer(io.Discard)
	renderer.SetColorProfile(termenv.ANSI256)
	return staticStyles{
		accent:        renderer.NewStyle().Foreground(lipgloss.Color(colorAccent)),
		activityEmpty: renderer.NewStyle().Foreground(lipgloss.Color(colorActivityEmpty)),
		activityHigh:  renderer.NewStyle().Foreground(lipgloss.Color(colorActivityMid)).Bold(true),
		activityLow:   renderer.NewStyle().Foreground(lipgloss.Color(colorActivityLow)),
		activityMid:   renderer.NewStyle().Foreground(lipgloss.Color(colorActivityMid)),
		border:        renderer.NewStyle().Foreground(lipgloss.Color(colorBorder)),
		info:          renderer.NewStyle().Foreground(lipgloss.Color(colorInfo)),
		muted:         renderer.NewStyle().Foreground(lipgloss.Color(colorMuted)),
		team:          renderer.NewStyle().Foreground(lipgloss.Color(colorTeam)).Bold(true),
		title:         renderer.NewStyle().Foreground(lipgloss.Color(colorAccent)).Bold(true),
		value:         renderer.NewStyle().Bold(true),
	}
}

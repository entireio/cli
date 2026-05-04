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

	colorLabelFeature     = "42"
	colorLabelFix         = "203"
	colorLabelInformation = "81"
	colorLabelPerformance = "214"
	colorLabelRefactor    = "220"
	colorLabelTesting     = "170"
)

type staticStyles struct {
	accent        lipgloss.Style
	activityEmpty lipgloss.Style
	activityHigh  lipgloss.Style
	activityLow   lipgloss.Style
	activityMid   lipgloss.Style
	border        lipgloss.Style
	info          lipgloss.Style
	labelFeature  lipgloss.Style
	labelFix      lipgloss.Style
	labelInfo     lipgloss.Style
	labelPerf     lipgloss.Style
	labelRefactor lipgloss.Style
	labelTesting  lipgloss.Style
	muted         lipgloss.Style
	skill         lipgloss.Style
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
		labelFeature:  renderer.NewStyle().Foreground(lipgloss.Color(colorLabelFeature)),
		labelFix:      renderer.NewStyle().Foreground(lipgloss.Color(colorLabelFix)),
		labelInfo:     renderer.NewStyle().Foreground(lipgloss.Color(colorLabelInformation)),
		labelPerf:     renderer.NewStyle().Foreground(lipgloss.Color(colorLabelPerformance)),
		labelRefactor: renderer.NewStyle().Foreground(lipgloss.Color(colorLabelRefactor)),
		labelTesting:  renderer.NewStyle().Foreground(lipgloss.Color(colorLabelTesting)),
		muted:         renderer.NewStyle().Foreground(lipgloss.Color(colorMuted)),
		skill:         renderer.NewStyle().Foreground(lipgloss.Color(colorInfo)),
		team:          renderer.NewStyle().Foreground(lipgloss.Color(colorTeam)).Bold(true),
		title:         renderer.NewStyle().Foreground(lipgloss.Color(colorAccent)).Bold(true),
		value:         renderer.NewStyle().Bold(true),
	}
}

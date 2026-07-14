package recap

import (
	"charm.land/lipgloss/v2"

	"github.com/entireio/cli/cmd/entire/cli/palette"
)

const (
	colorAccent = palette.Accent
	colorMuted  = palette.Muted
	colorBorder = palette.Muted
	colorInfo   = palette.Info
	colorTeam   = palette.Accent2

	colorActivityEmpty = palette.Muted
	colorActivityLow   = palette.Cyan
	colorActivityMid   = palette.Accent

	colorLabelFeature     = palette.Green
	colorLabelFix         = palette.Red
	colorLabelInformation = palette.Cyan
	colorLabelPerformance = palette.Yellow
	colorLabelRefactor    = palette.BrightYellow
	colorLabelTesting     = palette.Magenta
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
	return staticStyles{
		accent:        lipgloss.NewStyle().Foreground(lipgloss.Color(colorAccent)),
		activityEmpty: lipgloss.NewStyle().Foreground(lipgloss.Color(colorActivityEmpty)),
		activityHigh:  lipgloss.NewStyle().Foreground(lipgloss.Color(colorActivityMid)).Bold(true),
		activityLow:   lipgloss.NewStyle().Foreground(lipgloss.Color(colorActivityLow)),
		activityMid:   lipgloss.NewStyle().Foreground(lipgloss.Color(colorActivityMid)),
		border:        lipgloss.NewStyle().Foreground(lipgloss.Color(colorBorder)),
		info:          lipgloss.NewStyle().Foreground(lipgloss.Color(colorInfo)),
		labelFeature:  lipgloss.NewStyle().Foreground(lipgloss.Color(colorLabelFeature)),
		labelFix:      lipgloss.NewStyle().Foreground(lipgloss.Color(colorLabelFix)),
		labelInfo:     lipgloss.NewStyle().Foreground(lipgloss.Color(colorLabelInformation)),
		labelPerf:     lipgloss.NewStyle().Foreground(lipgloss.Color(colorLabelPerformance)),
		labelRefactor: lipgloss.NewStyle().Foreground(lipgloss.Color(colorLabelRefactor)),
		labelTesting:  lipgloss.NewStyle().Foreground(lipgloss.Color(colorLabelTesting)),
		muted:         lipgloss.NewStyle().Foreground(lipgloss.Color(colorMuted)),
		skill:         lipgloss.NewStyle().Foreground(lipgloss.Color(colorInfo)),
		team:          lipgloss.NewStyle().Foreground(lipgloss.Color(colorTeam)).Bold(true),
		title:         lipgloss.NewStyle().Foreground(lipgloss.Color(colorAccent)).Bold(true),
		value:         lipgloss.NewStyle().Bold(true),
	}
}

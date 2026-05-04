package recap

import "charm.land/lipgloss/v2"

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

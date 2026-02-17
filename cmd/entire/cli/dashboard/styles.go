// Package dashboard provides an interactive TUI for browsing sessions,
// checkpoints, and settings.
package dashboard

import "github.com/charmbracelet/lipgloss"

// Dracula palette colors (consistent with entireTheme in cli package).
const (
	colorPurple  = "#BD93F9"
	colorComment = "#6272A4"
	colorGreen   = "#50FA7B"
	colorYellow  = "#F1FA8C"
	colorRed     = "#FF5555"
	colorCyan    = "#8BE9FD"
	colorOrange  = "#FFB86C"
	colorPink    = "#FF79C6"
	colorFg      = "#F8F8F2"
	colorBg      = "#282A36"
	colorCurLine = "#44475A"
)

// Tab styles.
var (
	activeTabStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(colorPurple)).
			Padding(0, 2)

	inactiveTabStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color(colorComment)).
				Padding(0, 2)

	tabGapStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colorComment))
)

// Content styles.
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(colorPurple))

	selectedItemStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color(colorFg)).
				Background(lipgloss.Color(colorCurLine))

	activePhaseStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color(colorGreen))

	idlePhaseStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colorYellow))

	endedPhaseStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colorRed))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colorComment))

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colorRed))

	labelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colorCyan))

	valueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colorFg))

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colorComment))

	confirmStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(colorOrange))
)

// statusBar renders the bottom help bar.
var statusBarStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorComment)).
	Padding(0, 1)

package dashboard

import (
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	tabSessions    = 0
	tabCheckpoints = 1
	tabActive      = 2
	tabSettings    = 3
)

// Key string constants shared across tab models.
const (
	keyDown      = "down"
	keyUp        = "up"
	keyEnter     = "enter"
	keyEsc       = "esc"
	keyBackspace = "backspace"
	keyFilter    = "/"
	keyJ         = "j"
	keyK         = "k"
	keyR         = "r"
	keyY         = "y"
	keyYUpper    = "Y"
	keyN         = "n"
	keyNUpper    = "N"
)

// Checkpoint type labels.
const (
	cpTypeSession   = "session"
	cpTypeTask      = "task"
	cpTypeCommitted = "committed"
)

var tabNames = []string{"Sessions", "Checkpoints", "Active", "Settings"}

// RewindRequest is returned by Run when the user requests a rewind from the TUI.
type RewindRequest struct {
	PointID string
}

// Model is the root bubbletea model for the dashboard.
type Model struct {
	activeTab   int
	sessions    sessionsModel
	checkpoints checkpointsModel
	active      activeModel
	settings    settingsModel
	width       int
	height      int
	showHelp    bool
	err         error
	rewindReq   *RewindRequest
	quitting    bool
	dataLoaded  map[int]bool // tracks whether each tab's data has loaded
}

func newModel() Model {
	return Model{
		activeTab:   tabSessions,
		sessions:    newSessionsModel(),
		checkpoints: newCheckpointsModel(),
		active:      newActiveModel(),
		settings:    newSettingsModel(),
		dataLoaded:  make(map[int]bool),
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		loadSessionsCmd,
		loadCheckpointsCmd,
		loadActiveSessionsCmd,
		loadSettingsCmd,
	)
}

//nolint:ireturn // required by bubbletea.Model interface
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		contentHeight := m.contentHeight()
		m.sessions.setSize(m.width, contentHeight)
		m.checkpoints.setSize(m.width, contentHeight)
		m.active.setSize(m.width, contentHeight)
		m.settings.setSize(m.width, contentHeight)
		return m, nil

	case tea.KeyMsg:
		// If a tab is in filter/input mode, delegate to it first
		if m.isTabCapturingInput() {
			return m.updateActiveTab(msg)
		}

		switch {
		case msg.String() == "q" || msg.String() == "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case msg.String() == "?":
			m.showHelp = !m.showHelp
			return m, nil
		case msg.String() == "tab":
			m.activeTab = (m.activeTab + 1) % len(tabNames)
			return m, nil
		case msg.String() == "shift+tab":
			m.activeTab = (m.activeTab - 1 + len(tabNames)) % len(tabNames)
			return m, nil
		}

		return m.updateActiveTab(msg)

	case sessionsMsg:
		m.dataLoaded[tabSessions] = true
		m.sessions.setSessions(msg.sessions)
		if msg.err != nil {
			m.sessions.err = msg.err
		}
		return m, nil

	case checkpointsMsg:
		m.dataLoaded[tabCheckpoints] = true
		m.checkpoints.setCheckpoints(msg.points)
		if msg.err != nil {
			m.checkpoints.err = msg.err
		}
		return m, nil

	case activeSessionsMsg:
		m.dataLoaded[tabActive] = true
		m.active.setSessions(msg.states)
		if msg.err != nil {
			m.active.err = msg.err
		}
		return m, nil

	case settingsDataMsg:
		m.dataLoaded[tabSettings] = true
		m.settings.setData(msg)
		return m, nil

	case rewindRequestMsg:
		m.rewindReq = &RewindRequest{PointID: msg.pointID}
		m.quitting = true
		return m, tea.Quit
	}

	return m.updateActiveTab(msg)
}

//nolint:ireturn // required by bubbletea update pattern
func (m Model) updateActiveTab(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m.activeTab {
	case tabSessions:
		m.sessions, cmd = m.sessions.update(msg)
	case tabCheckpoints:
		m.checkpoints, cmd = m.checkpoints.update(msg)
	case tabActive:
		m.active, cmd = m.active.update(msg)
	case tabSettings:
		m.settings, cmd = m.settings.update(msg)
	}
	return m, cmd
}

func (m Model) isTabCapturingInput() bool {
	switch m.activeTab {
	case tabSessions:
		return m.sessions.filtering
	case tabCheckpoints:
		return m.checkpoints.filtering || m.checkpoints.confirming
	}
	return false
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	// Tab bar
	b.WriteString(m.renderTabBar())
	b.WriteString("\n")

	// Content
	contentHeight := m.contentHeight()
	content := m.renderActiveTab(contentHeight)
	b.WriteString(content)

	// Help / status bar
	b.WriteString("\n")
	b.WriteString(m.renderStatusBar())

	return b.String()
}

func (m Model) renderTabBar() string {
	var tabs []string
	for i, name := range tabNames {
		if i == m.activeTab {
			tabs = append(tabs, activeTabStyle.Render("["+name+"]"))
		} else {
			tabs = append(tabs, inactiveTabStyle.Render(" "+name+" "))
		}
	}
	bar := lipgloss.JoinHorizontal(lipgloss.Top, tabs...)
	separator := strings.Repeat("─", max(0, m.width-lipgloss.Width(bar)))
	return bar + tabGapStyle.Render(separator)
}

func (m Model) renderActiveTab(height int) string {
	switch m.activeTab {
	case tabSessions:
		if !m.dataLoaded[tabSessions] {
			return renderLoading("sessions", height)
		}
		return m.sessions.view(m.width, height)
	case tabCheckpoints:
		if !m.dataLoaded[tabCheckpoints] {
			return renderLoading("checkpoints", height)
		}
		return m.checkpoints.view(m.width, height)
	case tabActive:
		if !m.dataLoaded[tabActive] {
			return renderLoading("active sessions", height)
		}
		return m.active.view(m.width, height)
	case tabSettings:
		if !m.dataLoaded[tabSettings] {
			return renderLoading("settings", height)
		}
		return m.settings.view(m.width, height)
	}
	return ""
}

func (m Model) renderStatusBar() string {
	if m.showHelp {
		return m.renderFullHelp()
	}
	hint := "Tab: switch | j/k: navigate | Enter: detail | ?: help | q: quit"
	if m.activeTab == tabCheckpoints {
		hint = "Tab: switch | j/k: navigate | Enter: detail | r: rewind | /: filter | ?: help | q: quit"
	}
	if m.activeTab == tabSessions {
		hint = "Tab: switch | j/k: navigate | Enter: detail | /: filter | ?: help | q: quit"
	}
	return statusBarStyle.Render(hint)
}

func (m Model) renderFullHelp() string {
	help := []string{
		"  Tab/Shift+Tab  Switch tabs",
		"  j/k, Up/Down   Navigate items",
		"  Enter          View detail / expand",
		"  Esc            Close detail / clear filter",
		"  /              Search / filter",
		"  r              Rewind (Checkpoints tab)",
		"  ?              Toggle this help",
		"  q, Ctrl+C      Quit",
	}
	return helpStyle.Render(strings.Join(help, "\n"))
}

func (m Model) contentHeight() int {
	// Tab bar (1) + separator (0, part of tab bar) + status bar (1) + margins
	overhead := 3
	if m.showHelp {
		overhead = 10 // help takes more space
	}
	h := m.height - overhead
	if h < 5 {
		h = 5
	}
	return h
}

func renderLoading(what string, height int) string {
	msg := dimStyle.Render(fmt.Sprintf("Loading %s...", what))
	// Center vertically
	padding := height / 2
	return strings.Repeat("\n", padding) + "  " + msg
}

// Run starts the dashboard TUI. Returns a RewindRequest if the user wants to rewind.
func Run() (*RewindRequest, error) {
	m := newModel()
	p := tea.NewProgram(m, tea.WithAltScreen())

	finalModel, err := p.Run()
	if err != nil {
		return nil, fmt.Errorf("dashboard TUI error: %w", err)
	}

	result, ok := finalModel.(Model)
	if !ok {
		return nil, errors.New("unexpected model type from TUI")
	}
	if result.err != nil {
		return nil, result.err
	}

	return result.rewindReq, nil
}

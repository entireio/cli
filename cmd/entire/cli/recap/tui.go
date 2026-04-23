package recap

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TUIModel backs the interactive recap view. It keeps the raw sessions so
// range + agent toggles can rebuild the View instantly without re-fetching.
type TUIModel struct {
	sessions    []RecapSession
	view        View
	agentFilter string
	agents      []string // agents present in sessions, for cycling
	mode        ViewMode // me / contributors / both
	styles      Styles
	width       int
	height      int
}

// NewTUIModel wraps a pre-built View with the state bubbletea needs for
// interactive toggles. agentFilter is the initial filter ("" = all agents).
func NewTUIModel(sessions []RecapSession, initial View, agentFilter string) TUIModel {
	mode := initial.Mode
	if mode == "" {
		mode = ViewBoth
	}
	return TUIModel{
		sessions:    sessions,
		view:        initial,
		agentFilter: agentFilter,
		agents:      distinctAgents(sessions),
		mode:        mode,
		styles:      NewStyles(true),
		width:       100,
		height:      24,
	}
}

// Init is a no-op — no async work on startup.
func (m TUIModel) Init() tea.Cmd { return nil }

// Update dispatches on keyboard and window-resize messages. Range + agent
// toggles rebuild the View via BuildView; nothing else changes.
func (m TUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:ireturn // tea.Model is required by bubbletea's interface contract
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m TUIModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) { //nolint:ireturn // mirrors Update's bubbletea contract
	key := msg.String()
	switch key {
	case "q", "ctrl+c", "esc":
		return m, tea.Quit
	case "1", "d":
		m.view = rebuildView(m.sessions, RangeDay, m.agentFilter, m.mode)
	case "2", "w":
		m.view = rebuildView(m.sessions, RangeWeek, m.agentFilter, m.mode)
	case "3", "m":
		m.view = rebuildView(m.sessions, RangeMonth, m.agentFilter, m.mode)
	case "4":
		m.view = rebuildView(m.sessions, Range30d, m.agentFilter, m.mode)
	case "5":
		m.view = rebuildView(m.sessions, Range90d, m.agentFilter, m.mode)
	case "a":
		m.agentFilter = cycleAgent(m.agents, m.agentFilter)
		m.view = rebuildView(m.sessions, m.view.Range, m.agentFilter, m.mode)
	case "v":
		m.mode = cycleMode(m.mode)
		m.view = rebuildView(m.sessions, m.view.Range, m.agentFilter, m.mode)
	}
	return m, nil
}

// View renders the current state via the static renderer, plus a help line
// at the bottom so users discover the keybinds without reading docs.
func (m TUIModel) View() string {
	body := RenderStatic(m.view, m.styles, m.width)
	help := m.styles.help.Render("  d w m 4 5  range  ·  a  agent filter  ·  v  view (me/contributors/both)  ·  q  quit")
	return body + "\n" + help
}

// rebuildView is a tiny wrapper that anchors Now at call time so range math
// stays correct across long-running TUI sessions.
func rebuildView(sessions []RecapSession, r RangeKey, agent string, mode ViewMode) View {
	return BuildView(sessions, BuildOpts{Range: r, AgentFilter: agent, Mode: mode, Now: time.Now()})
}

// cycleMode advances the view mode: both → me → contributors → both.
func cycleMode(current ViewMode) ViewMode {
	switch current {
	case ViewBoth:
		return ViewMe
	case ViewMe:
		return ViewContributors
	case ViewContributors:
		return ViewBoth
	}
	return ViewBoth
}

// cycleAgent advances the filter in a stable round-robin: "" → agents[0] →
// agents[1] → … → "". Stable order means the same key press always moves
// to the same next agent.
func cycleAgent(agents []string, current string) string {
	if len(agents) == 0 {
		return ""
	}
	if current == "" {
		return agents[0]
	}
	for i, a := range agents {
		if a == current {
			if i+1 < len(agents) {
				return agents[i+1]
			}
			return ""
		}
	}
	return ""
}

func distinctAgents(sessions []RecapSession) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range sessions {
		for _, a := range s.AgentsUsed {
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	return out
}

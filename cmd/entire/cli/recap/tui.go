package recap

import (
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// TUIModel backs the interactive recap view. It keeps the raw sessions and
// the server-fetched data so range + agent toggles can rebuild the View
// instantly without re-fetching. Without the cached server data, every
// keypress would drop the team columns and revert me-metrics to local-only.
//
// Rendered output regularly exceeds the visible terminal height (summary +
// activity + agents card stack), so the body is wrapped in a bubbles
// viewport that handles scroll keys (↑/↓, j/k, page up/down). The help
// line stays pinned outside the viewport so users can always see how to
// quit / change range / cycle agents.
type TUIModel struct {
	sessions     []RecapSession
	view         View
	agentFilter  string
	agents       []string          // agents present in sessions, for cycling
	mode         ViewMode          // me / contributors / both
	serverMe     *ContributorsData // server me-side, carried across rebuilds
	contributors *ContributorsData // server team-side, carried across rebuilds
	serverDaily  []DailyCount      // server per-day activity, carried across rebuilds
	notes        []string          // diagnostic notes to preserve
	styles       Styles
	width        int
	height       int
	viewport     viewport.Model
	ready        bool // true once we've received a WindowSizeMsg and sized the viewport
}

// NewTUIModel wraps a pre-built View with the state bubbletea needs for
// interactive toggles. agentFilter is the initial filter ("" = all agents).
// serverMe, contributors, and serverDaily must be passed through so view
// rebuilds on keypress preserve server data — otherwise toggling range/view
// wipes the team columns and reverts the activity strip to local-only.
func NewTUIModel(
	sessions []RecapSession,
	initial View,
	agentFilter string,
	serverMe *ContributorsData,
	contributors *ContributorsData,
	serverDaily []DailyCount,
) TUIModel {
	mode := initial.Mode
	if mode == "" {
		mode = ViewBoth
	}
	return TUIModel{
		sessions:     sessions,
		view:         initial,
		agentFilter:  agentFilter,
		agents:       distinctAgents(sessions),
		mode:         mode,
		serverMe:     serverMe,
		contributors: contributors,
		serverDaily:  serverDaily,
		notes:        append([]string(nil), initial.Notes...),
		styles:       NewStyles(true),
		width:        100,
		height:       24,
	}
}

// Init is a no-op — no async work on startup.
func (m TUIModel) Init() tea.Cmd { return nil }

// Update dispatches on keyboard and window-resize messages. Range + agent
// toggles rebuild the View via BuildView; the new content is then handed to
// the viewport so its scroll position resets to the top on each rebuild.
// Unhandled keys (arrow keys, page up/down, j/k) fall through to the
// viewport so users can scroll the rendered output.
func (m TUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:ireturn // tea.Model is required by bubbletea's interface contract
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m = m.resizeViewport()
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
		m.view = m.rebuildView(RangeDay, m.agentFilter, m.mode)
		m = m.refreshViewportContent()
		return m, nil
	case "2", "w":
		m.view = m.rebuildView(RangeWeek, m.agentFilter, m.mode)
		m = m.refreshViewportContent()
		return m, nil
	case "3", "m":
		m.view = m.rebuildView(RangeMonth, m.agentFilter, m.mode)
		m = m.refreshViewportContent()
		return m, nil
	case "4":
		m.view = m.rebuildView(Range90d, m.agentFilter, m.mode)
		m = m.refreshViewportContent()
		return m, nil
	case "a":
		m.agentFilter = cycleAgent(m.agents, m.agentFilter)
		m.view = m.rebuildView(m.view.Range, m.agentFilter, m.mode)
		m = m.refreshViewportContent()
		return m, nil
	case "v":
		m.mode = cycleMode(m.mode)
		m.view = m.rebuildView(m.view.Range, m.agentFilter, m.mode)
		m = m.refreshViewportContent()
		return m, nil
	}
	// Anything else (↑/↓, page up/down, j/k, mouse wheel) is a scroll
	// signal — pass it through to the viewport so it can move within
	// the rendered content.
	if m.ready {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}
	return m, nil
}

// View stacks the scrollable content viewport on top of a pinned help line
// so the keybind hint stays visible regardless of scroll position. Before
// the first WindowSizeMsg arrives we render a minimal placeholder — bubbletea
// will redraw immediately on the size message, so this is only a brief flash.
func (m TUIModel) View() string {
	help := m.styles.help.Render("  d w m 4  range  ·  a  agent filter  ·  v  view  ·  ↑/↓ scroll  ·  q  quit")
	if !m.ready {
		return RenderStatic(m.view, m.styles, m.width) + "\n" + help
	}
	return m.viewport.View() + "\n" + help
}

// resizeViewport (re)builds the viewport sized to the current terminal,
// reserving one line for the help footer. Called on every WindowSizeMsg.
func (m TUIModel) resizeViewport() TUIModel {
	const helpLines = 1
	vpHeight := m.height - helpLines
	if vpHeight < 1 {
		vpHeight = 1
	}
	if !m.ready {
		m.viewport = viewport.New(m.width, vpHeight)
		m.ready = true
	} else {
		m.viewport.Width = m.width
		m.viewport.Height = vpHeight
	}
	m.viewport.SetContent(RenderStatic(m.view, m.styles, m.width))
	return m
}

// refreshViewportContent re-renders the body with the latest View and
// resets the viewport scroll to the top so users see the summary first
// after switching range / view mode / agent filter.
func (m TUIModel) refreshViewportContent() TUIModel {
	if !m.ready {
		return m
	}
	m.viewport.SetContent(RenderStatic(m.view, m.styles, m.width))
	m.viewport.GotoTop()
	return m
}

// rebuildView anchors Now at call time so range math stays correct across
// long-running TUI sessions, and re-supplies the cached server data so team
// columns and me-overrides persist through keypresses.
func (m TUIModel) rebuildView(r RangeKey, agent string, mode ViewMode) View {
	v := BuildView(m.sessions, BuildOpts{
		Range:        r,
		AgentFilter:  agent,
		Mode:         mode,
		ServerMe:     m.serverMe,
		Contributors: m.contributors,
		ServerDaily:  m.serverDaily,
		Now:          time.Now(),
	})
	v.Notes = append(v.Notes, m.notes...)
	return v
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

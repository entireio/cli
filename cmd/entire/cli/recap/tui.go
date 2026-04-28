package recap

import (
	"context"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// FetcherResult bundles the four pieces of server-derived recap data the TUI
// needs to refresh on a range change. Daily activity stays separate from
// ServerMe because the activity strip is range-shaped (per-day buckets) while
// ServerMe is per-agent aggregates over the whole window.
type FetcherResult struct {
	ServerMe     *ContributorsData
	Contributors *ContributorsData
	Daily        []DailyCount
	Diag         []string
}

// Fetcher refetches /me/recap-derived data for a given range. The TUI
// invokes this when the user toggles range so all server-side fields
// (team aggregates, me-side counts that override local sums, daily strip
// for week+) reflect the new window — not the one the CLI started with.
//
// Lives in this package as a callback type rather than a direct
// dependency so the recap package stays free of auth/api imports;
// runRecapTUI in the parent package builds the closure that captures the
// worktree root and bearer token.
type Fetcher func(ctx context.Context, rangeKey RangeKey, now time.Time) FetcherResult

// recapDataMsg is bubbletea's reply when an async Fetcher call returns.
// rangeKey carries the request's range so the result is filed into the
// cache under the right key — and so that out-of-order responses (user
// hammers d → w → 4) only update the visible view if the user is still
// looking at the range that was just fetched.
type recapDataMsg struct {
	rangeKey RangeKey
	result   FetcherResult
}

// prefetchAllMsg fires once at startup (emitted from Init) to seed the
// cache with the initial fetch's data and kick off background prefetches
// of the remaining ranges in parallel. After this round-trip, every range
// toggle is a cache hit — no network on the keypress.
type prefetchAllMsg struct{}

// TUIModel backs the interactive recap view. It keeps the raw sessions,
// cached server data, and the Fetcher callback so range toggles can
// trigger an async re-fetch — without that, server-derived fields (team
// counts, server me-side counts, daily activity for week+ ranges) stay
// frozen at whatever range the CLI was launched with.
//
// Rendered output regularly exceeds the visible terminal height (summary +
// activity + agents card stack), so the body is wrapped in a bubbles
// viewport that handles scroll keys (↑/↓, j/k, page up/down). The help
// line stays pinned outside the viewport so users can always see how to
// quit / change range / cycle agents.
type TUIModel struct {
	ctx          context.Context
	fetch        Fetcher
	sessions     []RecapSession
	view         View
	agentFilter  string
	agents       []string          // agents present in sessions, for cycling
	mode         ViewMode          // me / contributors / both
	serverMe     *ContributorsData // server me-side, swapped on range change
	contributors *ContributorsData // server team-side, swapped on range change
	serverDaily  []DailyCount      // server per-day activity, swapped on range change
	notes        []string          // diagnostic notes from the initial fetch
	styles       Styles
	width        int
	height       int
	viewport     viewport.Model
	ready        bool // true once we've received a WindowSizeMsg and sized the viewport
	loading      bool // true while the user's currently-displayed range has no cached data and a Fetcher call is in flight
	// cache stores prior fetch results keyed by range so toggling between
	// ranges hits the network at most once per range, per session.
	cache map[RangeKey]FetcherResult
	// inFlight tracks ranges with an outstanding Fetcher call to avoid
	// double-issuing requests when the user toggles into a range that's
	// already being prefetched in the background.
	inFlight map[RangeKey]bool
}

// NewTUIModel wraps a pre-built View with the state bubbletea needs for
// interactive toggles. agentFilter is the initial filter ("" = all agents).
// fetch may be nil — when nil, range toggles only re-bucket local sessions
// and the cached server data stays frozen (offline/test mode).
func NewTUIModel(
	ctx context.Context,
	fetch Fetcher,
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
		ctx:          ctx,
		fetch:        fetch,
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
		cache:        map[RangeKey]FetcherResult{},
		inFlight:     map[RangeKey]bool{},
	}
}

// Init seeds the cache with the initial fetch's data (already on the model)
// and emits prefetchAllMsg so Update can fire background fetches for the
// other ranges in parallel. After the prefetch round-trip completes, every
// range toggle is an instant cache hit instead of a 1-5s network round-trip.
func (m TUIModel) Init() tea.Cmd {
	return func() tea.Msg { return prefetchAllMsg{} }
}

// Update dispatches on keyboard, window-resize, and async fetch-result
// messages. Range/agent/view-mode toggles rebuild the View; range toggles
// additionally fire a Fetcher to refresh server-side fields; the rendered
// body is then handed to the viewport so its scroll position resets to the
// top on each rebuild. Unhandled keys (arrows, page up/down, j/k) fall
// through to the viewport so users can scroll the rendered output.
func (m TUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:ireturn // tea.Model is required by bubbletea's interface contract
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m = m.resizeViewport()
		return m, nil
	case prefetchAllMsg:
		// Seed the cache with the initial fetch's data so we don't refetch
		// the range we already have, then issue background fetches for every
		// other range in parallel. Once those land, range toggles never hit
		// the network again in this session.
		m.cache[m.view.Range] = FetcherResult{
			ServerMe:     m.serverMe,
			Contributors: m.contributors,
			Daily:        m.serverDaily,
			Diag:         append([]string(nil), m.notes...),
		}
		if m.fetch == nil {
			return m, nil
		}
		var cmds []tea.Cmd
		for _, r := range []RangeKey{RangeDay, RangeWeek, RangeMonth, Range90d} {
			if r == m.view.Range || m.inFlight[r] {
				continue
			}
			m.inFlight[r] = true
			cmds = append(cmds, m.fetchCmd(r))
		}
		return m, tea.Batch(cmds...)
	case recapDataMsg:
		// Always cache the result and clear the in-flight flag, even if
		// the user has since navigated to a different range — the data is
		// useful for the next time they toggle back.
		m.cache[msg.rangeKey] = msg.result
		delete(m.inFlight, msg.rangeKey)
		// Apply to the visible view only when the user is still on this
		// range. If they pressed w → 4 before w arrived, the w response
		// silently caches itself; the visible view stays on 4.
		if msg.rangeKey != m.view.Range {
			return m, nil
		}
		m.serverMe = msg.result.ServerMe
		m.contributors = msg.result.Contributors
		m.serverDaily = msg.result.Daily
		m.notes = append(m.notes[:0], msg.result.Diag...)
		m.loading = false
		m.view = m.rebuildView(m.view.Range, m.agentFilter, m.mode)
		m = m.refreshViewportContent()
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
		return m.changeRange(RangeDay)
	case "2", "w":
		return m.changeRange(RangeWeek)
	case "3", "m":
		return m.changeRange(RangeMonth)
	case "4":
		return m.changeRange(Range90d)
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

// changeRange flips the visible range immediately. If the cache already
// holds a result for this range (the common case after the startup
// prefetch lands), swap it in and re-render with no network at all. On a
// cache miss we issue a fetch and mark loading so the help footer shows
// "refreshing…"; if a prefetch is already in flight for this range we
// just wait for its reply rather than firing a duplicate request.
func (m TUIModel) changeRange(r RangeKey) (tea.Model, tea.Cmd) { //nolint:ireturn // mirrors handleKey's bubbletea contract
	// Cache hit — instant render, no network.
	if cached, ok := m.cache[r]; ok {
		m.serverMe = cached.ServerMe
		m.contributors = cached.Contributors
		m.serverDaily = cached.Daily
		m.notes = append(m.notes[:0], cached.Diag...)
		m.loading = false
		m.view = m.rebuildView(r, m.agentFilter, m.mode)
		m = m.refreshViewportContent()
		return m, nil
	}
	// Cache miss — clear the previous range's server data so we don't
	// display its numbers under the new range's title. Otherwise the user
	// sees, e.g., "Last 90 days" with last month's team aggregate baked
	// in until the actual 90d reply arrives. Falls back to local-only
	// mode (empty team columns, local me-side counts) until the fetch
	// lands.
	m.serverMe = nil
	m.contributors = nil
	m.serverDaily = nil
	m.notes = m.notes[:0]
	m.view = m.rebuildView(r, m.agentFilter, m.mode)
	m = m.refreshViewportContent()
	if m.fetch == nil {
		return m, nil
	}
	m.loading = true
	if m.inFlight[r] {
		// Prefetch is already running; reply will land via recapDataMsg.
		return m, nil
	}
	m.inFlight[r] = true
	return m, m.fetchCmd(r)
}

// fetchCmd builds a tea.Cmd that runs the Fetcher in the background and
// emits a recapDataMsg. The captured rangeKey on the message lets Update
// discard out-of-order replies when the user changes range mid-fetch.
func (m TUIModel) fetchCmd(r RangeKey) tea.Cmd {
	ctx := m.ctx
	fetch := m.fetch
	return func() tea.Msg {
		return recapDataMsg{
			rangeKey: r,
			result:   fetch(ctx, r, time.Now()),
		}
	}
}

// View stacks the scrollable content viewport on top of a pinned help line
// so the keybind hint stays visible regardless of scroll position. Before
// the first WindowSizeMsg arrives we render a minimal placeholder — bubbletea
// will redraw immediately on the size message, so this is only a brief flash.
func (m TUIModel) View() string {
	hint := "  d w m 4  range  ·  a  agent filter  ·  v  view  ·  ↑/↓ scroll  ·  q  quit"
	if m.loading {
		hint += "  ·  refreshing…"
	}
	help := m.styles.help.Render(hint)
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

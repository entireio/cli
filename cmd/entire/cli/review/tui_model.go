// Package review — see env.go for package-level rationale.
//
// tui_model.go provides reviewTUIModel, the Bubble Tea Model for the
// review dashboard. The model renders a per-agent status table
// during the run and supports Ctrl+O drill-in mode for inspecting one agent's
// live event buffer on the alt screen.
package review

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
)

// agentRow holds per-agent live state during the TUI run.
type agentRow struct {
	name     string
	status   reviewtypes.AgentStatus
	runStart time.Time          // stamped on first event from this agent
	runEnd   time.Time          // stamped on Finished/RunError event
	tokens   reviewtypes.Tokens // cumulative
	preview  string             // latest AssistantText preview, capped by display width
	buffer   []reviewtypes.Event
	err      error
}

// agentEventMsg is sent to the Bubble Tea program when an agent emits an event.
type agentEventMsg struct {
	agent string
	ev    reviewtypes.Event
}

// runFinishedMsg is sent when the orchestrator calls RunFinished.
type runFinishedMsg struct {
	summary reviewtypes.RunSummary
}

// tickMsg triggers spinner and duration column updates.
type tickMsg time.Time

// reviewTUIModel is the Bubble Tea model for the review dashboard.
type reviewTUIModel struct {
	rows       []agentRow
	rowIdx     map[string]int // agent name → row index (O(1) lookup)
	detailMode bool
	detailIdx  int // which agent is shown in drill-in
	// detail is the pager backing the drill-in body. Width/Height are kept
	// in sync with termWidth/termHeight (minus header+footer). Scroll
	// position is internal state; AtBottom drives auto-tail.
	detail viewport.Model

	cancel     context.CancelFunc
	cancelOnce *sync.Once

	spinner    spinner.Model
	termWidth  int
	termHeight int

	finished bool
	summary  reviewtypes.RunSummary

	// cancelling tracks whether a Ctrl+C-initiated cancellation is in flight.
	// Set true on the first Ctrl+C (in tandem with cancelOnce firing the shared
	// CancelFunc); the dashboard switches to a "cancelling" indicator and the
	// footer offers a force-quit hint. A second Ctrl+C while cancelling=true
	// force-quits without waiting for agents to drain.
	cancelling bool
}

// newReviewTUIModel builds an initial model pre-populated with one row per
// agent. cancel is the shared CancelFunc; it must be the same one passed to
// NewTUISink.
func newReviewTUIModel(agents []string, cancel context.CancelFunc) reviewTUIModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	rows := make([]agentRow, len(agents))
	rowIdx := make(map[string]int, len(agents))
	for i, name := range agents {
		rows[i] = agentRow{
			name:   name,
			status: reviewtypes.AgentStatusUnknown,
		}
		rowIdx[name] = i
	}
	// Seed viewport with defaults that match termWidth/termHeight so an
	// immediate Ctrl+O before any WindowSizeMsg still renders.
	const (
		defaultTermWidth  = 80
		defaultTermHeight = 24
	)
	vp := viewport.New(
		viewport.WithWidth(defaultTermWidth),
		viewport.WithHeight(defaultTermHeight-2),
	)
	return reviewTUIModel{
		rows:       rows,
		rowIdx:     rowIdx,
		detail:     vp,
		cancel:     cancel,
		cancelOnce: &sync.Once{},
		spinner:    sp,
		termWidth:  defaultTermWidth,
		termHeight: defaultTermHeight,
	}
}

// tickCmd schedules the next tick for duration/spinner refresh.
func tickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// Init returns the initial spinner tick command.
func (m reviewTUIModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, tickCmd())
}

// Update handles all incoming messages.
func (m reviewTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case agentEventMsg:
		return m.handleAgentEvent(msg)

	case runFinishedMsg:
		m.finished = true
		m.summary = msg.summary
		// Sync each row's status from the orchestrator's summary. The
		// in-stream events (Finished / RunError) update status as they
		// arrive, but the orchestrator's classifyStatus has access to
		// process-level signals (Wait error, ctx cancellation) that the
		// event stream never surfaces. Without this sync, an agent that
		// the orchestrator classified Cancelled (Ctrl+C path, no Finished
		// emitted) or Failed (process exit non-zero, no Finished emitted)
		// would still render as "running" in the final frame.
		//
		// Preserve any already-set status from the event stream — if the
		// stream said Failed (RunError), the summary may say Succeeded
		// (process exit 0); RunError stickiness wins. Only overwrite when
		// the row is still in AgentStatusUnknown.
		now := time.Now()
		for i, run := range msg.summary.AgentRuns {
			if i >= len(m.rows) {
				break
			}
			if m.rows[i].status == reviewtypes.AgentStatusUnknown {
				m.rows[i].status = run.Status
			}
			if m.rows[i].runEnd.IsZero() && !m.rows[i].runStart.IsZero() {
				m.rows[i].runEnd = now
			}
		}
		return m, nil

	case tickMsg:
		var spinCmd tea.Cmd
		m.spinner, spinCmd = m.spinner.Update(msg)
		return m, tea.Batch(spinCmd, tickCmd())

	case spinner.TickMsg:
		var spinCmd tea.Cmd
		m.spinner, spinCmd = m.spinner.Update(msg)
		return m, spinCmd

	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height
		m.detail.SetWidth(m.detailViewportWidth())
		m.detail.SetHeight(m.detailViewportHeight())
		m = m.refreshDetailContent()
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// detailViewportWidth returns the viewport's width, mirroring termWidth.
func (m reviewTUIModel) detailViewportWidth() int {
	if m.termWidth < 1 {
		return 1
	}
	return m.termWidth
}

// detailViewportHeight returns the viewport's body height, reserving one line
// for the header and one for the footer.
func (m reviewTUIModel) detailViewportHeight() int {
	h := m.termHeight - 2
	if h < 1 {
		return 1
	}
	return h
}

// refreshDetailContent re-renders the focused agent's events into the
// viewport. It preserves auto-tail: if the viewport was sitting at the bottom
// (or has no scrollable content), it jumps to the new bottom after the content
// is replaced; otherwise the user's scroll position is left untouched.
//
// reviewTUIModel uses value receivers throughout (matching the Bubble Tea
// idiom of returning an updated tea.Model from Update); the viewport is
// mutated in place on the returned copy and the caller assigns the result
// back.
func (m reviewTUIModel) refreshDetailContent() reviewTUIModel {
	if len(m.rows) == 0 || m.detailIdx < 0 || m.detailIdx >= len(m.rows) {
		m.detail.SetContent("")
		return m
	}
	wasAtBottom := m.detail.AtBottom()
	lines := buildEventLines(m.rows[m.detailIdx].buffer, m.detailViewportWidth())
	m.detail.SetContentLines(lines)
	if wasAtBottom {
		m.detail.GotoBottom()
	}
	return m
}

// handleAgentEvent processes an agentEventMsg, updating the relevant row.
func (m reviewTUIModel) handleAgentEvent(msg agentEventMsg) (tea.Model, tea.Cmd) {
	idx, ok := m.rowIdx[msg.agent]
	if !ok {
		return m, nil
	}
	row := &m.rows[idx]

	// Stamp run-start on the first event from this agent (per-row, not
	// TUI-start; prevents inflated durations for late-starting agents).
	if row.runStart.IsZero() {
		row.runStart = time.Now()
	}

	row.buffer = append(row.buffer, msg.ev)

	switch e := msg.ev.(type) {
	case reviewtypes.Started:
		// runStart already set; no other state update needed.
	case reviewtypes.AssistantText:
		collapsed := stringutil.CollapseWhitespace(sanitizeDisplayText(e.Text))
		row.preview = truncateDisplayWidth(collapsed, 80)
	case reviewtypes.Tokens:
		row.tokens = e // cumulative: overwrite, not sum
	case reviewtypes.Finished:
		// RunError is sticky: if a prior RunError event already classified
		// this row as Failed, a subsequent Finished{Success: true} must NOT
		// flip it back to Succeeded. CU3's parser-fix-loop guarantees that
		// RunError + Finished{Success:false} both accompany torn streams;
		// matching that contract here keeps the TUI consistent with
		// classifyStatus from CU4 (which honors sawRunError).
		if row.status != reviewtypes.AgentStatusFailed {
			if e.Success {
				row.status = reviewtypes.AgentStatusSucceeded
			} else {
				row.status = reviewtypes.AgentStatusFailed
			}
		}
		row.runEnd = time.Now()
	case reviewtypes.RunError:
		row.status = reviewtypes.AgentStatusFailed
		row.err = e.Err
		if row.runEnd.IsZero() {
			row.runEnd = time.Now()
		}
	case reviewtypes.ToolCall:
		// No visible state update for tool calls in the dashboard.
	}

	// Re-render the focused agent's viewport content when a new event lands
	// for it. refreshDetailContent's AtBottom check preserves user scroll if
	// they've scrolled up; auto-tails otherwise.
	if m.detailMode && m.detailIdx == idx {
		m = m.refreshDetailContent()
	}

	return m, nil
}

// handleKey processes keyboard input.
func (m reviewTUIModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Post-finish dashboard: only explicit exit keys dismiss. Any other key
	// (including Ctrl+O) falls through to normal handling so the user can
	// still drill into completed agent output. Ctrl+C also quits immediately
	// — there are no agents left to drain.
	if m.finished && !m.detailMode {
		if isPostFinishExitKey(msg) || (msg.Code == 'c' && msg.Mod == tea.ModCtrl) {
			return m, tea.Quit
		}
	}

	switch {
	case msg.Code == 'c' && msg.Mod == tea.ModCtrl:
		if m.detailMode {
			// In drill-in: Ctrl+C is intentionally ignored; Esc first.
			return m, nil
		}
		// Second Ctrl+C while a cancellation is already in flight force-quits
		// without waiting for agents to drain. The shared CancelFunc is
		// guarded by cancelOnce, so the second press doesn't double-cancel.
		if m.cancelling {
			return m, tea.Quit
		}
		m.cancelling = true
		m.cancelOnce.Do(m.cancel)
		// Do NOT quit on the first Ctrl+C: leave the TUI up so the user sees
		// the cancelling indicator while agents drain. Natural finish
		// (runFinishedMsg) or a second Ctrl+C dismisses the TUI.
		return m, nil

	case msg.Code == 'o' && msg.Mod == tea.ModCtrl:
		if m.detailMode {
			m.detailMode = false
			return m, nil
		}
		m.detailMode = true
		if len(m.rows) > 0 && (m.detailIdx < 0 || m.detailIdx >= len(m.rows)) {
			m.detailIdx = 0
		}
		// Resize viewport in case termWidth/termHeight have changed since
		// last detail-mode entry, then load the focused agent's events and
		// tail to the bottom.
		m.detail.SetWidth(m.detailViewportWidth())
		m.detail.SetHeight(m.detailViewportHeight())
		m = m.refreshDetailContent()
		m.detail.GotoBottom()
		return m, nil

	case msg.Code == tea.KeyEscape || msg.Code == tea.KeyEsc:
		if m.detailMode {
			m.detailMode = false
			return m, nil
		}
		return m, nil

	case msg.Code == tea.KeyLeft:
		if m.detailMode && len(m.rows) > 0 {
			m.detailIdx = (m.detailIdx - 1 + len(m.rows)) % len(m.rows)
			m = m.refreshDetailContent()
			m.detail.GotoBottom()
		}
		return m, nil

	case msg.Code == tea.KeyRight:
		if m.detailMode && len(m.rows) > 0 {
			m.detailIdx = (m.detailIdx + 1) % len(m.rows)
			m = m.refreshDetailContent()
			m.detail.GotoBottom()
		}
		return m, nil
	}

	// Delegate any unhandled key to the viewport so PgUp/PgDn/Home/End/↑/↓
	// reach its internal keymap. Only meaningful in detail mode — on the
	// dashboard the viewport is inert.
	if m.detailMode {
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(msg)
		return m, cmd
	}
	return m, nil
}

// isPostFinishExitKey reports whether msg is one of the explicit exit keys
// honored on the post-finish dashboard. Ctrl+C is handled separately in the
// normal switch so duplicate-press semantics (cancelling → force-quit) still
// apply; here we only need q/Esc/Enter without modifiers.
func isPostFinishExitKey(msg tea.KeyPressMsg) bool {
	if msg.Mod != 0 {
		return false
	}
	switch msg.Code {
	case 'q', tea.KeyEnter, tea.KeyEscape:
		return true
	}
	return false
}

// View renders the current state.
func (m reviewTUIModel) View() tea.View {
	var content string
	if m.detailMode && len(m.rows) > 0 {
		content = detailFrame(m.rows[m.detailIdx], m.detail.View(), m.termWidth, m.termHeight)
	} else {
		content = m.dashboardView()
	}
	v := tea.NewView(content)
	v.AltScreen = m.detailMode
	return v
}

// dashboardView renders the summary table.
func (m reviewTUIModel) dashboardView() string {
	var b strings.Builder

	m.writeDashboardLine(&b, m.headerLine())

	for _, row := range m.rows {
		m.writeDashboardLine(&b, m.renderRow(row))
	}
	b.WriteString("\n")

	switch {
	case m.finished:
		m.writeDashboardLine(&b, m.countsLine())
		m.writeDashboardLine(&b, "Ctrl+O: drill in · q/Esc/Enter: exit")
	case m.cancelling:
		m.writeDashboardLine(&b, "Cancelling agents... · Ctrl+C again: force quit")
	default:
		m.writeDashboardLine(&b, "Ctrl+O: drill in · Ctrl+C: cancel")
	}
	return b.String()
}

func (m reviewTUIModel) writeDashboardLine(b *strings.Builder, line string) {
	b.WriteString(truncateDisplayWidth(line, m.dashboardWidth()))
	b.WriteString("\n")
}

func (m reviewTUIModel) dashboardWidth() int {
	if m.termWidth <= 0 {
		return 80
	}
	return m.termWidth
}

// headerLine returns the column header row.
func (m reviewTUIModel) headerLine() string {
	return m.renderTableLine("AGENT", "STATUS", "DURATION", "TOKENS", "PREVIEW")
}

// renderRow renders one agent row.
func (m reviewTUIModel) renderRow(row agentRow) string {
	name := row.name

	var statusStr string
	switch row.status {
	case reviewtypes.AgentStatusSucceeded:
		statusStr = "✓ done"
	case reviewtypes.AgentStatusFailed:
		statusStr = "✗ failed"
	case reviewtypes.AgentStatusCancelled:
		statusStr = "— cancel"
	case reviewtypes.AgentStatusUnknown:
		switch {
		case m.cancelling:
			// In-flight cancellation: distinct from the terminal Cancelled
			// state ("— cancel") so the user can see that the cancel signal
			// has been sent but the agent is still draining.
			statusStr = "cancelling"
		case row.runStart.IsZero():
			statusStr = "queued"
		default:
			statusStr = m.spinner.View() + " running"
		}
	}

	durStr := ""
	if !row.runStart.IsZero() {
		if !row.runEnd.IsZero() {
			durStr = formatDuration(row.runEnd.Sub(row.runStart))
		} else {
			durStr = formatDuration(time.Since(row.runStart))
		}
	}

	tokStr := ""
	if row.tokens.In > 0 || row.tokens.Out > 0 {
		tokStr = fmt.Sprintf("%s/%s", formatCompact(row.tokens.In), formatCompact(row.tokens.Out))
	}

	return m.renderTableLine(name, statusStr, durStr, tokStr, row.preview)
}

func (m reviewTUIModel) renderTableLine(agent, status, duration, tokens, preview string) string {
	const (
		agentWidth    = 20
		statusWidth   = 10
		durationWidth = 8
		tokensWidth   = 8
		minWidth      = agentWidth + statusWidth + durationWidth + tokensWidth + 8 // four two-space separators
	)
	termWidth := m.dashboardWidth()

	previewWidth := termWidth - minWidth
	if previewWidth < 0 {
		previewWidth = 0
	}

	line := fmt.Sprintf("%s  %s  %s  %s  %s",
		padDisplayWidth(agent, agentWidth),
		padDisplayWidth(status, statusWidth),
		padDisplayWidth(duration, durationWidth),
		padDisplayWidth(tokens, tokensWidth),
		truncateDisplayWidth(preview, previewWidth))
	return truncateDisplayWidth(line, termWidth)
}

// countsLine produces the summary line shown after the run finishes.
func (m reviewTUIModel) countsLine() string {
	succ, fail, canc := 0, 0, 0
	for _, r := range m.summary.AgentRuns {
		switch r.Status {
		case reviewtypes.AgentStatusSucceeded:
			succ++
		case reviewtypes.AgentStatusFailed:
			fail++
		case reviewtypes.AgentStatusCancelled:
			canc++
		case reviewtypes.AgentStatusUnknown:
			// not counted
		}
	}
	return fmt.Sprintf("%d agent(s) done — %d succeeded, %d failed, %d cancelled",
		len(m.summary.AgentRuns), succ, fail, canc)
}

// formatDuration formats a duration compactly for the table column.
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}

// formatCompact formats a token count as e.g. "1.2k" or "450".
func formatCompact(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return strconv.Itoa(n)
}

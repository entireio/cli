// Package review — see env.go for package-level rationale.
//
// tui_model.go provides reviewTUIModel, the Bubble Tea Model for the
// multi-agent review dashboard. The model renders a per-agent status table
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
	preview  string             // last ~80 runes of latest AssistantText
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

// reviewTUIModel is the Bubble Tea model for the multi-agent review dashboard.
type reviewTUIModel struct {
	rows         []agentRow
	rowIdx       map[string]int // agent name → row index (O(1) lookup)
	detailMode   bool
	detailIdx    int // which agent is shown in drill-in
	detailScroll int

	cancel     context.CancelFunc
	cancelOnce *sync.Once

	spinner    spinner.Model
	termWidth  int
	termHeight int

	finished bool
	summary  reviewtypes.RunSummary
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
	return reviewTUIModel{
		rows:       rows,
		rowIdx:     rowIdx,
		cancel:     cancel,
		cancelOnce: &sync.Once{},
		spinner:    sp,
		termWidth:  80,
		termHeight: 24,
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
		m = m.clampScroll()
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
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
		collapsed := stringutil.CollapseWhitespace(e.Text)
		row.preview = stringutil.TruncateRunes(collapsed, 80, "…")
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

	// Auto-follow ONLY when the user is already at the bottom. This lets a
	// user scroll up to inspect older events without each new event yanking
	// them back to the tail. The pre-append max-scroll was for buffer
	// length-1; if detailScroll was at-or-past that, the user was tailing.
	if m.detailMode && m.detailIdx == idx {
		preAppendMax := len(row.buffer) - 2 // -1 for the just-appended event, -1 for max-index
		if preAppendMax < 0 || m.detailScroll >= preAppendMax {
			m.detailScroll = m.maxDetailScroll()
		}
	}

	return m, nil
}

// handleKey processes keyboard input.
func (m reviewTUIModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Any key after finished dismisses.
	if m.finished {
		return m, tea.Quit
	}

	switch {
	case msg.Code == 'c' && msg.Mod == tea.ModCtrl:
		if m.detailMode {
			// In drill-in: Ctrl+C is intentionally ignored; Esc first.
			return m, nil
		}
		m.cancelOnce.Do(m.cancel)
		return m, tea.Quit

	case msg.Code == 'o' && msg.Mod == tea.ModCtrl:
		if m.detailMode {
			m.detailMode = false
			return m, nil
		}
		m.detailMode = true
		if len(m.rows) > 0 && (m.detailIdx < 0 || m.detailIdx >= len(m.rows)) {
			m.detailIdx = 0
		}
		m.detailScroll = m.maxDetailScroll()
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
			m.detailScroll = m.maxDetailScroll()
		}
		return m, nil

	case msg.Code == tea.KeyRight:
		if m.detailMode && len(m.rows) > 0 {
			m.detailIdx = (m.detailIdx + 1) % len(m.rows)
			m.detailScroll = m.maxDetailScroll()
		}
		return m, nil

	case msg.Code == tea.KeyUp:
		if m.detailMode && m.detailScroll > 0 {
			m.detailScroll--
		}
		return m, nil

	case msg.Code == tea.KeyDown:
		if m.detailMode {
			if maxScroll := m.maxDetailScroll(); m.detailScroll < maxScroll {
				m.detailScroll++
			}
		}
		return m, nil
	}

	return m, nil
}

// maxDetailScroll returns the largest valid detailScroll value for the current
// agent's buffer (0 when the buffer is empty or no rows exist).
func (m reviewTUIModel) maxDetailScroll() int {
	if len(m.rows) == 0 {
		return 0
	}
	n := len(m.rows[m.detailIdx].buffer)
	if n == 0 {
		return 0
	}
	return n - 1
}

// clampScroll returns a copy of m with detailScroll clamped to valid bounds.
// Used after resize or index change.
func (m reviewTUIModel) clampScroll() reviewTUIModel {
	maxScroll := m.maxDetailScroll()
	if m.detailScroll > maxScroll {
		m.detailScroll = maxScroll
	}
	return m
}

// View renders the current state.
func (m reviewTUIModel) View() tea.View {
	var content string
	if m.detailMode && len(m.rows) > 0 {
		content = detailView(m.rows[m.detailIdx], m.detailScroll, m.termWidth, m.termHeight)
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

	b.WriteString(m.headerLine())
	b.WriteString("\n")

	for _, row := range m.rows {
		b.WriteString(m.renderRow(row))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if m.finished {
		b.WriteString(m.countsLine())
		b.WriteString("\n")
		b.WriteString("Press any key to exit.")
		b.WriteString("\n")
	} else {
		b.WriteString("Ctrl+O: drill in · Ctrl+C: cancel")
		b.WriteString("\n")
	}
	return b.String()
}

// headerLine returns the column header row.
func (m reviewTUIModel) headerLine() string {
	return fmt.Sprintf("%-20s  %-10s  %-8s  %-8s  %s",
		"AGENT", "STATUS", "DURATION", "TOKENS", "PREVIEW")
}

// renderRow renders one agent row.
func (m reviewTUIModel) renderRow(row agentRow) string {
	name := stringutil.TruncateRunes(row.name, 20, "…")

	var statusStr string
	switch row.status {
	case reviewtypes.AgentStatusSucceeded:
		statusStr = "✓ done"
	case reviewtypes.AgentStatusFailed:
		statusStr = "✗ failed"
	case reviewtypes.AgentStatusCancelled:
		statusStr = "— cancel"
	case reviewtypes.AgentStatusUnknown:
		if row.runStart.IsZero() {
			statusStr = "queued"
		} else {
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

	preview := stringutil.TruncateRunes(row.preview, 40, "…")

	return fmt.Sprintf("%-20s  %-10s  %-8s  %-8s  %s",
		name, statusStr, durStr, tokStr, preview)
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

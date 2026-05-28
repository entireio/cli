package investigate

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// rowStatus is the per-agent terminal state shown in the STATUS column.
type rowStatus int

const (
	rowStatusQueued rowStatus = iota
	rowStatusRunning
	rowStatusDone
	rowStatusFailed
)

// timelineEntry is one row in the drill-in detail view's per-agent buffer.
// Entries are turn-granular: one on TurnStarted and one on TurnFinished
// (or TurnFinished with kind="failed" when the loop treated the turn as a
// failure).
type timelineEntry struct {
	when     time.Time
	turn     int
	kind     string // "started" | "finished" | "failed"
	stance   string
	duration time.Duration
	errStr   string
	findings string
}

// agentRow holds per-agent live state during the TUI run.
type agentRow struct {
	name         string
	status       rowStatus
	currentStart time.Time     // stamped on TurnStarted, zeroed on TurnFinished
	accumulated  time.Duration // sum of completed turn durations
	turnsTaken   int           // increments on TurnFinished (success or fail)
	maxTurns     int
	latestStance string // canonical: "approve" | "request-changes" | "reject" | ""
	lastErr      error
	buffer       []timelineEntry
}

// turnStartedMsg is sent when the loop begins an agent turn.
type turnStartedMsg struct {
	agent string
	turn  int
}

// turnFinishedMsg is sent when the loop finishes an agent turn (success or
// failure).
type turnFinishedMsg struct {
	agent    string
	turn     int
	stance   string
	duration time.Duration
	failed   bool
	err      error
	findings string // optional preview line parsed from the timeline doc
}

// runFinishedMsg is sent once when the loop terminates.
type runFinishedMsg struct {
	outcome LoopOutcome
}

// tickMsg drives spinner + running-duration refresh between turn events.
type tickMsg time.Time

// investigateTUIModel is the Bubble Tea model for the investigate dashboard.
type investigateTUIModel struct {
	topic           string
	runID           string
	rows            []agentRow
	rowIdx          map[string]int
	quorum          int
	approvals       int
	completedRounds int
	maxRounds       int

	finished bool
	outcome  LoopOutcome

	cancel     context.CancelFunc
	cancelOnce *sync.Once

	spinner    spinner.Model
	termWidth  int
	termHeight int

	detailMode   bool
	detailIdx    int
	detailScroll int
}

// newInvestigateTUIModel builds an initial model pre-populated with one row
// per agent. cancel is invoked when the user presses Ctrl+C inside the TUI.
func newInvestigateTUIModel(topic, runID string, agents []string, maxTurns, quorum int, cancel context.CancelFunc) investigateTUIModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	rows := make([]agentRow, len(agents))
	rowIdx := make(map[string]int, len(agents))
	for i, name := range agents {
		rows[i] = agentRow{
			name:     name,
			status:   rowStatusQueued,
			maxTurns: maxTurns,
		}
		rowIdx[name] = i
	}
	return investigateTUIModel{
		topic:      topic,
		runID:      runID,
		rows:       rows,
		rowIdx:     rowIdx,
		quorum:     quorum,
		maxRounds:  maxTurns,
		cancel:     cancel,
		cancelOnce: &sync.Once{},
		spinner:    sp,
		termWidth:  80,
		termHeight: 24,
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// Init kicks off the spinner and the refresh tick.
func (m investigateTUIModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, tickCmd())
}

// Update handles all incoming messages.
func (m investigateTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case turnStartedMsg:
		return m.handleTurnStarted(msg), nil

	case turnFinishedMsg:
		return m.handleTurnFinished(msg), nil

	case runFinishedMsg:
		m.finished = true
		m.outcome = msg.outcome
		for i := range m.rows {
			switch {
			case m.rows[i].status == rowStatusFailed:
				// Preserve failure state.
			case m.rows[i].turnsTaken == 0:
				// Agent was queued but never ran (early termination, quorum
				// reached before this agent's turn). Leave it visually
				// distinct from agents that ran to completion.
				m.rows[i].status = rowStatusQueued
			default:
				m.rows[i].status = rowStatusDone
			}
			if !m.rows[i].currentStart.IsZero() {
				m.rows[i].accumulated += time.Since(m.rows[i].currentStart)
				m.rows[i].currentStart = time.Time{}
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
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.MouseWheelMsg:
		if !m.detailMode {
			return m, nil
		}
		switch msg.Button {
		case tea.MouseWheelUp:
			if m.detailScroll > 0 {
				m.detailScroll--
			}
		case tea.MouseWheelDown:
			if m.detailScroll < m.maxDetailScroll() {
				m.detailScroll++
			}
		}
		return m, nil
	}
	return m, nil
}

// handleTurnStarted marks the named agent as running and stamps the start
// time. Unknown agents are ignored (defensive — should not happen given
// rowIdx pre-population; dropping the message beats panicking).
func (m investigateTUIModel) handleTurnStarted(msg turnStartedMsg) investigateTUIModel {
	idx, ok := m.rowIdx[msg.agent]
	if !ok {
		return m
	}
	row := &m.rows[idx]
	row.status = rowStatusRunning
	row.currentStart = time.Now()
	row.buffer = append(row.buffer, timelineEntry{
		when: time.Now(),
		turn: msg.turn,
		kind: "started",
	})
	return m
}

// handleTurnFinished folds the just-completed turn into the row's
// accumulated state and updates the round counters.
func (m investigateTUIModel) handleTurnFinished(msg turnFinishedMsg) investigateTUIModel {
	idx, ok := m.rowIdx[msg.agent]
	if !ok {
		return m
	}
	row := &m.rows[idx]

	row.accumulated += msg.duration
	row.currentStart = time.Time{}
	row.turnsTaken++
	if msg.stance != "" && msg.stance != stanceUnknown {
		row.latestStance = msg.stance
	}
	if msg.failed {
		row.lastErr = msg.err
		if row.turnsTaken >= row.maxTurns {
			row.status = rowStatusFailed
		} else {
			row.status = rowStatusQueued
		}
	} else {
		row.status = rowStatusQueued
	}

	kind := "finished"
	if msg.failed {
		kind = "failed"
	}
	var errStr string
	if msg.err != nil {
		errStr = msg.err.Error()
	}
	row.buffer = append(row.buffer, timelineEntry{
		when:     time.Now(),
		turn:     msg.turn,
		kind:     kind,
		stance:   msg.stance,
		duration: msg.duration,
		errStr:   errStr,
		findings: msg.findings,
	})

	// Recompute round + approval counters from the full row set so it is
	// resilient to out-of-order messages and replays.
	totalTurns := 0
	approvals := 0
	for _, r := range m.rows {
		totalTurns += r.turnsTaken
		if r.latestStance == stanceApprove {
			approvals++
		}
	}
	if n := len(m.rows); n > 0 {
		m.completedRounds = totalTurns / n
	}
	m.approvals = approvals
	return m
}

// handleKey processes keyboard input.
func (m investigateTUIModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.finished && !m.detailMode {
		// Any key after finished dismisses.
		return m, tea.Quit
	}

	switch {
	case msg.Code == 'c' && msg.Mod == tea.ModCtrl:
		// Cancel from either view. Detail view's footer advertises "Ctrl+C
		// cancel", so the binding must do that — Esc steps back to the
		// dashboard, but Ctrl+C stops the run.
		m.cancelOnce.Do(m.cancel)
		return m, tea.Quit

	case msg.Code == 'o' && msg.Mod == tea.ModCtrl:
		if m.detailMode {
			m.detailMode = false
			return m, nil
		}
		m.detailMode = true
		if m.detailIdx < 0 || m.detailIdx >= len(m.rows) {
			m.detailIdx = 0
		}
		m.detailScroll = m.maxDetailScroll()
		return m, nil

	case msg.Code == tea.KeyEscape:
		if m.detailMode {
			m.detailMode = false
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
		if m.detailMode && m.detailScroll < m.maxDetailScroll() {
			m.detailScroll++
		}
		return m, nil
	}
	return m, nil
}

// maxDetailScroll returns the largest valid detailScroll value for the
// currently-focused agent's buffer (0 when the buffer is empty or no
// rows exist).
func (m investigateTUIModel) maxDetailScroll() int {
	if len(m.rows) == 0 {
		return 0
	}
	n := len(m.rows[m.detailIdx].buffer)
	if n == 0 {
		return 0
	}
	return n - 1
}

// View renders the current frame.
func (m investigateTUIModel) View() tea.View {
	var content string
	if m.detailMode && len(m.rows) > 0 {
		content = detailView(m.rows[m.detailIdx], m.detailScroll, m.termWidth, m.termHeight)
	} else {
		content = m.dashboardView()
	}
	v := tea.NewView(content)
	v.AltScreen = m.detailMode
	if m.detailMode {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

// dashboardWidth returns the effective rendering width (defaulted when the
// terminal hasn't reported a size yet).
func (m investigateTUIModel) dashboardWidth() int {
	if m.termWidth <= 0 {
		return 80
	}
	return m.termWidth
}

// dashboardView renders the top banner, the table, and the footer hint.
func (m investigateTUIModel) dashboardView() string {
	var b strings.Builder

	m.writeLine(&b, m.titleLine())
	m.writeLine(&b, m.progressLine())
	b.WriteString("\n")
	m.writeLine(&b, m.headerLine())
	for _, row := range m.rows {
		m.writeLine(&b, m.renderRow(row))
	}
	b.WriteString("\n")
	if m.finished {
		m.writeLine(&b, m.outcomeLine())
		m.writeLine(&b, m.countsLine())
		m.writeLine(&b, "Press any key to exit.")
	} else {
		m.writeLine(&b, "Ctrl+O: detail · Ctrl+C: cancel")
	}
	return b.String()
}

func (m investigateTUIModel) writeLine(b *strings.Builder, line string) {
	b.WriteString(truncateDisplayWidth(line, m.dashboardWidth()))
	b.WriteString("\n")
}

func (m investigateTUIModel) titleLine() string {
	if m.topic == "" {
		return fmt.Sprintf("Investigating (run %s)", m.runID)
	}
	return fmt.Sprintf("Investigating: %q (run %s)", sanitizeDisplayText(m.topic), m.runID)
}

func (m investigateTUIModel) progressLine() string {
	totalTurns := 0
	for _, r := range m.rows {
		totalTurns += r.turnsTaken
	}
	maxOverall := m.maxRounds * len(m.rows)
	round := m.completedRounds + 1
	if m.finished {
		round = max(m.completedRounds, 1)
	}
	return fmt.Sprintf("Round %d/%d · %d of %d turns · quorum %d/%d approvals",
		round, m.maxRounds, totalTurns, maxOverall, m.approvals, m.quorum)
}

func (m investigateTUIModel) headerLine() string {
	return m.renderTableLine("AGENT", "STATUS", "DURATION", "TURN", "APPROVED")
}

func (m investigateTUIModel) renderRow(row agentRow) string {
	statusStr := m.statusString(row)
	durStr := formatRowDuration(row)
	turnStr := fmt.Sprintf("%d/%d", row.turnsTaken, row.maxTurns)
	approvedStr := formatStance(row.latestStance)
	return m.renderTableLine(row.name, statusStr, durStr, turnStr, approvedStr)
}

// statusString renders STATUS for a row, including the live dot spinner for
// the currently-running agent.
func (m investigateTUIModel) statusString(row agentRow) string {
	switch row.status {
	case rowStatusRunning:
		return m.spinner.View() + " running"
	case rowStatusDone:
		return "✓ done"
	case rowStatusFailed:
		return "✗ failed"
	case rowStatusQueued:
		fallthrough
	default:
		return "queued"
	}
}

// renderTableLine emits one row of the table padded to fixed column widths.
// APPROVED takes whatever's left after the four fixed columns.
func (m investigateTUIModel) renderTableLine(agent, status, duration, turn, approved string) string {
	const (
		agentWidth    = 20
		statusWidth   = 12
		durationWidth = 9
		turnWidth     = 6
		separators    = 8 // four two-space separators between five columns
		minWidth      = agentWidth + statusWidth + durationWidth + turnWidth + separators
	)
	termWidth := m.dashboardWidth()
	approvedWidth := max(termWidth-minWidth, 0)
	line := fmt.Sprintf("%s  %s  %s  %s  %s",
		padDisplayWidth(agent, agentWidth),
		padDisplayWidth(status, statusWidth),
		padDisplayWidth(duration, durationWidth),
		padDisplayWidth(turn, turnWidth),
		truncateDisplayWidth(approved, approvedWidth))
	return truncateDisplayWidth(line, termWidth)
}

// outcomeLine renders the post-run "Outcome: <name>" summary.
func (m investigateTUIModel) outcomeLine() string {
	if m.outcome == "" {
		return ""
	}
	return fmt.Sprintf("Outcome: %s", m.outcome)
}

// countsLine renders the per-stance totals at the end of the run.
func (m investigateTUIModel) countsLine() string {
	app, chg, rej, unk := 0, 0, 0, 0
	for _, r := range m.rows {
		switch r.latestStance {
		case stanceApprove:
			app++
		case stanceRequestChanges:
			chg++
		case stanceReject:
			rej++
		default:
			unk++
		}
	}
	return fmt.Sprintf("%d agent(s) — %d approved, %d request-changes, %d reject, %d unknown",
		len(m.rows), app, chg, rej, unk)
}

// formatRowDuration returns the display string for the DURATION column.
// While running it accumulates the in-flight elapsed time; otherwise it
// shows the total accumulated across completed turns. Empty when nothing
// has run yet.
func formatRowDuration(row agentRow) string {
	total := row.accumulated
	if !row.currentStart.IsZero() {
		total += time.Since(row.currentStart)
	}
	if total <= 0 {
		return ""
	}
	return formatDuration(total)
}

// formatDuration delegates to tuiutil.FormatDuration.
func formatDuration(d time.Duration) string {
	return tuiutil.FormatDuration(d)
}

// formatStance renders the APPROVED column from a canonical stance.
func formatStance(stance string) string {
	switch stance {
	case stanceApprove:
		return "✓ approve"
	case stanceRequestChanges:
		return "✗ changes"
	case stanceReject:
		return "✗ reject"
	default:
		return ""
	}
}

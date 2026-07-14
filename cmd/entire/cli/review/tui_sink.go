// Package review — see env.go for package-level rationale.
//
// tui_sink.go provides TUISink, a Sink implementation that renders a Bubble
// Tea status dashboard during a review run and supports Ctrl+O
// drill-in mode for inspecting one agent's live event buffer. Used in
// interactive (TTY) runs; non-TTY runs use DumpSink instead.
package review

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// teaRunner is the slice of *tea.Program the sink depends on, extracted so
// tests can substitute a program with a deterministically stalled event loop.
type teaRunner interface {
	Run() (tea.Model, error)
	Send(msg tea.Msg)
	Kill()
}

// tuiSinkQueueCap bounds the sink's internal dispatch queue. Program.Send is
// an unbuffered BLOCKING send: if the Bubble Tea Update/render pipeline ever
// stalls, a direct Send from the orchestrator's dispatch goroutine parks
// forever — freezing sink dispatch, the fanIn drain loop, the parsers, and
// reviewer-timeout handling with it (observed live: the 2026-07-07 run-6
// wedge, where the TUI froze mid-run and a 20m --timeout never surfaced).
// The queue absorbs bursts; overflow beyond the cap is dropped and counted —
// a display that can lag must never backpressure the data plane.
const tuiSinkQueueCap = 4096

// TUISink is a Sink that renders a Bubble Tea dashboard. The orchestrator
// calls AgentEvent/RunFinished from a single goroutine (CU4 serial-dispatch
// contract); the sink translates each event into a tea.Msg, enqueues it on a
// bounded internal queue, and a pump goroutine forwards it via Program.Send —
// so only the pump can ever block on a stalled Bubble Tea loop, never the
// orchestrator.
//
// Cancellation: cancel is the same context.CancelFunc that controls the
// orchestrator's run context. The first KeyCtrlC in the dashboard fires this
// function (guarded by a sync.Once in the model) and switches the dashboard
// to a "Cancelling agents..." indicator while agents drain; a second KeyCtrlC
// force-quits without waiting. Out-of-TUI SIGINT routes through the cobra
// root's context, which cancels the same function — no parallel signal.Notify
// goroutine is needed here.
type TUISink struct {
	program teaRunner

	mu       sync.Mutex
	started  bool
	finished bool
	dropped  int

	msgs     chan tea.Msg  // bounded dispatch queue drained by the pump
	done     chan struct{} // closed when the tea.Program exits
	pumpDone chan struct{} // closed when the pump goroutine exits

	// group reconciles per-worker execution with one-row-per-agent display
	// (skill fan-out). Nil on the single-agent path, where events and the
	// summary pass through unchanged.
	group *agentGrouping
}

// groupWorkers enables one-row-per-agent collapsing: rowOrder is the ordered
// per-agent row labels (matching the model's rows) and workerToAgent maps
// each worker label to the agent row its events and summary entry fold into.
// Called before Start.
func (s *TUISink) groupWorkers(rowOrder []string, workerToAgent map[string]string) {
	s.group = newAgentGrouping(rowOrder, workerToAgent)
}

// Compile-time interface check.
var _ reviewtypes.Sink = (*TUISink)(nil)

var tuiPostRunCompleteGrace = 2 * time.Second

// NewTUISink creates a TUISink wired to cancel for Ctrl+C handling. agents is
// the ordered list of agent names that will run; the dashboard pre-renders one
// row per agent so the user sees the full run shape from the first frame.
// output is the writer the Bubble Tea program renders into (typically
// cmd.OutOrStdout()). input is the reader Bubble Tea reads keypresses from
// (typically os.Stdin in production); tests must pass a non-TTY reader (e.g.
// bytes.NewReader(nil)) so Bubble Tea does not put the inherited terminal
// into raw mode and corrupt sibling test output.
//
// tea.WithoutSignalHandler keeps SIGINT routing on the cobra root's existing
// handler (which cancels the run context), so the TUI's KeyCtrlC path and the
// OS signal path share a single cancel function with no race.
func NewTUISink(agents []string, cancel context.CancelFunc, output io.Writer, input io.Reader) *TUISink {
	model := newReviewTUIModel(agents, cancel)
	if measureTerminal := terminalMeasurer(output); measureTerminal != nil {
		if width, height, ok := measureTerminal(); ok {
			model.termWidth = width
			model.termHeight = height
		}
	}
	prog := tea.NewProgram(
		model,
		tea.WithOutput(output),
		tea.WithInput(input),
		tea.WithoutSignalHandler(), // SIGINT handled by cobra root; KeyCtrlC calls cancel directly
	)
	return newTUISinkWithProgram(prog)
}

// newTUISinkWithProgram wires a TUISink around any teaRunner; tests inject
// fakes with stalled or recording Send implementations.
func newTUISinkWithProgram(prog teaRunner) *TUISink {
	return &TUISink{
		program:  prog,
		msgs:     make(chan tea.Msg, tuiSinkQueueCap),
		done:     make(chan struct{}),
		pumpDone: make(chan struct{}),
	}
}

type fdWriter interface {
	Fd() uintptr
}

func terminalMeasurer(output io.Writer) func() (int, int, bool) {
	f, ok := output.(fdWriter)
	if !ok {
		return nil
	}
	return func() (int, int, bool) {
		width, height, err := term.GetSize(int(f.Fd())) //nolint:gosec // fd values fit in int on supported platforms
		if err != nil || width <= 0 || height <= 0 {
			return 0, 0, false
		}
		return width, height, true
	}
}

// Start spawns the Bubble Tea program in its own goroutine. Must be called
// before any AgentEvent calls. Subsequent calls are no-ops.
func (s *TUISink) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()

	go func() {
		defer close(s.done)
		if _, err := s.program.Run(); err != nil {
			// Bubble Tea program errors are non-actionable in a background
			// goroutine (e.g., terminal resize race on exit). Log nothing —
			// the run result is available via RunSummary and the sink's
			// finished state. Swallowing is intentional.
			_ = err
		}
	}()

	// Pump: the only goroutine allowed to block on Program.Send. When the
	// program exits (done closes), a blocked Send unblocks via the program's
	// context and the pump drains out. A Send that races program exit (done
	// closes while a queued msg is in hand) is equally safe: Bubble Tea's
	// Send is a context-guarded select and the msgs channel is never closed,
	// so a post-exit Send is an immediate no-op — not a panic, not a block.
	go func() {
		defer close(s.pumpDone)
		for {
			select {
			case <-s.done:
				return
			case msg := <-s.msgs:
				s.program.Send(msg)
			}
		}
	}()
}

// Wait blocks until the Bubble Tea program exits, with a bounded escalation
// so teardown can never hang: in the normal flow PostRunComplete has already
// quit the program and Wait returns immediately; otherwise (early-error
// return paths, or a wedged loop that survived Kill) Wait gives the program
// one grace period, Kills it, gives it one more, and then abandons the
// goroutine — a stuck display must not hold command exit hostage. Joins the
// pump goroutine whenever the program actually exited. Safe to call after
// Start; if Start was never called, returns immediately.
func (s *TUISink) Wait() {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		return
	}
	select {
	case <-s.done:
		<-s.pumpDone
		return
	case <-time.After(tuiPostRunCompleteGrace):
	}
	s.program.Kill()
	select {
	case <-s.done:
		<-s.pumpDone
	case <-time.After(tuiPostRunCompleteGrace):
		// Bubble Tea never returned from Run despite Kill. Abandon the
		// program and pump goroutines rather than hanging teardown.
	}
}

// AgentEvent (Sink interface): translate ev into a tea.Msg and enqueue it for
// the pump. NEVER blocks: display events beyond the queue cap are dropped and
// counted rather than backpressuring the orchestrator's dispatch goroutine —
// see tuiSinkQueueCap for the incident this guards against.
func (s *TUISink) AgentEvent(agent string, ev reviewtypes.Event) {
	s.mu.Lock()
	ok := s.started && !s.finished
	s.mu.Unlock()
	if !ok {
		return
	}
	// Collapse per-worker events onto the agent row (skill fan-out): route to
	// the agent row, and show the SUM of its workers' cumulative tokens (a
	// single worker's value would bounce the live number between siblings,
	// since row.tokens overwrites per CU2). source carries the original
	// worker label so drill-in can label interleaved skills. Ungrouped rows
	// pass through unchanged.
	row, source := agent, agent
	if s.group != nil {
		row = s.group.rowFor(agent)
		if tk, ok := ev.(reviewtypes.Tokens); ok {
			ev = s.group.liveTokens(agent, tk)
		}
	}
	select {
	case s.msgs <- agentEventMsg{agent: row, source: source, ev: ev}:
	default:
		s.mu.Lock()
		s.dropped++
		s.mu.Unlock()
	}
}

// enqueueControl enqueues a rare, must-not-be-lost-lightly message (run
// summary, phase transitions, quit) with a bounded wait: worth briefly
// waiting out a transient jam, but a wedged TUI must not hold the run
// hostage — callers all have degradation paths (PostRunComplete falls back
// to Kill; a lost summary leaves the footer stale until quit).
func (s *TUISink) enqueueControl(msg tea.Msg) {
	select {
	case s.msgs <- msg:
	case <-s.done:
	case <-time.After(tuiPostRunCompleteGrace):
		s.mu.Lock()
		s.dropped++
		s.mu.Unlock()
	}
}

// droppedCount reports how many messages were discarded due to a jammed
// queue. Zero in any healthy run.
func (s *TUISink) droppedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// RunFinished (Sink interface): mark reviewer execution complete and send the
// final summary message. It does not block or exit the TUI: post-run sinks may
// still run (for example the final judge), and they can update the dashboard via
// FinalPhaseStarted/FinalPhaseFinished. A later PostRunComplete call exits the
// TUI once buffered post-run output is ready to flush.
func (s *TUISink) RunFinished(summary reviewtypes.RunSummary) {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.mu.Unlock()

	if s.group != nil {
		summary = s.group.collapseSummary(summary)
	}
	s.enqueueControl(runFinishedMsg{summary: summary})
}

// FinalPhaseStarted updates the TUI with a visible post-run phase such as the
// profile judge consolidating reviewer reports.
func (s *TUISink) FinalPhaseStarted(name string) {
	s.mu.Lock()
	ok := s.started
	s.mu.Unlock()
	if !ok {
		return
	}
	s.enqueueControl(finalPhaseStartedMsg{name: name})
}

// FinalPhaseFinished marks the visible post-run phase complete.
func (s *TUISink) FinalPhaseFinished(err error) {
	s.mu.Lock()
	ok := s.started
	s.mu.Unlock()
	if !ok {
		return
	}
	msg := finalPhaseFinishedMsg{}
	if err != nil {
		msg.err = err.Error()
	}
	s.enqueueControl(msg)
}

// PostRunComplete exits the TUI and waits for the Bubble Tea program to finish.
// Call after post-run sinks have produced any buffered output.
func (s *TUISink) PostRunComplete() {
	s.mu.Lock()
	ok := s.started
	s.mu.Unlock()
	if !ok {
		return
	}

	// enqueueControl is bounded, so this cannot park forever even when the
	// Bubble Tea loop is stalled or never entered; the Kill fallback below
	// guarantees a lost post-run quit cannot leave the CLI stuck on
	// "Finalizing output..." forever.
	s.enqueueControl(postRunCompleteMsg{})

	select {
	case <-s.done:
	case <-time.After(tuiPostRunCompleteGrace):
		s.program.Kill()
	}

	select {
	case <-s.done:
	case <-time.After(tuiPostRunCompleteGrace):
		s.program.Kill()
	}

	// Surface silent loss: a healthy run never drops. A non-zero count means
	// the TUI loop stalled or lagged badly enough to jam the queue — exactly
	// the diagnostic a future wedge investigation needs first.
	if n := s.droppedCount(); n > 0 {
		logging.Debug(context.Background(), "tui sink dropped messages under backpressure",
			slog.Int("dropped", n))
	}
}

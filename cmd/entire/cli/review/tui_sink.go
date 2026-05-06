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
	"sync"

	tea "charm.land/bubbletea/v2"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// TUISink is a Sink that renders a Bubble Tea dashboard. The orchestrator
// calls AgentEvent/RunFinished from a single goroutine (CU4 serial-dispatch
// contract); the sink translates each event into a tea.Msg and sends it via
// Program.Send. Bubble Tea's Send is thread-safe, but we never rely on that
// property — the serial-dispatch promise means Send is only called from the
// orchestrator's dispatch goroutine.
//
// Cancellation: cancel is the same context.CancelFunc that controls the
// orchestrator's run context. KeyCtrlC in the dashboard calls this function
// (gated by a sync.Once in the model). Out-of-TUI SIGINT routes through the
// cobra root's context, which cancels the same function — no parallel
// signal.Notify goroutine is needed here.
type TUISink struct {
	program *tea.Program

	mu       sync.Mutex
	started  bool
	finished bool

	done chan struct{} // closed when the tea.Program exits
}

// Compile-time interface check.
var _ reviewtypes.Sink = (*TUISink)(nil)

// NewTUISink creates a TUISink wired to cancel for Ctrl+C handling. agents is
// the ordered list of agent names that will run; the dashboard pre-renders one
// row per agent so the user sees the full run shape from the first frame.
// output is the writer the Bubble Tea program renders into (typically
// cmd.OutOrStdout()).
//
// tea.WithoutSignalHandler keeps SIGINT routing on the cobra root's existing
// handler (which cancels the run context), so the TUI's KeyCtrlC path and the
// OS signal path share a single cancel function with no race.
func NewTUISink(agents []string, cancel context.CancelFunc, output io.Writer) *TUISink {
	model := newReviewTUIModel(agents, cancel)
	prog := tea.NewProgram(
		model,
		tea.WithOutput(output),
		tea.WithoutSignalHandler(), // SIGINT handled by cobra root; KeyCtrlC calls cancel directly
	)
	return &TUISink{
		program: prog,
		done:    make(chan struct{}),
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
}

// Wait blocks until the Bubble Tea program exits. Safe to call after Start.
// If Start was never called, Wait returns immediately.
func (s *TUISink) Wait() {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		return
	}
	<-s.done
}

// AgentEvent (Sink interface): translate ev into a tea.Msg and Send it to the
// Bubble Tea program. Implements the serial-dispatch contract: the orchestrator
// calls this from a single goroutine.
//
// Note: Send is safe to call from goroutines other than the TUI's update loop;
// Bubble Tea's implementation queues the message internally.
func (s *TUISink) AgentEvent(agent string, ev reviewtypes.Event) {
	s.mu.Lock()
	ok := s.started && !s.finished
	s.mu.Unlock()
	if !ok {
		return
	}
	s.program.Send(agentEventMsg{agent: agent, ev: ev})
}

// RunFinished (Sink interface): mark the run complete and send the final
// summary message. The TUI shows the dashboard one more frame with the
// terminal statuses and waits for the user to press any key to dismiss.
//
// IMPORTANT: RunFinished blocks until the user dismisses (presses any key)
// so that post-run sinks (e.g. DumpSink) render their narrative AFTER the
// TUI has exited and the terminal is back in normal mode.
func (s *TUISink) RunFinished(summary reviewtypes.RunSummary) {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.mu.Unlock()

	s.program.Send(runFinishedMsg{summary: summary})
	// Block until the Bubble Tea program exits (user presses any key after
	// seeing the final dashboard, or Ctrl+C was already received and the
	// program already quit).
	s.Wait()
}

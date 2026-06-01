package investigate

import (
	"context"
	"io"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// tuiProgressSink is a ProgressSink backed by a Bubble Tea program.
type tuiProgressSink struct {
	program *tea.Program

	mu       sync.Mutex
	started  bool
	finished bool

	done chan struct{} // closed when the tea.Program exits
}

// newTUIProgressSink builds a sink wired to cancel for Ctrl+C handling. The
// caller must invoke Start before any TurnStarted call and Wait after
// RunFinished.
//
// tea.WithoutSignalHandler keeps SIGINT routing on the cobra root's existing
// handler (which cancels the run context). The model's Ctrl+C path invokes
// the same cancel function so the two paths converge cleanly.
func newTUIProgressSink(topic, runID string, agents []string, maxTurns, quorum int, cancel context.CancelFunc, output io.Writer) *tuiProgressSink {
	model := newInvestigateTUIModel(topic, runID, agents, maxTurns, quorum, cancel)
	prog := tea.NewProgram(
		model,
		tea.WithOutput(output),
		tea.WithoutSignalHandler(),
	)
	return &tuiProgressSink{
		program: prog,
		done:    make(chan struct{}),
	}
}

// Start spawns the Bubble Tea program in its own goroutine. Subsequent
// calls are no-ops.
//
// ctx watcher: if ctx is cancelled (parent SIGINT, cobra shutdown, or the
// loop returns early before RunFinished), the watcher calls program.Quit()
// so the program exits and Wait() unblocks. Without this, an early-return
// path that never calls RunFinished would leave Wait() waiting forever.
func (s *tuiProgressSink) Start(ctx context.Context) {
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
			// goroutine. Run state and per-turn logs on disk remain
			// available.
			_ = err
		}
	}()

	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				s.program.Quit()
			case <-s.done:
			}
		}()
	}
}

// Wait blocks until the Bubble Tea program exits. Safe to call after Start.
// If Start was never called, Wait returns immediately.
func (s *tuiProgressSink) Wait() {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		return
	}
	<-s.done
}

// TurnStarted implements ProgressSink. Drops the event if the program has
// already finished.
func (s *tuiProgressSink) TurnStarted(agent string, turn, _, _ int) {
	if !s.ready() {
		return
	}
	s.program.Send(turnStartedMsg{agent: agent, turn: turn})
}

// TurnFinished implements ProgressSink.
func (s *tuiProgressSink) TurnFinished(agent string, turn int, stance string, duration time.Duration, failed bool, err error, preview string) {
	if !s.ready() {
		return
	}
	s.program.Send(turnFinishedMsg{
		agent:    agent,
		turn:     turn,
		stance:   stance,
		duration: duration,
		failed:   failed,
		err:      err,
		findings: preview,
	})
}

// RunFinished implements ProgressSink. Blocks until the user dismisses the
// final dashboard (presses any key) so post-run output (the investigate
// footer in cmd.go) renders only after the TUI exits.
func (s *tuiProgressSink) RunFinished(outcome LoopOutcome) {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.mu.Unlock()

	s.program.Send(runFinishedMsg{outcome: outcome})
	s.Wait()
}

// ready returns true when the program is running and not yet finished.
func (s *tuiProgressSink) ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started && !s.finished
}

// Compile-time interface check.
var _ ProgressSink = (*tuiProgressSink)(nil)

package investigate

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// ProgressSink consumes turn lifecycle events from RunInvestigateLoop. The
// loop invokes the methods from a single goroutine — implementations need
// not synchronize against themselves.
//
// Implementations MUST NOT block. The loop calls these synchronously around
// the per-turn agent spawn; a slow sink stalls the entire investigation.
type ProgressSink interface {
	// TurnStarted is called immediately before the agent process starts for
	// the given turn. perAgentTurn is the 1-indexed count of turns this
	// agent has taken (this one included); maxPerAgent is the configured
	// per-agent budget.
	TurnStarted(agent string, turn, perAgentTurn, maxPerAgent int)

	// TurnFinished is called once after the agent process exits AND the
	// timeline doc has been parsed for the freshly-added turn block. stance
	// is one of "approve", "request-changes", "reject", "unknown". duration
	// is the wall-clock duration of the agent process. failed is true when
	// the turn was treated as a failure by the loop (spawn error, missing
	// heading, etc.); err is the underlying error or nil.
	TurnFinished(agent string, turn int, stance string, duration time.Duration, failed bool, err error, preview string)

	// RunFinished is called once when the loop terminates (any outcome).
	// The TUI uses this to flip rows to a terminal status and freeze the
	// dashboard; the text sink may print a final outcome line.
	RunFinished(outcome LoopOutcome)
}

// nullProgressSink is the zero-overhead default: every method is a no-op.
// Used when callers pass LoopDeps.Progress == nil.
type nullProgressSink struct{}

func (nullProgressSink) TurnStarted(string, int, int, int)                                    {}
func (nullProgressSink) TurnFinished(string, int, string, time.Duration, bool, error, string) {}
func (nullProgressSink) RunFinished(LoopOutcome)                                              {}

// textProgressSink writes the headless two-line shape to a plain io.Writer:
//
//	Turn N · <agent>
//	  Stance: <stance>
//
// Used when the terminal cannot render the Bubble Tea TUI (non-TTY stdout,
// CI, agent-host invocations). The mutex guards Writer access against
// RunFinished firing after the loop returns.
type textProgressSink struct {
	mu sync.Mutex
	w  io.Writer
}

func newTextProgressSink(w io.Writer) *textProgressSink {
	return &textProgressSink{w: w}
}

func (s *textProgressSink) TurnStarted(agent string, turn, _, _ int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w == nil {
		return
	}

	_, _ = fmt.Fprintf(s.w, "Turn %d · %s\n", turn, agent)
}

func (s *textProgressSink) TurnFinished(_ string, _ int, stance string, _ time.Duration, _ bool, _ error, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w == nil {
		return
	}

	_, _ = fmt.Fprintf(s.w, "  Stance: %s\n", stance)
}

func (s *textProgressSink) RunFinished(_ LoopOutcome) {
	// The text sink emits per-turn lines only; the post-run footer is the
	// caller's responsibility (writeInvestigateFooter in cmd.go).
}

// Compile-time interface checks.
var (
	_ ProgressSink = nullProgressSink{}
	_ ProgressSink = (*textProgressSink)(nil)
)

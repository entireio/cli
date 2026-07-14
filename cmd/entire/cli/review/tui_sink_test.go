package review

import (
	"bytes"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

func finishAndDismissTUI(t *testing.T, sink *TUISink, summary reviewtypes.RunSummary) {
	t.Helper()
	sink.RunFinished(summary)

	done := make(chan struct{})
	go func() {
		sink.PostRunComplete()
		close(done)
	}()

	select {
	case <-done:
		return
	case <-time.After(10 * time.Second):
		t.Fatal("PostRunComplete() did not return within 10 seconds")
	}
}

// TestTUISink_StartIsIdempotent verifies that calling Start multiple times
// does not panic or spawn extra goroutines.
func TestTUISink_StartIsIdempotent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := NewTUISink([]string{"agent-a"}, func() {}, &buf, bytes.NewReader(nil))

	// Start twice — the second call must be a no-op (no panic, no deadlock).
	sink.Start()
	sink.Start()

	// Clean up: send RunFinished and then the explicit post-run completion signal.
	finishAndDismissTUI(t, sink, reviewtypes.RunSummary{})

	// Wait with a timeout to avoid hanging the test suite on failure.
	done := make(chan struct{})
	go func() {
		sink.Wait()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() did not return within 5 seconds after RunFinished")
	}
}

// TestTUISink_WaitBeforeStart_IsNoOp verifies that calling Wait before Start
// returns immediately without blocking.
func TestTUIPostRunCompleteSinkFlushesAfterExit(t *testing.T) {
	t.Parallel()
	var tuiOut bytes.Buffer
	sink := NewTUISink([]string{"agent-a"}, func() {}, &tuiOut, bytes.NewReader(nil))
	sink.Start()
	sink.RunFinished(reviewtypes.RunSummary{})

	var postRunOut bytes.Buffer
	postRunBuf := bytes.NewBufferString("final verdict\n")
	done := make(chan struct{})
	go func() {
		tuiPostRunCompleteSink{tui: sink, buf: postRunBuf, out: &postRunOut}.RunFinished(reviewtypes.RunSummary{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("post-run finalizer did not exit the TUI and flush output")
	}
	if got := postRunOut.String(); got != "final verdict\n" {
		t.Fatalf("flushed output = %q, want final verdict", got)
	}
}

func TestTUISink_PostRunCompleteDoesNotHangWhenProgramNeverConsumesQuit(t *testing.T) {
	oldGrace := tuiPostRunCompleteGrace
	tuiPostRunCompleteGrace = 10 * time.Millisecond
	t.Cleanup(func() { tuiPostRunCompleteGrace = oldGrace })

	var buf bytes.Buffer
	sink := &TUISink{
		program: tea.NewProgram(newReviewTUIModel([]string{"agent-a"}, func() {}), tea.WithOutput(&buf), tea.WithInput(bytes.NewReader(nil))),
		started: true,
		done:    make(chan struct{}), // deliberately never closed: models a stuck Bubble Tea shutdown.
	}

	done := make(chan struct{})
	go func() {
		sink.PostRunComplete()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("PostRunComplete hung when the TUI did not consume postRunCompleteMsg")
	}
}

func TestTUISink_WaitBeforeStart_IsNoOp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := NewTUISink([]string{"agent-a"}, func() {}, &buf, bytes.NewReader(nil))

	done := make(chan struct{})
	go func() {
		sink.Wait() // should return immediately
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(time.Second):
		t.Fatal("Wait() before Start() did not return immediately")
	}
}

// TestTUISink_AgentEvent_BeforeStart_IsNoOp verifies that AgentEvent before
// Start does not panic.
func TestTUISink_AgentEvent_BeforeStart_IsNoOp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := NewTUISink([]string{"agent-a"}, func() {}, &buf, bytes.NewReader(nil))

	// Must not panic.
	sink.AgentEvent("agent-a", reviewtypes.Started{})
	sink.AgentEvent("agent-a", reviewtypes.AssistantText{Text: "hello"})
}

// TestTUISink_RunFinished_EventuallyUnblocks verifies that RunFinished unblocks
// once the finished TUI receives an explicit exit key (q) like a user would press.
func TestTUISink_RunFinished_EventuallyUnblocks(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := NewTUISink([]string{"agent-a"}, func() {}, &buf, bytes.NewReader(nil))
	sink.Start()

	// Send some events before finishing.
	sink.AgentEvent("agent-a", reviewtypes.Started{})
	sink.AgentEvent("agent-a", reviewtypes.AssistantText{Text: "reviewing…"})
	sink.AgentEvent("agent-a", reviewtypes.Finished{Success: true})

	finishAndDismissTUI(t, sink, reviewtypes.RunSummary{
		AgentRuns: []reviewtypes.AgentRun{
			{Name: "agent-a", Status: reviewtypes.AgentStatusSucceeded},
		},
	})
}

// TestTUISink_RunFinished_AfterSecondCall_IsNoOp verifies that calling
// RunFinished a second time does not block or panic.
func TestTUISink_RunFinished_AfterSecondCall_IsNoOp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := NewTUISink([]string{"agent-a"}, func() {}, &buf, bytes.NewReader(nil))
	sink.Start()

	// First RunFinished should unblock the program.
	finishAndDismissTUI(t, sink, reviewtypes.RunSummary{})

	// Second call should return immediately (no-op after finished=true).
	secondDone := make(chan struct{})
	go func() {
		sink.RunFinished(reviewtypes.RunSummary{})
		close(secondDone)
	}()

	select {
	case <-secondDone:
		// OK
	case <-time.After(time.Second):
		t.Fatal("second RunFinished call blocked unexpectedly")
	}
}

// TestTUISink_ImplementsSink verifies the compile-time interface constraint
// is reflected at test time too.
func TestTUISink_ImplementsSink(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var _ reviewtypes.Sink = NewTUISink(nil, func() {}, &buf, bytes.NewReader(nil))
}

// fakeFDWriter implements fdWriter with a controllable Fd, letting us drive
// terminalMeasurer through both branches (non-fdWriter → nil; fdWriter → a
// measurer that returns (0,0,false) for a non-terminal fd).
type fakeFDWriter struct {
	fd uintptr
}

func (f *fakeFDWriter) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeFDWriter) Fd() uintptr                 { return f.fd }

// TestTerminalMeasurer_NonFDWriter verifies that a writer without an Fd()
// method yields a nil measurer, which is the signal NewTUISink uses to skip
// the early measurement and rely on the first tea.WindowSizeMsg.
func TestTerminalMeasurer_NonFDWriter(t *testing.T) {
	t.Parallel()
	if got := terminalMeasurer(&bytes.Buffer{}); got != nil {
		t.Errorf("terminalMeasurer for non-fdWriter = non-nil, want nil")
	}
}

// TestTerminalMeasurer_FDWriter_InvalidFD verifies the happy-path shape:
// when the output is an fdWriter, terminalMeasurer returns a non-nil
// function. Calling it with a non-terminal fd surfaces ok=false (the
// fallback contract that NewTUISink relies on to not over-set termWidth).
func TestTerminalMeasurer_FDWriter_InvalidFD(t *testing.T) {
	t.Parallel()
	// fd=999999 is almost certainly not a real open descriptor on the test
	// process, so term.GetSize returns an error → measurer reports ok=false.
	measurer := terminalMeasurer(&fakeFDWriter{fd: 999999})
	if measurer == nil {
		t.Fatal("terminalMeasurer for fdWriter returned nil")
	}
	width, height, ok := measurer()
	if ok {
		t.Errorf("invalid fd should yield ok=false, got width=%d height=%d", width, height)
	}
	if width != 0 || height != 0 {
		t.Errorf("invalid fd should yield zero dims, got width=%d height=%d", width, height)
	}
}

// --- Non-blocking dispatch (wedge hardening) ---

// wedgedProgram is a teaRunner whose event loop never consumes messages:
// Send blocks until Kill, modeling a Bubble Tea program whose Update/render
// pipeline has stalled (the 2026-07-07 run-6 incident shape). Run blocks
// until Kill so the sink's done channel behaves like a live program's.
type wedgedProgram struct {
	killed chan struct{}
}

func newWedgedProgram() *wedgedProgram {
	return &wedgedProgram{killed: make(chan struct{})}
}

func (w *wedgedProgram) Run() (tea.Model, error) {
	<-w.killed
	return nil, nil //nolint:nilnil // mirrors tea.Program.Run's exit shape; callers ignore both values
}

func (w *wedgedProgram) Send(tea.Msg) { <-w.killed }

func (w *wedgedProgram) Kill() {
	select {
	case <-w.killed:
	default:
		close(w.killed)
	}
}

// recordingProgram is a teaRunner that records every message it receives.
type recordingProgram struct {
	killed chan struct{}
	mu     sync.Mutex
	msgs   []tea.Msg
}

func newRecordingProgram() *recordingProgram {
	return &recordingProgram{killed: make(chan struct{})}
}

func (r *recordingProgram) Run() (tea.Model, error) {
	<-r.killed
	return nil, nil //nolint:nilnil // mirrors tea.Program.Run's exit shape; callers ignore both values
}

func (r *recordingProgram) Send(msg tea.Msg) {
	r.mu.Lock()
	r.msgs = append(r.msgs, msg)
	r.mu.Unlock()
}

func (r *recordingProgram) Kill() {
	select {
	case <-r.killed:
	default:
		close(r.killed)
	}
}

func (r *recordingProgram) recorded() []tea.Msg {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]tea.Msg(nil), r.msgs...)
}

// TestTUISink_AgentEventNeverBlocksWhenProgramLoopIsWedged pins the wedge
// hardening: a stalled Bubble Tea loop must never backpressure the
// orchestrator. Before the fix, the first AgentEvent after the stall parked
// forever inside Program.Send, freezing sink dispatch, the fanIn drain loop,
// the parsers, and reviewer-timeout handling with them.
func TestTUISink_AgentEventNeverBlocksWhenProgramLoopIsWedged(t *testing.T) {
	t.Parallel()
	prog := newWedgedProgram()
	sink := newTUISinkWithProgram(prog)
	sink.Start()
	defer func() {
		prog.Kill()
		sink.Wait()
	}()

	finished := make(chan struct{})
	go func() {
		for range 3 * tuiSinkQueueCap {
			sink.AgentEvent("agent-a", reviewtypes.AssistantText{Text: "x"})
		}
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("AgentEvent blocked on a wedged TUI loop — orchestrator freeze")
	}

	if got := sink.droppedCount(); got == 0 {
		t.Error("expected overflow drops to be counted when the queue jams")
	}
}

// TestTUISink_EventsReachProgramInOrder pins that the async pump preserves
// dispatch order for a healthy program.
func TestTUISink_EventsReachProgramInOrder(t *testing.T) {
	t.Parallel()
	prog := newRecordingProgram()
	sink := newTUISinkWithProgram(prog)
	sink.Start()
	defer func() {
		prog.Kill()
		sink.Wait()
	}()

	for i := range 50 {
		sink.AgentEvent("agent-a", reviewtypes.AssistantText{Text: string(rune('a' + i%26))})
	}
	sink.RunFinished(reviewtypes.RunSummary{})

	deadline := time.After(5 * time.Second)
	for {
		msgs := prog.recorded()
		if len(msgs) >= 51 {
			for i := range 50 {
				if _, ok := msgs[i].(agentEventMsg); !ok {
					t.Fatalf("msgs[%d] = %T, want agentEventMsg", i, msgs[i])
				}
			}
			if _, ok := msgs[50].(runFinishedMsg); !ok {
				t.Fatalf("msgs[50] = %T, want runFinishedMsg (order violated)", msgs[50])
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("only %d/51 messages reached the program", len(msgs))
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestTUISink_RunFinishedBoundedWhenWedged pins that control messages use a
// bounded wait rather than blocking forever when the queue is jammed.
func TestTUISink_RunFinishedBoundedWhenWedged(t *testing.T) {
	t.Parallel()
	prog := newWedgedProgram()
	sink := newTUISinkWithProgram(prog)
	sink.Start()
	defer func() {
		prog.Kill()
		sink.Wait()
	}()

	// Jam the queue.
	for range 2 * tuiSinkQueueCap {
		sink.AgentEvent("agent-a", reviewtypes.AssistantText{Text: "x"})
	}

	finished := make(chan struct{})
	go func() {
		sink.RunFinished(reviewtypes.RunSummary{})
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(tuiPostRunCompleteGrace + 3*time.Second):
		t.Fatal("RunFinished blocked past its bounded wait on a wedged TUI")
	}
}

// stubbornProgram is a teaRunner whose Run NEVER returns, even after Kill —
// modeling a Bubble Tea teardown stuck restoring a blocked terminal. Send
// unblocks on Kill so the pump can drain, but done never closes.
type stubbornProgram struct {
	killed chan struct{}
	block  chan struct{}
}

func newStubbornProgram() *stubbornProgram {
	return &stubbornProgram{killed: make(chan struct{}), block: make(chan struct{})}
}

func (p *stubbornProgram) Run() (tea.Model, error) {
	<-p.block       // never closed — Run never returns
	return nil, nil //nolint:nilnil // unreachable; mirrors tea.Program.Run's shape
}

func (p *stubbornProgram) Send(tea.Msg) { <-p.killed }

func (p *stubbornProgram) Kill() {
	select {
	case <-p.killed:
	default:
		close(p.killed)
	}
}

// TestTUISink_WaitIsBoundedWhenProgramNeverExits pins the teardown guarantee:
// `defer tuiSink.Wait()` must not hang the command forever when the Bubble
// Tea program never returns from Run, even after Kill. Wait escalates
// (grace → Kill → grace) and then abandons the goroutine.
func TestTUISink_WaitIsBoundedWhenProgramNeverExits(t *testing.T) {
	t.Parallel()
	prog := newStubbornProgram()
	sink := newTUISinkWithProgram(prog)
	sink.Start()

	finished := make(chan struct{})
	go func() {
		sink.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2*tuiPostRunCompleteGrace + 3*time.Second):
		t.Fatal("Wait hung on a program that never exits — teardown wedge")
	}
}

// TestTUISink_WaitJoinsPump pins that a normal Wait joins the pump goroutine
// (no leak between done closing and the pump observing it).
func TestTUISink_WaitJoinsPump(t *testing.T) {
	t.Parallel()
	prog := newRecordingProgram()
	sink := newTUISinkWithProgram(prog)
	sink.Start()
	prog.Kill()
	sink.Wait()
	select {
	case <-sink.pumpDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait returned before the pump goroutine exited")
	}
}

// TestTUISink_GroupsWorkerEventsByAgent pins the one-row-per-agent display:
// skill fan-out runs N parallel workers per agent (claude-code:review,
// claude-code:pr-review), but the dashboard must show a single row per agent
// — the fan-out was behavior-only and must not leak per-skill rows. The sink
// routes each worker's events to its agent row.
func TestTUISink_GroupsWorkerEventsByAgent(t *testing.T) {
	t.Parallel()
	prog := newRecordingProgram()
	sink := newTUISinkWithProgram(prog)
	sink.groupWorkers([]string{tAgentClaude}, map[string]string{
		"claude-code:review":    tAgentClaude,
		"claude-code:pr-review": tAgentClaude,
	})
	sink.Start()
	defer func() { prog.Kill(); sink.Wait() }()

	sink.AgentEvent("claude-code:review", reviewtypes.AssistantText{Text: "a"})
	sink.AgentEvent("claude-code:pr-review", reviewtypes.AssistantText{Text: "b"})

	deadline := time.After(5 * time.Second)
	seen := 0
	for seen < 2 {
		select {
		case <-deadline:
			t.Fatalf("only %d/2 events reached the program", seen)
		default:
		}
		for _, m := range prog.recorded() {
			if ae, ok := m.(agentEventMsg); ok {
				if ae.agent != tAgentClaude {
					t.Fatalf("event routed to row %q, want collapsed agent row tAgentClaude", ae.agent)
				}
			}
		}
		seen = len(prog.recorded())
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTUISink_CollapsesSummaryToAgentRows pins that the per-worker
// RunFinished summary folds into one entry per agent row (worst-status wins,
// tokens summed) so the model's by-index row sync still aligns.
func TestTUISink_CollapsesSummaryToAgentRows(t *testing.T) {
	t.Parallel()
	sink := newTUISinkWithProgram(newRecordingProgram())
	sink.groupWorkers([]string{tAgentClaude, "codex"}, map[string]string{
		"claude-code:review":    tAgentClaude,
		"claude-code:pr-review": tAgentClaude,
		"codex":                 "codex",
	})
	in := reviewtypes.RunSummary{AgentRuns: []reviewtypes.AgentRun{
		{Name: "claude-code:review", Status: reviewtypes.AgentStatusSucceeded, Tokens: reviewtypes.Tokens{In: 100, Out: 10}},
		{Name: "claude-code:pr-review", Status: reviewtypes.AgentStatusFailed, Tokens: reviewtypes.Tokens{In: 50, Out: 5}},
		{Name: "codex", Status: reviewtypes.AgentStatusSucceeded, Tokens: reviewtypes.Tokens{In: 200, Out: 20}},
	}}
	got := sink.collapseSummaryForRows(in)
	if len(got.AgentRuns) != 2 {
		t.Fatalf("collapsed runs = %d, want 2 (one per agent row): %+v", len(got.AgentRuns), got.AgentRuns)
	}
	cc := got.AgentRuns[0]
	if cc.Name != tAgentClaude || cc.Status != reviewtypes.AgentStatusFailed {
		t.Errorf("claude-code row = {%s,%v}, want {claude-code, Failed} (worst-status wins)", cc.Name, cc.Status)
	}
	if cc.Tokens.In != 150 || cc.Tokens.Out != 15 {
		t.Errorf("claude-code tokens = %+v, want {150,15} (summed)", cc.Tokens)
	}
	if got.AgentRuns[1].Name != "codex" || got.AgentRuns[1].Tokens.In != 200 {
		t.Errorf("codex row = %+v, want single {codex,200}", got.AgentRuns[1])
	}
}

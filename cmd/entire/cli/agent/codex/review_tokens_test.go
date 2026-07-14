package codex

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

const tailTestThreadID = "019e8d8f-9d70-7021-b8fe-2c13802e3443"

func tokenLine(in, out int) string {
	return `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":` +
		`{"input_tokens":` + strconv.Itoa(in) + `,"output_tokens":` + strconv.Itoa(out) + `}}}}` + "\n"
}

func TestParseRolloutTokenCount(t *testing.T) {
	t.Parallel()
	in, out, ok := parseRolloutTokenCount([]byte(tokenLine(25338, 595)))
	if !ok || in != 25338 || out != 595 {
		t.Fatalf("token_count line: got in=%d out=%d ok=%v, want 25338/595/true", in, out, ok)
	}
	// Non-token_count lines are ignored.
	for _, line := range []string{
		`{"type":"response_item","payload":{"type":"reasoning"}}`,
		`{"type":"event_msg","payload":{"type":"agent_message"}}`,
		`not json`,
		``,
	} {
		if _, _, ok := parseRolloutTokenCount([]byte(line)); ok {
			t.Errorf("expected ok=false for %q", line)
		}
	}
}

// TestTailRolloutTokens_TailsAppendedLines is the core behavior: the tailer
// must emit Tokens for token_count lines that codex appends *after* the tailer
// has already caught up to EOF (a plain bufio.Reader would miss these).
func TestTailRolloutTokens_TailsAppendedLines(t *testing.T) {
	// Cannot t.Parallel — uses t.Setenv.
	dir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", dir)

	rollout := filepath.Join(dir, "rollout-2026-06-03T08-57-39-"+tailTestThreadID+".jsonl")
	if err := os.WriteFile(rollout, []byte(tokenLine(25338, 595)), 0o644); err != nil {
		t.Fatal(err)
	}

	out := make(chan reviewtypes.Event, 16)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		tailRolloutTokens(tailTestThreadID, out, stop, new(atomic.Bool))
		close(done)
	}()
	defer func() {
		close(stop)
		<-done
	}()

	first := awaitTokens(t, out)
	if first.In != 25338 || first.Out != 595 {
		t.Fatalf("first tokens = %+v, want {25338, 595}", first)
	}

	// Append a second token_count after the tailer caught up — it must see it.
	f, err := os.OpenFile(rollout, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(tokenLine(52798, 1123)); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	second := awaitTokens(t, out)
	if second.In != 52798 || second.Out != 1123 {
		t.Fatalf("second tokens = %+v, want {52798, 1123} (appended line not tailed)", second)
	}
}

// awaitTokens waits for the next Tokens event or fails on timeout.
func awaitTokens(t *testing.T, out <-chan reviewtypes.Event) reviewtypes.Tokens {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-out:
			if tk, ok := ev.(reviewtypes.Tokens); ok {
				return tk
			}
		case <-timeout:
			t.Fatal("timed out waiting for a Tokens event")
		}
	}
}

// startTailerFixture writes a rollout file for tailTestThreadID, starts the
// parser on a pipe, sends thread.started, and waits for the tailer's first
// Tokens. Returns the pipe writer, the event channel, and the rollout path.
func startTailerFixture(t *testing.T, firstLine string, wantIn, wantOut int) (*io.PipeWriter, <-chan reviewtypes.Event, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", dir)
	rollout := filepath.Join(dir, "rollout-2026-06-03T08-57-39-"+tailTestThreadID+".jsonl")
	if err := os.WriteFile(rollout, []byte(firstLine), 0o644); err != nil {
		t.Fatal(err)
	}

	pr, pw := io.Pipe()
	events := parseCodexOutput(pr)
	// Inline write is safe: the parser goroutine is already draining pr.
	if _, err := pw.Write([]byte(`{"type":"thread.started","thread_id":"` + tailTestThreadID + `"}` + "\n")); err != nil {
		t.Fatalf("write thread.started: %v", err)
	}

	// The tailer (not stdout — no turn.completed was written yet) must
	// deliver Tokens while the stream is still open.
	tk := awaitTokens(t, events)
	if tk.In != wantIn || tk.Out != wantOut {
		t.Fatalf("tailer tokens = %+v, want {%d, %d}", tk, wantIn, wantOut)
	}
	return pw, events, rollout
}

// collectUntilClose drains events until the channel closes, failing the test
// if it doesn't close within 5s.
func collectUntilClose(t *testing.T, events <-chan reviewtypes.Event) []reviewtypes.Event {
	t.Helper()
	var got []reviewtypes.Event
	drained := make(chan struct{})
	go func() {
		for ev := range events {
			got = append(got, ev)
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("event channel did not close — tailer not stopped")
	}
	return got
}

// TestParseCodexOutput_StartsRolloutTailerOnThreadStarted locks the wiring:
// the parser launches the rollout tailer when thread.started carries a
// thread_id, so Tokens flow from the rollout file between turn boundaries,
// and the parser stops the tailer and waits for it before closing the event
// channel (no send-on-closed-channel race).
func TestParseCodexOutput_StartsRolloutTailerOnThreadStarted(t *testing.T) {
	// Cannot t.Parallel — uses t.Setenv.
	pw, events, _ := startTailerFixture(t, tokenLine(11111, 22), 11111, 22)
	_ = pw.Close()
	collectUntilClose(t, events)
}

// TestParseCodexOutput_FinishedIsLastEvenWithPendingTailerLines pins the
// parser contract that Finished is the final event: the tailer must be
// stopped and awaited BEFORE the terminal emissions, not in a defer that
// runs after them — otherwise a tailer with unread rollout lines keeps
// sending Tokens after Finished.
func TestParseCodexOutput_FinishedIsLastEvenWithPendingTailerLines(t *testing.T) {
	// Cannot t.Parallel — uses t.Setenv.
	pw, events, rollout := startTailerFixture(t, tokenLine(1000, 50), 1000, 50)

	// Append a large backlog, then wait until the tailer is actively
	// draining it (a few backlog Tokens observed) before signalling EOF —
	// that pins the tailer mid-send exactly when the parser emits its
	// terminal events.
	f, err := os.OpenFile(rollout, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2000; i++ {
		if _, err := f.WriteString(tokenLine(1000+i, 50+i)); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	for range 3 {
		awaitTokens(t, events)
	}
	_ = pw.Close() // EOF with tailer mid-backlog

	got := collectUntilClose(t, events)
	if len(got) == 0 {
		t.Fatal("no events after EOF")
	}
	last := got[len(got)-1]
	if _, ok := last.(reviewtypes.Finished); !ok {
		t.Fatalf("last event = %#v, want Finished (Tokens after Finished violates the parser contract)", last)
	}
}

// TestParseCodexOutput_UsagelessTurnCompletedDoesNotClobberTailerTokens pins
// the backstop behavior: a terminal turn.completed WITHOUT a usage block
// must not emit Tokens{0,0} — under overwrite-not-sum consumer semantics
// that would erase the rollout tailer's genuine totals.
func TestParseCodexOutput_UsagelessTurnCompletedDoesNotClobberTailerTokens(t *testing.T) {
	// Cannot t.Parallel — uses t.Setenv.
	pw, events, _ := startTailerFixture(t, tokenLine(1000, 50), 1000, 50)

	if _, err := pw.Write([]byte(`{"type":"turn.completed"}` + "\n")); err != nil {
		t.Fatalf("write turn.completed: %v", err)
	}
	_ = pw.Close()

	got := collectUntilClose(t, events)
	// The tailer's {1000, 50} was already consumed by startTailerFixture;
	// it must remain the final observed value — any later Tokens (in
	// particular {0,0} from the old backstop) would clobber it under the
	// consumers' overwrite semantics.
	lastTokens := reviewtypes.Tokens{In: 1000, Out: 50}
	finishedOK := false
	for _, ev := range got {
		switch e := ev.(type) {
		case reviewtypes.Tokens:
			if e.In == 0 && e.Out == 0 {
				t.Fatalf("observed Tokens{0,0} — clobbers the tailer's totals")
			}
			lastTokens = e
		case reviewtypes.Finished:
			finishedOK = e.Success
		}
	}
	if !finishedOK {
		t.Error("turn.completed present: want Finished{Success:true}")
	}
	if lastTokens.In != 1000 || lastTokens.Out != 50 {
		t.Errorf("final tokens = %+v, want tailer's {1000, 50} to stand", lastTokens)
	}
}

// TestParseCodexOutput_TailerSuppressesPerTurnStdoutTokens pins single-source
// authority: rollout token_count totals are session-cumulative while
// turn.completed usage is per-turn scale, so once the tailer has emitted,
// per-turn stdout values must be suppressed — mixing the two makes the live
// counter flap between scales and the final value nondeterministic.
func TestParseCodexOutput_TailerSuppressesPerTurnStdoutTokens(t *testing.T) {
	// Cannot t.Parallel — uses t.Setenv.
	pw, events, rollout := startTailerFixture(t, tokenLine(1000, 50), 1000, 50)

	// The session-cumulative rollout advances to 2000/150...
	f, err := os.OpenFile(rollout, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(tokenLine(2000, 150)); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	// ...then a per-turn-scale turn.completed arrives on stdout.
	if _, err := pw.Write([]byte(`{"type":"turn.completed","usage":{"input_tokens":500,"output_tokens":30}}` + "\n")); err != nil {
		t.Fatalf("write turn.completed: %v", err)
	}
	_ = pw.Close()

	got := collectUntilClose(t, events)
	var lastTokens reviewtypes.Tokens
	for _, ev := range got {
		switch e := ev.(type) {
		case reviewtypes.Tokens:
			if e.In == 500 && e.Out == 30 {
				t.Fatalf("per-turn stdout Tokens{500,30} emitted despite active tailer — scale flap")
			}
			lastTokens = e
		case reviewtypes.Finished:
			if !e.Success {
				t.Error("want Finished{Success:true}")
			}
		}
	}
	if lastTokens.In != 2000 || lastTokens.Out != 150 {
		t.Errorf("final tokens = %+v, want the tailer's cumulative {2000, 150}", lastTokens)
	}
}

// TestTailRolloutTokens_PartialLineAppend covers the hand-rolled line buffer:
// a token_count written in two partial chunks must be parsed exactly once,
// when the newline completes it.
func TestTailRolloutTokens_PartialLineAppend(t *testing.T) {
	// Cannot t.Parallel — uses t.Setenv.
	dir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", dir)
	rollout := filepath.Join(dir, "rollout-2026-06-03T08-57-39-"+tailTestThreadID+".jsonl")
	line := tokenLine(31337, 42)
	half := len(line) / 2
	if err := os.WriteFile(rollout, []byte(line[:half]), 0o644); err != nil {
		t.Fatal(err)
	}

	out := make(chan reviewtypes.Event, 16)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		tailRolloutTokens(tailTestThreadID, out, stop, new(atomic.Bool))
		close(done)
	}()
	defer func() {
		close(stop)
		<-done
	}()

	// Give the tailer a moment on the partial line, then complete it.
	select {
	case ev := <-out:
		t.Fatalf("event %#v emitted from a partial line", ev)
	case <-time.After(600 * time.Millisecond):
	}
	f, err := os.OpenFile(rollout, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line[half:]); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	tk := awaitTokens(t, out)
	if tk.In != 31337 || tk.Out != 42 {
		t.Fatalf("tokens = %+v, want {31337, 42}", tk)
	}
}

// TestTailRolloutTokens_ReemitsLastTotalsOnStop pins the TOCTOU hardening:
// on stop, after the final catch-up drain, the tailer re-emits its last
// known totals. This guarantees the tailer's session-cumulative value is the
// final Tokens even if a per-turn stdout emission raced past the parser's
// tailerEmitted check in the instant before the tailer's first Store(true).
func TestTailRolloutTokens_ReemitsLastTotalsOnStop(t *testing.T) {
	// Cannot t.Parallel — uses t.Setenv.
	dir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", dir)
	rollout := filepath.Join(dir, "rollout-2026-06-03T08-57-39-"+tailTestThreadID+".jsonl")
	if err := os.WriteFile(rollout, []byte(tokenLine(7000, 300)), 0o644); err != nil {
		t.Fatal(err)
	}

	out := make(chan reviewtypes.Event, 16)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		tailRolloutTokens(tailTestThreadID, out, stop, new(atomic.Bool))
		close(done)
	}()

	first := awaitTokens(t, out)
	if first.In != 7000 || first.Out != 300 {
		t.Fatalf("first tokens = %+v, want {7000, 300}", first)
	}

	close(stop)
	<-done
	// The stop path must have re-emitted the last totals (dedup bypassed).
	select {
	case ev := <-out:
		tk, ok := ev.(reviewtypes.Tokens)
		if !ok || tk.In != 7000 || tk.Out != 300 {
			t.Fatalf("post-stop event = %#v, want re-emitted Tokens{7000, 300}", ev)
		}
	default:
		t.Fatal("no re-emitted Tokens after stop — TOCTOU window unguarded")
	}
}

func TestTailRolloutTokens_ReturnsOnStopWhenNoRollout(t *testing.T) {
	// Cannot t.Parallel — uses t.Setenv.
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", t.TempDir())
	out := make(chan reviewtypes.Event, 4)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		tailRolloutTokens(tailTestThreadID, out, stop, new(atomic.Bool))
		close(done)
	}()
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tailRolloutTokens did not return promptly after stop with no rollout file")
	}
}

// TestPollForRollout_KeepsLookingPastTheWindow pins that the poll never
// gives up while stop is open: a rollout that materialises after the
// expected-quickly window must still be found (previously the poll returned
// "" after ~30s and live tokens were lost for the rest of the run).
func TestPollForRollout_KeepsLookingPastTheWindow(t *testing.T) {
	// Cannot t.Parallel — uses t.Setenv.
	dir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", dir)
	rollout := filepath.Join(dir, "rollout-2026-06-03T08-57-39-"+tailTestThreadID+".jsonl")

	stop := make(chan struct{})
	defer close(stop)
	got := make(chan string, 1)
	go func() {
		got <- pollForRollout(context.Background(), dir, tailTestThreadID, stop, 3, 10*time.Millisecond)
	}()

	// Create the file well after the 3-attempt window has elapsed.
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(rollout, []byte(tokenLine(1, 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case path := <-got:
		if path != rollout {
			t.Fatalf("pollForRollout = %q, want %q (gave up instead of continuing past the window)", path, rollout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pollForRollout did not find the late rollout")
	}
}

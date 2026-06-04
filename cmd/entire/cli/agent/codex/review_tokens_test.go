package codex

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

const tailTestThreadID = "019e8d8f-9d70-7021-b8fe-2c13802e3443"

func tokenLine(in, out int) string {
	return `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":` +
		`{"input_tokens":` + itoa(in) + `,"output_tokens":` + itoa(out) + `}}}}` + "\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
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
		tailRolloutTokens(tailTestThreadID, out, stop)
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

func TestTailRolloutTokens_ReturnsOnStopWhenNoRollout(t *testing.T) {
	// Cannot t.Parallel — uses t.Setenv.
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", t.TempDir())
	out := make(chan reviewtypes.Event, 4)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		tailRolloutTokens(tailTestThreadID, out, stop)
		close(done)
	}()
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tailRolloutTokens did not return promptly after stop with no rollout file")
	}
}

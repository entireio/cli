package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// Polling/tailing cadence for the rollout token tailer.
const (
	rolloutPollInterval = 300 * time.Millisecond
	rolloutPollAttempts = 100 // ~30s for codex to create the rollout file
	rolloutTailInterval = 400 * time.Millisecond
	rolloutReadChunk    = 8192
)

// tailRolloutTokens resolves the codex rollout transcript for threadID and
// tails it, emitting a cumulative reviewtypes.Tokens event for every
// token_count codex writes (~once per model turn). codex's `exec --json`
// stdout only carries usage on turn.completed envelopes, and a review is
// usually a single turn — so without this, consumers see no token movement
// until the run ends. The rollout file is the same source codex's
// interactive UI reads for its live token counter.
//
// token_count.total_token_usage is a running SESSION total (not per-turn
// scale like turn.completed usage), so each emission is an absolute count —
// matching consumers' overwrite-not-sum semantics. Duplicate totals are
// suppressed so we only emit on real movement. emitted is set after the
// first successful send; the parser uses it to suppress its per-turn-scale
// stdout emissions so a single source stays authoritative.
//
// Returns when stop is closed (the stdout stream ended) — after one final
// catch-up drain of the file, so the last token_count codex wrote is not
// lost to tick timing — or when the rollout file never appears. The caller
// must wait for this to return before closing the event channel (see
// parseCodexOutputBuf), and the run contract guarantees the consumer drains
// events until close, so sends here can neither race a close nor deadlock.
func tailRolloutTokens(threadID string, out chan<- reviewtypes.Event, stop <-chan struct{}, emitted *atomic.Bool) {
	ctx := context.Background()
	sessionDir, err := (&CodexAgent{}).GetSessionDir("")
	if err != nil {
		logging.Debug(ctx, "codex token tail: session dir unresolved", slog.String("error", err.Error()))
		return
	}
	path := waitForRollout(ctx, sessionDir, threadID, stop)
	if path == "" {
		return
	}
	f, err := os.Open(path) //nolint:gosec // path is a glob match under codex's session dir, not user input
	if err != nil {
		logging.Debug(ctx, "codex token tail: open rollout failed", slog.String("error", err.Error()))
		return
	}
	defer f.Close()

	// Tail via os.File.Read rather than bufio.Reader: bufio is sticky on EOF
	// and would never observe lines codex appends after we first catch up.
	tail := rolloutTail{f: f, out: out, emitted: emitted, lastIn: -1, lastOut: -1}
	ticker := time.NewTicker(rolloutTailInterval)
	defer ticker.Stop()
	for {
		if err := tail.drain(); err != nil {
			logging.Debug(ctx, "codex token tail: read rollout failed", slog.String("error", err.Error()))
			return
		}
		select {
		case <-stop:
			// Final catch-up: codex may have flushed the terminal
			// token_count between our last drain and stream end.
			if err := tail.drain(); err != nil {
				logging.Debug(ctx, "codex token tail: final drain failed", slog.String("error", err.Error()))
			}
			// Re-emit the last totals unconditionally (bypassing dedup):
			// a per-turn stdout emission can race past the parser's
			// tailerEmitted check in the instant before this tailer's
			// first send is observed, and this re-send guarantees the
			// session-cumulative value is the final Tokens regardless.
			if tail.lastIn >= 0 {
				out <- reviewtypes.Tokens{In: tail.lastIn, Out: tail.lastOut}
			}
			return
		case <-ticker.C:
		}
	}
}

// rolloutTail holds the incremental read state for one rollout file.
type rolloutTail struct {
	f       *os.File
	out     chan<- reviewtypes.Event
	emitted *atomic.Bool
	pending []byte
	lastIn  int
	lastOut int
}

// drain reads the file to EOF, emitting Tokens for every complete
// token_count line with new totals. Returns a non-nil error only for
// non-EOF read failures (deleted file, I/O error) — persistent failures
// must stop the tailer instead of silently re-polling forever.
func (t *rolloutTail) drain() error {
	chunk := make([]byte, rolloutReadChunk)
	for {
		n, readErr := t.f.Read(chunk)
		if n > 0 {
			t.pending = append(t.pending, chunk[:n]...)
			for {
				idx := bytes.IndexByte(t.pending, '\n')
				if idx < 0 {
					break
				}
				line := t.pending[:idx]
				t.pending = t.pending[idx+1:]
				in, outTok, ok := parseRolloutTokenCount(line)
				if !ok || (in == t.lastIn && outTok == t.lastOut) {
					continue
				}
				t.lastIn, t.lastOut = in, outTok
				// Unconditional send is safe: the parser waits for the
				// tailer before closing the channel, and the run contract
				// guarantees the consumer drains until close.
				t.out <- reviewtypes.Tokens{In: in, Out: outTok}
				t.emitted.Store(true)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil // caught up — wait for the file to grow
			}
			return fmt.Errorf("read rollout: %w", readErr)
		}
	}
}

// waitForRollout polls for the rollout file matching threadID until it
// appears or stop fires — never giving up while the review is running, since
// a rollout that materialises late (slow codex startup, unusual layout
// timing) should still get live tokens for the rest of the run. After the
// expected-quickly window it debug-logs once (the likely signature of a
// codex release changing the rollout layout, which would otherwise silently
// disable live tokens) and backs off to a slower poll.
func waitForRollout(ctx context.Context, sessionDir, threadID string, stop <-chan struct{}) string {
	return pollForRollout(ctx, sessionDir, threadID, stop, rolloutPollAttempts, rolloutPollInterval)
}

func pollForRollout(ctx context.Context, sessionDir, threadID string, stop <-chan struct{}, window int, interval time.Duration) string {
	for attempt := 0; ; attempt++ {
		if path := findRolloutBySessionID(sessionDir, threadID); path != "" {
			return path
		}
		wait := interval
		if attempt >= window {
			if attempt == window {
				logging.Debug(ctx, "codex token tail: rollout file still missing; continuing to poll",
					slog.String("session_dir", sessionDir), slog.String("thread_id", threadID))
			}
			wait = interval * 8 // ~2.4s at production cadence — cheap for a minutes-long run
		}
		select {
		case <-stop:
			return ""
		case <-time.After(wait):
		}
	}
}

// parseRolloutTokenCount extracts cumulative input/output token totals from one
// rollout JSONL line. ok is false for any line that isn't a token_count event
// carrying total_token_usage. Reuses the rolloutLine/eventMsgPayload/
// tokenCountInfo shapes from transcript.go so the two readers can't drift.
func parseRolloutTokenCount(data []byte) (in, out int, ok bool) {
	var line rolloutLine
	if json.Unmarshal(data, &line) != nil || line.Type != "event_msg" {
		return 0, 0, false
	}
	var evt eventMsgPayload
	if json.Unmarshal(line.Payload, &evt) != nil || evt.Type != "token_count" || len(evt.Info) == 0 {
		return 0, 0, false
	}
	var info tokenCountInfo
	if json.Unmarshal(evt.Info, &info) != nil || info.TotalTokenUsage == nil {
		return 0, 0, false
	}
	return info.TotalTokenUsage.InputTokens, info.TotalTokenUsage.OutputTokens, true
}

package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
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
// stdout only carries usage on the terminal turn.completed envelope, and a
// review is usually a single turn — so without this the TUI shows no token
// movement until the run ends. The rollout file is the same source codex's
// interactive UI reads for its live token counter.
//
// token_count.total_token_usage is a running total, so each emission is an
// absolute count — matching the TUI's overwrite-not-sum semantics. Duplicate
// totals are suppressed so we only emit on real movement.
//
// Returns when stop is closed (the stdout stream ended) or the rollout file
// never appears. The caller must wait for this to return before closing the
// event channel (see parseCodexOutputBuf), so a send here can never race a
// channel close.
func tailRolloutTokens(threadID string, out chan<- reviewtypes.Event, stop <-chan struct{}) {
	ctx := context.Background()
	sessionDir, err := (&CodexAgent{}).GetSessionDir("")
	if err != nil {
		logging.Debug(ctx, "codex token tail: session dir unresolved", slog.String("error", err.Error()))
		return
	}
	path := waitForRollout(sessionDir, threadID, stop)
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
	var pending []byte
	chunk := make([]byte, rolloutReadChunk)
	lastIn, lastOut := -1, -1
	ticker := time.NewTicker(rolloutTailInterval)
	defer ticker.Stop()
	for {
		for {
			n, readErr := f.Read(chunk)
			if n > 0 {
				pending = append(pending, chunk[:n]...)
				for {
					idx := bytes.IndexByte(pending, '\n')
					if idx < 0 {
						break
					}
					line := pending[:idx]
					pending = pending[idx+1:]
					in, outTok, ok := parseRolloutTokenCount(line)
					if !ok || (in == lastIn && outTok == lastOut) {
						continue
					}
					lastIn, lastOut = in, outTok
					select {
					case out <- reviewtypes.Tokens{In: in, Out: outTok}:
					case <-stop:
						return
					}
				}
			}
			if readErr != nil {
				break // EOF or error — wait for the file to grow, then retry
			}
		}
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

// waitForRollout polls for the rollout file matching threadID, returning its
// path or "" if stop fires or the attempts are exhausted.
func waitForRollout(sessionDir, threadID string, stop <-chan struct{}) string {
	for range rolloutPollAttempts {
		if path := findRolloutBySessionID(sessionDir, threadID); path != "" {
			return path
		}
		select {
		case <-stop:
			return ""
		case <-time.After(rolloutPollInterval):
		}
	}
	return ""
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

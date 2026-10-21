package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/redact"
)

// compactWithAgent builds transcript.jsonl with the agent's own compactor, since
// the built-in one only knows built-in agents' formats. ok=false means the
// caller should fall back to the built-in compactor. The boundary is how many
// leading lines compacting the raw lines before startLine shares with the full
// output (0 if that fails), so a line the plugin merges across startLine stays
// in this checkpoint.
// agentCompactTimeout bounds each compactor run: they happen inside the
// checkpoint ref lock, so a slow plugin would stall every other checkpoint write.
var agentCompactTimeout = 5 * time.Second //nolint:gochecknoglobals // overridable for testing

func compactWithAgent(ctx context.Context, agentType types.AgentType, startLine int, transcriptBytes []byte) ([]byte, int, bool) {
	ag, err := agent.GetByAgentType(agentType)
	if err != nil {
		return nil, 0, false
	}
	compactor, ok := agent.AsTranscriptCompactor(ag)
	if !ok {
		return nil, 0, false
	}

	full, err := runAgentCompactor(ctx, compactor, transcriptBytes)
	if err != nil {
		logging.Warn(ctx, "agent compact transcript failed, using built-in compactor",
			slog.String("agent", string(agentType)),
			slog.String("error", err.Error()),
		)
		return nil, 0, false
	}

	boundary := 0
	if prefix := leadingLines(transcriptBytes, startLine); len(prefix) > 0 {
		if compactedPrefix, err := runAgentCompactor(ctx, compactor, prefix); err == nil {
			boundary = sharedLeadingLines(compactedPrefix, full)
		} else if errors.Is(err, context.DeadlineExceeded) {
			logging.Warn(ctx, "agent compact transcript prefix timed out, scoping from line 0",
				slog.String("agent", string(agentType)),
			)
		} else {
			logging.Debug(ctx, "agent compact transcript prefix not compactable, scoping from line 0",
				slog.String("agent", string(agentType)),
				slog.String("error", err.Error()),
			)
		}
	}
	return full, boundary, true
}

// runAgentCompactor passes the already-redacted bytes to the agent via a temp
// file (the protocol takes a path) and re-redacts the untrusted output.
func runAgentCompactor(ctx context.Context, compactor agent.TranscriptCompactor, transcriptBytes []byte) ([]byte, error) {
	tmp, err := os.CreateTemp("", "entire-compact-transcript-*.jsonl")
	if err != nil {
		return nil, fmt.Errorf("create temp transcript: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil {
			logging.Debug(ctx, "failed to remove temp compact transcript input",
				slog.String("path", tmpPath),
				slog.String("error", rmErr.Error()),
			)
		}
	}()
	if _, err := tmp.Write(transcriptBytes); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write temp transcript: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close temp transcript: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, agentCompactTimeout)
	defer cancel()
	compacted, err := compactor.CompactTranscript(runCtx, tmpPath)
	if err != nil {
		// The plugin's stderr is in err and may hold transcript text; keep it out of logs.
		if ctxErr := runCtx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("agent compact-transcript: %w", ctxErr)
		}
		return nil, errors.New("agent compact-transcript failed")
	}
	if compacted == nil || len(bytes.TrimSpace(compacted.Transcript)) == 0 {
		return nil, errors.New("agent returned an empty compact transcript")
	}
	// Checkpoints don't store agent-extracted assets.
	if len(compacted.Assets) > 0 {
		return nil, errors.New("agent compact transcript has assets, which checkpoints do not store")
	}
	redacted, err := redact.JSONLBytes(compacted.Transcript)
	if err != nil {
		return nil, fmt.Errorf("redact agent compact transcript: %w", err)
	}
	return redacted.Bytes(), nil
}

// leadingLines returns the first n lines of b, or all of b when it has fewer.
func leadingLines(b []byte, n int) []byte {
	if n <= 0 {
		return nil
	}
	end := 0
	for range n {
		next := bytes.IndexByte(b[end:], '\n')
		if next < 0 {
			return b
		}
		end += next + 1
	}
	return b[:end]
}

// sharedLeadingLines counts the leading non-blank lines that prefix and full have
// in common.
func sharedLeadingLines(prefix, full []byte) int {
	p, f := nonBlankLines(prefix), nonBlankLines(full)
	n := 0
	for n < len(p) && n < len(f) && bytes.Equal(p[n], f[n]) {
		n++
	}
	return n
}

func nonBlankLines(b []byte) [][]byte {
	var lines [][]byte
	for line := range bytes.SplitSeq(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			lines = append(lines, line)
		}
	}
	return lines
}

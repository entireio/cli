package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/redact"
)

// compactWithAgent builds transcript.jsonl with the agent's own compactor, since
// the built-in one only knows built-in agents' formats. ok=false means the
// caller should fall back to the built-in compactor. The boundary is the line
// count of compacting the raw lines before startLine (0 if that fails).
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
			boundary = min(countLines(compactedPrefix), countLines(full))
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

	compacted, err := compactor.CompactTranscript(ctx, tmpPath)
	if err != nil {
		return nil, err //nolint:wrapcheck // already labelled with the subcommand
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

// countLines counts non-blank lines.
func countLines(b []byte) int {
	n := 0
	for line := range bytes.SplitSeq(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			n++
		}
	}
	return n
}

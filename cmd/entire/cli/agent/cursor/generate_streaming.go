package cursor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

const streamBufferMax = 4 * 1024 * 1024 // 4 MiB

// parseCursorStream consumes `agent --print --output-format stream-json
// --stream-partial-output` NDJSON output, dispatches progress callbacks, and
// returns the canonical result text (from the terminal result event, not
// from concatenated assistant deltas — see note on aggregated final message
// below).
//
// Real-CLI shape (Cursor agent, Composer 2.5 Fast):
//   - system,subtype:init                          → PhaseConnecting (once)
//   - user.message                                 → ignored (echoed input)
//   - thinking,subtype:delta / completed           → ignored (internal
//     reasoning; PhaseConnecting persists during this window so users see
//     "Sending request to provider..." for longer on reasoning-heavy turns)
//   - assistant w/ timestamp_ms (1st)              → PhaseFirstToken (no parens —
//     cacheReadTokens isn't known until result event)
//   - assistant w/ timestamp_ms (subsequent)       → PhaseGenerating (running
//     token estimate from streamed character length)
//   - assistant w/o timestamp_ms                   → ignored (aggregated final
//     message; result.result is canonical)
//   - result,subtype:success                       → PhaseDone (real
//     OutputTokens / CachedInputTokens / DurationMs from result.usage and
//     result.duration_ms)
//   - result,is_error:true                         → typed error (privacy
//     decode: surfaces result.result only, never the raw line)
func parseCursorStream(stdout io.Reader, progress agent.ProgressFn) (string, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), streamBufferMax)

	var (
		connectingFired bool
		firstTokenFired bool
		sawResult       bool
		resultText      string
		streamedChars   int
		malformed       int
		usage           *cursorStreamUsage
		durationMs      int
	)

	dispatch := func(p agent.GenerationProgress) {
		if progress != nil {
			progress(p)
		}
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev cursorStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// Cursor may emit transient noise (blank lines, partial flushes);
			// a schema-incompatible line is recoverable per-event but tracked
			// so protocol regressions surface in the missing-result error
			// instead of disappearing silently.
			malformed++
			continue
		}

		switch ev.Type {
		case "system":
			if ev.Subtype == "init" && !connectingFired {
				connectingFired = true
				dispatch(agent.GenerationProgress{Phase: agent.PhaseConnecting})
			}

		case "assistant":
			// Skip the aggregated final assistant message (no timestamp_ms);
			// only delta events (with timestamp_ms) drive PhaseGenerating.
			if ev.TimestampMs == 0 {
				continue
			}
			var textBuilder strings.Builder
			if ev.Message != nil {
				for _, c := range ev.Message.Content {
					if c.Type == "text" {
						textBuilder.WriteString(c.Text)
					}
				}
			}
			text := textBuilder.String()
			if text == "" {
				continue
			}
			if !firstTokenFired {
				firstTokenFired = true
				// CacheReadTokens isn't known until the result event arrives;
				// FirstToken renders without parens here, and Done will
				// expose cached/output/duration from result.usage.
				dispatch(agent.GenerationProgress{Phase: agent.PhaseFirstToken})
			} else {
				streamedChars += len(text)
				dispatch(agent.GenerationProgress{Phase: agent.PhaseGenerating, OutputTokens: streamedChars / 4})
			}

		case "result":
			sawResult = true
			if ev.IsError {
				// Privacy decode: surface only result text (CLI-authored,
				// user-safe), never the raw line. Cursor's error envelope
				// has not been observed in the wild beyond synthesized
				// fixtures, so fall through to a generic message rather
				// than leaking anything we haven't audited.
				detail := ev.Result
				if detail == "" {
					detail = "unspecified error"
				}
				return "", fmt.Errorf("cursor stream error: %s", detail)
			}
			resultText = ev.Result
			usage = ev.Usage
			durationMs = ev.DurationMs

			// "thinking" and "user" events are intentionally ignored.
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading cursor stream: %w", err)
	}
	if !sawResult {
		if malformed > 0 {
			return "", fmt.Errorf("cursor stream ended without a result event (%d malformed lines skipped)", malformed)
		}
		return "", errors.New("cursor stream ended without a result event")
	}
	if resultText == "" {
		return "", errors.New("cursor stream produced no result text")
	}
	if progress != nil {
		done := agent.GenerationProgress{Phase: agent.PhaseDone, DurationMs: durationMs}
		if usage != nil {
			done.OutputTokens = usage.OutputTokens
			done.InputTokens = usage.InputTokens
			done.CachedInputTokens = usage.CacheReadTokens
		}
		dispatch(done)
	}
	return resultText, nil
}

// GenerateTextStreaming implements agent.StreamingTextGenerator for *CursorAgent.
func (c *CursorAgent) GenerateTextStreaming(
	ctx context.Context,
	prompt, model string,
	progress agent.ProgressFn,
) (string, error) {
	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName: "cursor",
		BuildCmd:  c.buildStreamCmd,
		Parser:    parseCursorStream,
		LooksLikeUnrecognizedFlag: func(stderr string) bool {
			return agent.LooksLikeUnrecognizedFlag(stderr, "stream-json", "stream-partial-output", "output-format")
		},
	}

	result, err := tmpl.Generate(ctx, prompt, model, progress)
	if err != nil {
		if errors.Is(err, agent.ErrUnrecognizedStreamingFlag) {
			return c.GenerateText(ctx, prompt, model)
		}
		return "", fmt.Errorf("cursor streaming generate: %w", err)
	}
	return result, nil
}

func (c *CursorAgent) buildStreamCmd(ctx context.Context, prompt, model string) *exec.Cmd {
	commandRunner := c.CommandRunner
	if commandRunner == nil {
		commandRunner = exec.CommandContext
	}
	args := []string{
		"--print",
		"--force",
		"--trust",
		"--workspace", os.TempDir(),
		"--output-format", "stream-json",
		"--stream-partial-output",
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	cmd := commandRunner(ctx, "agent", args...)
	cmd.Stdin = strings.NewReader(prompt)
	return cmd
}

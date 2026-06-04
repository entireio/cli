package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// envelopeTypeAssistant is the stream-json envelope type for assistant
// messages (per-content-block events). Shared with transcript.go's usage.
const envelopeTypeAssistant = "assistant"

// NewReviewer returns the AgentReviewer for claude-code.
//
// Argv shape: claude -p <prompt> --output-format stream-json --verbose.
// The prompt is passed as a command-line argument; stdin is unused.
// Stdout is newline-delimited JSON envelopes (one event per line), which the
// parser decodes into the review Event stream. This format gives the parser
// per-message granularity (each assistant content block surfaces as it is
// produced) instead of buffering until end-of-run like plain-text -p mode.
func NewReviewer() *reviewtypes.ReviewerTemplate {
	return &reviewtypes.ReviewerTemplate{
		AgentName: "claude-code",
		BuildCmd:  buildReviewCmd,
		Parser:    parseClaudeOutput,
	}
}

// buildReviewCmd builds the exec.Cmd for a claude review run.
// Exposed at package level for test inspection of argv and env.
func buildReviewCmd(ctx context.Context, cfg reviewtypes.RunConfig) *exec.Cmd {
	prompt := review.ComposeReviewPrompt(cfg)
	cmd := exec.CommandContext(ctx, "claude", "-p", prompt, "--output-format", "stream-json", "--verbose")
	cmd.Env = review.AppendReviewEnv(os.Environ(), "claude-code", cfg, prompt)
	return cmd
}

// parseClaudeOutput converts claude's --output-format stream-json --verbose
// stdout into a stream of Events. Each stdout line is one JSON envelope:
//   - {"type":"system",...}      session metadata / hooks; swallowed
//   - {"type":"assistant",...}   per content block: text → AssistantText,
//     tool_use → ToolCall, thinking → swallowed
//   - {"type":"user",...}        tool_result echoes; swallowed
//   - {"type":"result",...}      final summary; emits Tokens then Finished
//
// Emits Started first, Finished{Success:...} last (success follows result.is_error).
// On a scanner error (torn stream), emits RunError then Finished{Success:false}.
//
// Live-token semantics: Claude's assistant envelopes carry a usage snapshot
// taken at the START of each turn — input_tokens/cache_* are populated but
// output_tokens is essentially zero (a 1–8 token "initial decision" count
// that does not update as text streams). The true per-turn output is only
// surfaced on `result` (aggregate across all turns in the run) or on the
// late `message_delta` event of --include-partial-messages mode.
//
// We emit `Tokens{In: <sum>, Out: 0}` on each assistant envelope so the TUI
// can show context size growing across multi-turn runs, and `Tokens{In, Out}`
// on `result` with the final aggregate. Sending the misleading early
// "Out: 1" snapshot was removed after capturing real claude stream-json.
//
// Package-private; called directly from this package's tests so they can
// drive raw stdout fixtures through the parser without going through the
// ReviewerTemplate.Start spawn path.
func parseClaudeOutput(r io.Reader) <-chan reviewtypes.Event {
	return parseClaudeOutputBuf(r, claudeReviewMaxScannerBuf)
}

// claudeReviewMaxScannerBuf is the production bufio.Scanner cap for the Claude
// review parser. 64MB matches codex (which can pack large command stdout into
// aggregated_output); Claude's stream-json envelopes are small in practice but
// we share the cap so both parsers tolerate the same worst case. One buffer
// per active review run; memory cost is modest.
const claudeReviewMaxScannerBuf = 64 * 1024 * 1024

// parseClaudeOutputBuf is the parameterized variant of parseClaudeOutput, used
// by tests to shrink the scanner cap so the "token too long" branch can be
// exercised without writing 64MB of fixture data.
func parseClaudeOutputBuf(r io.Reader, maxBuf int) <-chan reviewtypes.Event {
	out := make(chan reviewtypes.Event, 32)
	go func() {
		defer close(out)
		out <- reviewtypes.Started{}
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, min(1024*1024, maxBuf)), maxBuf)
		var sawResult bool
		var resultErr bool
		var resultUsage messageUsage
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var env claudeEnvelope
			if err := json.Unmarshal(line, &env); err != nil {
				out <- reviewtypes.RunError{Err: fmt.Errorf("claude stream-json: %w", err)}
				continue
			}
			switch env.Type {
			case envelopeTypeAssistant:
				for _, block := range env.Message.Content {
					switch block.Type {
					case "text":
						if block.Text != "" {
							out <- reviewtypes.AssistantText{Text: block.Text}
						}
					case "tool_use":
						// block.Input is a json.RawMessage; passing it through as a
						// string preserves the agent-defined shape without a
						// re-marshal round trip. Empty input becomes "" so consumers
						// see a falsy Args.
						out <- reviewtypes.ToolCall{Name: block.Name, Args: string(block.Input)}
					}
				}
				// The usage snapshot here is captured at the start of the
				// turn, so output_tokens is essentially zero (1–8 tokens of
				// initial decision, not the true output count). Surface input
				// only — TUI consumers render input-only Tokens differently
				// from finalized {In, Out} pairs. The final tally comes from
				// the `result` envelope below.
				if env.Message.Usage.InputTokens > 0 ||
					env.Message.Usage.CacheReadInputTokens > 0 ||
					env.Message.Usage.CacheCreationInputTokens > 0 {
					in := env.Message.Usage.InputTokens +
						env.Message.Usage.CacheReadInputTokens +
						env.Message.Usage.CacheCreationInputTokens
					out <- reviewtypes.Tokens{In: in, Out: 0}
				}
			case "result":
				sawResult = true
				resultErr = env.IsError
				resultUsage = env.Usage
			}
		}
		if err := scanner.Err(); err != nil {
			out <- reviewtypes.RunError{Err: fmt.Errorf("read stdout: %w", err)}
			out <- reviewtypes.Finished{Success: false}
			return
		}
		if sawResult {
			in := resultUsage.InputTokens + resultUsage.CacheReadInputTokens + resultUsage.CacheCreationInputTokens
			out <- reviewtypes.Tokens{In: in, Out: resultUsage.OutputTokens}
			out <- reviewtypes.Finished{Success: !resultErr}
			return
		}
		out <- reviewtypes.Finished{Success: false}
	}()
	return out
}

type claudeEnvelope struct {
	Type    string        `json:"type"`
	Message claudeMessage `json:"message"`
	IsError bool          `json:"is_error"`
	// Usage reuses the package-local messageUsage type (declared in types.go)
	// rather than a duplicate ad-hoc struct, so the two consumers of the
	// Claude API usage shape (transcript parsing + stream-json review parser)
	// can't drift apart.
	Usage messageUsage `json:"usage"`
}

type claudeMessage struct {
	Content []claudeBlock `json:"content"`
	// Usage on assistant envelopes is the per-turn-START snapshot — input
	// counts are populated but output_tokens reflects only the model's
	// initial decision, not the streamed text. Final aggregate usage
	// arrives on the `result` envelope. Reuses messageUsage (declared in
	// types.go) to stay aligned with the transcript-parser usage shape.
	Usage messageUsage `json:"usage"`
}

type claudeBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

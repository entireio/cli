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
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}
	args = review.AppendModelFlag(args, cfg.Model)
	cmd := exec.CommandContext(ctx, "claude", args...)
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
// taken at the START of each API call — input_tokens/cache_* are populated
// but output_tokens is essentially zero (a 1–8 token "initial decision"
// count that does not update as text streams). The true output is only
// surfaced on `result` (aggregate across all calls in the run) or on the
// late `message_delta` event of --include-partial-messages mode.
//
// The Tokens contract (types/reviewer.go) is cumulative running totals, so
// the parser accumulates the input sum across unique message ids (the same
// usage block repeats verbatim on every content-block envelope of one API
// call — summing per envelope would multi-count) and emits
// `Tokens{In: <running sum>, Out: 0}` once per new message id. The running
// sum converges to the `result` aggregate, which is emitted last with the
// true {In, Out}. Out stays 0 mid-run because consumers render every Tokens
// event the same way — surfacing the 1–8 token stub would display a
// misleading real-looking output count.
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
		seenMsgIDs := map[string]struct{}{}
		var cumInputTokens int
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
				// Accumulate input once per unique message id: every
				// content-block envelope of one API call repeats the same
				// usage snapshot, and its output_tokens is a 1–8 token stub
				// (see the parser doc). Emitting the running sum keeps
				// mid-run values on the cumulative Tokens contract; the
				// true {In, Out} tally comes from `result` below.
				in := env.Message.Usage.InputTokens +
					env.Message.Usage.CacheReadInputTokens +
					env.Message.Usage.CacheCreationInputTokens
				if in > 0 && env.Message.ID != "" {
					if _, seen := seenMsgIDs[env.Message.ID]; !seen {
						seenMsgIDs[env.Message.ID] = struct{}{}
						cumInputTokens += in
						out <- reviewtypes.Tokens{In: cumInputTokens, Out: 0}
					}
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
			// Gate on non-zero usage: a result envelope without a usage
			// block would emit Tokens{0,0}, which only ever ERASES the
			// mid-run cumulative total under the consumers'
			// overwrite-not-sum semantics (mirrors the codex guard).
			in := resultUsage.InputTokens + resultUsage.CacheReadInputTokens + resultUsage.CacheCreationInputTokens
			if in > 0 || resultUsage.OutputTokens > 0 {
				out <- reviewtypes.Tokens{In: in, Out: resultUsage.OutputTokens}
			}
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
	// ID is the API message id — identical across the multiple
	// content-block envelopes of one API call; the parser dedupes usage
	// accumulation on it.
	ID      string        `json:"id"`
	Content []claudeBlock `json:"content"`
	// Usage on assistant envelopes is the per-call-START snapshot — input
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

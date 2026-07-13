package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

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

// reviewToolAllowlist is the read-only tool surface a headless claude
// review child gets pre-approved. Print mode (-p) auto-denies any tool call
// that would have prompted interactively, so without an explicit allowlist
// every finder that runs `git diff`/`git log` bounces off a denial and
// detours (slower reviews, findings verified by workaround or not at all).
//
// Deliberately narrow: read-only git subcommands are enumerated instead of
// granting Bash(git:*) — git can execute arbitrary code via aliases/hooks,
// and push/commit match the blanket pattern. No file tools that write (Edit,
// Write) and never --dangerously-skip-permissions: reviewers process
// untrusted input (the diff under review), so the child must stay unable to
// modify the repo. Known accepted residual: the granted subcommands accept
// --output=<path>, so a prompt-injected reviewer could write query output to
// an arbitrary path — defense-in-depth weakening, not code execution; the
// tradeoff is accepted because the alternative is denying the git access a
// review needs. --allowedTools ADDS to the user's own permission config; it
// cannot revoke anything.
var reviewToolAllowlist = []string{
	"Read", "Grep", "Glob", "Task", "TodoWrite", "Skill",
	"Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)",
	"Bash(git status:*)", "Bash(git blame:*)", "Bash(git rev-parse:*)",
	"Bash(git merge-base:*)", "Bash(git ls-files:*)",
	// `entire explain` is the command the injected checkpoint context tells
	// reviewers to run; granting only the canonical `checkpoint explain`
	// form left the prompt's own guidance auto-denied in headless -p.
	"Bash(entire search:*)", "Bash(entire checkpoint explain:*)", "Bash(entire explain:*)",
}

// buildReviewCmd builds the exec.Cmd for a claude review run.
// Exposed at package level for test inspection of argv and env.
func buildReviewCmd(ctx context.Context, cfg reviewtypes.RunConfig) *exec.Cmd {
	prompt := guardSlashExpansion(review.ComposeReviewPrompt(cfg))
	args := []string{
		"-p", prompt,
		"--output-format", "stream-json", "--verbose",
		"--allowedTools", strings.Join(reviewToolAllowlist, ","),
	}
	args = review.AppendModelFlag(args, cfg.Model)
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Env = review.AppendReviewEnv(os.Environ(), "claude-code", cfg, prompt)
	return cmd
}

// guardSlashExpansion prevents claude -p from interpreting the prompt as a
// slash command. The composed prompt leads with the configured skill line
// (e.g. "/pr-review-toolkit:review-pr"); print mode expands a leading slash
// token as a command invocation — observed to resolve to the BUILT-IN
// /review skill with the entire prompt (skill name included) as its
// arguments, so the configured skill never ran and the built-in PR-review
// harness treated the skill name as a PR number. A prose preamble makes
// expansion impossible while telling the agent to invoke the skill lines
// properly via the Skill tool (which the allowlist pre-approves).
func guardSlashExpansion(prompt string) string {
	if !strings.HasPrefix(prompt, "/") {
		return prompt
	}
	return "Perform the review below. Lines starting with \"/\" name configured review skills: invoke each with the Skill tool and follow it.\n\n" + prompt
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

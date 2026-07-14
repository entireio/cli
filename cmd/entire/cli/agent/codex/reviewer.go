package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// NewReviewer returns the AgentReviewer for codex.
//
// Argv shape: codex exec --skip-git-repo-check --json -.
// Prompt is piped via stdin (the trailing "-" tells codex to read from stdin).
// Stdout is newline-delimited JSON envelopes (one event per line); no chrome
// filter needed — each line is parsed directly into an Event.
func NewReviewer() *reviewtypes.ReviewerTemplate {
	return &reviewtypes.ReviewerTemplate{
		AgentName: "codex",
		BuildCmd:  buildCodexReviewCmd,
		Parser:    parseCodexOutput,
	}
}

// buildCodexReviewCmd builds the exec.Cmd for a codex review run.
// Exposed at package level for test inspection of argv, stdin, and env.
// buildCodexReviewCmd builds the exec.Cmd for a codex review run.
//
// Configured skills are passed through in codex's native $name form — NOT
// paraphrased. Codex's skill system injects a catalog of installed skills
// into every exec session and loads the matching SKILL.md when the prompt
// names one, so the agent runs the real configured workflow. A previous
// version silently REPLACED /review with a generic 28-word instruction: the
// configured skill never ran (the codex sibling of the claude -p
// slash-expansion bug, where the built-in /review hijacked the prompt).
//
// Native `codex exec review` is intentionally NOT used: it rejects an extra
// prompt when a scope flag is set, and codex hooks don't fire during it —
// leaving no channel for Entire's scope enumeration, per-run prompt, and
// checkpoint context. Plain `codex exec -` with the composed prompt on stdin
// runs the same skill while carrying our arguments.
func buildCodexReviewCmd(ctx context.Context, cfg reviewtypes.RunConfig) *exec.Cmd {
	promptCfg := cfg
	promptCfg.Skills = codexNativeSkillInvocations(cfg.Skills)
	args := []string{codexExecCommand, "--skip-git-repo-check", "--json"}
	args = review.AppendModelFlag(args, cfg.Model)
	args = append(args, "-")
	prompt := review.ComposeReviewPrompt(promptCfg)
	cmd := exec.CommandContext(ctx, "codex", args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = review.AppendReviewEnv(os.Environ(), "codex", cfg, prompt)
	return cmd
}

const codexExecCommand = "exec"

// codexNativeSkillInvocations rewrites slash-form skill invocations (the
// agent-portable form profiles are configured with) into codex's native
// $name form. Non-slash entries (plain instruction text) pass verbatim.
func codexNativeSkillInvocations(skills []string) []string {
	out := make([]string, 0, len(skills))
	for _, skill := range skills {
		if rest, ok := strings.CutPrefix(skill, "/"); ok && rest != "" {
			out = append(out, "$"+rest)
			continue
		}
		out = append(out, skill)
	}
	return out
}

// parseCodexOutput converts codex's `exec --json` stdout into a stream of
// Events. Each stdout line is one JSON envelope (top-level "type" field).
//
// Envelope types this parser handles:
//   - thread.started        session id; swallowed
//   - turn.started          marker; swallowed
//   - item.started          tool invocation begins → emits ToolCall when
//     item.type == "command_execution"
//   - item.completed        tool invocation ends OR an agent_message;
//     agent_message → AssistantText
//     command_execution → swallowed (start already announced)
//   - turn.completed        terminal usage block → Tokens, then Finished
//
// On a scanner error or a missing turn.completed envelope, emits RunError
// (scanner) or Finished{Success: false} (missing turn) accordingly.
//
// Live-token semantics: codex's `--json` output carries `usage` ONLY on
// `turn.completed` envelopes. Verified against codex-cli 0.130.0 stdout
// for both short and long prompts — no intermediate envelope
// (item.started, item.completed, etc.) carries a usage block. Codex's
// on-disk session log (the `event_msg{type:"token_count"}` shape the
// transcript parser consumes) is a separate format, not surfaced
// through `exec --json` — the rollout tailer (review_tokens.go) reads
// it for live counts between turn boundaries.
//
// Tokens are emitted at every `turn.completed` envelope so multi-turn
// runs show iterative updates.
//
// Package-private; called directly from this package's tests so they can
// drive raw stdout fixtures through the parser without going through the
// ReviewerTemplate.Start spawn path.
func parseCodexOutput(r io.Reader) <-chan reviewtypes.Event {
	return parseCodexOutputBuf(r, codexReviewMaxScannerBuf)
}

// codexReviewMaxScannerBuf is the production bufio.Scanner cap for the codex
// review parser. Codex packs the entire stdout of `command_execution` tools
// into the aggregated_output field on item.completed envelopes inline, so a
// chatty grep/cat/find over a large repo can put many MB into one envelope.
// 16MB was too tight; 64MB is generous without imposing real memory cost
// (one buffer per active review run).
const codexReviewMaxScannerBuf = 64 * 1024 * 1024

// parseCodexOutputBuf is the parameterized variant of parseCodexOutput, used
// by tests to shrink the scanner cap so the "token too long" branch can be
// exercised without writing 64MB of fixture data.
func parseCodexOutputBuf(r io.Reader, maxBuf int) <-chan reviewtypes.Event {
	out := make(chan reviewtypes.Event, 32)
	go func() {
		defer close(out)
		// The rollout token tailer (started on thread.started) runs concurrently
		// and also sends on out. It must be stopped and awaited BEFORE the
		// terminal Tokens/RunError/Finished emissions — Finished is contractually
		// the last event, and a lagging tailer tick would otherwise overwrite the
		// final recorded totals after completion. stopTailer is called explicitly
		// on every exit path ahead of the terminal sends; the deferred call is a
		// safety net (sync.Once) that also guarantees no send can hit the closed
		// channel.
		stop := make(chan struct{})
		var tailWG sync.WaitGroup
		var tailerEmitted atomic.Bool
		stopTailer := sync.OnceFunc(func() {
			close(stop)
			tailWG.Wait()
		})
		defer stopTailer()
		out <- reviewtypes.Started{}
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, min(1024*1024, maxBuf)), maxBuf)
		var seenTurnComplete, emittedTokens, tailerStarted bool
		var turnUsage codexUsage
		var failureMsg string
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var env codexEnvelope
			if err := json.Unmarshal(line, &env); err != nil {
				out <- reviewtypes.RunError{Err: fmt.Errorf("codex --json: %w", err)}
				continue
			}
			// Codex reports failures as a stdout envelope carrying a message
			// (type "error"/"turn.failed"/…) and exits non-zero with empty
			// stderr. Capture the message so the reason surfaces instead of a
			// bare "exit status 1". Only emitted below if the turn never
			// completes, so a stray message on a successful run is ignored.
			if msg := strings.TrimSpace(firstNonEmptyString(env.Error.Message, env.Message)); msg != "" {
				failureMsg = cleanCodexFailureMessage(msg)
			}
			// Add cases here when codex's envelope or item types grow; the
			// default arm logs unknown types at Debug so drift can be
			// triaged via ENTIRE_LOG_LEVEL=debug.
			switch env.Type {
			case "thread.started":
				// Launch the rollout token tailer once. codex's exec --json stdout
				// only carries usage on turn.completed, so we tail the rollout
				// file (located by thread_id) for live per-turn token totals —
				// the same source codex's interactive UI reads.
				if !tailerStarted && env.ThreadID != "" {
					tailerStarted = true
					tailWG.Add(1)
					go func(id string) {
						defer tailWG.Done()
						tailRolloutTokens(id, out, stop, &tailerEmitted)
					}(env.ThreadID)
				}
			case "turn.started":
				// Turn marker — no event emitted.
			case "item.started":
				if env.Item.Type == "command_execution" {
					out <- reviewtypes.ToolCall{Name: "exec", Args: env.Item.Command}
				}
			case "item.completed":
				if env.Item.Type == "agent_message" && env.Item.Text != "" {
					out <- reviewtypes.AssistantText{Text: env.Item.Text}
				}
				// command_execution completion is intentionally swallowed —
				// item.started already announced it. aggregated_output is the
				// tool's stdout, not the model's narrative.
			case "turn.completed":
				seenTurnComplete = true
				turnUsage = env.Usage
				// Emit Tokens at every turn boundary so multi-turn reviews
				// show iterative updates — but only while the rollout tailer
				// hasn't produced values: turn.completed usage is treated as
				// per-turn scale, the tailer's token_count totals are
				// session-cumulative, and mixing the two in one
				// overwrite-not-sum slot makes the counter flap between
				// scales. Once the tailer has emitted, it is the single
				// authoritative source.
				//
				// Scale caveat: whether turn.completed usage is per-turn or
				// session-cumulative is unverified against real MULTI-turn
				// codex output — exec-mode reviews are single-turn, where
				// the two are identical and this code is exact. In the rare
				// multi-turn no-rollout fallback, the recorded total is the
				// last turn's usage (an under-count if per-turn); when the
				// rollout tailer runs — the normal case — its cumulative
				// totals win regardless.
				//
				// codex reports cached_input_tokens as a subset of
				// input_tokens and reasoning_output_tokens as a subset of
				// output_tokens (matching OpenAI's chat-completions usage
				// shape), so do NOT sum the subset fields — that would
				// double-count.
				if !tailerEmitted.Load() && (env.Usage.InputTokens > 0 || env.Usage.OutputTokens > 0) {
					out <- reviewtypes.Tokens{
						In:  env.Usage.InputTokens,
						Out: env.Usage.OutputTokens,
					}
					emittedTokens = true
				}
			default:
				logging.Debug(context.Background(), "codex parser: unknown envelope type",
					slog.String("type", env.Type))
			}
		}
		// Stream over — stop the tailer BEFORE any terminal emission so
		// Finished stays the last event and no lagging tailer tick can
		// overwrite the final recorded totals.
		stopTailer()
		if err := scanner.Err(); err != nil {
			out <- reviewtypes.RunError{Err: fmt.Errorf("read stdout: %w", err)}
			out <- reviewtypes.Finished{Success: false}
			return
		}
		if !seenTurnComplete && failureMsg != "" {
			out <- reviewtypes.RunError{Err: fmt.Errorf("codex: %s", failureMsg)}
		}
		if seenTurnComplete {
			// Defensive backstop for a stream whose turn.completed carried
			// usage that never got emitted (can't happen today — the
			// per-turn arm emits whenever usage is non-zero and no tailer
			// value exists). Gated on non-zero usage: emitting {0,0} here
			// would only ever ERASE the tailer's genuine totals under the
			// consumers' overwrite-not-sum semantics.
			if !emittedTokens && !tailerEmitted.Load() &&
				(turnUsage.InputTokens > 0 || turnUsage.OutputTokens > 0) {
				out <- reviewtypes.Tokens{
					In:  turnUsage.InputTokens,
					Out: turnUsage.OutputTokens,
				}
			}
			// Success is hard-coded true here because codex's `turn.completed`
			// envelope has no turn-level error field in 0.130.0. If a future
			// codex version adds one (e.g., an `error` or `is_error` field on
			// the envelope), capture it into a local during the switch case and
			// thread it through here as `!turnErr` — mirroring claude's
			// `!resultErr` pattern.
			out <- reviewtypes.Finished{Success: true}
			return
		}
		out <- reviewtypes.Finished{Success: false}
	}()
	return out
}

type codexEnvelope struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"` // present on thread.started; locates the rollout file
	Item     codexItem       `json:"item"`
	Usage    codexUsage      `json:"usage"`
	Message  string          `json:"message"`
	Error    codexErrorField `json:"error"`
}

// codexErrorField captures the nested error message shape some codex envelopes
// use ({"error":{"message":"..."}}); top-level "message" covers the flat shape.
type codexErrorField struct {
	Message string `json:"message"`
}

// cleanCodexFailureMessage unwraps a codex failure message that is itself a
// JSON-encoded API error, returning the human-readable text at `.error.message`
// (or a top-level `.message`) instead of the raw blob. Codex surfaces upstream
// API errors this way — e.g. a model/version mismatch arrives as
// `{"type":"error","status":400,"error":{"message":"The '…' model requires a
// newer version of Codex…"}}`. A plain (non-JSON) message, or JSON without a
// message field, passes through unchanged. Unwrapping is bounded in case the
// message is nested more than once.
func cleanCodexFailureMessage(msg string) string {
	current := strings.TrimSpace(msg)
	for range 3 {
		trimmed := strings.TrimSpace(current)
		if !strings.HasPrefix(trimmed, "{") {
			return current
		}
		var wrapped struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrapped); err != nil {
			return current
		}
		inner := firstNonEmptyString(wrapped.Error.Message, wrapped.Message)
		if strings.TrimSpace(inner) == "" {
			return current
		}
		current = strings.TrimSpace(inner)
	}
	return current
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

type codexItem struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Text    string `json:"text"`
}

type codexUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

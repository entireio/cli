package pi

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
	"time"

	"golang.org/x/mod/semver"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// NewReviewer returns the AgentReviewer for Pi.
//
// Argv shape: pi --mode json --print --append-system-prompt <guardrail>
// [--model <model>].
// The prompt is piped via stdin, which Pi reads as the initial message in every
// non-RPC mode, the same as a positional message. argv is readable by other
// local users through the process table (ps, /proc/<pid>/cmdline) for the life
// of the run. Stdout is newline-delimited JSON
// session events; the parser maps Pi's AgentSessionEvent stream into Entire's
// review Event stream.
func NewReviewer() *reviewtypes.ReviewerTemplate {
	return &reviewtypes.ReviewerTemplate{
		AgentName: string(agent.AgentNamePi),
		BuildCmd:  buildPiReviewCmd,
		Parser:    parsePiReviewOutput,
		Prepare:   preparePiReviewConfig,
		Preflight: func(ctx context.Context) error {
			return checkPiReviewVersion(ctx, piVersion)
		},
	}
}

// MinPiStdinJSONVersion is the first Pi release that keeps --mode json output
// for a prompt piped on stdin. Earlier releases fall back to plain text, which
// the review parser cannot read.
const MinPiStdinJSONVersion = "0.65.1"

// piVersionTimeout bounds the `pi --version` probe; it reads no config and
// makes no network call, so a slow answer means something is wrong.
const piVersionTimeout = 10 * time.Second

// piVersion returns what `pi --version` prints (Pi writes it to stderr).
func piVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, piVersionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pi", "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("pi --version: %w", err)
	}
	return string(out), nil
}

// checkPiReviewVersion refuses a Pi too old to read the review prompt from
// stdin in --mode json. A version it cannot run or parse is let through: the
// review then fails or succeeds on its own, as it did before this check.
func checkPiReviewVersion(ctx context.Context, version func(context.Context) (string, error)) error {
	raw, err := version(ctx)
	if err != nil {
		return nil //nolint:nilerr // best-effort probe; a missing pi fails at Start with its own error
	}
	// Pi 0.70 prints just the version; take the first full x.y.z token so a
	// prefix ("pi 0.64.0"), a trailing notice, or a bare number in a settings
	// warning printed first ("...position 25...") can't hide an old release.
	// semver alone would accept shorthand such as v25.
	have := ""
	for _, field := range strings.Fields(raw) {
		v := "v" + strings.TrimPrefix(field, "v")
		if semver.Canonical(v) == v {
			have = v[1:]
			break
		}
	}
	if have == "" || semver.Compare("v"+have, "v"+MinPiStdinJSONVersion) >= 0 {
		return nil
	}
	// Start prefixes the agent name.
	return fmt.Errorf("version %s is too old for review: it needs pi >= %s to keep JSON output for a prompt piped on stdin; upgrade pi and retry", have, MinPiStdinJSONVersion)
}

func buildPiReviewCmd(ctx context.Context, cfg reviewtypes.RunConfig) *exec.Cmd {
	prompt := review.ComposeReviewPrompt(cfg)
	args := []string{"--mode", "json", "--print", "--append-system-prompt", review.ReviewerGuardrail}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	args = append(args, cfg.ExtraArgs...)
	cmd := exec.CommandContext(ctx, "pi", args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = review.AppendReviewEnv(os.Environ(), string(agent.AgentNamePi), cfg, prompt)
	return cmd
}

func parsePiReviewOutput(r io.Reader) <-chan reviewtypes.Event {
	out := make(chan reviewtypes.Event, 32)
	go func() {
		defer close(out)
		out <- reviewtypes.Started{}

		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, min(1024*1024, piReviewMaxScannerBuf)), piReviewMaxScannerBuf)
		// inTextBlock marks that a delta already started the current text
		// block, so the next one continues it; a new block starts a new line.
		// messageHadDelta marks that the current message streamed text, so
		// message_end does not emit it again. Message IDs can be empty, so
		// both track position in the stream instead of IDs.
		inTextBlock := false
		messageHadDelta := false
		// pendingErr is the latest failure reason. It is reported only if the
		// run still fails at the end: Pi auto-retries errors such as
		// "overloaded", and a recovered error is not a failure.
		pendingErr := ""
		messageIDsWithUsage := map[string]struct{}{}
		messageUsageByTurn := map[int]map[piReviewUsageKey]struct{}{}
		turnNumber := 0
		tokens := reviewtypes.Tokens{}
		finished := false
		success := true
		// lastTurnStop is the stopReason of the latest turn_end. Some Pi
		// versions end the stream after a final turn without agent_end.
		lastTurnStop := ""
		// lengthErr explains a turn cut off at the output token limit. It
		// fails the review only when that turn ends the run: Pi recovers from
		// a cut-off tool call by asking the model to re-issue it.
		lengthErr := ""

		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var env piReviewEnvelope
			if err := json.Unmarshal(line, &env); err != nil {
				out <- reviewtypes.RunError{Err: fmt.Errorf("pi --mode json: %w", err)}
				continue
			}

			switch env.Type {
			case "turn_start":
				turnNumber++
				lastTurnStop = "" // a turn cut off before turn_end didn't finish
			case "auto_retry_start":
				// Pi is retrying the failed attempt; judge the retry instead.
				success = true
				pendingErr = ""
				lastTurnStop = ""
				inTextBlock = false
				messageHadDelta = false
			case "session", "agent_start", "queue_update", "compaction_start", "compaction_end", "auto_retry_end":
				// Session/control events do not map to user-visible review output.
			case "message_update":
				if env.AssistantMessageEvent.Type == "text_start" {
					inTextBlock = false
				}
				if text := env.AssistantMessageEvent.TextDelta(); text != "" {
					out <- reviewtypes.AssistantText{Text: text, Delta: inTextBlock}
					inTextBlock = true
					messageHadDelta = true
				}
			case "message_end":
				sawDelta := messageHadDelta
				inTextBlock = false
				messageHadDelta = false
				if env.Message.Role == "assistant" {
					if piReviewFailed(env.Message.StopReason) {
						success = false
						pendingErr = piReviewFailureReason(env.Message)
					}
					if env.Message.Usage != nil {
						emitPiReviewTokens(out, env, &tokens, messageIDsWithUsage, messageUsageByTurn, turnNumber)
					}
					if !sawDelta {
						if text := piReviewMessageText(env.Message.Content); text != "" {
							out <- reviewtypes.AssistantText{Text: text}
						}
					}
				}
			case "tool_execution_start":
				out <- reviewtypes.ToolCall{Name: env.ToolName, Args: piReviewJSONArg(env.Args)}
			case "tool_execution_end":
				// Tool errors are part of normal agent execution (for example grep
				// finding no matches). The agent's stopReason determines review
				// success/failure.
			case "turn_end":
				lastTurnStop = env.Message.StopReason
				if piReviewFailed(env.Message.StopReason) {
					success = false
					pendingErr = piReviewFailureReason(env.Message)
				}
				if env.Message.StopReason == piStopLength {
					lengthErr = piReviewFailureReason(env.Message)
				}
				if env.Message.Usage != nil {
					emitPiReviewTokens(out, env, &tokens, messageIDsWithUsage, messageUsageByTurn, turnNumber)
				}
			case "agent_end":
				if env.WillRetry {
					// The retry continues in this stream and ends in its own agent_end.
					continue
				}
				finished = true
				if lastTurnStop == piStopLength {
					success = false
					pendingErr = lengthErr
				}
				if !success && pendingErr != "" {
					out <- reviewtypes.RunError{Err: errors.New(pendingErr)}
				}
				out <- reviewtypes.Finished{Success: success}
			default:
				// Unknown future events are ignored; Pi's event stream is additive.
			}
		}

		if err := scanner.Err(); err != nil {
			out <- reviewtypes.RunError{Err: fmt.Errorf("read stdout: %w", err)}
			out <- reviewtypes.Finished{Success: false}
			return
		}
		if !finished {
			// A stream that ends right after a turn the model stopped on
			// finished the review, as agent_end would have. Anything else (a
			// turn waiting on tool results, cut off, or still being retried)
			// was cut short.
			if lastTurnStop == "stop" && success {
				out <- reviewtypes.Finished{Success: true}
				return
			}
			if lastTurnStop == piStopLength {
				pendingErr = lengthErr
			}
			if pendingErr != "" {
				out <- reviewtypes.RunError{Err: errors.New(pendingErr)}
			}
			out <- reviewtypes.Finished{Success: false}
		}
	}()
	return out
}

const piReviewMaxScannerBuf = 64 * 1024 * 1024

type piReviewEnvelope struct {
	Type                  string                  `json:"type"`
	ID                    string                  `json:"id"`
	Message               piReviewMessage         `json:"message"`
	AssistantMessageEvent piAssistantMessageEvent `json:"assistantMessageEvent"`
	ToolName              string                  `json:"toolName"`
	Args                  json.RawMessage         `json:"args"`
	// WillRetry marks an agent_end Pi follows with an automatic retry.
	WillRetry bool `json:"willRetry"`
}

func (e piReviewEnvelope) MessageID() string {
	if e.Message.ID != "" {
		return e.Message.ID
	}
	return e.ID
}

type piReviewMessage struct {
	ID         string          `json:"id"`
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Usage      *piReviewUsage  `json:"usage"`
	StopReason string          `json:"stopReason"`
	// ErrorMessage explains an "error" or "aborted" stop, e.g. a provider
	// rejecting the request; without it a failed review shows an empty report.
	ErrorMessage string `json:"errorMessage"`
}

// piStopLength is the stop reason for a message cut off at the output token
// limit.
const piStopLength = "length"

// piReviewFailed reports a stop that fails the review whatever follows: an
// error or an abort. A "length" stop is judged on the run's last turn.
func piReviewFailed(stopReason string) bool {
	return stopReason == "error" || stopReason == "aborted"
}

// piReviewFailureReason explains a failed message: Pi's errorMessage, or the
// stop reason when Pi gave none.
func piReviewFailureReason(msg piReviewMessage) string {
	if reason := strings.TrimSpace(msg.ErrorMessage); reason != "" {
		return "pi: " + reason
	}
	if msg.StopReason == piStopLength {
		return "pi: stopped at the output token limit, so the review is incomplete"
	}
	return "pi: stopped with " + msg.StopReason
}

type piAssistantMessageEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
	Text  string `json:"text"`
}

func (e piAssistantMessageEvent) TextDelta() string {
	switch e.Type {
	case "text_delta":
		if e.Delta != "" {
			return e.Delta
		}
		return e.Text
	default:
		return ""
	}
}

type piReviewUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
}

type piReviewUsageKey struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
}

func emitPiReviewTokens(out chan<- reviewtypes.Event, env piReviewEnvelope, total *reviewtypes.Tokens, seen map[string]struct{}, messageUsageByTurn map[int]map[piReviewUsageKey]struct{}, turnNumber int) {
	if env.Message.Usage == nil || total == nil {
		return
	}
	if shouldSkipPiReviewUsage(env, seen, messageUsageByTurn, turnNumber) {
		return
	}
	*total = addPiReviewTokens(*total, env.Message.Usage)
	out <- *total
}

func shouldSkipPiReviewUsage(env piReviewEnvelope, seen map[string]struct{}, messageUsageByTurn map[int]map[piReviewUsageKey]struct{}, turnNumber int) bool {
	usage := env.Message.Usage
	if usage == nil {
		return true
	}
	sig := piReviewUsageKey{Input: usage.Input, Output: usage.Output, CacheRead: usage.CacheRead, CacheWrite: usage.CacheWrite}
	if env.Type == "message_end" && messageUsageByTurn != nil {
		if messageUsageByTurn[turnNumber] == nil {
			messageUsageByTurn[turnNumber] = map[piReviewUsageKey]struct{}{}
		}
		messageUsageByTurn[turnNumber][sig] = struct{}{}
	}
	if key := env.MessageID(); key != "" {
		if _, ok := seen[key]; ok {
			return true
		}
		seen[key] = struct{}{}
		return false
	}
	// Pi streams can emit usage on both message_end and turn_end. Some realistic
	// streams omit ids on both events, so fall back to the current turn's usage
	// signature to avoid counting a no-id turn_end duplicate of the message_end.
	if env.Type == "turn_end" && messageUsageByTurn != nil {
		if _, ok := messageUsageByTurn[turnNumber][sig]; ok {
			return true
		}
	}
	return false
}

func addPiReviewTokens(total reviewtypes.Tokens, usage *piReviewUsage) reviewtypes.Tokens {
	if usage == nil {
		return total
	}
	// Pi's usage shape is normalized across providers. For OpenAI-shaped
	// backends, cached input is reported as a subset of input tokens; summing
	// cacheRead/cacheWrite into Tokens.In would therefore double-count. The
	// review event contract has only aggregate input/output fields, so report
	// the provider's top-level input total and leave cache detail to transcript
	// token accounting, which stores cache fields separately.
	total.In += usage.Input
	total.Out += usage.Output
	return total
}

func piReviewJSONArg(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	return string(raw)
}

func piReviewMessageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	text := ""
	for _, item := range items {
		if item.Type != "text" || item.Text == "" {
			continue
		}
		if text != "" {
			text += "\n"
		}
		text += item.Text
	}
	return text
}

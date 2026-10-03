package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/textutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// attachTranscriptStart scopes append-only JSONL snapshots to their new content.
// Compare stored bytes, after sanitization/redaction, rather than trusting a
// stale line count after an agent rewrites or truncates its transcript. If the
// earlier snapshot is absent or no longer a complete prefix (including a changed
// redaction policy), capture the full transcript instead of skipping content.
// OpenCode exports a JSON document with message offsets, not JSONL line offsets.
func attachTranscriptStart(ctx context.Context, store cpkg.PersistentStore, state *session.State, agentType types.AgentType, current []byte) (int, error) {
	if state == nil || state.LastCheckpointID.IsEmpty() || agentType == agent.AgentTypeOpenCode {
		return 0, nil
	}
	index, err := attachSessionIndex(ctx, store, state.LastCheckpointID, state.SessionID)
	if err != nil {
		return attachPrefixUnavailable(ctx, err)
	}
	if index < 0 {
		return 0, nil
	}
	content, err := store.ReadSessionContent(ctx, state.LastCheckpointID, index)
	if errors.Is(err, cpkg.ErrNoTranscript) {
		return 0, nil
	}
	if err != nil {
		return attachPrefixUnavailable(ctx, err)
	}
	if content == nil {
		return 0, nil
	}
	previous := content.Transcript
	if !bytes.HasSuffix(previous, []byte{'\n'}) || !bytes.HasPrefix(current, previous) {
		return 0, nil
	}
	return bytes.Count(previous, []byte{'\n'}), nil
}

// Previous snapshots are only prefix evidence; losing them must not prevent
// capturing the full current transcript. Cancellation still stops the command.
func attachPrefixUnavailable(ctx context.Context, err error) (int, error) {
	if ctx.Err() != nil {
		return 0, fmt.Errorf("read previous attach snapshot: %w", ctx.Err())
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 0, fmt.Errorf("read previous attach snapshot: %w", err)
	}
	logging.Warn(ctx, "previous attach snapshot unavailable; capturing full transcript", "error", err)
	return 0, nil
}

// attachCheckpointTokens translates the stored line boundary into the agent's
// token-calculation offset, retaining the cumulative result for full snapshots.
func attachCheckpointTokens(ctx context.Context, ag agent.Agent, data []byte, startLine int, total *agent.TokenUsage) *agent.TokenUsage {
	if startLine == 0 {
		return total
	}
	offset := startLine
	if external.IsExternal(ag) {
		// The external protocol takes byte offsets into the raw transcript;
		// redacted byte lengths differ and cannot address the original input.
		offset = len(data) - len(transcript.SliceFromLine(data, startLine))
	}
	return agent.CalculateTokenUsage(ctx, ag, data, offset, "")
}

// transcriptMetadata holds metadata extracted from a single transcript parse pass.
type transcriptMetadata struct {
	FirstPrompt string
	TurnCount   int
	Model       string
}

// extractTranscriptMetadata parses transcript bytes once and extracts the first user prompt,
// user turn count, and model name from JSONL transcripts (Claude Code, Cursor, OpenCode).
func extractTranscriptMetadata(data []byte) transcriptMetadata {
	var meta transcriptMetadata

	// firstUserPrompt is the unfiltered first prompt, kept as a last-resort title
	// for a transcript whose every user message is agent-injected.
	var firstUserPrompt string

	// Try JSONL format first (Claude Code, Cursor, OpenCode, etc.)
	lines, err := transcript.ParseFromBytes(data)
	if err == nil {
		for _, line := range lines {
			if line.Type == transcript.TypeUser {
				if prompt := transcript.ExtractUserContent(line.Message); prompt != "" {
					if firstUserPrompt == "" {
						firstUserPrompt = prompt
					}
					// An injected preamble is not a user turn: counting it
					// inflates the step count attach reports.
					if textutil.IsInjectedPrompt(prompt) {
						continue
					}
					meta.TurnCount++
					if meta.FirstPrompt == "" {
						meta.FirstPrompt = prompt
					}
				}
			}
			if line.Type == transcript.TypeAssistant && meta.Model == "" {
				var msg struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(line.Message, &msg) == nil && msg.Model != "" {
					meta.Model = msg.Model
				}
			}
		}
		// A checkpoint with no prompt at all is worse than one titled with a
		// noisy-but-present preamble, and an empty FirstPrompt alongside a
		// non-zero TurnCount would also suppress warnEmptyTranscriptMetadata.
		if meta.FirstPrompt == "" {
			meta.FirstPrompt = firstUserPrompt
		}
	}

	return meta
}

// countUserTurns counts the prompts that represent an actual user turn, skipping
// agent-injected preambles and notifications. Once a prompt is deemed not to be
// user-authored it must not be counted as a user turn either, or attach reports
// more steps than the session had.
func countUserTurns(prompts []string) int {
	turns := 0
	for _, prompt := range prompts {
		if strings.TrimSpace(prompt) == "" || textutil.IsInjectedPrompt(prompt) {
			continue
		}
		turns++
	}
	return turns
}

// extractTranscriptMetadataForAgent augments the generic attach parser with
// agent-native prompt and model extraction when available. Native extractors
// are authoritative because they understand format-specific nesting and
// conversation branches (Pi, Codex, Droid, etc.); failures remain best-effort
// and preserve whatever the generic parser found.
func extractTranscriptMetadataForAgent(ag agent.Agent, sessionRef string, data []byte) transcriptMetadata {
	meta := extractTranscriptMetadata(data)

	if extractor, ok := agent.AsPromptExtractor(ag); ok {
		if prompts, err := extractor.ExtractPrompts(sessionRef, 0); err == nil && len(prompts) > 0 {
			// Native extractors return every user-role item in transcript order,
			// including the agent's own injected preambles (Codex leads with an
			// AGENTS.md dump and/or <environment_context>). Title from the first
			// genuine prompt, falling back to the raw first one so the checkpoint
			// is never left untitled.
			if first := strategy.FirstDisplayPrompt(prompts); first != "" {
				meta.FirstPrompt = first
			} else {
				meta.FirstPrompt = prompts[0]
			}
			meta.TurnCount = countUserTurns(prompts)
		}
	}
	if extractor, ok := agent.AsModelExtractor(ag); ok {
		if model, err := extractor.ExtractModel(data); err == nil && model != "" {
			meta.Model = model
		}
	}

	return meta
}

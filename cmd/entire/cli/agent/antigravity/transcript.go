package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertion.
var _ agent.PromptExtractor = (*AntigravityAgent)(nil)

// Antigravity 2.0 (agy) writes JSONL transcripts at
//   ~/.gemini/antigravity-cli/brain/<conversation-id>/.system_generated/logs/transcript.jsonl
// The on-disk schema is a sequence of "step" objects:
//   {
//     "step_index":  int,
//     "source":      "USER_EXPLICIT" | "SYSTEM" | "MODEL" | ...,
//     "type":        "USER_INPUT" | "CONVERSATION_HISTORY" | "PLANNER_RESPONSE" | ...,
//     "status":      "DONE" | ...,
//     "created_at":  RFC3339 timestamp,
//     "content":     string (optional — user request / model text),
//     "tool_calls":  [ { "name": string, "args": object } ] (optional)
//   }
// ReadTranscript/Chunk/Reassemble remain JSONL passthrough. Prompt extraction
// is implemented below (token counting is handled out-of-band elsewhere);
// field-aware modified-file analysis (TranscriptAnalyzer) is being added in a
// sibling change. See testdata/transcript_sample.jsonl for a captured fixture.

// agyStep is one line of agy's step-based JSONL transcript.
type agyStep struct {
	StepIndex int               `json:"step_index"`
	Source    string            `json:"source"`
	Type      string            `json:"type"`
	Content   string            `json:"content"`
	ToolCalls []agyStepToolCall `json:"tool_calls"`
}

type agyStepToolCall struct {
	Name string                     `json:"name"`
	Args map[string]json.RawMessage `json:"args"`
}

var userRequestRe = regexp.MustCompile(`(?s)<USER_REQUEST>\s*(.*?)\s*</USER_REQUEST>`)

// extractUserRequest returns the inner text of the first <USER_REQUEST> block,
// or the whole trimmed content if no wrapper is present.
func extractUserRequest(content string) string {
	if m := userRequestRe.FindStringSubmatch(content); m != nil {
		return strings.TrimSpace(m[1])
	}
	// No wrapper: assume the content is itself the prompt. A hypothetical
	// metadata-only USER_INPUT step would surface verbatim — acceptable for v1.
	return strings.TrimSpace(content)
}

// ExtractPrompts implements agent.PromptExtractor. agy's PreInvocation hook
// carries no prompt, so the user prompt is recovered from the transcript's
// USER_INPUT steps. fromOffset is a count of non-blank lines already consumed.
func (a *AntigravityAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // path supplied by agent hook stdin
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("antigravity: read transcript for prompts: %w", err)
	}
	var prompts []string
	lineNum := 0
	for _, raw := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue // skip blank lines BEFORE counting (matches codex splitJSONL)
		}
		lineNum++
		if lineNum <= fromOffset {
			continue
		}
		var step agyStep
		if json.Unmarshal(raw, &step) != nil {
			continue
		}
		if step.Type != "USER_INPUT" {
			continue
		}
		if text := extractUserRequest(step.Content); text != "" {
			prompts = append(prompts, text)
		}
	}
	return prompts, nil
}

func (a *AntigravityAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // path supplied by agent hook stdin
	if err != nil {
		return nil, fmt.Errorf("antigravity: read transcript: %w", err)
	}
	return data, nil
}

func (a *AntigravityAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("antigravity: chunk transcript: %w", err)
	}
	return chunks, nil
}

func (a *AntigravityAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

// PrepareTranscript implements the optional TranscriptPreparer interface. The
// framework calls this in handleLifecycleTurnEnd BEFORE its fileExists check
// — so we use it to handle agy's asynchronous transcript write.
//
// Background: agy writes its transcript file at
//
//	~/.gemini/antigravity-cli/brain/<conv-id>/.system_generated/logs/transcript.jsonl
//
// AFTER the Stop hook fires (sometimes seconds later, depending on session
// shutdown timing). Our TurnEnd event maps to Stop, so we routinely race the
// transcript write. Without PrepareTranscript, the framework's fileExists
// check fails with "transcript file not found" and our hook returns exit 1,
// terminating agy's agent turn.
//
// We materialise an empty placeholder if the file is missing. files_touched
// is already captured via the PreToolUse hook (independent of transcript
// content), so condensation can still produce a meaningful checkpoint from
// an empty transcript. Full token-usage + prompt-extraction decoding is
// deferred (see file header) and would benefit from the real transcript once
// agy has finished writing it.
func (a *AntigravityAgent) PrepareTranscript(_ context.Context, transcriptRef string) error {
	if transcriptRef == "" {
		return nil
	}
	if _, err := os.Stat(transcriptRef); err == nil {
		return nil // already present, nothing to do
	}
	if err := os.MkdirAll(filepath.Dir(transcriptRef), 0o750); err != nil {
		return fmt.Errorf("antigravity: prepare transcript dir: %w", err)
	}
	if err := os.WriteFile(transcriptRef, []byte{}, 0o600); err != nil {
		return fmt.Errorf("antigravity: create empty transcript placeholder: %w", err)
	}
	return nil
}

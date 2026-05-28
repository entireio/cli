package antigravity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

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
// v1 ships only the JSONL chunk/reassemble passthrough; field-aware decoding
// (token counting, file-change replay, prompt extraction) is deferred to a
// follow-up plan. See testdata/transcript_sample.jsonl for a captured fixture.

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

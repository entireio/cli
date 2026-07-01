package antigravity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Antigravity 2.0 (agy) writes JSONL transcripts at
//   ~/.gemini/antigravity-cli/brain/<conversation-id>/.system_generated/logs/transcript_full.jsonl
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
// This file ships the JSONL chunk/reassemble passthrough plus PrepareTranscript
// (agy's asynchronous transcript write). Field-aware decoding — prompt
// extraction, transcript-position tracking, and file-change replay — lives in
// the transcript-decode work (PR #1381) so there is a single owner of that
// surface. See testdata/transcript_sample.jsonl for a captured fixture.

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
//	~/.gemini/antigravity-cli/brain/<conv-id>/.system_generated/logs/transcript_full.jsonl
//
// AFTER the Stop hook fires (sometimes seconds later, depending on session
// shutdown timing). Our TurnEnd event maps to Stop, so we routinely race the
// transcript write. Without PrepareTranscript, the framework's fileExists
// check fails with "transcript file not found" and our hook returns exit 1,
// terminating agy's agent turn.
//
// We briefly wait for the real transcript first. If it is still missing,
// we materialise an empty placeholder. files_touched is already captured via
// the PreToolUse hook (independent of transcript content), so condensation can
// still produce a meaningful checkpoint from an empty transcript.
func (a *AntigravityAgent) PrepareTranscript(ctx context.Context, transcriptRef string) error {
	if transcriptRef == "" {
		return nil
	}

	deadline := time.Now().Add(1 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	for {
		info, err := os.Stat(transcriptRef)
		if err == nil {
			if info.Size() > 0 {
				return nil
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("antigravity: stat transcript: %w", err)
		}

		if !time.Now().Before(deadline) {
			break
		}

		wait := 50 * time.Millisecond
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("antigravity: context ended while waiting for transcript: %w", ctx.Err())
		case <-timer.C:
		}
	}

	if err := os.MkdirAll(filepath.Dir(transcriptRef), 0o750); err != nil {
		return fmt.Errorf("antigravity: prepare transcript dir: %w", err)
	}
	if _, err := os.Stat(transcriptRef); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("antigravity: stat transcript before placeholder: %w", err)
	}
	file, err := os.OpenFile(transcriptRef, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path supplied by agent hook stdin
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("antigravity: create empty transcript placeholder: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("antigravity: close empty transcript placeholder: %w", err)
	}
	return nil
}

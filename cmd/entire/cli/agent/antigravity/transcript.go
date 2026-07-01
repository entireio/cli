package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/textutil"
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
// v1 ships JSONL chunk/reassemble passthrough plus prompt extraction. Token
// counting and file-change replay are deferred to a follow-up plan. See
// testdata/transcript_sample.jsonl for a captured fixture.

var _ agent.PromptExtractor = (*AntigravityAgent)(nil)

type antigravityTranscriptStep struct {
	Source  string `json:"source"`
	Type    string `json:"type"`
	Content string `json:"content"`
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

func (a *AntigravityAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("antigravity: read transcript: %w", err)
	}

	var prompts []string
	lineNum := 0
	for _, lineData := range splitAntigravityJSONL(data) {
		lineNum++
		if lineNum <= fromOffset {
			continue
		}

		var step antigravityTranscriptStep
		if json.Unmarshal(lineData, &step) != nil {
			continue
		}
		if step.Type != "USER_INPUT" {
			continue
		}
		if prompt := cleanAntigravityPrompt(step.Content); prompt != "" {
			prompts = append(prompts, prompt)
		}
	}

	return prompts, nil
}

func splitAntigravityJSONL(data []byte) [][]byte {
	var lines [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			lines = append(lines, line)
		}
	}
	return lines
}

func cleanAntigravityPrompt(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}

	if start := strings.Index(content, "<USER_REQUEST>"); start >= 0 {
		content = content[start+len("<USER_REQUEST>"):]
		if end := strings.Index(content, "</USER_REQUEST>"); end >= 0 {
			content = content[:end]
		}
	}
	if metadata := strings.Index(content, "<ADDITIONAL_METADATA>"); metadata >= 0 {
		content = content[:metadata]
	}
	content = strings.TrimSpace(content)

	const requestMarker = "Request:\n"
	if idx := strings.Index(content, requestMarker); idx >= 0 {
		content = content[idx+len(requestMarker):]
	}

	return textutil.StripIDEContextTags(content)
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

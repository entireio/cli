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
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertions.
var (
	_ agent.PromptExtractor      = (*AntigravityAgent)(nil)
	_ agent.TranscriptAnalyzer   = (*AntigravityAgent)(nil)
	_ agent.LateTranscriptWriter = (*AntigravityAgent)(nil)
)

// Antigravity 2.0 (agy) writes JSONL transcripts at
//   ~/.gemini/antigravity-cli/brain/<conversation-id>/.system_generated/logs/transcript_full.jsonl (the hook payload sends transcript_full; agy also writes a truncated transcript.jsonl alongside it)
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
// Prompt extraction and field-aware modified-file/position analysis
// (TranscriptAnalyzer) are implemented below. ReadTranscript/Chunk/Reassemble
// remain JSONL passthrough, and token counting is handled out-of-band
// elsewhere. See testdata/transcript_sample.jsonl for a captured fixture.

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

// forEachNonBlankLine iterates data's non-blank JSONL lines, counting them
// with the codex splitJSONL convention (blank lines skipped BEFORE counting),
// and calls fn for each line past fromOffset. Returns the total non-blank
// line count.
//
// This is the single owner of agy's transcript-offset metric: the position
// one method stores (GetTranscriptPosition → CheckpointTranscriptStart) is
// consumed as fromOffset by the others (ExtractPrompts,
// ExtractModifiedFilesFromOffset), so the counting MUST stay byte-identical
// across all of them — hence one iterator instead of three hand-synced loops.
func forEachNonBlankLine(data []byte, fromOffset int, fn func(raw []byte)) int {
	lineNum := 0
	for _, raw := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		lineNum++
		if fn != nil && lineNum > fromOffset {
			fn(raw)
		}
	}
	return lineNum
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
	forEachNonBlankLine(data, fromOffset, func(raw []byte) {
		var step agyStep
		if json.Unmarshal(raw, &step) != nil {
			return
		}
		if step.Type != "USER_INPUT" {
			return
		}
		if text := extractUserRequest(step.Content); text != "" {
			prompts = append(prompts, text)
		}
	})
	return prompts, nil
}

// GetTranscriptPosition implements agent.TranscriptAnalyzer. It returns the
// number of non-blank JSONL lines in the transcript, which the framework uses
// as a stable offset to bound subsequent extraction to a single checkpoint
// range. A missing file yields (0, nil) so a not-yet-flushed transcript (agy
// writes asynchronously) doesn't fail the hook.
func (a *AntigravityAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // path supplied by agent hook stdin
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("antigravity: transcript position: %w", err)
	}
	return forEachNonBlankLine(data, 0, nil), nil
}

// CountTranscriptPosition implements agent.LateTranscriptWriter: agy writes
// its transcript only after the Stop hook, and its offset metric counts
// non-blank lines. Delegating to forEachNonBlankLine keeps this byte-identical
// with GetTranscriptPosition/ExtractPrompts (see the iterator's doc comment).
func (a *AntigravityAgent) CountTranscriptPosition(content []byte) int {
	return forEachNonBlankLine(content, 0, nil)
}

// ExtractModifiedFilesFromOffset implements agent.TranscriptAnalyzer. It scans
// agy step lines after startOffset for mutating tool calls and returns the
// target file paths they touch, deduplicated, alongside the new line position.
//
// Path convention: returned paths are ABSOLUTE and symlink-resolved — the same
// shape lifecycle.go's parsePreToolUse records into FilesTouched. The framework
// relativizes downstream via FilterAndNormalizePaths -> paths.ToRelativePath
// against the worktree root, so we must NOT pre-relativize here. We mirror
// parsePreToolUse exactly: decode the double-encoded TargetFile arg, then
// resolveAgySymlinks so the path matches what attribution diffs against (e.g.
// macOS /tmp -> /private/tmp). Both helpers live in lifecycle.go (same package)
// and are reused, not duplicated.
//
// The blank-skip -> lineNum++ -> (lineNum <= startOffset) ordering matches
// ExtractPrompts so positions stay consistent across analyzer methods.
func (a *AntigravityAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error) {
	if path == "" {
		return nil, 0, nil
	}
	data, readErr := os.ReadFile(path) //nolint:gosec // path supplied by agent hook stdin
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("antigravity: extract modified files: %w", readErr)
	}
	seen := map[string]bool{}
	lineNum := forEachNonBlankLine(data, startOffset, func(raw []byte) {
		var step agyStep
		if json.Unmarshal(raw, &step) != nil {
			return
		}
		for _, tc := range step.ToolCalls {
			switch tc.Name {
			case "write_to_file", "replace_file_content", "multi_replace_file_content":
				target := resolveAgySymlinks(decodeAgyString(tc.Args["TargetFile"]))
				if target != "" && !seen[target] {
					seen[target] = true
					files = append(files, target)
				}
			}
		}
	})
	return files, lineNum, nil
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

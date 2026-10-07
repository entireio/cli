package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/textutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

var (
	_ agent.PromptExtractor           = (*ClaudeCodeAgent)(nil)
	_ agent.TranscriptPromptExtractor = (*ClaudeCodeAgent)(nil)
)

// promptLine is the subset of a Claude Code transcript entry the prompt
// extractor reads. Claude Code marks the user entries it writes itself rather
// than the user typing them:
//
//   - isMeta: slash-command and skill expansions, local-command caveats, and
//     injected <system-reminder> blocks.
//   - isCompactSummary: the "This session is being continued from a previous
//     conversation" summary written after /compact or an automatic compaction.
type promptLine struct {
	Type             string          `json:"type"`
	Role             string          `json:"role,omitempty"`
	IsMeta           bool            `json:"isMeta,omitempty"`
	IsCompactSummary bool            `json:"isCompactSummary,omitempty"`
	Message          json.RawMessage `json:"message"`
}

// injectedUserPrefixes are leading markers of user-side text Claude Code writes
// without the isMeta flag: background-task completion notices and the marker
// left when the user interrupts a turn. Anchored on purpose, so a prompt that
// merely mentions one of them stays a prompt.
var injectedUserPrefixes = []string{
	"<task-notification>",
	"[Request interrupted by user",
}

// ExtractPrompts implements agent.PromptExtractor: the user prompts in the
// transcript at sessionRef after fromOffset lines. A missing transcript has no
// prompts.
func (c *ClaudeCodeAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	data, err := c.ReadTranscript(sessionRef)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("claude-code: read transcript for prompts: %w", err)
	}
	return extractPromptsFromContent(data, fromOffset), nil
}

// ExtractPromptsFromTranscript implements agent.TranscriptPromptExtractor over
// transcript bytes the caller already holds, with the same offset metric as
// ExtractPrompts. It never reads a path, so the prompts describe exactly the
// bytes being checkpointed.
func (c *ClaudeCodeAgent) ExtractPromptsFromTranscript(content []byte, fromOffset int) ([]string, error) {
	return extractPromptsFromContent(content, fromOffset), nil
}

// extractPromptsFromContent returns the user-typed prompts after fromOffset
// lines, in transcript order. The offset counts lines the way
// GetTranscriptPosition and ExtractModifiedFilesFromOffset do: every
// newline-terminated line, plus a final unterminated one.
//
// Dropped: tool results (user entries whose content carries no text block),
// isMeta and isCompactSummary entries, injected task notifications and
// interruption markers, agent-injected preambles (textutil.IsInjectedPrompt),
// and entries left empty once system and IDE tags (<system-reminder>,
// <command-name>, <local-command-stdout>, ...) are stripped.
func extractPromptsFromContent(data []byte, fromOffset int) []string {
	var prompts []string
	lineNum := 0
	for len(data) > 0 {
		var raw []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			raw, data = data[:i], data[i+1:]
		} else {
			raw, data = data, nil
		}
		lineNum++
		if lineNum <= fromOffset {
			continue
		}
		if prompt, ok := userPrompt(raw); ok {
			prompts = append(prompts, prompt)
		}
	}
	return prompts
}

// userPrompt returns the prompt a transcript line carries, if it is one the
// user typed.
func userPrompt(raw []byte) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", false
	}
	var line promptLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return "", false
	}
	typ := line.Type
	if typ == "" {
		typ = line.Role
	}
	if typ != transcript.TypeUser || line.IsMeta || line.IsCompactSummary {
		return "", false
	}
	// ExtractUserContent keeps only text blocks (tool_result content is
	// dropped) and strips system and IDE context tags.
	text := transcript.ExtractUserContent(line.Message)
	if text == "" || textutil.IsInjectedPrompt(text) {
		return "", false
	}
	for _, prefix := range injectedUserPrefixes {
		if strings.HasPrefix(text, prefix) {
			return "", false
		}
	}
	return text, true
}

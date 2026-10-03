package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/pi/pijsonl"
)

// Compile-time interface assertions
var (
	_ agent.TokenCalculator            = (*PiAgent)(nil)
	_ agent.TranscriptAnalyzer         = (*PiAgent)(nil)
	_ agent.ConfinedTranscriptAnalyzer = (*PiAgent)(nil)
	_ agent.PromptExtractor            = (*PiAgent)(nil)
	_ agent.TranscriptPromptExtractor  = (*PiAgent)(nil)
	_ agent.ModelExtractor             = (*PiAgent)(nil)
)

// CalculateTokenUsage sums per-assistant-message token usage from a Pi JSONL
// transcript starting at the given line offset. Only assistant messages on
// the active conversation branch contribute to the totals — see
// pijsonl.ResolveActiveBranch for the rationale.
func (a *PiAgent) CalculateTokenUsage(transcriptData []byte, fromOffset int) (*agent.TokenUsage, error) {
	usage := &agent.TokenUsage{}
	err := pijsonl.ForEachActiveMessage(transcriptData, fromOffset, func(entry pijsonl.Entry) {
		if entry.Message.Role != pijsonl.RoleAssistant || entry.Message.Usage == nil {
			return
		}
		usage.InputTokens += entry.Message.Usage.Input
		usage.OutputTokens += entry.Message.Usage.Output
		usage.CacheReadTokens += entry.Message.Usage.CacheRead
		usage.CacheCreationTokens += entry.Message.Usage.CacheWrite
		usage.APICallCount++
	})
	if err != nil {
		return usage, fmt.Errorf("calculate token usage: %w", err)
	}
	return usage, nil
}

// ExtractModel returns the model identifier from the most recent assistant
// message on the active conversation branch. Pi records the model on every
// assistant message (message.model, e.g. "gpt-5.5") but never reports it through
// hooks, so the transcript is the only source. Using the most recent message
// reflects mid-session model changes. Returns "" when no active-branch assistant
// message carries a model.
func (a *PiAgent) ExtractModel(transcriptData []byte) (string, error) {
	model := ""
	err := pijsonl.ForEachActiveMessage(transcriptData, 0, func(entry pijsonl.Entry) {
		if entry.Message.Role == pijsonl.RoleAssistant && entry.Message.Model != "" {
			model = entry.Message.Model
		}
	})
	if err != nil {
		return model, fmt.Errorf("extract model: %w", err)
	}
	return model, nil
}

// GetTranscriptPosition returns the JSONL line count of the file at path.
// Used by the strategy as the offset for incremental ExtractModifiedFiles
// calls. Missing files report 0 (consistent with Claude Code).
func (a *PiAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}
	data, err := agent.ReadTranscriptFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read pi transcript: %w", err)
	}
	return pijsonl.CountLines(data), nil
}

// ExtractModifiedFilesFromOffset scans Pi assistant tool calls from startOffset
// onward and returns file paths touched by file-modifying tools (`write`,
// `edit`). Branch-aware: only counts entries on the active conversation
// branch.
func (a *PiAgent) ExtractModifiedFilesFromOffset(_ context.Context, path string, startOffset int) ([]string, int, error) {
	if path == "" {
		return nil, 0, nil
	}
	data, err := agent.ReadTranscriptFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("read pi transcript: %w", err)
	}
	return a.ExtractModifiedFilesFromBytes(data, startOffset)
}

// ExtractModifiedFilesFromBytes is the bytes-based equivalent of
// ExtractModifiedFilesFromOffset, for callers that already hold a
// home-confined read of the transcript (agent.ConfinedTranscriptAnalyzer).
func (a *PiAgent) ExtractModifiedFilesFromBytes(data []byte, startOffset int) ([]string, int, error) {
	totalLines := pijsonl.CountLines(data)
	seen := make(map[string]bool)
	var files []string

	err := pijsonl.ForEachActiveMessage(data, startOffset, func(entry pijsonl.Entry) {
		if entry.Message.Role != pijsonl.RoleAssistant {
			return
		}
		var items []pijsonl.ContentItem
		if err := json.Unmarshal(entry.Message.Content, &items); err != nil {
			return
		}
		for _, item := range items {
			if item.Type != "toolCall" {
				continue
			}
			if item.Name != "write" && item.Name != "edit" {
				continue
			}
			var args struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(item.Arguments, &args); err != nil {
				continue
			}
			if args.Path != "" && !seen[args.Path] {
				seen[args.Path] = true
				files = append(files, args.Path)
			}
		}
	})
	if err != nil {
		return files, totalLines, fmt.Errorf("extract modified files: %w", err)
	}
	return files, totalLines, nil
}

// ExtractPrompts returns user-message text from the transcript starting at
// the given line offset. Branch-aware (drops abandoned-branch prompts). Reads
// sessionRef itself via agent.ReadTranscriptFile — only .entire-cache
// confined, not AgentHome confined. Callers that already hold a
// home-confined read of the transcript (e.g. condensation, which must not
// trust a bare sessionRef from adopted session state) should call
// ExtractPromptsFromTranscript instead.
func (a *PiAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	if sessionRef == "" {
		return nil, nil
	}
	data, err := agent.ReadTranscriptFile(sessionRef)
	if err != nil {
		return nil, fmt.Errorf("read pi transcript: %w", err)
	}
	return a.ExtractPromptsFromTranscript(data, fromOffset)
}

// ExtractPromptsFromTranscript implements agent.TranscriptPromptExtractor over
// transcript bytes the caller already holds (e.g. a home-confined read via
// agent.ReadTranscriptFileUnderHome), with the same offset metric as
// ExtractPrompts. It never reads a path itself.
func (a *PiAgent) ExtractPromptsFromTranscript(data []byte, fromOffset int) ([]string, error) {
	var prompts []string
	err := pijsonl.ForEachActiveMessage(data, fromOffset, func(entry pijsonl.Entry) {
		if entry.Message.Role != pijsonl.RoleUser {
			return
		}
		// User content can be either a plain string or an array of typed blocks.
		if text := pijsonl.DecodeStringContent(entry.Message.Content); text != "" {
			prompts = append(prompts, text)
			return
		}
		var items []pijsonl.ContentItem
		if err := json.Unmarshal(entry.Message.Content, &items); err != nil {
			return
		}
		for _, item := range items {
			if item.Type == pijsonl.ContentTypeText && item.Text != "" {
				prompts = append(prompts, item.Text)
			}
		}
	})
	if err != nil {
		return prompts, fmt.Errorf("extract prompts: %w", err)
	}
	return prompts, nil
}

// GetTranscriptPositionFromReader counts raw JSONL lines without parsing messages.
func (a *PiAgent) GetTranscriptPositionFromReader(r io.Reader) (int, error) {
	return agent.CountTranscriptLines(r) //nolint:wrapcheck // preserve shared counter errors
}

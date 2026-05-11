package pi

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/pi/pijsonl"
)

// Compile-time interface assertions
var (
	_ agent.TokenCalculator    = (*PiAgent)(nil)
	_ agent.TranscriptAnalyzer = (*PiAgent)(nil)
	_ agent.PromptExtractor    = (*PiAgent)(nil)
)

// CalculateTokenUsage sums per-assistant-message token usage from a Pi JSONL
// transcript starting at the given line offset. Only assistant messages on
// the active conversation branch contribute to the totals — see
// pijsonl.ResolveActiveBranch for the rationale.
func (a *PiAgent) CalculateTokenUsage(transcriptData []byte, fromOffset int) (*agent.TokenUsage, error) {
	usage := &agent.TokenUsage{}
	if len(transcriptData) == 0 {
		return usage, nil
	}

	// IMPORTANT: resolve active branch on FULL data before slicing — a
	// truncated buffer breaks parentId chains and leaks abandoned branches in.
	active := pijsonl.ResolveActiveBranch(transcriptData)
	content := pijsonl.SkipLines(transcriptData, fromOffset)

	scanner := pijsonl.NewScanner(content)
	for scanner.Scan() {
		var entry pijsonl.Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type != pijsonl.EntryTypeMessage || entry.Message.Role != pijsonl.RoleAssistant || entry.Message.Usage == nil {
			continue
		}
		if active != nil && !active[entry.ID] {
			continue
		}
		usage.InputTokens += entry.Message.Usage.Input
		usage.OutputTokens += entry.Message.Usage.Output
		usage.CacheReadTokens += entry.Message.Usage.CacheRead
		usage.CacheCreationTokens += entry.Message.Usage.CacheWrite
		usage.APICallCount++
	}
	if err := scanner.Err(); err != nil {
		return usage, fmt.Errorf("pi transcript scanner: %w", err)
	}
	return usage, nil
}

// GetTranscriptPosition returns the JSONL line count of the file at path.
// Used by the strategy as the offset for incremental ExtractModifiedFiles
// calls. Missing files report 0 (consistent with Claude Code).
func (a *PiAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}
	//nolint:gosec // path from validated SessionRef set by lifecycle hooks
	data, err := os.ReadFile(path)
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
func (a *PiAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) ([]string, int, error) {
	if path == "" {
		return nil, 0, nil
	}
	//nolint:gosec // path from validated SessionRef
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("read pi transcript: %w", err)
	}

	totalLines := pijsonl.CountLines(data)
	active := pijsonl.ResolveActiveBranch(data)
	content := pijsonl.SkipLines(data, startOffset)

	seen := make(map[string]bool)
	var files []string

	scanner := pijsonl.NewScanner(content)
	for scanner.Scan() {
		var entry pijsonl.Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type != pijsonl.EntryTypeMessage || entry.Message.Role != pijsonl.RoleAssistant {
			continue
		}
		if active != nil && !active[entry.ID] {
			continue
		}
		var items []pijsonl.ContentItem
		if err := json.Unmarshal(entry.Message.Content, &items); err != nil {
			continue
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
	}
	if err := scanner.Err(); err != nil {
		return files, totalLines, fmt.Errorf("pi transcript scanner: %w", err)
	}
	return files, totalLines, nil
}

// ExtractPrompts returns user-message text from the transcript starting at
// the given line offset. Branch-aware (drops abandoned-branch prompts).
func (a *PiAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	if sessionRef == "" {
		return nil, nil
	}
	//nolint:gosec // sessionRef from validated SessionRef
	data, err := os.ReadFile(sessionRef)
	if err != nil {
		return nil, fmt.Errorf("read pi transcript: %w", err)
	}

	active := pijsonl.ResolveActiveBranch(data)
	content := pijsonl.SkipLines(data, fromOffset)

	var prompts []string
	scanner := pijsonl.NewScanner(content)
	for scanner.Scan() {
		var entry pijsonl.Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type != pijsonl.EntryTypeMessage || entry.Message.Role != pijsonl.RoleUser {
			continue
		}
		if active != nil && !active[entry.ID] {
			continue
		}
		// User content can be either a plain string or an array of typed blocks.
		if text := pijsonl.DecodeStringContent(entry.Message.Content); text != "" {
			prompts = append(prompts, text)
			continue
		}
		var items []pijsonl.ContentItem
		if err := json.Unmarshal(entry.Message.Content, &items); err != nil {
			continue
		}
		for _, item := range items {
			if item.Type == pijsonl.ContentTypeText && item.Text != "" {
				prompts = append(prompts, item.Text)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return prompts, fmt.Errorf("pi transcript scanner: %w", err)
	}
	return prompts, nil
}

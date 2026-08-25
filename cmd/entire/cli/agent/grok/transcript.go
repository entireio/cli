package grok

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GetTranscriptPosition returns the line count of a Grok transcript.
// updates.jsonl is JSONL, so position is lines.
func (g *GrokAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}
	file, err := os.Open(path) //nolint:gosec // path comes from Grok transcript location
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to open transcript file: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lines := 0
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				if len(line) > 0 {
					lines++
				}
				break
			}
			return 0, fmt.Errorf("failed to read transcript: %w", err)
		}
		lines++
	}
	return lines, nil
}

// ExtractModifiedFilesFromOffset returns files Grok changed after startOffset
// lines, along with the transcript's current line count.
func (g *GrokAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error) {
	if path == "" {
		return nil, 0, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // path comes from Grok transcript location
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("failed to open transcript file: %w", err)
	}
	found, lines := modifiedFilesFrom(data, startOffset)
	return found, lines, nil
}

// modifiedFilesFrom walks the transcript from startOffset and returns the
// deduplicated set of files Grok wrote, plus the total line count.
//
// A `diff` content block on a tool_call_update is the authoritative signal that
// a file changed: it is emitted only for an actual edit and carries the
// absolute path. locations[] is the fallback — it is present on the same
// updates but holds a repo-relative path, and read-only tools populate it too,
// so it is consulted only when no diff block named a path for that tool call.
func modifiedFilesFrom(data []byte, startOffset int) ([]string, int) {
	var (
		out      []string
		seen     = map[string]bool{}
		lineNum  int
		fallback = map[string][]string{} // toolCallID -> relative paths
		diffed   = map[string]bool{}     // toolCallID -> saw a diff block
	)

	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}

	for _, raw := range splitJSONL(data) {
		lineNum++
		if lineNum <= startOffset {
			continue
		}
		var line transcriptLine
		if err := json.Unmarshal(raw, &line); err != nil {
			continue // skip malformed lines
		}
		u := line.Params.Update
		if u.SessionUpdate != "tool_call_update" && u.SessionUpdate != "tool_call" {
			continue
		}
		for _, c := range u.Content {
			if c.Type == "diff" && c.Path != "" {
				diffed[u.ToolCallID] = true
				add(c.Path)
			}
		}
		for _, l := range u.Locations {
			if l.Path != "" {
				fallback[u.ToolCallID] = append(fallback[u.ToolCallID], l.Path)
			}
		}
	}

	for id, paths := range fallback {
		if diffed[id] {
			continue
		}
		for _, p := range paths {
			add(p)
		}
	}

	// Count any lines before startOffset that the loop skipped.
	return out, lineNum
}

// splitJSONL splits raw JSONL into non-empty lines.
func splitJSONL(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := range data {
		if data[i] != '\n' {
			continue
		}
		if line := trimCR(data[start:i]); len(line) > 0 {
			out = append(out, line)
		}
		start = i + 1
	}
	if start < len(data) {
		if line := trimCR(data[start:]); len(line) > 0 {
			out = append(out, line)
		}
	}
	return out
}

func trimCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		b = b[:len(b)-1]
	}
	return b
}

// CalculateTokenUsage sums the turn_completed usage blocks after fromOffset.
//
// Grok reports inputTokens as the TOTAL input, cache inclusive, so the fresh
// portion is derived here — the same shape Cursor needs. Verified against the
// headless JSON summary for the same turn, which reports the fresh figure
// directly.
func (g *GrokAgent) CalculateTokenUsage(transcriptData []byte, fromOffset int) (*agent.TokenUsage, error) {
	var (
		total    agent.TokenUsage
		lineNum  int
		sawUsage bool
	)
	for _, raw := range splitJSONL(transcriptData) {
		lineNum++
		if lineNum <= fromOffset {
			continue
		}
		var line transcriptLine
		if err := json.Unmarshal(raw, &line); err != nil {
			continue
		}
		u := line.Params.Update
		if u.SessionUpdate != "turn_completed" || u.Usage == nil {
			continue
		}
		sawUsage = true
		fresh := u.Usage.InputTokens - u.Usage.CachedReadTokens - u.Usage.CacheCreationTokens
		if fresh < 0 {
			fresh = 0
		}
		total.InputTokens += fresh
		total.CacheReadTokens += u.Usage.CachedReadTokens
		total.CacheCreationTokens += u.Usage.CacheCreationTokens
		total.OutputTokens += u.Usage.OutputTokens
		calls := u.Usage.ModelCalls
		if calls == 0 {
			calls = 1
		}
		total.APICallCount += calls
	}
	if !sawUsage {
		return nil, nil //nolint:nilnil // no usage data is "unknown", not zero
	}
	return &total, nil
}

// ExtractModel returns the model Grok used, read from the per-message _meta
// Grok stamps on message chunks. Hook payloads carry no model field, so the
// transcript is the only source.
func (g *GrokAgent) ExtractModel(transcriptData []byte) (string, error) {
	for _, raw := range splitJSONL(transcriptData) {
		var line transcriptLine
		if err := json.Unmarshal(raw, &line); err != nil {
			continue
		}
		if m := line.Params.Update.Meta.ModelID; m != "" {
			return m, nil
		}
	}
	return "", nil
}

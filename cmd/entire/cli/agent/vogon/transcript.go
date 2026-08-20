package vogon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time assertion that vogon can report transcript-derived file lists.
var _ agent.TranscriptAnalyzer = (*Agent)(nil)

// transcriptTypeToolUse is the entry type the vogon binary writes for file
// writes. Keep in step with appendToolUse in e2e/vogon/main.go.
const transcriptTypeToolUse = "tool_use"

// transcriptLine is the subset of the vogon binary's JSONL the analyzer reads.
// Keep the `files` field in step with e2e/vogon/main.go's transcriptEntry.
type transcriptLine struct {
	Type  string   `json:"type"`
	Files []string `json:"files,omitempty"`
}

// GetTranscriptPosition returns the transcript's line count, or 0 when it does not
// exist yet.
func (v *Agent) GetTranscriptPosition(path string) (int, error) {
	lines, err := readTranscriptLines(path)
	if err != nil {
		return 0, err
	}
	return len(lines), nil
}

// ExtractModifiedFilesFromOffset returns the files recorded by tool-use entries at
// or after startOffset, plus the transcript's current line count.
//
// Vogon implements this so the canary exercises the *transcript-derived* file path
// that real agents take — in particular the subagent case, where a subagent's writes
// appear only in its own transcript. Without it, `entire hooks vogon post-task`
// would fall back to worktree state alone and the canary could not see regressions
// in subagent transcript resolution or file attribution.
func (v *Agent) ExtractModifiedFilesFromOffset(path string, startOffset int) ([]string, int, error) {
	lines, err := readTranscriptLines(path)
	if err != nil {
		return nil, 0, err
	}

	seen := make(map[string]struct{})
	var files []string
	for i, raw := range lines {
		if i < startOffset {
			continue
		}
		var entry transcriptLine
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			continue // a malformed line is not fatal; the real agents skip too
		}
		// Only tool-use entries report file writes. Accepting a `files` field on
		// any line would misattribute files if the transcript format grows another
		// entry type that happens to carry paths.
		if entry.Type != transcriptTypeToolUse {
			continue
		}
		for _, f := range entry.Files {
			if f == "" {
				continue
			}
			if _, dup := seen[f]; dup {
				continue
			}
			seen[f] = struct{}{}
			files = append(files, f)
		}
	}
	return files, len(lines), nil
}

// readTranscriptLines returns the transcript's non-empty lines. A missing file is
// not an error — it just has no content yet.
func readTranscriptLines(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path) //nolint:gosec // path comes from the hook payload
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}
	return lines, nil
}

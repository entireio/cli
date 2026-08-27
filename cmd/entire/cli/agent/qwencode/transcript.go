package qwencode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Compile-time capability assertions.
var (
	_ agent.TranscriptAnalyzer = (*QwenCodeAgent)(nil)
	_ agent.TokenCalculator    = (*QwenCodeAgent)(nil)
	_ agent.PromptExtractor    = (*QwenCodeAgent)(nil)
	_ agent.ModelExtractor     = (*QwenCodeAgent)(nil)
)

// Line envelope types.
const (
	typeUser       = "user"
	typeAssistant  = "assistant"
	typeToolResult = "tool_result"

	// provenanceRealUser marks a genuine user prompt. A tool_result line also
	// carries message.role "user", so the role alone cannot distinguish them.
	provenanceRealUser = "real_user"

	// maxTranscriptLine bounds a single JSONL line, matching the Copilot CLI
	// reader's 10MB ceiling.
	maxTranscriptLine = 10 * 1024 * 1024
)

// scanLines walks a JSONL transcript from a starting line offset, invoking fn
// for each line that parses. Unparseable lines are skipped rather than aborting
// the walk: a truncated final line is normal while a session is being written.
func scanLines(data []byte, fromOffset int, fn func(Line)) int {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// A single line holds a whole message, including file contents written by a
	// tool call, so the default 64KB token limit is far too small.
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxTranscriptLine)

	count := 0
	for scanner.Scan() {
		count++
		if count <= fromOffset {
			continue
		}
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var line Line
		if err := json.Unmarshal(raw, &line); err != nil {
			continue
		}
		fn(line)
	}
	return count
}

// GetTranscriptPosition returns the line count, which is the offset unit used
// throughout this package.
func (a *QwenCodeAgent) GetTranscriptPosition(path string) (int, error) {
	data, err := os.ReadFile(path) //nolint:gosec // Path from the agent hook
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read qwen transcript: %w", err)
	}
	return scanLines(data, -1, func(Line) {}), nil
}

// ExtractModifiedFilesFromOffset returns files touched from startOffset onward.
func (a *QwenCodeAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) ([]string, int, error) {
	data, err := os.ReadFile(path) //nolint:gosec // Path from the agent hook
	if err != nil {
		if os.IsNotExist(err) {
			return nil, startOffset, nil
		}
		return nil, startOffset, fmt.Errorf("failed to read qwen transcript: %w", err)
	}

	seen := make(map[string]struct{})
	var files []string
	total := scanLines(data, startOffset, func(line Line) {
		collectFiles(line, seen, &files)
	})
	return files, total, nil
}

// ExtractModifiedFiles returns every file touched across a whole transcript.
//
// Returns no error: scanLines skips lines it cannot parse rather than failing,
// so a partially written transcript yields the files it can see.
func ExtractModifiedFiles(data []byte) []string {
	seen := make(map[string]struct{})
	var files []string
	scanLines(data, 0, func(line Line) {
		collectFiles(line, seen, &files)
	})
	return files
}

// fileToolArgs covers the argument shapes Qwen's file-writing tools use.
type fileToolArgs struct {
	FilePath string `json:"file_path"`
	Path     string `json:"path"`
	AbsPath  string `json:"absolute_path"`
}

// collectFiles appends file paths from any functionCall parts on this line.
func collectFiles(line Line, seen map[string]struct{}, files *[]string) {
	if line.Message == nil {
		return
	}
	for _, part := range line.Message.Parts {
		if part.FunctionCall == nil || !isFileTool(part.FunctionCall.Name) {
			continue
		}
		var args fileToolArgs
		if err := json.Unmarshal(part.FunctionCall.Args, &args); err != nil {
			continue
		}
		path := firstNonEmpty(args.FilePath, args.Path, args.AbsPath)
		if path == "" {
			continue
		}
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		*files = append(*files, path)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// fileTools are the tools that write to disk.
//
// Captured from the tool list qwen v0.21.14 actually advertises to the model, by
// running a real session against a local endpoint that logged the request.
//
// "replace" is deliberately absent: that is Gemini CLI's name for the edit tool
// and Qwen does not advertise it, so matching it would be dead code inherited
// from the fork's ancestry. Read and search tools (read_file, glob, grep_search,
// list_directory, run_shell_command) are excluded because they name files
// without changing them.
var fileTools = map[string]bool{
	"write_file":    true,
	"edit":          true,
	"notebook_edit": true,
}

func isFileTool(name string) bool {
	return fileTools[strings.ToLower(name)]
}

// ExtractPrompts returns user prompt text from fromOffset onward.
//
// Filters on provenance, not role: a tool_result line also carries
// message.role "user", so keying on the role would report every tool result as
// a prompt.
func (a *QwenCodeAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path from the agent hook
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read qwen transcript: %w", err)
	}

	var prompts []string
	scanLines(data, fromOffset, func(line Line) {
		if line.Type != typeUser || line.Provenance != provenanceRealUser || line.Message == nil {
			return
		}
		for _, part := range line.Message.Parts {
			if strings.TrimSpace(part.Text) != "" {
				prompts = append(prompts, part.Text)
			}
		}
	})
	return prompts, nil
}

// ExtractModel returns the most recent model recorded on an assistant line.
func (a *QwenCodeAgent) ExtractModel(transcriptData []byte) (string, error) {
	model := ""
	scanLines(transcriptData, 0, func(line Line) {
		if line.Type == typeAssistant && line.Model != "" {
			model = line.Model
		}
	})
	return model, nil
}

// CalculateTokenUsage sums per-message usage from fromOffset onward.
//
// Qwen reports usage per assistant message, so unlike Goose the offset scopes
// correctly. Qwen publishes no cache-write figure, so CacheCreationTokens stays
// zero rather than being inferred from the totals.
func (a *QwenCodeAgent) CalculateTokenUsage(transcriptData []byte, fromOffset int) (*types.TokenUsage, error) {
	usage := &types.TokenUsage{}
	scanLines(transcriptData, fromOffset, func(line Line) {
		if line.Usage == nil {
			return
		}
		usage.InputTokens += line.Usage.PromptTokenCount
		// Reasoning tokens are billed as output, matching the Gemini integration.
		usage.OutputTokens += line.Usage.CandidatesTokenCount + line.Usage.ThoughtsTokenCount
		usage.CacheReadTokens += line.Usage.CachedContentTokenCount
		usage.APICallCount++
	})
	return usage, nil
}

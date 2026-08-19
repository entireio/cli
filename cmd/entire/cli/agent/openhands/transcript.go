package openhands

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time capability assertions.
//
// No TokenCalculator: OpenHands events carry no usage block, so there is
// nothing to report and inventing a number would be worse than reporting none.
var (
	_ agent.TranscriptAnalyzer = (*OpenHandsAgent)(nil)
	_ agent.PromptExtractor    = (*OpenHandsAgent)(nil)
)

// Event kinds.
const (
	kindMessage = "MessageEvent"
	kindAction  = "ActionEvent"

	// sourceUser marks events originating from the person, not the agent.
	sourceUser = "user"

	// maxTranscriptLine bounds one serialized event. A SystemPromptEvent embeds
	// the whole system prompt and tool schema, so these lines are large.
	maxTranscriptLine = 10 * 1024 * 1024
)

// scanEvents walks serialized JSONL from a line offset, invoking fn per event.
// Unparseable lines are skipped so a partial write cannot abort the walk.
func scanEvents(data []byte, fromOffset int, fn func(Event)) int {
	scanner := bufio.NewScanner(bytes.NewReader(data))
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
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue
		}
		fn(ev)
	}
	return count
}

// GetTranscriptPosition returns the number of events in a conversation.
func (a *OpenHandsAgent) GetTranscriptPosition(path string) (int, error) {
	data, err := readEventDir(path)
	if err != nil {
		return 0, err
	}
	return scanEvents(data, -1, func(Event) {}), nil
}

// ExtractModifiedFilesFromOffset returns files touched from startOffset onward.
func (a *OpenHandsAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) ([]string, int, error) {
	data, err := readEventDir(path)
	if err != nil {
		return nil, startOffset, err
	}

	seen := make(map[string]struct{})
	var files []string
	total := scanEvents(data, startOffset, func(ev Event) {
		collectFiles(ev, seen, &files)
	})
	return files, total, nil
}

// ExtractModifiedFiles returns every file touched across a serialized transcript.
//
// Returns no error: scanEvents skips lines it cannot parse rather than failing,
// so a partially written transcript yields the files it can see.
func ExtractModifiedFiles(data []byte) []string {
	seen := make(map[string]struct{})
	var files []string
	scanEvents(data, 0, func(ev Event) {
		collectFiles(ev, seen, &files)
	})
	return files
}

// fileToolArgs covers the argument shapes OpenHands' editor tool uses.
type fileToolArgs struct {
	Path     string `json:"path"`
	FilePath string `json:"file_path"`
	Command  string `json:"command"`
}

// collectFiles appends file paths from an ActionEvent's tool call.
func collectFiles(ev Event, seen map[string]struct{}, files *[]string) {
	if ev.Kind != kindAction || ev.ToolCall == nil {
		return
	}
	if !isFileTool(ev.ToolCall.Name) {
		return
	}
	// tool_call.arguments is a JSON-encoded string, so this is a second decode.
	var args fileToolArgs
	if err := json.Unmarshal([]byte(ev.ToolCall.Arguments), &args); err != nil {
		return
	}
	// The editor's "command" field selects the operation (view/create/str_replace).
	// A pure read must not count as a modification.
	if args.Command == "view" {
		return
	}
	path := args.Path
	if path == "" {
		path = args.FilePath
	}
	if path == "" {
		return
	}
	if _, dup := seen[path]; dup {
		return
	}
	seen[path] = struct{}{}
	*files = append(*files, path)
}

// fileTools are the tools that write to disk.
//
// The live tool name is file_editor. The Almanac documents the same trap for
// the shell tool, where the real name is "terminal" and a matcher of
// "execute_bash" or "bash" silently never matches, so the names here come from
// a real run rather than from the docs.
var fileTools = map[string]bool{
	"file_editor":        true,
	"str_replace_editor": true,
	"edit_file":          true,
}

func isFileTool(name string) bool {
	return fileTools[strings.ToLower(name)]
}

// ExtractPrompts returns user prompt text from fromOffset onward.
//
// Filters on source == "user", which is what separates a person's message from
// the agent's own MessageEvents.
func (a *OpenHandsAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	data, err := readEventDir(sessionRef)
	if err != nil {
		return nil, err
	}

	var prompts []string
	scanEvents(data, fromOffset, func(ev Event) {
		if ev.Kind != kindMessage || ev.Source != sourceUser || ev.LLMMessage == nil {
			return
		}
		for _, c := range ev.LLMMessage.Content {
			if strings.TrimSpace(c.Text) != "" {
				prompts = append(prompts, c.Text)
			}
		}
	})
	return prompts, nil
}

// ParseEvents decodes a serialized transcript for external consumers.
// Unparseable lines are skipped, so this cannot fail.
func ParseEvents(data []byte) []Event {
	if len(data) == 0 {
		return nil
	}
	var events []Event
	scanEvents(data, 0, func(ev Event) {
		events = append(events, ev)
	})
	return events
}

// ToolDetail renders a short detail string for a tool call.
func ToolDetail(arguments string) string {
	if arguments == "" {
		return ""
	}
	var args fileToolArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return ""
	}
	switch {
	case args.Path != "":
		return args.Path
	case args.FilePath != "":
		return args.FilePath
	default:
		return args.Command
	}
}

// TextOf concatenates the text blocks of a message event.
func TextOf(ev Event) string {
	if ev.LLMMessage == nil {
		return ""
	}
	var parts []string
	for _, c := range ev.LLMMessage.Content {
		if strings.TrimSpace(c.Text) != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// SliceFromEvent returns the transcript from startIndex onward, preserving the
// JSONL shape. Offsets count events, matching GetTranscriptPosition.
func SliceFromEvent(data []byte, startIndex int) []byte {
	if len(data) == 0 || startIndex <= 0 {
		return data
	}
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if startIndex >= len(lines) {
		return nil
	}
	out := bytes.Join(lines[startIndex:], []byte("\n"))
	if len(out) == 0 {
		return nil
	}
	return append(out, '\n')
}

// ThoughtOf concatenates an ActionEvent's thought blocks.
func ThoughtOf(ev Event) string {
	var parts []string
	for _, c := range ev.Thought {
		if strings.TrimSpace(c.Text) != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

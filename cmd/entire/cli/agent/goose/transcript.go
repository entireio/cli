package goose

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Compile-time capability assertions.
var (
	_ agent.TranscriptAnalyzer = (*GooseAgent)(nil)
	_ agent.TokenCalculator    = (*GooseAgent)(nil)
	_ agent.PromptExtractor    = (*GooseAgent)(nil)
	_ agent.ModelExtractor     = (*GooseAgent)(nil)
)

// conversationKey is the export's message-array field. Goose names it
// "conversation"; OpenCode's analogous export uses "messages". Getting this
// wrong yields a silently empty transcript, so it is named once here.
const conversationKey = "conversation"

// splitExport separates a Goose export into its top-level field map and its raw
// conversation entries. Working with a field map (rather than ExportSession)
// keeps top-level keys this package does not model intact across a chunk and
// reassemble round-trip.
func splitExport(content []byte) (map[string]json.RawMessage, []json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(content, &fields); err != nil {
		return nil, nil, fmt.Errorf("failed to parse goose export: %w", err)
	}
	raw, ok := fields[conversationKey]
	if !ok {
		return fields, nil, nil
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return nil, nil, fmt.Errorf("failed to parse goose conversation array: %w", err)
	}
	return fields, messages, nil
}

// withConversation re-serializes an export envelope with the given conversation
// entries substituted in.
func withConversation(fields map[string]json.RawMessage, messages []json.RawMessage) ([]byte, error) {
	out := make(map[string]json.RawMessage, len(fields)+1)
	for k, v := range fields {
		out[k] = v
	}
	encoded, err := json.Marshal(messages)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal goose conversation: %w", err)
	}
	out[conversationKey] = encoded

	result, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal goose export: %w", err)
	}
	return result, nil
}

// ParseExportSession decodes a Goose export for external consumers (summaries,
// explain). Returns nil for empty input rather than an error, so callers can
// treat "no transcript yet" as an ordinary case.
func ParseExportSession(data []byte) (*ExportSession, error) {
	if len(data) == 0 {
		return nil, nil //nolint:nilnil // (nil, nil) means "no transcript yet", which is not an error
	}
	return parseExport(data)
}

// ExtractText concatenates the text blocks of a message, skipping tool traffic.
func ExtractText(content []ContentBlock) string {
	var parts []string
	for _, block := range content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// ToolDetail renders a short, human-readable detail for a tool call, preferring
// the fields most useful in a summary.
func ToolDetail(arguments json.RawMessage) string {
	if len(arguments) == 0 {
		return ""
	}
	var args struct {
		Command  string `json:"command"`
		Path     string `json:"path"`
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil {
		return ""
	}
	switch {
	case args.Command != "":
		return args.Command
	case args.Path != "":
		return args.Path
	default:
		return args.FilePath
	}
}

// CountMessages returns the number of conversation entries in an export. This
// is the unit checkpoint offsets are expressed in for Goose.
func CountMessages(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	_, messages, err := splitExport(data)
	if err != nil {
		return 0, err
	}
	return len(messages), nil
}

// SliceFromMessage returns an export containing only the conversation entries
// from startMessageIndex onward, preserving the session envelope.
//
// This is the Goose analogue of opencode.SliceFromMessage and exists for the
// same reason: checkpoint offsets for a JSON-document transcript count messages,
// not lines, so the generic line slicer would corrupt the document.
func SliceFromMessage(data []byte, startMessageIndex int) ([]byte, error) {
	if len(data) == 0 || startMessageIndex <= 0 {
		return data, nil
	}

	fields, messages, err := splitExport(data)
	if err != nil {
		return nil, err
	}
	if startMessageIndex >= len(messages) {
		return nil, nil
	}
	return withConversation(fields, messages[startMessageIndex:])
}

// parseExport decodes an export into the modelled subset used for analysis.
func parseExport(content []byte) (*ExportSession, error) {
	var session ExportSession
	if err := json.Unmarshal(content, &session); err != nil {
		return nil, fmt.Errorf("failed to parse goose export: %w", err)
	}
	return &session, nil
}

// GetTranscriptPosition returns the number of conversation entries, which is the
// offset unit every other method in this file uses.
//
// Goose's transcript is a JSON document rather than an append-only JSONL file,
// so a byte offset would be meaningless: re-exporting the same session can shift
// every byte. Message count is stable under re-export and is what the offsets
// passed to ExtractModifiedFilesFromOffset and CalculateTokenUsage mean.
func (a *GooseAgent) GetTranscriptPosition(path string) (int, error) {
	data, err := os.ReadFile(path) //nolint:gosec // Path from validated session ID
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read goose transcript: %w", err)
	}
	_, messages, err := splitExport(data)
	if err != nil {
		return 0, err
	}
	return len(messages), nil
}

// ExtractModifiedFilesFromOffset returns files touched from startOffset onward,
// plus the new position.
func (a *GooseAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) ([]string, int, error) {
	data, err := os.ReadFile(path) //nolint:gosec // Path from validated session ID
	if err != nil {
		if os.IsNotExist(err) {
			return nil, startOffset, nil
		}
		return nil, startOffset, fmt.Errorf("failed to read goose transcript: %w", err)
	}

	session, err := parseExport(data)
	if err != nil {
		return nil, startOffset, err
	}

	messages := sliceFrom(session.Conversation, startOffset)
	files := extractFilesFromMessages(messages)
	return files, len(session.Conversation), nil
}

// ExtractModifiedFiles returns every file touched across a whole export.
func ExtractModifiedFiles(content []byte) ([]string, error) {
	session, err := parseExport(content)
	if err != nil {
		return nil, err
	}
	return extractFilesFromMessages(session.Conversation), nil
}

// ExtractPrompts returns user prompt text from startOffset onward.
func (a *GooseAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path from validated session ID
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read goose transcript: %w", err)
	}

	session, err := parseExport(data)
	if err != nil {
		return nil, err
	}

	var prompts []string
	for _, msg := range sliceFrom(session.Conversation, fromOffset) {
		if msg.Role != "user" {
			continue
		}
		for _, block := range msg.Content {
			// A user message also carries toolResponse blocks (tool output is
			// attributed to the user role). Only text blocks are prompts.
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				prompts = append(prompts, block.Text)
			}
		}
	}
	return prompts, nil
}

// ExtractModel returns the model identifier recorded on the session.
//
// Goose stores the model once on the session envelope rather than per message,
// so there is nothing to scan and no offset to respect.
func (a *GooseAgent) ExtractModel(transcriptData []byte) (string, error) {
	session, err := parseExport(transcriptData)
	if err != nil {
		return "", err
	}
	if session.ModelConfig != nil {
		return session.ModelConfig.Model, nil
	}
	return "", nil
}

// CalculateTokenUsage returns the session's token usage.
//
// CONTRACT NOTE: fromOffset is accepted to satisfy the TokenCalculator
// interface but cannot be honoured. Goose reports usage only as session-level
// totals (`usage` / `accumulated_usage` on the export envelope) — individual
// messages carry no per-message usage block — so there is nothing to scope to a
// message range. The value returned is therefore always the session total.
//
// Callers that need a per-checkpoint delta must subtract the previous
// checkpoint's recorded total themselves; this mirrors how Copilot CLI backfills
// full-session totals from session.shutdown.
func (a *GooseAgent) CalculateTokenUsage(transcriptData []byte, _ int) (*types.TokenUsage, error) {
	session, err := parseExport(transcriptData)
	if err != nil {
		return nil, err
	}

	// accumulated_usage spans the whole session including compacted history;
	// usage covers the current context window. Prefer the former, fall back to
	// the latter.
	usage := session.Accumulated
	if usage == nil {
		usage = session.Usage
	}
	if usage == nil {
		return &types.TokenUsage{}, nil
	}

	return &types.TokenUsage{
		InputTokens:         usage.InputTokens,
		OutputTokens:        usage.OutputTokens,
		CacheReadTokens:     usage.CacheReadTokens,
		CacheCreationTokens: usage.CacheWriteTokens,
	}, nil
}

// sliceFrom returns messages from offset onward, tolerating an out-of-range or
// negative offset. A transcript can shrink between reads when a session is
// compacted or rewound, so an offset past the end must not panic.
func sliceFrom(messages []ExportMessage, offset int) []ExportMessage {
	if offset <= 0 {
		return messages
	}
	if offset >= len(messages) {
		return nil
	}
	return messages[offset:]
}

// fileToolArgs covers the argument shapes Goose's file-touching tools use.
type fileToolArgs struct {
	Path     string `json:"path"`
	FilePath string `json:"file_path"`
}

// extractFilesFromMessages collects file paths from tool requests.
func extractFilesFromMessages(messages []ExportMessage) []string {
	seen := make(map[string]struct{})
	var files []string

	for _, msg := range messages {
		for _, block := range msg.Content {
			if block.Type != "toolRequest" || block.ToolCall == nil || block.ToolCall.Value == nil {
				continue
			}
			if !isFileTool(block.ToolCall.Value.Name) {
				continue
			}
			var args fileToolArgs
			if err := json.Unmarshal(block.ToolCall.Value.Arguments, &args); err != nil {
				continue
			}
			path := args.Path
			if path == "" {
				path = args.FilePath
			}
			if path == "" {
				continue
			}
			if _, dup := seen[path]; dup {
				continue
			}
			seen[path] = struct{}{}
			files = append(files, path)
		}
	}
	return files
}

// fileToolSuffixes are the tool names that write files, matched by suffix.
//
// These come from the tool list goose v1.46.0 actually advertises to the model,
// captured by running a real session against a local endpoint that logged the
// request. The full list is: analyze, apps__create_app, apps__delete_app,
// apps__iterate_app, apps__list_apps, delegate, edit, extensionmanager__*,
// load, load_skill, read_image, shell, todo__todo_write, tree, write.
//
// Only edit and write touch files. Two traps that list exposes:
//   - "create" is NOT a goose tool. Matching it would catch apps__create_app,
//     which creates an application, not a file.
//   - The vendor docs and the Agentic Tools Almanac both describe a
//     developer__text_editor tool. No such tool is advertised; the editor is
//     the bare name "edit".
//
// Matching is on the suffix after "__" so a namespaced spelling (should an
// extension provide its own editor) still resolves.
var fileToolSuffixes = []string{
	"edit",
	"write",
}

// isFileTool reports whether a tool name denotes a file-touching tool.
func isFileTool(name string) bool {
	tool := strings.ToLower(name)
	// Strip the "<extension>__" namespace if present.
	if idx := strings.LastIndex(tool, "__"); idx >= 0 {
		tool = tool[idx+2:]
	}
	for _, suffix := range fileToolSuffixes {
		if tool == suffix {
			return true
		}
	}
	return false
}

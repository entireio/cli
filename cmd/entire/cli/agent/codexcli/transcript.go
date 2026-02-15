package codexcli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// scannerBufferSize for reading large JSONL files (10MB).
const scannerBufferSize = 10 * 1024 * 1024

// ParsedSession holds the normalized data extracted from a Codex JSONL event stream.
type ParsedSession struct {
	ThreadID      string
	Messages      []string
	Commands      []CommandExecutionItem
	FileChanges   []FileChange
	ModifiedFiles []string
	TokenUsage    *agent.TokenUsage
	Errors        []string
}

// ParseEventStream parses raw Codex JSONL bytes into a ParsedSession.
// Unknown event types and malformed lines are silently skipped.
func ParseEventStream(data []byte) (*ParsedSession, error) {
	return parseEvents(bufio.NewScanner(bytes.NewReader(data)))
}

// ParseEventStreamFromFile parses a Codex JSONL file into a ParsedSession.
func ParseEventStreamFromFile(path string) (*ParsedSession, error) {
	file, err := os.Open(path) //nolint:gosec // path comes from controlled transcript location
	if err != nil {
		return nil, fmt.Errorf("failed to open transcript file: %w", err)
	}
	defer file.Close()

	return parseEvents(bufio.NewScanner(file))
}

func parseEvents(scanner *bufio.Scanner) (*ParsedSession, error) {
	scanner.Buffer(make([]byte, 0, scannerBufferSize), scannerBufferSize)

	session := &ParsedSession{
		TokenUsage: &agent.TokenUsage{},
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			continue // skip malformed lines
		}

		processEvent(session, &event)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan event stream: %w", err)
	}

	return session, nil
}

func processEvent(s *ParsedSession, event *Event) {
	switch event.Type {
	case EventThreadStarted:
		s.ThreadID = event.ThreadID

	case EventTurnCompleted:
		if event.Usage != nil {
			s.TokenUsage.InputTokens += event.Usage.InputTokens
			s.TokenUsage.CacheReadTokens += event.Usage.CachedInputTokens
			s.TokenUsage.OutputTokens += event.Usage.OutputTokens
			s.TokenUsage.APICallCount++
		}

	case EventTurnFailed:
		if event.Error != nil {
			s.Errors = append(s.Errors, event.Error.Message)
		}

	case EventError:
		if event.Message != "" {
			s.Errors = append(s.Errors, event.Message)
		}

	case EventItemCompleted:
		processCompletedItem(s, event.Item)

	case EventTurnStarted, EventItemStarted, EventItemUpdated:
		// no action needed for these lifecycle events
	}
}

func processCompletedItem(s *ParsedSession, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}

	var envelope ItemEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return
	}

	switch envelope.Type {
	case ItemAgentMessage:
		var item AgentMessageItem
		if err := json.Unmarshal(raw, &item); err == nil && item.Text != "" {
			s.Messages = append(s.Messages, item.Text)
		}

	case ItemCommandExecution:
		var item CommandExecutionItem
		if err := json.Unmarshal(raw, &item); err == nil {
			s.Commands = append(s.Commands, item)
		}

	case ItemFileChange:
		var item FileChangeItem
		if err := json.Unmarshal(raw, &item); err == nil {
			s.FileChanges = append(s.FileChanges, item.Changes...)
			for _, change := range item.Changes {
				s.ModifiedFiles = appendUnique(s.ModifiedFiles, change.Path)
			}
		}

	case ItemReasoning, ItemMCPToolCall, ItemWebSearch, ItemTodoList, ItemError:
		// recognized but not stored for checkpoint purposes
	}
}

// ExtractModifiedFiles returns deduplicated file paths from parsed session data.
func ExtractModifiedFiles(s *ParsedSession) []string {
	return s.ModifiedFiles
}

// ExtractLastMessage returns the last agent message, or empty string.
func ExtractLastMessage(s *ParsedSession) string {
	if len(s.Messages) == 0 {
		return ""
	}
	return s.Messages[len(s.Messages)-1]
}

// GetTranscriptPosition returns the line count of a Codex JSONL event file.
// Returns 0 if the file does not exist or is empty.
func GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}

	file, err := os.Open(path) //nolint:gosec // path comes from controlled transcript location
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to open transcript file: %w", err)
	}
	defer file.Close()

	return countLines(file)
}

// countLines counts newline-terminated lines in a reader.
func countLines(r io.Reader) (int, error) {
	reader := bufio.NewReader(r)
	count := 0
	for {
		_, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return 0, fmt.Errorf("failed to read transcript: %w", err)
		}
		count++
	}
	return count, nil
}

func appendUnique(slice []string, val string) []string {
	for _, existing := range slice {
		if existing == val {
			return slice
		}
	}
	return append(slice, val)
}

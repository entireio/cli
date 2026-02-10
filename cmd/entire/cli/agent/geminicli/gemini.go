// Package geminicli implements the Agent interface for Gemini CLI.
package geminicli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/sessionid"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameGemini, NewGeminiCLIAgent)
}

// GeminiCLIAgent implements the Agent interface for Gemini CLI.
//
//nolint:revive // GeminiCLIAgent is clearer than Agent in this context
type GeminiCLIAgent struct{}

func NewGeminiCLIAgent() agent.Agent {
	return &GeminiCLIAgent{}
}

// Name returns the agent registry key.
func (g *GeminiCLIAgent) Name() agent.AgentName {
	return agent.AgentNameGemini
}

// Type returns the agent type identifier.
func (g *GeminiCLIAgent) Type() agent.AgentType {
	return agent.AgentTypeGemini
}

// Description returns a human-readable description.
func (g *GeminiCLIAgent) Description() string {
	return "Gemini CLI - Google's AI coding assistant"
}

// DetectPresence checks if Gemini CLI is configured in the repository.
func (g *GeminiCLIAgent) DetectPresence() (bool, error) {
	// Get repo root to check for .gemini directory
	// This is needed because the CLI may be run from a subdirectory
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		// Not in a git repo, fall back to CWD-relative check
		repoRoot = "."
	}

	// Check for .gemini directory
	geminiDir := filepath.Join(repoRoot, ".gemini")
	if _, err := os.Stat(geminiDir); err == nil {
		return true, nil
	}
	// Check for .gemini/settings.json
	settingsFile := filepath.Join(repoRoot, ".gemini", "settings.json")
	if _, err := os.Stat(settingsFile); err == nil {
		return true, nil
	}
	return false, nil
}

// GetHookConfigPath returns the path to Gemini's hook config file.
func (g *GeminiCLIAgent) GetHookConfigPath() string {
	return ".gemini/settings.json"
}

// SupportsHooks returns true as Gemini CLI supports lifecycle hooks.
func (g *GeminiCLIAgent) SupportsHooks() bool {
	return true
}

// ParseHookInput parses Gemini CLI hook input from stdin.
func (g *GeminiCLIAgent) ParseHookInput(hookType agent.HookType, reader io.Reader) (*agent.HookInput, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}

	if len(data) == 0 {
		return nil, errors.New("empty input")
	}

	input := &agent.HookInput{
		HookType:  hookType,
		Timestamp: time.Now(),
		RawData:   make(map[string]interface{}),
	}

	// Parse based on hook type
	switch hookType {
	case agent.HookSessionStart, agent.HookSessionEnd, agent.HookStop:
		var raw sessionInfoRaw
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("failed to parse session info: %w", err)
		}
		input.SessionID = raw.SessionID
		input.SessionRef = raw.TranscriptPath
		// Store Gemini-specific fields in RawData
		input.RawData["cwd"] = raw.Cwd
		input.RawData["hook_event_name"] = raw.HookEventName
		if raw.Source != "" {
			input.RawData["source"] = raw.Source
		}
		if raw.Reason != "" {
			input.RawData["reason"] = raw.Reason
		}

	case agent.HookUserPromptSubmit:
		// BeforeAgent is Gemini's equivalent to Claude's UserPromptSubmit
		// It provides the user's prompt in the "prompt" field
		var raw agentHookInputRaw
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("failed to parse agent hook input: %w", err)
		}
		input.SessionID = raw.SessionID
		input.SessionRef = raw.TranscriptPath
		input.RawData["cwd"] = raw.Cwd
		input.RawData["hook_event_name"] = raw.HookEventName
		if raw.Prompt != "" {
			input.UserPrompt = raw.Prompt
			input.RawData["prompt"] = raw.Prompt
		}

	case agent.HookPreToolUse, agent.HookPostToolUse:
		var raw toolHookInputRaw
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("failed to parse tool hook input: %w", err)
		}
		input.SessionID = raw.SessionID
		input.SessionRef = raw.TranscriptPath
		input.ToolName = raw.ToolName
		input.ToolInput = raw.ToolInput
		if hookType == agent.HookPostToolUse {
			input.ToolResponse = raw.ToolResponse
		}
		input.RawData["cwd"] = raw.Cwd
		input.RawData["hook_event_name"] = raw.HookEventName
	}

	return input, nil
}

// GetSessionID extracts the session ID from hook input.
func (g *GeminiCLIAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// TransformSessionID converts a Gemini session ID to an Entire session ID.
// This is now an identity function - the agent session ID IS the Entire session ID.
func (g *GeminiCLIAgent) TransformSessionID(agentSessionID string) string {
	return agentSessionID
}

// ExtractAgentSessionID extracts the Gemini session ID from an Entire session ID.
// Since Entire session ID = agent session ID (identity), this returns the input unchanged.
// For backwards compatibility with legacy date-prefixed IDs, it strips the prefix if present.
func (g *GeminiCLIAgent) ExtractAgentSessionID(entireSessionID string) string {
	return sessionid.ModelSessionID(entireSessionID)
}

// SessionFileExtension returns ".json" - Gemini stores transcripts as JSON.
func (g *GeminiCLIAgent) SessionFileExtension() string { return ".json" }

// ProtectedDirs returns directories that Gemini uses for config/state.
func (g *GeminiCLIAgent) ProtectedDirs() []string { return []string{".gemini"} }

// GetSessionDir returns the directory where Gemini stores session transcripts.
// Gemini stores sessions in ~/.gemini/tmp/<project-hash>/chats/
func (g *GeminiCLIAgent) GetSessionDir(repoPath string) (string, error) {
	// Check for test environment override
	if override := os.Getenv("ENTIRE_TEST_GEMINI_PROJECT_DIR"); override != "" {
		return override, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	// Gemini uses a hash of the project path for the directory name
	projectDir := SanitizePathForGemini(repoPath)
	return filepath.Join(homeDir, ".gemini", "tmp", projectDir, "chats"), nil
}

// ReadSession reads a session from Gemini's storage (JSON transcript file).
// The session data is stored in NativeData as raw JSON bytes.
func (g *GeminiCLIAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	// Read the raw JSON file
	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	// Parse to extract computed fields
	modifiedFiles, err := ExtractModifiedFiles(data)
	if err != nil {
		// Non-fatal: we can still return the session without modified files
		modifiedFiles = nil
	}

	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     g.Name(),
		SessionRef:    input.SessionRef,
		StartTime:     time.Now(),
		NativeData:    data,
		ModifiedFiles: modifiedFiles,
	}, nil
}

// WriteSession writes a session to Gemini's storage (JSON transcript file).
// Uses the NativeData field which contains raw JSON bytes.
func (g *GeminiCLIAgent) WriteSession(session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}

	// Verify this session belongs to Gemini CLI
	if session.AgentName != "" && session.AgentName != g.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, g.Name())
	}

	if session.SessionRef == "" {
		return errors.New("session reference (transcript path) is required")
	}

	if len(session.NativeData) == 0 {
		return errors.New("session has no native data to write")
	}

	// Write the raw JSON data
	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}

	return nil
}

// FormatResumeCommand returns the command to resume a Gemini CLI session.
func (g *GeminiCLIAgent) FormatResumeCommand(sessionID string) string {
	return "gemini --resume " + sessionID
}

// SanitizePathForGemini converts a path to Gemini's project directory format.
// Gemini uses a hash-like sanitization similar to Claude.
var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9]`)

func SanitizePathForGemini(path string) string {
	return nonAlphanumericRegex.ReplaceAllString(path, "-")
}

// TranscriptAnalyzer interface implementation

// GetTranscriptPosition returns the current message count of a Gemini transcript.
// Gemini uses JSON format with a messages array, so position is the message count.
// Returns 0 if the file doesn't exist or is empty.
func (g *GeminiCLIAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}

	data, err := os.ReadFile(path) //nolint:gosec // Reading from controlled transcript path
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read transcript: %w", err)
	}

	if len(data) == 0 {
		return 0, nil
	}

	transcript, err := ParseTranscript(data)
	if err != nil {
		return 0, fmt.Errorf("failed to parse transcript: %w", err)
	}

	return len(transcript.Messages), nil
}

// ExtractModifiedFilesFromOffset extracts files modified since a given message index.
// For Gemini (JSON format), offset is the starting message index.
// Returns:
//   - files: list of file paths modified by Gemini (from Write/Edit tools)
//   - currentPosition: total number of messages in the transcript
//   - error: any error encountered during reading
func (g *GeminiCLIAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error) {
	if path == "" {
		return nil, 0, nil
	}

	data, readErr := os.ReadFile(path) //nolint:gosec // Reading from controlled transcript path
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("failed to read transcript: %w", readErr)
	}

	if len(data) == 0 {
		return nil, 0, nil
	}

	transcript, parseErr := ParseTranscript(data)
	if parseErr != nil {
		return nil, 0, parseErr
	}

	totalMessages := len(transcript.Messages)

	// Extract files from messages starting at startOffset
	fileSet := make(map[string]bool)
	for i := startOffset; i < len(transcript.Messages); i++ {
		msg := transcript.Messages[i]
		// Only process gemini messages (assistant messages)
		if msg.Type != MessageTypeGemini {
			continue
		}

		// Process tool calls in this message
		for _, toolCall := range msg.ToolCalls {
			// Check if it's a file modification tool
			isModifyTool := false
			for _, name := range FileModificationTools {
				if toolCall.Name == name {
					isModifyTool = true
					break
				}
			}

			if !isModifyTool {
				continue
			}

			// Extract file path from args map
			var file string
			if fp, ok := toolCall.Args["file_path"].(string); ok && fp != "" {
				file = fp
			} else if p, ok := toolCall.Args["path"].(string); ok && p != "" {
				file = p
			} else if fn, ok := toolCall.Args["filename"].(string); ok && fn != "" {
				file = fn
			}

			if file != "" && !fileSet[file] {
				fileSet[file] = true
				files = append(files, file)
			}
		}
	}

	return files, totalMessages, nil
}

// TranscriptChunker interface implementation

// ChunkTranscript splits a Gemini JSON transcript by distributing messages across chunks.
// Gemini uses JSON format with a {"messages": [...]} structure, so chunking splits
// the messages array while preserving the JSON structure in each chunk.
func (g *GeminiCLIAgent) ChunkTranscript(content []byte, maxSize int) ([][]byte, error) {
	var transcript GeminiTranscript
	if err := json.Unmarshal(content, &transcript); err != nil {
		// Fall back to JSONL chunking if not valid Gemini JSON
		chunks, chunkErr := agent.ChunkJSONL(content, maxSize)
		if chunkErr != nil {
			return nil, fmt.Errorf("failed to chunk as JSONL: %w", chunkErr)
		}
		return chunks, nil
	}

	if len(transcript.Messages) == 0 {
		return [][]byte{content}, nil
	}

	var chunks [][]byte
	var currentMessages []GeminiMessage
	currentSize := len(`{"messages":[]}`) // Base JSON structure size

	for i, msg := range transcript.Messages {
		// Marshal message to get its size
		msgBytes, err := json.Marshal(msg)
		if err != nil {
			logging.Warn(context.Background(), "failed to marshal Gemini message during chunking",
				slog.Int("message_index", i),
				slog.String("error", err.Error()),
			)
			continue
		}
		msgSize := len(msgBytes) + 1 // +1 for comma separator

		if currentSize+msgSize > maxSize && len(currentMessages) > 0 {
			// Save current chunk
			chunkData, err := json.Marshal(GeminiTranscript{Messages: currentMessages})
			if err != nil {
				return nil, fmt.Errorf("failed to marshal chunk: %w", err)
			}
			chunks = append(chunks, chunkData)

			// Start new chunk
			currentMessages = nil
			currentSize = len(`{"messages":[]}`)
		}

		currentMessages = append(currentMessages, msg)
		currentSize += msgSize
	}

	// Add the last chunk
	if len(currentMessages) > 0 {
		chunkData, err := json.Marshal(GeminiTranscript{Messages: currentMessages})
		if err != nil {
			return nil, fmt.Errorf("failed to marshal final chunk: %w", err)
		}
		chunks = append(chunks, chunkData)
	}

	// Ensure we created at least one chunk (could be empty if all messages failed to marshal)
	if len(chunks) == 0 {
		return nil, errors.New("failed to create any chunks: all messages failed to marshal")
	}

	return chunks, nil
}

// ReassembleTranscript merges Gemini JSON chunks by combining their message arrays.
func (g *GeminiCLIAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	var allMessages []GeminiMessage

	for _, chunk := range chunks {
		var transcript GeminiTranscript
		if err := json.Unmarshal(chunk, &transcript); err != nil {
			return nil, fmt.Errorf("failed to unmarshal chunk: %w", err)
		}
		allMessages = append(allMessages, transcript.Messages...)
	}

	result, err := json.Marshal(GeminiTranscript{Messages: allMessages})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal reassembled transcript: %w", err)
	}
	return result, nil
}

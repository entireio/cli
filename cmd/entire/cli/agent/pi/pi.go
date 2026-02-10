// Package pi implements the Agent interface for pi coding agent.
package pi

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Agent name and type constants
const (
	AgentNamePi agent.AgentName = "pi"
	AgentTypePi agent.AgentType = "Pi Coding Agent"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(AgentNamePi, NewPiAgent)
}

// PiAgent implements the Agent interface for pi coding agent.
type PiAgent struct{}

// NewPiAgent creates a new pi agent instance.
func NewPiAgent() agent.Agent {
	return &PiAgent{}
}

// Name returns the agent registry key.
func (p *PiAgent) Name() agent.AgentName {
	return AgentNamePi
}

// Type returns the agent type identifier.
func (p *PiAgent) Type() agent.AgentType {
	return AgentTypePi
}

// Description returns a human-readable description.
func (p *PiAgent) Description() string {
	return "Pi - AI coding agent CLI"
}

// DetectPresence checks if pi is configured in the repository.
func (p *PiAgent) DetectPresence() (bool, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}

	// Check for .pi directory
	piDir := filepath.Join(repoRoot, ".pi")
	if _, err := os.Stat(piDir); err == nil {
		return true, nil
	}

	// Check for .pi/settings.json
	settingsFile := filepath.Join(repoRoot, ".pi", "settings.json")
	if _, err := os.Stat(settingsFile); err == nil {
		return true, nil
	}

	return false, nil
}

// GetHookConfigPath returns the path to pi's settings file.
// Pi uses extensions rather than hook config, so this returns empty.
func (p *PiAgent) GetHookConfigPath() string {
	return ""
}

// SupportsHooks returns true as pi supports lifecycle events via extensions.
func (p *PiAgent) SupportsHooks() bool {
	return true
}

// ParseHookInput parses pi hook input from stdin.
func (p *PiAgent) ParseHookInput(hookType agent.HookType, reader io.Reader) (*agent.HookInput, error) {
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

	// Parse the JSON input
	var raw piHookInput
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}

	input.SessionID = raw.SessionID
	input.SessionRef = raw.TranscriptPath
	input.UserPrompt = raw.Prompt

	// Store modified files in RawData
	if len(raw.ModifiedFiles) > 0 {
		input.RawData["modified_files"] = raw.ModifiedFiles
	}

	return input, nil
}

// GetSessionID extracts the session ID from hook input.
func (p *PiAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// TransformSessionID converts a pi session ID to an Entire session ID.
// Pi uses UUIDs, which we use directly.
func (p *PiAgent) TransformSessionID(agentSessionID string) string {
	return agentSessionID
}

// ExtractAgentSessionID extracts the pi session ID from an Entire session ID.
func (p *PiAgent) ExtractAgentSessionID(entireSessionID string) string {
	return entireSessionID
}

// GetSessionDir returns the directory where pi stores session transcripts.
func (p *PiAgent) GetSessionDir(repoPath string) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	// Pi stores sessions in ~/.pi/agent/sessions/--<sanitized-path>--/
	sanitizedPath := sanitizePathForPi(repoPath)
	return filepath.Join(homeDir, ".pi", "agent", "sessions", sanitizedPath), nil
}

// sanitizePathForPi converts a path to pi's session directory format.
// Pi replaces / with - and adds -- prefix/suffix.
func sanitizePathForPi(path string) string {
	// Replace path separators with dashes
	sanitized := strings.ReplaceAll(path, "/", "-")
	sanitized = strings.ReplaceAll(sanitized, "\\", "-")
	return "--" + sanitized + "--"
}

// ReadSession reads a session from pi's storage (JSONL transcript file).
func (p *PiAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	// Read the raw JSONL file
	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	// Parse to extract computed fields
	modifiedFiles := p.extractModifiedFiles(data)

	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     p.Name(),
		SessionRef:    input.SessionRef,
		StartTime:     time.Now(),
		NativeData:    data,
		ModifiedFiles: modifiedFiles,
	}, nil
}

// WriteSession writes a session to pi's storage.
func (p *PiAgent) WriteSession(session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}

	if session.AgentName != "" && session.AgentName != p.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, p.Name())
	}

	if session.SessionRef == "" {
		return errors.New("session reference (transcript path) is required")
	}

	if len(session.NativeData) == 0 {
		return errors.New("session has no native data to write")
	}

	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}

	return nil
}

// FormatResumeCommand returns the command to resume a pi session.
func (p *PiAgent) FormatResumeCommand(sessionID string) string {
	return "pi  # then use /resume to select session"
}

// extractModifiedFiles parses JSONL and extracts modified file paths.
func (p *PiAgent) extractModifiedFiles(data []byte) []string {
	files := make(map[string]bool)

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	// Increase buffer for large lines
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry piSessionEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		// Check for tool results with file paths
		if entry.Type == "message" && entry.Message != nil {
			msg := entry.Message

			// Check toolResult messages for write/edit
			if msg.Role == "toolResult" {
				if msg.ToolName == "write" || msg.ToolName == "edit" {
					if details, ok := msg.Details.(map[string]interface{}); ok {
						if path, ok := details["path"].(string); ok && path != "" {
							files[path] = true
						}
					}
				}
			}

			// Check assistant messages for tool calls
			if msg.Role == "assistant" {
				for _, content := range msg.Content {
					if content.Type == "toolCall" {
						if content.Name == "write" || content.Name == "edit" {
							if args, ok := content.Arguments.(map[string]interface{}); ok {
								if path, ok := args["path"].(string); ok && path != "" {
									files[path] = true
								}
							}
						}
					}
				}
			}
		}
	}

	result := make([]string, 0, len(files))
	for f := range files {
		result = append(result, f)
	}
	return result
}

// TranscriptAnalyzer interface implementation

// GetTranscriptPosition returns the current line count of a pi transcript.
func (p *PiAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}

	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to open transcript file: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineCount := 0

	for {
		_, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return 0, fmt.Errorf("failed to read transcript: %w", err)
		}
		lineCount++
	}

	return lineCount, nil
}

// ExtractModifiedFilesFromOffset extracts files modified since a given line number.
func (p *PiAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error) {
	if path == "" {
		return nil, 0, nil
	}

	file, openErr := os.Open(path) //nolint:gosec
	if openErr != nil {
		return nil, 0, fmt.Errorf("failed to open transcript file: %w", openErr)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	modFiles := make(map[string]bool)
	lineNum := 0

	for {
		lineData, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, 0, fmt.Errorf("failed to read transcript: %w", readErr)
		}

		if len(lineData) > 0 {
			lineNum++
			if lineNum > startOffset {
				var entry piSessionEntry
				if parseErr := json.Unmarshal(lineData, &entry); parseErr == nil {
					// Extract files from this entry
					if entry.Type == "message" && entry.Message != nil {
						msg := entry.Message
						if msg.Role == "toolResult" && (msg.ToolName == "write" || msg.ToolName == "edit") {
							if details, ok := msg.Details.(map[string]interface{}); ok {
								if filePath, ok := details["path"].(string); ok && filePath != "" {
									modFiles[filePath] = true
								}
							}
						}
					}
				}
			}
		}

		if readErr == io.EOF {
			break
		}
	}

	result := make([]string, 0, len(modFiles))
	for f := range modFiles {
		result = append(result, f)
	}

	return result, lineNum, nil
}

// TranscriptChunker interface implementation

// ChunkTranscript splits a JSONL transcript at line boundaries.
func (p *PiAgent) ChunkTranscript(content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk JSONL transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks.
func (p *PiAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

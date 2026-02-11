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

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNamePi, NewPiAgent)
}

// PiAgent implements the Agent interface for pi coding agent.
//
//nolint:revive // Keep explicit PiAgent naming for consistency with other agent implementations.
type PiAgent struct{}

// NewPiAgent creates a new pi agent instance.
func NewPiAgent() agent.Agent {
	return &PiAgent{}
}

// Name returns the agent registry key.
func (p *PiAgent) Name() agent.AgentName {
	return agent.AgentNamePi
}

// Type returns the agent type identifier.
func (p *PiAgent) Type() agent.AgentType {
	return agent.AgentTypePi
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
	input.ToolName = raw.ToolName
	input.ToolUseID = raw.ToolUseID
	input.ToolInput = raw.ToolInput
	if hookType == agent.HookPostToolUse {
		input.ToolResponse = raw.ToolResponse
	}

	// Store agent-specific fields in RawData
	if len(raw.ModifiedFiles) > 0 {
		input.RawData["modified_files"] = raw.ModifiedFiles
	}
	if raw.ToolName != "" {
		input.RawData["tool_name"] = raw.ToolName
	}
	if raw.ToolUseID != "" {
		input.RawData["tool_use_id"] = raw.ToolUseID
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

// ResolveSessionFile returns the path to a Pi session file.
// Pi names files directly as <session-id>.jsonl.
func (p *PiAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

// ProtectedDirs returns directories that Pi uses for config/state.
func (p *PiAgent) ProtectedDirs() []string {
	return []string{".pi"}
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

// sanitizePathForPi converts a path to Pi's session directory format.
// Matches Pi's implementation:
//   - trim a leading slash/backslash
//   - replace /, \\, and : with -
//   - wrap with --<...>--
func sanitizePathForPi(path string) string {
	trimmed := strings.TrimLeft(path, "/\\")
	replacer := strings.NewReplacer("/", "-", "\\", "-", ":", "-")
	sanitized := replacer.Replace(trimmed)
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
func (p *PiAgent) FormatResumeCommand(_ string) string {
	return "pi  # then use /resume to select session"
}

// GetLastUserPrompt extracts the last user prompt from the session.
func (p *PiAgent) GetLastUserPrompt(session *agent.AgentSession) string {
	if session == nil || len(session.NativeData) == 0 {
		return ""
	}

	prompt, err := ExtractLastUserPrompt(session.NativeData)
	if err != nil {
		return ""
	}
	return prompt
}

// TruncateAtUUID returns a new session truncated at the given entry ID (inclusive).
func (p *PiAgent) TruncateAtUUID(session *agent.AgentSession, entryID string) (*agent.AgentSession, error) {
	if session == nil {
		return nil, errors.New("session is nil")
	}

	if len(session.NativeData) == 0 {
		return nil, errors.New("session has no native data")
	}

	if entryID == "" {
		// No truncation needed, return copy
		return &agent.AgentSession{
			SessionID:     session.SessionID,
			AgentName:     session.AgentName,
			RepoPath:      session.RepoPath,
			SessionRef:    session.SessionRef,
			StartTime:     session.StartTime,
			NativeData:    session.NativeData,
			ModifiedFiles: session.ModifiedFiles,
		}, nil
	}

	// Parse and truncate
	var result []byte
	scanner := bufio.NewScanner(strings.NewReader(string(session.NativeData)))
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Add this line to result
		result = append(result, line...)
		result = append(result, '\n')

		// Check if this is the target entry.
		// Parse only the ID fields to avoid failing on message content schema variants.
		var entry struct {
			ID   string `json:"id"`
			UUID string `json:"uuid"`
		}
		if err := json.Unmarshal(line, &entry); err == nil {
			if entry.ID == entryID || entry.UUID == entryID {
				break
			}
		}
	}

	return &agent.AgentSession{
		SessionID:     session.SessionID,
		AgentName:     session.AgentName,
		RepoPath:      session.RepoPath,
		SessionRef:    session.SessionRef,
		StartTime:     session.StartTime,
		NativeData:    result,
		ModifiedFiles: p.extractModifiedFiles(result),
	}, nil
}

// FindCheckpointUUID finds the entry ID of the message containing the tool result
// for the given tool call ID.
func (p *PiAgent) FindCheckpointUUID(session *agent.AgentSession, toolCallID string) (string, bool) {
	if session == nil || len(session.NativeData) == 0 {
		return "", false
	}
	return FindCheckpointEntryID(session.NativeData, toolCallID)
}

// CalculateTokenUsage calculates token usage from a Pi transcript.
func (p *PiAgent) CalculateTokenUsage(transcript []byte) *agent.TokenUsage {
	return CalculateTokenUsageFromTranscript(transcript, 0)
}

// extractModifiedFiles parses JSONL and extracts modified file paths.
func (p *PiAgent) extractModifiedFiles(data []byte) []string {
	files, err := ExtractModifiedFiles(data)
	if err != nil {
		return nil
	}
	return files
}

// TranscriptAnalyzer interface implementation

// GetTranscriptPosition returns the current line count of a pi transcript.
func (p *PiAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}

	file, err := os.Open(path) //nolint:gosec // Reading a local transcript path provided by the agent hook.
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
	return ExtractModifiedFilesSinceOffset(path, startOffset)
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
//
//nolint:unparam // error return is required by interface, kept for consistency
func (p *PiAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

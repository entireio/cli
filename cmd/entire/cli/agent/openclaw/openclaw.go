// Package openclaw implements the Agent interface for OpenClaw.
package openclaw

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/sessionid"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameOpenClaw, NewOpenClawAgent)
}

// OpenClawAgent implements the Agent interface for OpenClaw.
//
//nolint:revive // OpenClawAgent is clearer than Agent in this context
type OpenClawAgent struct{}

// NewOpenClawAgent creates a new OpenClaw agent instance.
func NewOpenClawAgent() agent.Agent {
	return &OpenClawAgent{}
}

// Name returns the agent registry key.
func (o *OpenClawAgent) Name() agent.AgentName {
	return agent.AgentNameOpenClaw
}

// Type returns the agent type identifier.
func (o *OpenClawAgent) Type() agent.AgentType {
	return agent.AgentTypeOpenClaw
}

// Description returns a human-readable description.
func (o *OpenClawAgent) Description() string {
	return "OpenClaw - AI agent runtime for coding assistants"
}

// DetectPresence checks if OpenClaw is configured in the repository.
func (o *OpenClawAgent) DetectPresence() (bool, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}

	// Check for .openclaw directory
	openclawDir := filepath.Join(repoRoot, ".openclaw")
	if _, err := os.Stat(openclawDir); err == nil {
		return true, nil
	}

	// Check for AGENTS.md (OpenClaw workspace marker)
	agentsFile := filepath.Join(repoRoot, "AGENTS.md")
	if _, err := os.Stat(agentsFile); err == nil {
		return true, nil
	}

	return false, nil
}

// GetHookConfigPath returns the path to OpenClaw's hook config file.
func (o *OpenClawAgent) GetHookConfigPath() string {
	return ""
}

// SupportsHooks returns true as OpenClaw supports lifecycle hooks.
func (o *OpenClawAgent) SupportsHooks() bool {
	return true
}

// ParseHookInput parses OpenClaw hook input from stdin.
// OpenClaw sends JSON with session_id and transcript_path fields.
func (o *OpenClawAgent) ParseHookInput(hookType agent.HookType, reader io.Reader) (*agent.HookInput, error) {
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

	var raw openClawHookInput
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}

	input.SessionID = raw.SessionID
	input.SessionRef = raw.TranscriptPath
	input.UserPrompt = raw.Prompt

	return input, nil
}

// openClawHookInput represents the JSON structure sent by OpenClaw hooks.
type openClawHookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Prompt         string `json:"prompt,omitempty"`
}

// GetSessionID extracts the session ID from hook input.
func (o *OpenClawAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// TransformSessionID converts an OpenClaw session ID to an Entire session ID.
func (o *OpenClawAgent) TransformSessionID(agentSessionID string) string {
	return agentSessionID
}

// ExtractAgentSessionID extracts the OpenClaw session ID from an Entire session ID.
func (o *OpenClawAgent) ExtractAgentSessionID(entireSessionID string) string {
	return sessionid.ModelSessionID(entireSessionID)
}

// ResolveSessionFile returns the path to an OpenClaw session file.
// OpenClaw names session files directly as <id>.jsonl.
func (o *OpenClawAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

// ProtectedDirs returns directories that OpenClaw uses for config/state.
func (o *OpenClawAgent) ProtectedDirs() []string { return []string{".openclaw"} }

// GetSessionDir returns the directory where OpenClaw stores session transcripts.
func (o *OpenClawAgent) GetSessionDir(_ string) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	return filepath.Join(homeDir, ".openclaw", "sessions"), nil
}

// ReadSession reads a session from OpenClaw's storage (JSONL transcript file).
func (o *OpenClawAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	lines, err := ParseTranscript(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}

	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     o.Name(),
		SessionRef:    input.SessionRef,
		StartTime:     time.Now(),
		NativeData:    data,
		ModifiedFiles: ExtractModifiedFiles(lines),
	}, nil
}

// WriteSession writes a session to OpenClaw's storage (JSONL transcript file).
func (o *OpenClawAgent) WriteSession(session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}

	if session.AgentName != "" && session.AgentName != o.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, o.Name())
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

// FormatResumeCommand returns the command to resume an OpenClaw session.
func (o *OpenClawAgent) FormatResumeCommand(sessionID string) string {
	return "openclaw session resume " + sessionID
}

// TranscriptLine represents a single line in an OpenClaw JSONL transcript.
type TranscriptLine struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content,omitempty"`
	Timestamp string          `json:"timestamp,omitempty"`
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
	Usage     json.RawMessage `json:"usage,omitempty"`
}

// toolCall represents a tool call in an OpenClaw transcript.
type toolCall struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

// toolInput represents the input to a file-modifying tool.
type toolInput struct {
	FilePath string `json:"file_path,omitempty"`
	Path     string `json:"path,omitempty"`
	Command  string `json:"command,omitempty"`
}

// ParseTranscript parses raw JSONL content into transcript lines.
func ParseTranscript(data []byte) ([]TranscriptLine, error) {
	var lines []TranscriptLine
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, scannerBufferSize), scannerBufferSize)

	for scanner.Scan() {
		var line TranscriptLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue // Skip malformed lines
		}
		lines = append(lines, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan transcript: %w", err)
	}
	return lines, nil
}

// scannerBufferSize for large transcript files (10MB).
const scannerBufferSize = 10 * 1024 * 1024

// ExtractModifiedFiles extracts files modified by tool calls from an OpenClaw transcript.
// OpenClaw uses tool calls with names like "write", "edit", "exec" that contain file paths.
func ExtractModifiedFiles(lines []TranscriptLine) []string {
	fileSet := make(map[string]bool)
	var files []string

	for _, line := range lines {
		if line.Role != "assistant" || len(line.ToolCalls) == 0 {
			continue
		}

		var calls []toolCall
		if err := json.Unmarshal(line.ToolCalls, &calls); err != nil {
			continue
		}

		for _, call := range calls {
			switch call.Name {
			case "Write", "write", "Edit", "edit":
				var input toolInput
				if err := json.Unmarshal(call.Input, &input); err != nil {
					continue
				}
				path := input.FilePath
				if path == "" {
					path = input.Path
				}
				if path != "" && !fileSet[path] {
					fileSet[path] = true
					files = append(files, path)
				}
			}
		}
	}

	return files
}

// ExtractLastUserPrompt extracts the last user prompt from the transcript.
func ExtractLastUserPrompt(lines []TranscriptLine) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].Role == "user" {
			var content string
			if err := json.Unmarshal(lines[i].Content, &content); err != nil {
				return string(lines[i].Content)
			}
			return content
		}
	}
	return ""
}

// GetTranscriptPosition returns the current line count of an OpenClaw transcript.
func (o *OpenClawAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}

	file, err := os.Open(path) //nolint:gosec // Path comes from OpenClaw transcript location
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
func (o *OpenClawAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error) {
	if path == "" {
		return nil, 0, nil
	}

	file, openErr := os.Open(path) //nolint:gosec // Path comes from OpenClaw transcript location
	if openErr != nil {
		return nil, 0, fmt.Errorf("failed to open transcript file: %w", openErr)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var lines []TranscriptLine
	lineNum := 0

	for {
		lineData, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, 0, fmt.Errorf("failed to read transcript: %w", readErr)
		}

		if len(lineData) > 0 {
			lineNum++
			if lineNum > startOffset {
				var line TranscriptLine
				if parseErr := json.Unmarshal(lineData, &line); parseErr == nil {
					lines = append(lines, line)
				}
			}
		}

		if readErr == io.EOF {
			break
		}
	}

	return ExtractModifiedFiles(lines), lineNum, nil
}

// ChunkTranscript splits a JSONL transcript at line boundaries.
func (o *OpenClawAgent) ChunkTranscript(content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk JSONL transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks with newlines.
//
//nolint:unparam // error return is required by interface, kept for consistency
func (o *OpenClawAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

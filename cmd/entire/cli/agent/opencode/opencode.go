// Package opencode implements the Agent interface for OpenCode.
package opencode

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameOpenCode, NewOpenCodeAgent)
}

// OpenCodeAgent implements the Agent interface for OpenCode.
//
//nolint:revive // OpenCodeAgent is clearer than Agent in this context
type OpenCodeAgent struct{}

// NewOpenCodeAgent creates a new OpenCode agent instance.
func NewOpenCodeAgent() agent.Agent {
	return &OpenCodeAgent{}
}

// Name returns the agent registry key.
func (o *OpenCodeAgent) Name() agent.AgentName {
	return agent.AgentNameOpenCode
}

// Type returns the agent type identifier.
func (o *OpenCodeAgent) Type() agent.AgentType {
	return agent.AgentTypeOpenCode
}

// Description returns a human-readable description.
func (o *OpenCodeAgent) Description() string {
	return "OpenCode - AI coding agent"
}

// DetectPresence checks if OpenCode is configured in the repository.
func (o *OpenCodeAgent) DetectPresence() (bool, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}

	// Check for .opencode directory
	opencodeDir := filepath.Join(repoRoot, ".opencode")
	if _, err := os.Stat(opencodeDir); err == nil {
		return true, nil
	}
	return false, nil
}

// GetHookConfigPath returns the path to OpenCode's plugin file.
func (o *OpenCodeAgent) GetHookConfigPath() string {
	return ".opencode/plugins/entire.ts"
}

// SupportsHooks returns true as OpenCode supports lifecycle hooks via plugins.
func (o *OpenCodeAgent) SupportsHooks() bool {
	return true
}

// ParseHookInput parses OpenCode hook input from stdin.
// OpenCode uses a uniform JSON structure for all hook events.
func (o *OpenCodeAgent) ParseHookInput(hookType agent.HookType, reader io.Reader) (*agent.HookInput, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}

	if len(data) == 0 {
		return nil, errors.New("empty input")
	}

	var raw hookInputRaw
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse hook input: %w", err)
	}

	input := &agent.HookInput{
		HookType:  hookType,
		Timestamp: time.Now(),
		RawData:   make(map[string]interface{}),
	}

	input.SessionID = raw.SessionID
	// Map transcript_path to SessionRef for downstream compatibility.
	// SessionRef is used by commitWithMetadata and other shared handlers.
	if raw.SessionRef != "" {
		input.SessionRef = raw.SessionRef
	}
	if raw.TranscriptPath != "" {
		input.SessionRef = raw.TranscriptPath
		input.RawData["transcript_path"] = raw.TranscriptPath
	}

	if raw.ToolName != "" {
		input.ToolName = raw.ToolName
	}
	if raw.ToolUseID != "" {
		input.ToolUseID = raw.ToolUseID
	}
	if raw.ToolInput != nil {
		input.ToolInput = raw.ToolInput
	}
	if raw.ToolResponse != nil {
		input.ToolResponse = raw.ToolResponse
	}
	if raw.SubagentTranscriptPath != "" {
		input.RawData["subagent_transcript_path"] = raw.SubagentTranscriptPath
	}

	if raw.Timestamp != "" {
		if ts, parseErr := time.Parse(time.RFC3339, raw.Timestamp); parseErr == nil {
			input.Timestamp = ts
		}
	}

	return input, nil
}

// GetSessionID extracts the session ID from hook input.
func (o *OpenCodeAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// ProtectedDirs returns directories that OpenCode uses for config/state.
func (o *OpenCodeAgent) ProtectedDirs() []string { return []string{".opencode"} }

// ResolveSessionFile returns the path to an OpenCode session file.
// OpenCode uses JSONL transcript files named by session ID.
func (o *OpenCodeAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

// GetSessionDir returns where OpenCode stores session data.
// OpenCode stores sessions in .opencode/sessions/ within the project directory.
func (o *OpenCodeAgent) GetSessionDir(repoPath string) (string, error) {
	if override := os.Getenv("ENTIRE_TEST_OPENCODE_PROJECT_DIR"); override != "" {
		return override, nil
	}

	return filepath.Join(repoPath, ".opencode", "sessions"), nil
}

// ReadSession reads a session from OpenCode's storage (JSONL transcript file).
func (o *OpenCodeAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	modifiedFiles := ExtractModifiedFiles(data)

	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     o.Name(),
		SessionRef:    input.SessionRef,
		StartTime:     time.Now(),
		NativeData:    data,
		ModifiedFiles: modifiedFiles,
	}, nil
}

// WriteSession writes a session to OpenCode's storage (JSONL transcript file).
func (o *OpenCodeAgent) WriteSession(session *agent.AgentSession) error {
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

// FormatResumeCommand returns the command to resume an OpenCode session.
func (o *OpenCodeAgent) FormatResumeCommand(sessionID string) string {
	return "opencode --resume " + sessionID
}

// TranscriptAnalyzer interface implementation

// GetTranscriptPosition returns the current line count of an OpenCode transcript.
// OpenCode uses JSONL format, so position is the number of lines.
func (o *OpenCodeAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}

	file, err := os.Open(path) //nolint:gosec // Path comes from OpenCode transcript location
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
// For OpenCode (JSONL format), offset is the starting line number.
func (o *OpenCodeAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error) {
	if path == "" {
		return nil, 0, nil
	}

	file, openErr := os.Open(path) //nolint:gosec // Path comes from OpenCode transcript location
	if openErr != nil {
		if os.IsNotExist(openErr) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("failed to open transcript file: %w", openErr)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	fileSet := make(map[string]bool)
	lineNum := 0

	for {
		lineData, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, 0, fmt.Errorf("failed to read transcript: %w", readErr)
		}

		if len(lineData) > 0 {
			lineNum++
			if lineNum > startOffset {
				var entry TranscriptEntry
				if parseErr := json.Unmarshal(lineData, &entry); parseErr == nil {
					for _, f := range extractFilesFromEntry(&entry) {
						if !fileSet[f] {
							fileSet[f] = true
							files = append(files, f)
						}
					}
				}
			}
		}

		if readErr == io.EOF {
			break
		}
	}

	return files, lineNum, nil
}

// TranscriptChunker interface implementation

// ChunkTranscript splits a JSONL transcript at line boundaries.
func (o *OpenCodeAgent) ChunkTranscript(content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk JSONL transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks with newlines.
func (o *OpenCodeAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

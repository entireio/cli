// Package openclaw implements the Agent interface for OpenClaw.
package openclaw

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
	return "OpenClaw - AI agent orchestration platform"
}

// DetectPresence checks if OpenClaw is configured in the repository.
func (o *OpenClawAgent) DetectPresence() (bool, error) {
	// Check for OPENCLAW_SESSION env var
	if os.Getenv("OPENCLAW_SESSION") != "" {
		return true, nil
	}

	// Get repo root to check for .openclaw directory
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		// Not in a git repo, fall back to CWD-relative check
		repoRoot = "."
	}

	// Check for .openclaw directory
	openclawDir := filepath.Join(repoRoot, ".openclaw")
	if _, err := os.Stat(openclawDir); err == nil {
		return true, nil
	}

	return false, nil
}

// GetHookConfigPath returns empty since OpenClaw uses git hooks, not agent-side hooks.
func (o *OpenClawAgent) GetHookConfigPath() string {
	return ""
}

// SupportsHooks returns false as OpenClaw uses git hooks exclusively.
// OpenClaw sessions are captured via prepare-commit-msg, post-commit, and pre-push
// hooks that `entire enable` already installs.
func (o *OpenClawAgent) SupportsHooks() bool {
	return false
}

// ParseHookInput parses OpenClaw hook input from stdin.
// OpenClaw doesn't use agent-side hooks, but this is required by the interface.
// It handles the case where git hooks pass session context.
func (o *OpenClawAgent) ParseHookInput(_ agent.HookType, reader io.Reader) (*agent.HookInput, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}

	if len(data) == 0 {
		return nil, errors.New("empty input")
	}

	input := &agent.HookInput{
		Timestamp: time.Now(),
		RawData:   make(map[string]interface{}),
	}

	// Try to parse as JSON with session info
	var raw struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse input: %w", err)
	}

	input.SessionID = raw.SessionID
	input.SessionRef = raw.TranscriptPath

	return input, nil
}

// GetSessionID extracts the session ID from hook input.
func (o *OpenClawAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// TransformSessionID converts an OpenClaw session ID to an Entire session ID.
// This is an identity function - the agent session ID IS the Entire session ID.
func (o *OpenClawAgent) TransformSessionID(agentSessionID string) string {
	return agentSessionID
}

// ExtractAgentSessionID extracts the OpenClaw session ID from an Entire session ID.
// Since Entire session ID = agent session ID (identity), this returns the input unchanged.
// For backwards compatibility with legacy date-prefixed IDs, it strips the prefix if present.
func (o *OpenClawAgent) ExtractAgentSessionID(entireSessionID string) string {
	return sessionid.ModelSessionID(entireSessionID)
}

// GetSessionDir returns the directory where OpenClaw stores session transcripts.
// OpenClaw stores sessions at ~/.openclaw/sessions/ by default,
// or respects the OPENCLAW_SESSION_DIR env var.
func (o *OpenClawAgent) GetSessionDir(_ string) (string, error) {
	// Check for test environment override
	if override := os.Getenv("ENTIRE_TEST_OPENCLAW_SESSION_DIR"); override != "" {
		return override, nil
	}

	// Check for OpenClaw session dir env var
	if sessionDir := os.Getenv("OPENCLAW_SESSION_DIR"); sessionDir != "" {
		return sessionDir, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	return filepath.Join(homeDir, ".openclaw", "sessions"), nil
}

// ReadSession reads a session from OpenClaw's storage (JSONL transcript file).
// The session data is stored in NativeData as raw JSONL bytes.
// ModifiedFiles is computed by parsing the transcript.
func (o *OpenClawAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	// Read the raw JSONL file
	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	// Parse to extract computed fields
	messages, err := ParseTranscript(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}

	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     o.Name(),
		SessionRef:    input.SessionRef,
		StartTime:     time.Now(),
		NativeData:    data,
		ModifiedFiles: ExtractModifiedFiles(messages),
	}, nil
}

// WriteSession writes a session to OpenClaw's storage (JSONL transcript file).
// Uses the NativeData field which contains raw JSONL bytes.
func (o *OpenClawAgent) WriteSession(session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}

	// Verify this session belongs to OpenClaw
	if session.AgentName != "" && session.AgentName != o.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, o.Name())
	}

	if session.SessionRef == "" {
		return errors.New("session reference (transcript path) is required")
	}

	if len(session.NativeData) == 0 {
		return errors.New("session has no native data to write")
	}

	// Ensure parent directory exists
	dir := filepath.Dir(session.SessionRef)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	// Write the raw JSONL data
	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}

	return nil
}

// FormatResumeCommand returns the command to resume an OpenClaw session.
func (o *OpenClawAgent) FormatResumeCommand(sessionID string) string {
	return "openclaw resume " + sessionID
}

// TranscriptAnalyzer interface implementation

// GetTranscriptPosition returns the current line count of an OpenClaw transcript.
// OpenClaw uses JSONL format, so position is the number of lines.
// Returns 0 if the file doesn't exist or is empty.
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
// For OpenClaw (JSONL format), offset is the starting line number.
// Returns:
//   - files: list of file paths modified by OpenClaw (from write/edit tools)
//   - currentPosition: total number of lines in the file
//   - error: any error encountered during reading
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
	var messages []OpenClawMessage
	lineNum := 0

	for {
		lineData, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, 0, fmt.Errorf("failed to read transcript: %w", readErr)
		}

		if len(lineData) > 0 {
			lineNum++
			if lineNum > startOffset {
				var msg OpenClawMessage
				if parseErr := json.Unmarshal(lineData, &msg); parseErr == nil {
					messages = append(messages, msg)
				}
				// Skip malformed lines silently
			}
		}

		if readErr == io.EOF {
			break
		}
	}

	return ExtractModifiedFiles(messages), lineNum, nil
}

// TranscriptChunker interface implementation

// ChunkTranscript splits a JSONL transcript at line boundaries.
// OpenClaw uses JSONL format (one JSON object per line), so chunking
// is done at newline boundaries to preserve message integrity.
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

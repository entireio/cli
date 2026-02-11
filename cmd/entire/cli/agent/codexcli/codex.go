// Package codexcli implements the Agent interface for OpenAI Codex CLI.
// Codex does not support lifecycle hooks, so integration is done via a wrapper
// command (entire codex exec) that captures the JSONL event stream.
package codexcli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/sessionid"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameCodex, NewCodexCLIAgent)
}

// CodexCLIAgent implements the Agent interface for OpenAI Codex CLI.
//
//nolint:revive // CodexCLIAgent is clearer than Agent in this context
type CodexCLIAgent struct{}

// NewCodexCLIAgent creates a new Codex CLI agent instance.
func NewCodexCLIAgent() agent.Agent {
	return &CodexCLIAgent{}
}

// Name returns the agent registry key.
func (c *CodexCLIAgent) Name() agent.AgentName {
	return agent.AgentNameCodex
}

// Type returns the agent type identifier.
func (c *CodexCLIAgent) Type() agent.AgentType {
	return agent.AgentTypeCodex
}

// Description returns a human-readable description.
func (c *CodexCLIAgent) Description() string {
	return "Codex CLI - OpenAI's CLI coding assistant"
}

// DetectPresence checks if Codex CLI is available.
// Unlike hook-based agents, Codex detection checks for the binary in PATH
// since Codex does not create per-repo configuration directories.
func (c *CodexCLIAgent) DetectPresence() (bool, error) {
	_, err := exec.LookPath("codex")
	if err != nil {
		return false, nil //nolint:nilerr // binary not found is not an error
	}
	return true, nil
}

// GetHookConfigPath returns empty since Codex does not use hook config files.
func (c *CodexCLIAgent) GetHookConfigPath() string {
	return ""
}

// SupportsHooks returns false. Codex does not have a lifecycle hook system.
// Integration is achieved through the wrapper command (entire codex exec).
func (c *CodexCLIAgent) SupportsHooks() bool {
	return false
}

// ParseHookInput is not used for Codex since it does not support hooks.
// Returns an error if called.
func (c *CodexCLIAgent) ParseHookInput(_ agent.HookType, _ io.Reader) (*agent.HookInput, error) {
	return nil, errors.New("codex CLI does not support hooks; use 'entire codex exec' instead")
}

// GetSessionID extracts the session ID from hook input.
func (c *CodexCLIAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// TransformSessionID converts a Codex thread ID to an Entire session ID.
func (c *CodexCLIAgent) TransformSessionID(agentSessionID string) string {
	return agentSessionID
}

// ExtractAgentSessionID extracts the Codex thread ID from an Entire session ID.
func (c *CodexCLIAgent) ExtractAgentSessionID(entireSessionID string) string {
	return sessionid.ModelSessionID(entireSessionID)
}

// ProtectedDirs returns an empty list. Codex does not create per-repo directories.
func (c *CodexCLIAgent) ProtectedDirs() []string { return nil }

// userHomeDir is the function used to resolve the user's home directory.
// Overridden in tests to simulate os.UserHomeDir failures.
var userHomeDir = os.UserHomeDir

// parseEventStreamFn is the function used to parse event streams.
// Overridden in tests to simulate parse failures.
var parseEventStreamFn = ParseEventStream

// CodexHome returns the Codex home directory, respecting the CODEX_HOME env var.
func CodexHome() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return home
	}
	homeDir, err := userHomeDir()
	if err != nil {
		return filepath.Join(".", ".codex")
	}
	return filepath.Join(homeDir, ".codex")
}

// GetSessionDir returns the directory where Codex stores session data.
func (c *CodexCLIAgent) GetSessionDir(_ string) (string, error) {
	return filepath.Join(CodexHome(), "sessions"), nil
}

// ResolveSessionFile returns the path to a Codex session file.
func (c *CodexCLIAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

// ReadSession reads a session from the Codex event stream file.
func (c *CodexCLIAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	parsed, err := parseEventStreamFn(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse event stream: %w", err)
	}

	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     c.Name(),
		SessionRef:    input.SessionRef,
		StartTime:     time.Now(),
		NativeData:    data,
		ModifiedFiles: parsed.ModifiedFiles,
	}, nil
}

// WriteSession writes session data to a file.
func (c *CodexCLIAgent) WriteSession(session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}
	if session.AgentName != "" && session.AgentName != c.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, c.Name())
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

// FormatResumeCommand returns the command to resume a Codex session.
func (c *CodexCLIAgent) FormatResumeCommand(sessionID string) string {
	return "codex exec resume " + sessionID
}

// TranscriptAnalyzer interface implementation

// GetTranscriptPosition returns the current line count of a Codex event stream file.
func (c *CodexCLIAgent) GetTranscriptPosition(path string) (int, error) {
	return GetTranscriptPosition(path)
}

// ExtractModifiedFilesFromOffset extracts files modified since a given line number.
// For Codex (JSONL format), offset is the starting line number.
func (c *CodexCLIAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error) {
	if path == "" {
		return nil, 0, nil
	}

	file, openErr := os.Open(path) //nolint:gosec // path comes from controlled transcript location
	if openErr != nil {
		return nil, 0, fmt.Errorf("failed to open transcript file: %w", openErr)
	}
	defer file.Close()

	return scanModifiedFiles(file, startOffset)
}

// scanModifiedFiles scans JSONL lines from a reader and extracts modified file paths.
func scanModifiedFiles(r io.Reader, startOffset int) ([]string, int, error) {
	scanner := newBufferedScanner(r)
	lineNum := 0
	var modifiedFiles []string
	seen := make(map[string]bool)

	for scanner.Scan() {
		lineNum++
		if lineNum <= startOffset {
			continue
		}

		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}

		if event.Type != EventItemCompleted || len(event.Item) == 0 {
			continue
		}

		var envelope ItemEnvelope
		if err := json.Unmarshal(event.Item, &envelope); err != nil {
			continue
		}

		if envelope.Type == ItemFileChange {
			var item FileChangeItem
			if err := json.Unmarshal(event.Item, &item); err != nil {
				continue
			}
			for _, change := range item.Changes {
				if !seen[change.Path] {
					seen[change.Path] = true
					modifiedFiles = append(modifiedFiles, change.Path)
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to scan transcript: %w", err)
	}

	return modifiedFiles, lineNum, nil
}

// TranscriptChunker interface implementation

// ChunkTranscript splits a JSONL event stream at line boundaries.
func (c *CodexCLIAgent) ChunkTranscript(content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk JSONL transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks.
//
//nolint:unparam // error return is required by interface, kept for consistency
func (c *CodexCLIAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

func newBufferedScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, scannerBufferSize), scannerBufferSize)
	return s
}

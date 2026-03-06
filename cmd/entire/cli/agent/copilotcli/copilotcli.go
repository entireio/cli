// Package copilotcli implements the Agent interface for GitHub Copilot CLI.
package copilotcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameCopilotCLI, NewCopilotCLIAgent)
}

// CopilotCLIAgent implements the Agent interface for GitHub Copilot CLI.
//
//nolint:revive // CopilotCLIAgent is clearer than Agent in this context
type CopilotCLIAgent struct{}

// NewCopilotCLIAgent creates a new Copilot CLI agent instance.
func NewCopilotCLIAgent() agent.Agent {
	return &CopilotCLIAgent{}
}

// Name returns the agent registry key.
func (c *CopilotCLIAgent) Name() types.AgentName {
	return agent.AgentNameCopilotCLI
}

// Type returns the agent type identifier.
func (c *CopilotCLIAgent) Type() types.AgentType {
	return agent.AgentTypeCopilotCLI
}

// Description returns a human-readable description.
func (c *CopilotCLIAgent) Description() string {
	return "Copilot CLI - GitHub's AI-powered coding agent"
}

// IsPreview returns true because this is a new integration.
func (c *CopilotCLIAgent) IsPreview() bool { return true }

// DetectPresence checks if Entire hooks are installed in the Copilot CLI config.
// Delegates to AreHooksInstalled which checks .github/hooks/entire.json for Entire hook entries.
func (c *CopilotCLIAgent) DetectPresence(ctx context.Context) (bool, error) {
	return c.AreHooksInstalled(ctx), nil
}

// GetSessionID extracts the session ID from hook input.
func (c *CopilotCLIAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// GetSessionDir returns the directory where Copilot CLI stores session transcripts.
func (c *CopilotCLIAgent) GetSessionDir(_ string) (string, error) {
	if override := os.Getenv("ENTIRE_TEST_COPILOT_SESSION_DIR"); override != "" {
		return override, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	return filepath.Join(homeDir, ".copilot", "session-state"), nil
}

// ResolveSessionFile returns the path to a Copilot CLI session transcript file.
// Copilot CLI stores transcripts at <sessionDir>/<sessionId>/events.jsonl.
func (c *CopilotCLIAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID, "events.jsonl")
}

// ProtectedDirs returns directories that Copilot CLI uses for config/state.
func (c *CopilotCLIAgent) ProtectedDirs() []string {
	return []string{filepath.Join(".github", "hooks")}
}

// ReadSession reads a session from Copilot CLI's storage (JSONL transcript file).
func (c *CopilotCLIAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	// Parse events for ModifiedFiles; non-fatal if parsing fails since NativeData
	// is the authoritative session content. ReadSession has no context.Context so
	// we cannot log here — scanner errors are surfaced by other code paths that
	// do have context (e.g. ExtractModifiedFilesFromOffset, ExtractPrompts).
	events, _ := parseEventsFromBytes(data) //nolint:errcheck // best-effort extraction
	var modifiedFiles []string
	if len(events) > 0 {
		modifiedFiles = extractModifiedFilesFromEvents(events)
	}

	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     c.Name(),
		SessionRef:    input.SessionRef,
		StartTime:     time.Now(),
		NativeData:    data,
		ModifiedFiles: modifiedFiles,
	}, nil
}

// WriteSession writes a session to Copilot CLI's storage (JSONL transcript file).
func (c *CopilotCLIAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
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

// FormatResumeCommand returns a command to resume a Copilot CLI session.
func (c *CopilotCLIAgent) FormatResumeCommand(sessionID string) string {
	return "copilot --resume " + sessionID
}

// ReadTranscript reads the raw JSONL transcript bytes for a session.
func (c *CopilotCLIAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}
	return data, nil
}

// ChunkTranscript splits a JSONL transcript at line boundaries.
func (c *CopilotCLIAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk JSONL transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks with newlines.
func (c *CopilotCLIAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

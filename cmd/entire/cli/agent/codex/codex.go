// Package codex implements the Agent interface for OpenAI's Codex CLI.
package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameCodex, NewCodexAgent)
}

// CodexAgent implements the Agent interface for OpenAI's Codex CLI.
//
//nolint:revive // CodexAgent is clearer than Agent in this context
type CodexAgent struct{}

// NewCodexAgent creates a new Codex agent instance.
func NewCodexAgent() agent.Agent {
	return &CodexAgent{}
}

// Name returns the agent registry key.
func (c *CodexAgent) Name() types.AgentName {
	return agent.AgentNameCodex
}

// Type returns the agent type identifier.
func (c *CodexAgent) Type() types.AgentType {
	return agent.AgentTypeCodex
}

// Description returns a human-readable description.
func (c *CodexAgent) Description() string {
	return "Codex - OpenAI's CLI coding agent"
}

// IsPreview returns true because this is a new integration.
func (c *CodexAgent) IsPreview() bool { return true }

// DetectPresence checks if Codex is configured in the repository.
func (c *CodexAgent) DetectPresence(ctx context.Context) (bool, error) {
	return c.AreHooksInstalled(ctx), nil
}

// GetSessionID extracts the session ID from hook input.
func (c *CodexAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// resolveCodexHome returns the Codex home directory (CODEX_HOME or ~/.codex).
func resolveCodexHome() (string, error) {
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return codexHome, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".codex"), nil
}

// GetSessionDir returns the directory where Codex stores session transcripts.
// Codex stores transcripts under CODEX_HOME/sessions/YYYY/MM/DD/.
func (c *CodexAgent) GetSessionDir(_ string) (string, error) {
	if override := os.Getenv("ENTIRE_TEST_CODEX_SESSION_DIR"); override != "" {
		return override, nil
	}
	codexHome, err := resolveCodexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(codexHome, "sessions"), nil
}

// ResolveSessionFile returns the path to a Codex session transcript file.
// Codex provides the transcript path directly in hook payloads as an absolute path.
// When only a session ID is available, attach/rewind must recover it from the
// sessions/YYYY/MM/DD/rollout-...-<session-id>.jsonl layout.
func (c *CodexAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	if filepath.IsAbs(agentSessionID) {
		return agentSessionID
	}
	if sessionDir != "" {
		patterns := []string{
			filepath.Join(sessionDir, "rollout-*-"+agentSessionID+".jsonl"),
			filepath.Join(sessionDir, "*", "*", "*", "rollout-*-"+agentSessionID+".jsonl"),
		}
		for _, pattern := range patterns {
			matches, err := filepath.Glob(pattern)
			if err == nil && len(matches) > 0 {
				sort.Strings(matches)
				return matches[len(matches)-1]
			}
		}

		return filepath.Join(sessionDir, agentSessionID+".jsonl")
	}
	return agentSessionID
}

// ProtectedDirs returns directories that Codex uses for config/state.
func (c *CodexAgent) ProtectedDirs() []string { return []string{".codex"} }

// ReadSession reads a session from Codex's storage (JSONL rollout file).
func (c *CodexAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	startTime, err := parseSessionStartTime(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse session start time: %w", err)
	}

	// Extract modified files from the rollout transcript (best-effort, deduplicated).
	var modifiedFiles []string
	seen := make(map[string]struct{})
	for _, lineData := range splitJSONL(data) {
		for _, f := range extractFilesFromLine(lineData) {
			if _, exists := seen[f]; !exists {
				seen[f] = struct{}{}
				modifiedFiles = append(modifiedFiles, f)
			}
		}
	}

	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     c.Name(),
		SessionRef:    input.SessionRef,
		StartTime:     startTime,
		NativeData:    data,
		ModifiedFiles: modifiedFiles,
	}, nil
}

// WriteSession writes a session to Codex's storage (JSONL rollout file).
func (c *CodexAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
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
func (c *CodexAgent) FormatResumeCommand(sessionID string) string {
	return "codex resume " + sessionID
}

// ReadTranscript reads the raw JSONL transcript bytes for a session.
func (c *CodexAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}
	return data, nil
}

// ChunkTranscript splits a JSONL transcript at line boundaries.
func (c *CodexAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk JSONL transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks with newlines.
func (c *CodexAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

// Package kiro implements the Agent interface for Kiro.
package kiro

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameKiro, NewKiroAgent)
}

// KiroAgent implements the Agent interface for Kiro.
//
//nolint:revive // KiroAgent is clearer than Agent in this context
type KiroAgent struct{}

// NewKiroAgent creates a new Kiro agent instance.
func NewKiroAgent() agent.Agent {
	return &KiroAgent{}
}

// Name returns the agent registry key.
func (k *KiroAgent) Name() agent.AgentName {
	return agent.AgentNameKiro
}

// Type returns the agent type identifier.
func (k *KiroAgent) Type() agent.AgentType {
	return agent.AgentTypeKiro
}

// Description returns a human-readable description.
func (k *KiroAgent) Description() string {
	return "Kiro - AI-powered coding agent by Amazon"
}

// IsPreview returns true because Kiro integration is in preview.
func (k *KiroAgent) IsPreview() bool { return true }

// DetectPresence checks if Kiro is configured in the repository.
func (k *KiroAgent) DetectPresence(ctx context.Context) (bool, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		worktreeRoot = "."
	}

	kiroDir := filepath.Join(worktreeRoot, ".kiro")
	if _, err := os.Stat(kiroDir); err == nil {
		return true, nil
	}
	return false, nil
}

// GetSessionID extracts the session ID from hook input.
func (k *KiroAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// ResolveSessionFile returns the path to a Kiro session file.
func (k *KiroAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".json")
}

// ProtectedDirs returns directories that Kiro uses for config/state.
func (k *KiroAgent) ProtectedDirs() []string { return []string{".kiro"} }

// GetSessionDir returns the directory where Kiro stores session data for this repo.
func (k *KiroAgent) GetSessionDir(repoPath string) (string, error) {
	if override := os.Getenv("ENTIRE_TEST_KIRO_PROJECT_DIR"); override != "" {
		return override, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	projectDir := sanitizePathForKiro(repoPath)
	return filepath.Join(homeDir, ".kiro", "projects", projectDir), nil
}

// ReadSession reads a session from Kiro's storage.
// ModifiedFiles is left empty because Kiro uses SQLite for sessions.
// File detection relies on git status instead.
func (k *KiroAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference is required")
	}

	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read session file: %w", err)
	}

	return &agent.AgentSession{
		SessionID:  input.SessionID,
		AgentName:  k.Name(),
		SessionRef: input.SessionRef,
		StartTime:  time.Now(),
		NativeData: data,
	}, nil
}

// WriteSession writes a session to Kiro's storage.
func (k *KiroAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}

	if session.AgentName != "" && session.AgentName != k.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, k.Name())
	}

	if session.SessionRef == "" {
		return errors.New("session reference is required")
	}

	if len(session.NativeData) == 0 {
		return errors.New("session has no native data to write")
	}

	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("failed to write session file: %w", err)
	}

	return nil
}

// FormatResumeCommand returns an instruction to resume a Kiro session.
func (k *KiroAgent) FormatResumeCommand(_ string) string {
	return "kiro-cli chat --resume"
}

var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9]`)

func sanitizePathForKiro(path string) string {
	return nonAlphanumericRegex.ReplaceAllString(path, "-")
}

// ChunkTranscript splits a JSON transcript into chunks.
func (k *KiroAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates transcript chunks.
func (k *KiroAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

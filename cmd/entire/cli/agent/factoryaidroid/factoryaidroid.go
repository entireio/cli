// Package factoryaidroid implements the Agent interface for Factory AI Droid.
package factoryaidroid

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// nonAlphanumericRegex matches any non-alphanumeric character for path sanitization.
// Same pattern as claudecode.SanitizePathForClaude — duplicated to avoid cross-package dependency.
var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9]`)

func sanitizeRepoPath(path string) string {
	return nonAlphanumericRegex.ReplaceAllString(path, "-")
}

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameFactoryAIDroid, NewFactoryAIDroidAgent)
}

// FactoryAIDroidAgent implements the agent.Agent interface for Factory AI Droid.
//
//nolint:revive // FactoryAIDroidAgent is clearer than Agent in this context
type FactoryAIDroidAgent struct{}

// NewFactoryAIDroidAgent creates a new Factory AI Droid agent instance.
func NewFactoryAIDroidAgent() agent.Agent {
	return &FactoryAIDroidAgent{}
}

// Name returns the agent registry key.
func (f *FactoryAIDroidAgent) Name() types.AgentName { return agent.AgentNameFactoryAIDroid }

// Type returns the agent type identifier.
func (f *FactoryAIDroidAgent) Type() types.AgentType { return agent.AgentTypeFactoryAIDroid }

// Description returns a human-readable description.
func (f *FactoryAIDroidAgent) Description() string {
	return "Factory AI Droid - agent-native development platform"
}

// IsPreview returns true as Factory AI Droid integration is in preview.
func (f *FactoryAIDroidAgent) IsPreview() bool { return true }

// ProtectedDirs returns directories that Factory AI Droid uses for config/state.
func (f *FactoryAIDroidAgent) ProtectedDirs() []string { return []string{".factory"} }

// DetectPresence checks if Factory AI Droid is configured in the repository.
func (f *FactoryAIDroidAgent) DetectPresence(ctx context.Context) (bool, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".factory")); err == nil {
		return true, nil
	}
	return false, nil
}

// ReadTranscript reads the raw JSONL transcript bytes for a session.
func (f *FactoryAIDroidAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}
	return data, nil
}

// ChunkTranscript splits a JSONL transcript at line boundaries.
func (f *FactoryAIDroidAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks with newlines.
func (f *FactoryAIDroidAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

// GetSessionID extracts the session ID from hook input.
func (f *FactoryAIDroidAgent) GetSessionID(input *agent.HookInput) string { return input.SessionID }

// GetSessionDir returns the directory where Factory AI Droid stores session transcripts.
// Path: ~/.factory/sessions/<sanitized-repo-path>/
func (f *FactoryAIDroidAgent) GetSessionDir(repoPath string) (string, error) {
	if override := os.Getenv("ENTIRE_TEST_DROID_PROJECT_DIR"); override != "" {
		return override, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	projectDir := sanitizeRepoPath(repoPath)
	return filepath.Join(homeDir, ".factory", "sessions", projectDir), nil
}

// GetSessionBaseDir returns the base directory containing per-project session subdirectories.
// Unlike GetSessionDir, this does NOT use test overrides because the override
// points to a specific project dir, not the base containing all projects.
func (f *FactoryAIDroidAgent) GetSessionBaseDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".factory", "sessions"), nil
}

// ResolveSessionFile returns the path to a Factory AI Droid session file.
func (f *FactoryAIDroidAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

// ReadSession reads a session from Factory AI Droid's storage (JSONL transcript file).
// The session data is stored in NativeData as raw JSONL bytes.
// ModifiedFiles is computed by parsing the transcript.
func (f *FactoryAIDroidAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	lines, _, err := ParseDroidTranscriptFromBytes(data, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}

	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     f.Name(),
		SessionRef:    input.SessionRef,
		StartTime:     time.Now(),
		NativeData:    data,
		ModifiedFiles: ExtractModifiedFiles(lines),
	}, nil
}

// WriteSession writes a session to Factory AI Droid's storage (JSONL transcript file).
// Uses the NativeData field which contains raw JSONL bytes.
func (f *FactoryAIDroidAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}

	if session.AgentName != "" && session.AgentName != f.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, f.Name())
	}

	if session.SessionRef == "" {
		return errors.New("session reference (transcript path) is required")
	}

	if len(session.NativeData) == 0 {
		return errors.New("session has no native data to write")
	}

	if err := os.MkdirAll(filepath.Dir(session.SessionRef), 0o750); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}

	return nil
}

// FormatResumeCommand returns the command to resume a Factory AI Droid session.
func (f *FactoryAIDroidAgent) FormatResumeCommand(sessionID string) string {
	return "droid --session-id " + sessionID
}

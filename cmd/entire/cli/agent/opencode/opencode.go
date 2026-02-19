// Package opencode implements the Agent interface for OpenCode.
package opencode

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameOpenCode, NewOpenCodeAgent)
}

//nolint:revive // OpenCodeAgent is clearer than Agent in this context
type OpenCodeAgent struct{}

// NewOpenCodeAgent creates a new OpenCode agent instance.
func NewOpenCodeAgent() agent.Agent {
	return &OpenCodeAgent{}
}

// --- Identity ---

func (a *OpenCodeAgent) Name() agent.AgentName   { return agent.AgentNameOpenCode }
func (a *OpenCodeAgent) Type() agent.AgentType   { return agent.AgentTypeOpenCode }
func (a *OpenCodeAgent) Description() string     { return "OpenCode - AI-powered terminal coding agent" }
func (a *OpenCodeAgent) IsPreview() bool         { return true }
func (a *OpenCodeAgent) ProtectedDirs() []string { return []string{".opencode"} }

func (a *OpenCodeAgent) DetectPresence() (bool, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}
	// Check for .opencode directory or opencode.json config
	if _, err := os.Stat(filepath.Join(repoRoot, ".opencode")); err == nil {
		return true, nil
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "opencode.json")); err == nil {
		return true, nil
	}
	return false, nil
}

// --- Transcript Storage ---

func (a *OpenCodeAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path from agent hook
	if err != nil {
		return nil, fmt.Errorf("failed to read opencode transcript: %w", err)
	}
	return data, nil
}

func (a *OpenCodeAgent) ChunkTranscript(content []byte, maxSize int) ([][]byte, error) {
	// OpenCode uses JSONL (one message per line) — use the shared JSONL chunker.
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk opencode transcript: %w", err)
	}
	return chunks, nil
}

func (a *OpenCodeAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	// JSONL reassembly is simple concatenation.
	return agent.ReassembleJSONL(chunks), nil
}

// --- Legacy methods ---

func (a *OpenCodeAgent) GetHookConfigPath() string { return "" } // Plugin file, not a JSON config
func (a *OpenCodeAgent) SupportsHooks() bool       { return true }

func (a *OpenCodeAgent) ParseHookInput(_ agent.HookType, r io.Reader) (*agent.HookInput, error) {
	raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](r)
	if err != nil {
		return nil, err
	}
	return &agent.HookInput{
		SessionID:  raw.SessionID,
		SessionRef: raw.TranscriptPath,
	}, nil
}

func (a *OpenCodeAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// GetSessionDir returns the directory where Entire stores OpenCode session transcripts.
// Transcripts are ephemeral handoff files between the TS plugin and the Go hook handler.
// Once checkpointed, the data lives on git refs and the file is disposable.
// Stored in os.TempDir()/entire-opencode/<sanitized-path>/ to avoid squatting on
// OpenCode's own directories (~/.opencode/ is project-level, not home-level).
func (a *OpenCodeAgent) GetSessionDir(repoPath string) (string, error) {
	// Check for test environment override
	if override := os.Getenv("ENTIRE_TEST_OPENCODE_PROJECT_DIR"); override != "" {
		return override, nil
	}

	projectDir := SanitizePathForOpenCode(repoPath)
	return filepath.Join(os.TempDir(), "entire-opencode", projectDir), nil
}

func (a *OpenCodeAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

func (a *OpenCodeAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("no session ref provided")
	}
	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read session: %w", err)
	}

	// Parse to extract computed fields
	modifiedFiles, err := ExtractModifiedFiles(data)
	if err != nil {
		// Non-fatal: we can still return the session without modified files
		modifiedFiles = nil
	}

	return &agent.AgentSession{
		AgentName:     a.Name(),
		SessionID:     input.SessionID,
		SessionRef:    input.SessionRef,
		NativeData:    data,
		ModifiedFiles: modifiedFiles,
	}, nil
}

func (a *OpenCodeAgent) WriteSession(session *agent.AgentSession) error {
	if session == nil {
		return errors.New("nil session")
	}
	if session.SessionRef == "" {
		return errors.New("no session ref to write to")
	}
	if len(session.NativeData) == 0 {
		return errors.New("no session data to write")
	}
	dir := filepath.Dir(session.SessionRef)
	//nolint:gosec // G301: Session directory needs standard permissions
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}
	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("failed to write session data: %w", err)
	}
	return nil
}

func (a *OpenCodeAgent) FormatResumeCommand(sessionID string) string {
	return "opencode --session " + sessionID
}

// nonAlphanumericRegex matches any non-alphanumeric character.
var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9]`)

// SanitizePathForOpenCode converts a path to a safe directory name.
// Replaces any non-alphanumeric character with a dash (same approach as Claude/Gemini).
func SanitizePathForOpenCode(path string) string {
	return nonAlphanumericRegex.ReplaceAllString(path, "-")
}

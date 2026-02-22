// Package generic implements a configurable Agent adapter for arbitrary coding agents
// that produce JSONL session transcripts. It uses file watching (not hooks) to detect
// session activity, making it suitable for agents like OpenClaw and AMP that don't
// expose lifecycle hooks.
//
// Configuration is via `.entire/generic.json` in the repository root. See Config
// for the available fields and README.md for usage examples.
package generic

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Compile-time interface assertions.
var (
	_ agent.Agent       = (*GenericAgent)(nil)
	_ agent.FileWatcher = (*GenericAgent)(nil)
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameGeneric, NewGenericAgent)
}

// GenericAgent adapts any JSONL-transcript-producing coding agent to the Entire Agent interface.
//
//nolint:revive // GenericAgent is clearer than Agent in this context
type GenericAgent struct {
	config *Config
}

// NewGenericAgent creates a new generic agent instance.
func NewGenericAgent() agent.Agent {
	return &GenericAgent{}
}

// loadOrCachedConfig returns the config, loading it lazily on first access.
func (a *GenericAgent) loadOrCachedConfig() (*Config, error) {
	if a.config != nil {
		return a.config, nil
	}
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		return nil, fmt.Errorf("failed to find repo root: %w", err)
	}
	cfg, err := loadConfig(repoRoot)
	if err != nil {
		return nil, err
	}
	a.config = cfg
	return cfg, nil
}

// --- Identity ---

func (a *GenericAgent) Name() agent.AgentName { return agent.AgentNameGeneric }

func (a *GenericAgent) Type() agent.AgentType {
	cfg, err := a.loadOrCachedConfig()
	if err != nil || cfg.AgentType == "" {
		return agent.AgentTypeGeneric
	}
	return agent.AgentType(cfg.AgentType)
}

func (a *GenericAgent) Description() string {
	return "Generic adapter for JSONL-transcript-producing coding agents"
}

func (a *GenericAgent) IsPreview() bool         { return true }
func (a *GenericAgent) ProtectedDirs() []string { return nil }

// DetectPresence returns true if a `.entire/generic.json` config file exists
// or if the configured transcript directory exists.
func (a *GenericAgent) DetectPresence() (bool, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}
	// Check for config file
	configPath := filepath.Join(repoRoot, ".entire", configFileName)
	if _, err := os.Stat(configPath); err == nil {
		return true, nil
	}
	return false, nil
}

// --- Transcript Storage ---

func (a *GenericAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path from agent hook
	if err != nil {
		return nil, fmt.Errorf("failed to read generic transcript: %w", err)
	}
	return data, nil
}

func (a *GenericAgent) ChunkTranscript(content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk generic transcript: %w", err)
	}
	return chunks, nil
}

func (a *GenericAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

// --- Legacy methods ---

func (a *GenericAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

func (a *GenericAgent) GetSessionDir(repoPath string) (string, error) {
	cfg, err := a.loadOrCachedConfig()
	if err != nil {
		return "", err
	}
	if cfg.TranscriptDir != "" {
		return cfg.TranscriptDir, nil
	}
	return filepath.Join(repoPath, ".entire", "generic-sessions"), nil
}

func (a *GenericAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

func (a *GenericAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("no session ref provided")
	}
	data, err := os.ReadFile(input.SessionRef) //nolint:gosec // Path from agent hook
	if err != nil {
		return nil, fmt.Errorf("failed to read session: %w", err)
	}

	modifiedFiles := extractModifiedFilesFromJSONL(data)

	return &agent.AgentSession{
		AgentName:     a.Name(),
		SessionID:     input.SessionID,
		SessionRef:    input.SessionRef,
		NativeData:    data,
		ModifiedFiles: modifiedFiles,
	}, nil
}

func (a *GenericAgent) WriteSession(session *agent.AgentSession) error {
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

func (a *GenericAgent) FormatResumeCommand(sessionID string) string {
	return fmt.Sprintf("# Resume session %s (check agent-specific docs)", sessionID)
}

// --- FileWatcher ---

// GetWatchPaths returns the transcript directory to watch for new/modified files.
func (a *GenericAgent) GetWatchPaths() ([]string, error) {
	cfg, err := a.loadOrCachedConfig()
	if err != nil {
		return nil, err
	}
	if cfg.TranscriptDir == "" {
		return nil, errors.New("transcript_dir not configured in generic.json")
	}
	return []string{cfg.TranscriptDir}, nil
}

// OnFileChange handles a detected transcript file change and returns session info.
func (a *GenericAgent) OnFileChange(path string) (*agent.SessionChange, error) {
	cfg, err := a.loadOrCachedConfig()
	if err != nil {
		return nil, err
	}

	// Check if the file matches the transcript pattern
	base := filepath.Base(path)
	matched, err := filepath.Match(cfg.TranscriptPattern, base)
	if err != nil || !matched {
		return nil, nil //nolint:nilnil // Non-matching files are silently ignored
	}

	sessionID := a.extractSessionID(path, cfg)

	return &agent.SessionChange{
		SessionID:  sessionID,
		SessionRef: path,
		EventType:  agent.HookStop, // Treat file changes as session activity
	}, nil
}

// extractSessionID derives a session ID based on the configured strategy.
func (a *GenericAgent) extractSessionID(path string, cfg *Config) string {
	if strings.HasPrefix(cfg.SessionIDFrom, "field:") {
		fieldName := strings.TrimPrefix(cfg.SessionIDFrom, "field:")
		if id := extractFieldFromFirstLine(path, fieldName); id != "" {
			return id
		}
	}
	// Default: use filename without extension
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// extractFieldFromFirstLine reads the first line of a JSONL file and extracts a string field.
func extractFieldFromFirstLine(path string, fieldName string) string {
	data, err := os.ReadFile(path) //nolint:gosec // Path from file watcher
	if err != nil {
		return ""
	}
	// Find first newline
	idx := strings.IndexByte(string(data), '\n')
	firstLine := string(data)
	if idx >= 0 {
		firstLine = string(data[:idx])
	}

	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(firstLine), &obj); err != nil {
		return ""
	}
	if v, ok := obj[fieldName]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

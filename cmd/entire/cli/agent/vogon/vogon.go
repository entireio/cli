// Package vogon implements the Agent interface for a deterministic test agent
// used as an E2E canary. It exercises the full checkpoint/hook lifecycle without
// real API calls. Named after the Vogons from The Hitchhiker's Guide to the
// Galaxy — bureaucratic, procedural, and deterministic to a fault.
package vogon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(AgentNameVogon, NewAgent)
}

const (
	// AgentNameVogon is the registry key for the vogon agent.
	AgentNameVogon types.AgentName = "vogon"
	// AgentTypeVogon is the type identifier stored in metadata.
	AgentTypeVogon types.AgentType = "Vogon Agent"
)

// Agent implements agent.Agent and agent.HookSupport for E2E canary tests.
type Agent struct{}

// NewAgent creates a new vogon agent instance.
func NewAgent() agent.Agent { return &Agent{} }

func (v *Agent) Name() types.AgentName { return AgentNameVogon }
func (v *Agent) Type() types.AgentType { return AgentTypeVogon }
func (v *Agent) Description() string {
	return "Vogon Agent - deterministic E2E canary (no API calls)"
}
func (v *Agent) IsPreview() bool         { return false }
func (v *Agent) ProtectedDirs() []string { return []string{".vogon"} }

// DetectPresence returns false — vogon agent is never auto-detected.
func (v *Agent) DetectPresence(_ context.Context) (bool, error) { return false, nil }

// IsTestOnly marks this agent as test-only, excluding it from `entire enable`.
func (v *Agent) IsTestOnly() bool { return true }

// --- Transcript Storage ---

func (v *Agent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path from hook input
	if err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}
	return data, nil
}

func (v *Agent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("chunk transcript: %w", err)
	}
	return chunks, nil
}

func (v *Agent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

// --- Legacy Session Methods ---

func (v *Agent) GetSessionID(input *agent.HookInput) string { return input.SessionID }

func (v *Agent) GetSessionDir(_ string) (string, error) {
	if override := os.Getenv("ENTIRE_TEST_VOGON_PROJECT_DIR"); override != "" {
		return override, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(homeDir, ".vogon", "sessions"), nil
}

func (v *Agent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

func (v *Agent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}
	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		if os.IsNotExist(err) {
			// Transcript may not exist yet — return empty session
			return &agent.AgentSession{
				SessionID:  input.SessionID,
				AgentName:  v.Name(),
				SessionRef: input.SessionRef,
			}, nil
		}
		return nil, fmt.Errorf("read transcript: %w", err)
	}
	return &agent.AgentSession{
		SessionID:  input.SessionID,
		AgentName:  v.Name(),
		SessionRef: input.SessionRef,
		NativeData: data,
	}, nil
}

func (v *Agent) WriteSession(_ context.Context, session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}
	if session.SessionRef == "" {
		return errors.New("session reference is required")
	}
	if err := os.MkdirAll(filepath.Dir(session.SessionRef), 0o750); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}
	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	return nil
}

func (v *Agent) FormatResumeCommand(sessionID string) string {
	return "vogon --session-id " + sessionID
}

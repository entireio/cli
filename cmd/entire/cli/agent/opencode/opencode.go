// Package opencode implements the Agent interface for OpenCode.
package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
	// OpenCode uses JSON format (like Gemini). Parse and split by messages.
	if len(content) <= maxSize {
		return [][]byte{content}, nil
	}

	var transcript Transcript
	if err := json.Unmarshal(content, &transcript); err != nil {
		// Fallback to JSONL chunking if not valid JSON
		chunks, chunkErr := agent.ChunkJSONL(content, maxSize)
		if chunkErr != nil {
			return nil, fmt.Errorf("failed to chunk transcript as JSONL: %w", chunkErr)
		}
		return chunks, nil
	}

	if len(transcript.Messages) == 0 {
		return [][]byte{content}, nil
	}

	// Pre-marshal each message to avoid O(n²) re-serialization.
	// Track running size and split at chunk boundaries (same approach as Gemini).
	var chunks [][]byte
	var currentMessages []Message
	baseSize := len(fmt.Sprintf(`{"session_id":%q,"messages":[]}`, transcript.SessionID))
	currentSize := baseSize

	for _, msg := range transcript.Messages {
		msgBytes, err := json.Marshal(msg)
		if err != nil {
			continue // Skip messages that fail to marshal
		}
		msgSize := len(msgBytes) + 1 // +1 for comma separator

		if currentSize+msgSize > maxSize && len(currentMessages) > 0 {
			// Save current chunk
			chunkData, marshalErr := json.Marshal(Transcript{
				SessionID: transcript.SessionID,
				Messages:  currentMessages,
			})
			if marshalErr != nil {
				return nil, fmt.Errorf("failed to marshal transcript chunk: %w", marshalErr)
			}
			chunks = append(chunks, chunkData)

			// Start new chunk
			currentMessages = nil
			currentSize = baseSize
		}

		currentMessages = append(currentMessages, msg)
		currentSize += msgSize
	}

	// Save remaining messages
	if len(currentMessages) > 0 {
		chunkData, err := json.Marshal(Transcript{
			SessionID: transcript.SessionID,
			Messages:  currentMessages,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to marshal final transcript chunk: %w", err)
		}
		chunks = append(chunks, chunkData)
	}

	return chunks, nil
}

func (a *OpenCodeAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	if len(chunks) == 0 {
		return nil, nil
	}
	if len(chunks) == 1 {
		return chunks[0], nil
	}

	var combined Transcript
	for i, chunk := range chunks {
		var t Transcript
		if err := json.Unmarshal(chunk, &t); err != nil {
			return nil, fmt.Errorf("failed to parse transcript chunk %d: %w", i, err)
		}
		if i == 0 {
			combined.SessionID = t.SessionID
		}
		combined.Messages = append(combined.Messages, t.Messages...)
	}

	data, err := json.Marshal(combined)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal reassembled transcript: %w", err)
	}
	return data, nil
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

func (a *OpenCodeAgent) GetSessionDir(repoPath string) (string, error) {
	// OpenCode transcript files are written by the plugin to .opencode/sessions/entire/
	return filepath.Join(repoPath, ".opencode", "sessions", "entire"), nil
}

func (a *OpenCodeAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".json")
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

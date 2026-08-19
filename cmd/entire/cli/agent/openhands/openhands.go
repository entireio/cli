// Package openhands implements the Agent interface for OpenHands, All Hands
// AI's MIT-licensed coding agent (https://docs.openhands.dev/).
//
// OpenHands is the only agent Entire supports whose transcript is a directory of
// per-event JSON files rather than a single file, and it ships no export
// command. This package serializes that directory to JSONL and reconstructs it
// on write; the mapping is lossless because each filename is fully determined by
// the event's index and id. See AGENT.md for the reasoning and the checklist
// rule it trades against.
package openhands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameOpenHands, NewOpenHandsAgent)
}

//nolint:revive // OpenHandsAgent is clearer than Agent in this context
type OpenHandsAgent struct{}

// NewOpenHandsAgent creates a new OpenHands agent instance.
func NewOpenHandsAgent() agent.Agent {
	return &OpenHandsAgent{}
}

// --- Identity ---

func (a *OpenHandsAgent) Name() types.AgentName { return agent.AgentNameOpenHands }
func (a *OpenHandsAgent) Type() types.AgentType { return agent.AgentTypeOpenHands }
func (a *OpenHandsAgent) Description() string {
	return "OpenHands - open-source AI software engineer (All Hands AI)"
}
func (a *OpenHandsAgent) IsPreview() bool         { return true }
func (a *OpenHandsAgent) ProtectedDirs() []string { return []string{configDirName} }

// DetectPresence reports whether this repo looks like an OpenHands project.
func (a *OpenHandsAgent) DetectPresence(ctx context.Context) (bool, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}
	if _, err := os.Stat(filepath.Join(repoRoot, configDirName)); err == nil {
		return true, nil
	}
	return false, nil
}

// --- Transcript Storage ---

// ReadTranscript serializes a conversation's event directory to JSONL.
//
// sessionRef is the conversation directory, not a file: OpenHands stores one
// JSON document per event. Entries are emitted in index order, so line N is
// event N.
func (a *OpenHandsAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	return readEventDir(sessionRef)
}

// ChunkTranscript splits the serialized JSONL on line boundaries.
func (a *OpenHandsAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk openhands transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks.
func (a *OpenHandsAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	if len(chunks) == 0 {
		return nil, errors.New("no chunks to reassemble")
	}
	return agent.ReassembleJSONL(chunks), nil
}

// --- Session handling ---

func (a *OpenHandsAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// GetSessionDir returns the conversations root.
//
// repoPath is unused: OpenHands stores conversations under its persistence
// directory keyed by conversation id, not per project.
func (a *OpenHandsAgent) GetSessionDir(_ string) (string, error) {
	return conversationsRoot()
}

// ResolveSessionFile returns the conversation's event directory.
//
// SECURITY: agentSessionID becomes a path component, so callers handling
// untrusted input must pre-validate with validation.ValidateSessionID. See
// security_contract_test.go.
func (a *OpenHandsAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, conversationDirID(agentSessionID), eventsDirName)
}

func (a *OpenHandsAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("no session ref provided")
	}
	data, err := readEventDir(input.SessionRef)
	if err != nil {
		return nil, err
	}

	modifiedFiles := ExtractModifiedFiles(data)

	return &agent.AgentSession{
		AgentName:     a.Name(),
		SessionID:     input.SessionID,
		SessionRef:    input.SessionRef,
		NativeData:    data,
		ModifiedFiles: modifiedFiles,
	}, nil
}

// WriteSession expands the serialized JSONL back into one file per event,
// reconstructing OpenHands' own filenames so `openhands --resume` sees a
// well-formed conversation.
//
// base_state.json is deliberately untouched: it holds agent configuration and
// run state, not conversation content.
func (a *OpenHandsAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
	if session == nil {
		return errors.New("nil session")
	}
	if len(session.NativeData) == 0 {
		return errors.New("no session data to write")
	}
	if session.SessionRef == "" {
		return errors.New("no session ref to write to")
	}
	return writeEventDir(session.SessionRef, session.NativeData)
}

// FormatResumeCommand returns the command that resumes a conversation.
//
// --resume takes the dashed UUID form even though the on-disk directory uses
// undashed hex, so the id is normalized here.
func (a *OpenHandsAgent) FormatResumeCommand(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return "openhands"
	}
	return "openhands --resume " + resumeID(sessionID)
}

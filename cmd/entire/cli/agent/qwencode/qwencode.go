// Package qwencode implements the Agent interface for Qwen Code, Alibaba's
// Apache-2.0 coding agent (https://qwenlm.github.io/qwen-code-docs/).
//
// Qwen is a Gemini CLI fork, but only its message payload inherited that
// lineage: sessions are stored as JSONL with a Claude-style envelope, so this
// package parses both halves itself rather than reusing either existing agent.
// See AGENT.md.
package qwencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameQwenCode, NewQwenCodeAgent)
}

//nolint:revive // QwenCodeAgent is clearer than Agent in this context
type QwenCodeAgent struct{}

// NewQwenCodeAgent creates a new Qwen Code agent instance.
func NewQwenCodeAgent() agent.Agent {
	return &QwenCodeAgent{}
}

// --- Identity ---

func (a *QwenCodeAgent) Name() types.AgentName { return agent.AgentNameQwenCode }
func (a *QwenCodeAgent) Type() types.AgentType { return agent.AgentTypeQwenCode }
func (a *QwenCodeAgent) Description() string {
	return "Qwen Code - Alibaba's open-source coding agent"
}
func (a *QwenCodeAgent) IsPreview() bool         { return true }
func (a *QwenCodeAgent) ProtectedDirs() []string { return []string{".qwen"} }

// DetectPresence reports whether this repo looks like a Qwen Code project.
func (a *QwenCodeAgent) DetectPresence(ctx context.Context) (bool, error) {
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

func (a *QwenCodeAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path from validated session ID
	if err != nil {
		return nil, fmt.Errorf("failed to read qwen transcript: %w", err)
	}
	return data, nil
}

// ChunkTranscript splits the session JSONL on line boundaries.
func (a *QwenCodeAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk qwen transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks.
func (a *QwenCodeAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	if len(chunks) == 0 {
		return nil, errors.New("no chunks to reassemble")
	}
	return agent.ReassembleJSONL(chunks), nil
}

// --- Session handling ---

func (a *QwenCodeAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// GetSessionDir returns the directory Qwen stores this project's chats in.
//
// Qwen slugs the working directory the same way Claude and Gemini do, so the
// path is reconstructible. In practice the hook hands over transcript_path
// directly and this is only the fallback.
func (a *QwenCodeAgent) GetSessionDir(repoPath string) (string, error) {
	if override := os.Getenv("ENTIRE_TEST_QWEN_PROJECT_DIR"); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".qwen", "projects", sanitizePathForQwen(repoPath), "chats"), nil
}

// ResolveSessionFile builds the path to a session's JSONL.
//
// SECURITY: agentSessionID becomes a filename component, so callers handling
// untrusted input must pre-validate with validation.ValidateSessionID. See
// security_contract_test.go.
func (a *QwenCodeAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

func (a *QwenCodeAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("no session ref provided")
	}
	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read session: %w", err)
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

// WriteSession restores a session by writing the JSONL back to its chats
// directory, which is what makes `qwen -r <id>` work after a rewind.
func (a *QwenCodeAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
	if session == nil {
		return errors.New("nil session")
	}
	if len(session.NativeData) == 0 {
		return errors.New("no session data to write")
	}
	if session.SessionRef == "" {
		return errors.New("no session ref to write to")
	}

	//nolint:gosec // G301: qwen reads this directory
	if err := os.MkdirAll(filepath.Dir(session.SessionRef), 0o755); err != nil {
		return fmt.Errorf("create qwen chats dir: %w", err)
	}
	//nolint:gosec // G306: qwen reads this file
	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o644); err != nil {
		return fmt.Errorf("write qwen session: %w", err)
	}
	return nil
}

// FormatResumeCommand returns the command that resumes a Qwen session.
// Bare `qwen` with no ID would start a fresh session, so the no-ID case uses
// --continue, which resumes the most recent session for this project.
func (a *QwenCodeAgent) FormatResumeCommand(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return "qwen --continue"
	}
	return "qwen --resume " + sessionID
}

var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9]`)

// sanitizePathForQwen converts a path into Qwen's project directory slug.
func sanitizePathForQwen(path string) string {
	return nonAlphanumericRegex.ReplaceAllString(path, "-")
}

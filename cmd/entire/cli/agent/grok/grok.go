// Package grok implements the Agent interface for xAI's Grok Build CLI.
//
// See AGENT.md in this directory for the researched hook and transcript
// contract, including captured payloads.
package grok

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameGrok, NewGrokAgent)
}

// GrokAgent implements the Agent interface for Grok Build.
//
//nolint:revive // GrokAgent is clearer than Agent in this context
type GrokAgent struct{}

// NewGrokAgent creates a new Grok agent instance.
func NewGrokAgent() agent.Agent { return &GrokAgent{} }

// Compile-time interface assertions.
var (
	_ agent.Agent              = (*GrokAgent)(nil)
	_ agent.HookSupport        = (*GrokAgent)(nil)
	_ agent.HookFreshness      = (*GrokAgent)(nil)
	_ agent.TranscriptAnalyzer = (*GrokAgent)(nil)
	_ agent.TokenCalculator    = (*GrokAgent)(nil)
	_ agent.ModelExtractor     = (*GrokAgent)(nil)
)

func (g *GrokAgent) Name() types.AgentName { return agent.AgentNameGrok }
func (g *GrokAgent) Type() types.AgentType { return agent.AgentTypeGrok }

func (g *GrokAgent) Description() string {
	return "Grok Build - xAI's terminal coding agent"
}

func (g *GrokAgent) IsPreview() bool { return true }

// ProtectedDirs returns directories Grok uses for repo-local config/state.
func (g *GrokAgent) ProtectedDirs() []string { return []string{".grok"} }

// DetectPresence reports whether Grok is configured in this repository.
func (g *GrokAgent) DetectPresence(ctx context.Context) (bool, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".grok")); err == nil {
		return true, nil
	}
	return false, nil
}

// GetSessionID extracts the session ID from hook input.
func (g *GrokAgent) GetSessionID(input *agent.HookInput) string { return input.SessionID }

// grokHome returns Grok's base directory, honoring the GROK_HOME override the
// E2E runner relies on for isolation.
func grokHome() (string, error) {
	if override := os.Getenv("GROK_HOME"); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, ".grok"), nil
}

// GetSessionDir returns the directory holding this repo's Grok sessions.
//
// Grok groups sessions by working directory, naming the group with the
// URL-encoded absolute path — every character escaped, separators included, so
// "/a/b" becomes "%2Fa%2Fb". Verified against a live session.
//
// Grok switches to a slug-plus-hash name (recording the real path in a .cwd
// file inside the group) once the encoded name would exceed 255 bytes. That
// branch is not reproduced here: every hook payload except session_start
// carries transcriptPath outright, so this path is a fallback rather than the
// primary resolution route.
func (g *GrokAgent) GetSessionDir(repoPath string) (string, error) {
	home, err := grokHome()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		abs = repoPath
	}
	return filepath.Join(home, "sessions", encodeCWD(abs)), nil
}

// GetSessionBaseDir returns the directory containing all per-project session
// groups.
func (g *GrokAgent) GetSessionBaseDir() (string, error) {
	home, err := grokHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "sessions"), nil
}

// encodeCWD percent-encodes a path the way Grok names a session group:
// everything outside the RFC 3986 unreserved set is escaped, separators
// included, and a space becomes %20 rather than "+".
//
// This is written out rather than layered on net/url because neither helper
// has the right unreserved set. url.PathEscape leaves the sub-delims
// "@ $ & + = :" alone, so /home/a@corp would encode to %2Fhome%2Fa@corp and
// miss the directory Grok actually created; url.QueryEscape renders a space
// as "+". Both look correct on a plain path and are wrong on a real one.
func encodeCWD(path string) string {
	const upperhex = "0123456789ABCDEF"
	out := make([]byte, 0, len(path))
	for i := range len(path) {
		c := path[i]
		if isUnreservedByte(c) {
			out = append(out, c)
			continue
		}
		out = append(out, '%', upperhex[c>>4], upperhex[c&0x0F])
	}
	return string(out)
}

// isUnreservedByte reports whether c is RFC 3986 unreserved (ALPHA / DIGIT /
// "-" / "." / "_" / "~"), the only bytes left literal in a group name.
func isUnreservedByte(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '-', c == '.', c == '_', c == '~':
		return true
	default:
		return false
	}
}

// ResolveSessionFile returns the transcript path for a session.
// Grok stores it as <sessionDir>/<session-id>/updates.jsonl.
func (g *GrokAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID, transcriptFileName)
}

// transcriptFileName is Grok's authoritative conversation log. The sibling
// chat_history.jsonl holds raw model-facing messages and is deliberately not
// used: updates.jsonl is what the docs call the source of truth and what
// /resume replays.
const transcriptFileName = "updates.jsonl"

// ReadTranscript reads the raw JSONL transcript bytes for a session.
func (g *GrokAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // path comes from Grok hook input
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}
	return data, nil
}

// ChunkTranscript splits the JSONL transcript at line boundaries.
func (g *GrokAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk JSONL transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks.
func (g *GrokAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

// ReadSession reads a session from Grok's storage.
func (g *GrokAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}
	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}
	files, _ := modifiedFilesFrom(data, 0)
	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     g.Name(),
		SessionRef:    input.SessionRef,
		StartTime:     time.Now(),
		NativeData:    data,
		ModifiedFiles: files,
	}, nil
}

// WriteSession writes a session back to Grok's storage for resumption.
func (g *GrokAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}
	if session.AgentName != "" && session.AgentName != g.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, g.Name())
	}
	if session.SessionRef == "" {
		return errors.New("session reference (transcript path) is required")
	}
	if len(session.NativeData) == 0 {
		return errors.New("session has no native data to write")
	}
	if err := os.MkdirAll(filepath.Dir(session.SessionRef), 0o750); err != nil {
		return fmt.Errorf("failed to create session dir: %w", err)
	}
	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}
	return nil
}

// FormatResumeCommand returns the command to resume a Grok session.
func (g *GrokAgent) FormatResumeCommand(sessionID string) string {
	return "grok --resume " + sessionID
}

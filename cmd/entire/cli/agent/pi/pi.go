// Package pi implements the Agent interface for the pi coding agent
// (https://github.com/earendil-works/pi-mono).
// The npm package the embedded extension imports a type from is
// `@earendil-works/pi-coding-agent`.
//
// This is an in-tree port of the previously-external entire-agent-pi plugin
// (github.com/entireio/external-agents/agents/entire-agent-pi). The behaviour
// matches the external version — most notably the active-branch resolution
// for Pi's tree-shaped sessions — but the integration is plumbed directly
// through the in-tree Agent / HookSupport / TokenCalculator / TranscriptAnalyzer
// interfaces rather than the external JSON-over-stdio protocol.
package pi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// piHomeEnvVar overrides the default Pi home directory (~/.pi/agent).
// Pi itself reads this variable, so honoring it keeps Entire and Pi in
// agreement when a developer points Pi at a non-default home.
const piHomeEnvVar = "PI_CODING_AGENT_DIR"

// piSessionDirEnvVar lets tests redirect Pi's session lookup without
// touching the real ~/.pi/agent. Mirrors ENTIRE_TEST_<AGENT>_SESSION_DIR
// used by Codex.
const piSessionDirEnvVar = "ENTIRE_TEST_PI_SESSION_DIR"

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNamePi, NewPiAgent)
}

// PiAgent implements agent.Agent for the pi coding agent.
//
//nolint:revive // PiAgent is clearer than Agent in this context
type PiAgent struct{}

// NewPiAgent returns a new Pi agent instance.
func NewPiAgent() agent.Agent {
	return &PiAgent{}
}

// --- Identity ---

func (a *PiAgent) Name() types.AgentName    { return agent.AgentNamePi }
func (a *PiAgent) Type() types.AgentType    { return agent.AgentTypePi }
func (a *PiAgent) Description() string      { return "Pi coding agent integration for Entire" }
func (a *PiAgent) IsPreview() bool          { return true }
func (a *PiAgent) ProtectedDirs() []string  { return []string{".pi"} }
func (a *PiAgent) ProtectedFiles() []string { return nil }

// DetectPresence reports whether pi is configured for *this repo*. We only
// check repo-local config (.pi/) and intentionally ignore $PATH — in-tree
// agents follow the convention used by Claude/Gemini/OpenCode where
// detection means "this repo is set up for this agent", not "this agent is
// installed somewhere on this machine". The external plugin uses the broader
// $PATH check because it can't see repo state; we don't have that limitation.
func (a *PiAgent) DetectPresence(ctx context.Context) (bool, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".pi")); err == nil {
		return true, nil
	}
	return false, nil
}

// --- Transcript Storage (chunking) ---

// ReadTranscript reads a captured Pi JSONL session transcript from disk.
// SessionRef is the absolute path returned by captureTranscript().
func (a *PiAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	if sessionRef == "" {
		return nil, errors.New("empty session ref")
	}
	//nolint:gosec // SessionRef from validated lifecycle hook input
	data, err := os.ReadFile(sessionRef)
	if err != nil {
		return nil, fmt.Errorf("read pi transcript %s: %w", sessionRef, err)
	}
	return data, nil
}

// ChunkTranscript splits a Pi JSONL transcript at line boundaries.
func (a *PiAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("chunk pi transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks with newlines.
func (a *PiAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

// --- Legacy methods ---

// GetSessionID extracts the session ID from a hook input.
func (a *PiAgent) GetSessionID(input *agent.HookInput) string {
	if input == nil {
		return ""
	}
	return input.SessionID
}

// GetSessionDir returns the directory where Pi natively stores session
// transcripts for repoPath: <piHome>/sessions/<encoded-repo-path>/.
//
// Pointing this at the native store (rather than the per-repo
// .entire/tmp/pi/ cache populated by the agent_end hook) is what lets
// `entire session attach <id>` resolve cold sessions — sessions that
// were never hooked, or whose hook capture failed. attach falls through
// to GetSessionDir + ResolveSessionFile when no SessionRef is recorded
// in metadata, and the live Pi store is the only place that always has
// the transcript on disk.
//
// Resolution order:
//  1. ENTIRE_TEST_PI_SESSION_DIR (test override; no encoding applied)
//  2. PI_CODING_AGENT_DIR (Pi's own override; encoding still applies)
//  3. ~/.pi/agent (default)
//
// The .entire/tmp/pi/ cache stays as a hook-internal detail —
// captureTranscript writes there and the TurnEnd event records that
// path as SessionRef in checkpoint metadata, so subsequent operations
// on hooked sessions go through the recorded SessionRef and never call
// GetSessionDir.
func (a *PiAgent) GetSessionDir(repoPath string) (string, error) {
	if override := os.Getenv(piSessionDirEnvVar); override != "" {
		return override, nil
	}
	home, err := resolvePiHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "sessions", encodeRepoPathForPi(repoPath)), nil
}

// GetSessionBaseDir returns the base directory containing per-project
// session subdirectories. Used by attach's cross-project fallback
// (searchTranscriptInProjectDirs) when a session was started from a
// different cwd than the current worktree root.
func (a *PiAgent) GetSessionBaseDir() (string, error) {
	home, err := resolvePiHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "sessions"), nil
}

// ResolveSessionFile returns the path to the Pi session file for
// agentSessionID in sessionDir. Pi names files <timestamp>_<id>.jsonl,
// so glob for the matching ID; on multiple matches the lexicographically
// latest (most recent timestamp) wins.
//
// Absolute paths pass through unchanged so hook payloads carrying live
// pi paths work without re-resolution. When no match exists, fall back
// to a deterministic non-existent path so downstream stat checks fail
// cleanly rather than panicking on an empty path.
func (a *PiAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	if filepath.IsAbs(agentSessionID) {
		return agentSessionID
	}
	if path := findPiSessionByID(sessionDir, agentSessionID); path != "" {
		return path
	}
	if sessionDir == "" {
		return agentSessionID
	}
	return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

// resolvePiHome returns Pi's home directory: $PI_CODING_AGENT_DIR or
// ~/.pi/agent.
func resolvePiHome() (string, error) {
	if dir := os.Getenv(piHomeEnvVar); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".pi", "agent"), nil
}

// encodeRepoPathForPi encodes an absolute repo path into Pi's
// session-directory naming scheme: every path separator becomes '-',
// wrapped with '--' delimiters. /Users/foo/repo encodes as
// --Users-foo-repo--. Leading and trailing separators are absorbed so
// /a/b/ and /a/b encode the same way. Empty input returns "".
//
// Both '/' and '\\' are replaced regardless of host OS: on Windows,
// git rev-parse --show-toplevel returns forward slashes, but native
// Windows APIs use backslashes — a host-only replacement would leak
// the other form through and produce nested directories instead of a
// single name, breaking session lookup. filepath.ToSlash isn't enough
// because it only normalises the host's separator.
func encodeRepoPathForPi(repoPath string) string {
	if repoPath == "" {
		return ""
	}
	body := strings.NewReplacer("/", "-", `\`, "-").Replace(repoPath)
	body = strings.Trim(body, "-")
	return "--" + body + "--"
}

// findPiSessionByID globs sessionDir for *_<sessionID>.jsonl. Returns
// the lexicographically latest match (most recent timestamp) or "" when
// no match exists or sessionDir/sessionID is empty.
func findPiSessionByID(sessionDir, sessionID string) string {
	if sessionDir == "" || sessionID == "" {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(sessionDir, "*_"+sessionID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	return matches[len(matches)-1]
}

// ReadSession loads a captured Pi transcript and returns it as an AgentSession.
func (a *PiAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input == nil || input.SessionRef == "" {
		return nil, errors.New("no session ref provided")
	}

	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("read pi session: %w", err)
	}
	return &agent.AgentSession{
		AgentName:  a.Name(),
		SessionID:  input.SessionID,
		SessionRef: input.SessionRef,
		NativeData: data,
	}, nil
}

// WriteSession writes a captured Pi transcript back to disk so Pi can resume
// from it. Pi loads sessions from arbitrary paths via `pi --session <path>`,
// so a plain write is sufficient.
func (a *PiAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
	if session == nil {
		return errors.New("nil session")
	}
	if session.SessionRef == "" {
		return errors.New("session has empty SessionRef")
	}
	if len(session.NativeData) == 0 {
		return errors.New("session has empty NativeData")
	}
	if err := os.MkdirAll(filepath.Dir(session.SessionRef), 0o750); err != nil {
		return fmt.Errorf("create pi session dir: %w", err)
	}

	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("write pi session file: %w", err)
	}
	return nil
}

// FormatResumeCommand returns the shell command to resume a specific Pi
// session by ID. Pi accepts a partial UUID via `pi --session <id>`. When no
// session is specified, fall back to `pi --continue` which reopens the most
// recent session.
func (a *PiAgent) FormatResumeCommand(sessionID string) string {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return "pi --continue"
	}
	return "pi --session " + id
}

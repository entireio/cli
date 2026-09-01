// Package codex implements the Agent interface for OpenAI's Codex CLI.
package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameCodex, NewCodexAgent)
}

// CodexAgent implements the Agent interface for OpenAI's Codex CLI.
//
//nolint:revive // CodexAgent is clearer than Agent in this context
type CodexAgent struct {
	CommandRunner agent.TextCommandRunner
	// RolloutRoots overrides the active and archived rollout roots for callers
	// that already know them (notably tests). Nil uses Codex's normal home.
	RolloutRoots []string
	// loadRollout and walkDir are package-private deterministic test seams.
	// Production always uses regular-file, same-descriptor loading and
	// filepath.WalkDir respectively.
	loadRollout func(string) (loadedRollout, error)
	walkDir     func(string, fs.WalkDirFunc) error
}

type loadedRollout struct {
	Path string
	Data []byte
}

func rolloutRegularMode(mode fs.FileMode) bool {
	return mode.Type() == 0
}

func readRegularRollout(path string) (loadedRollout, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return loadedRollout{}, fmt.Errorf("lstat rollout: %w", err)
	}
	if !rolloutRegularMode(info.Mode()) {
		return loadedRollout{}, errors.New("rollout is not a regular file")
	}
	file, err := os.Open(path) //nolint:gosec // Lstat above rejects known special entries; Stat below verifies the opened descriptor.
	if err != nil {
		return loadedRollout{}, fmt.Errorf("open rollout: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return loadedRollout{}, fmt.Errorf("stat opened rollout: %w", err)
	}
	if !rolloutRegularMode(opened.Mode()) || !os.SameFile(info, opened) {
		return loadedRollout{}, errors.New("rollout changed or is not a regular file")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return loadedRollout{}, fmt.Errorf("read rollout: %w", err)
	}
	return loadedRollout{Path: path, Data: data}, nil
}

func (c *CodexAgent) loadCandidateRollout(path string) (loadedRollout, error) {
	if c.loadRollout != nil {
		return c.loadRollout(path)
	}
	return readRegularRollout(path)
}

func (c *CodexAgent) loadVerifiedRollout(path, agentID string) (loadedRollout, bool) {
	loaded, err := c.loadCandidateRollout(path)
	if err != nil {
		return loadedRollout{}, false
	}
	if loaded.Path == "" {
		loaded.Path = path
	}
	if loaded.Path != path {
		return loadedRollout{}, false
	}
	id, err := sessionMetaID(loaded.Data)
	if err != nil || id != agentID {
		return loadedRollout{}, false
	}
	return loaded, true
}

func (c *CodexAgent) rolloutRoots() []string {
	if c.RolloutRoots != nil {
		return c.RolloutRoots
	}
	sessionDir, err := c.GetSessionDir("")
	if err != nil {
		return nil
	}
	codexHome, err := resolveCodexHome()
	if err != nil {
		return []string{sessionDir}
	}
	return []string{sessionDir, filepath.Join(codexHome, "archived_sessions")}
}

func (c *CodexAgent) loadDirectRollout(ref agent.SubagentReference) (loadedRollout, bool) {
	for _, path := range []string{ref.DeclaredTranscriptPath, ref.ResolvedTranscriptPath} {
		if path == "" {
			continue
		}
		if loaded, ok := c.loadVerifiedRollout(path, ref.AgentID); ok {
			return loaded, true
		}
	}
	return loadedRollout{}, false
}

func (c *CodexAgent) walkRollouts(root string, visit fs.WalkDirFunc) error {
	if c.walkDir != nil {
		return c.walkDir(root, visit)
	}
	if err := filepath.WalkDir(root, visit); err != nil {
		return fmt.Errorf("walk Codex rollouts: %w", err)
	}
	return nil
}

// scanFallbackRollouts scans every configured root once. Any traversal or
// regular-candidate metadata failure discards all results: partial results
// cannot prove a child ID is unique.
func (c *CodexAgent) scanFallbackRollouts(agentIDs map[string]struct{}) map[string]loadedRollout {
	if len(agentIDs) == 0 {
		return map[string]loadedRollout{}
	}
	matches := make(map[string][]loadedRollout)
	seenPaths := make(map[string]struct{})
	for _, root := range c.rolloutRoots() {
		if root == "" {
			continue
		}
		walkErr := c.walkRollouts(root, func(path string, entry fs.DirEntry, entryErr error) error {
			if entryErr != nil {
				if path == root && errors.Is(entryErr, fs.ErrNotExist) {
					return nil // Missing configured roots are normal.
				}
				return fmt.Errorf("walk rollout candidate: %w", entryErr)
			}
			if entry.IsDir() || filepath.Ext(path) != ".jsonl" {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("stat rollout candidate: %w", err)
			}
			if !rolloutRegularMode(info.Mode()) {
				return nil
			}
			loaded, err := c.loadCandidateRollout(path)
			if err != nil {
				return fmt.Errorf("load rollout candidate: %w", err)
			}
			if loaded.Path == "" {
				loaded.Path = path
			}
			if loaded.Path != path {
				return errors.New("rollout loader returned a different path")
			}
			id, err := sessionMetaID(loaded.Data)
			if err != nil {
				return fmt.Errorf("read rollout metadata: %w", err)
			}
			if _, wanted := agentIDs[id]; !wanted {
				return nil
			}
			if _, duplicate := seenPaths[path]; !duplicate {
				seenPaths[path] = struct{}{}
				matches[id] = append(matches[id], loaded)
			}
			return nil
		})
		if walkErr != nil {
			return nil
		}
	}
	resolved := make(map[string]loadedRollout)
	for id, candidates := range matches {
		if len(candidates) == 1 {
			resolved[id] = candidates[0]
		}
	}
	return resolved
}

// resolveSubagentRollout is the path-only compatibility wrapper used by
// callers that need only discovery. Inventory extraction uses the verified
// bytes returned by the same load operation instead.
func (c *CodexAgent) resolveSubagentRollout(ref agent.SubagentReference) string {
	if ref.AgentID == "" {
		return ""
	}
	if loaded, ok := c.loadDirectRollout(ref); ok {
		return loaded.Path
	}
	if loaded, ok := c.scanFallbackRollouts(map[string]struct{}{ref.AgentID: {}})[ref.AgentID]; ok {
		return loaded.Path
	}
	return ""
}

// NewCodexAgent creates a new Codex agent instance.
func NewCodexAgent() agent.Agent {
	return &CodexAgent{}
}

// Name returns the agent registry key.
func (c *CodexAgent) Name() types.AgentName {
	return agent.AgentNameCodex
}

// Type returns the agent type identifier.
func (c *CodexAgent) Type() types.AgentType {
	return agent.AgentTypeCodex
}

// Description returns a human-readable description.
func (c *CodexAgent) Description() string {
	return "Codex - OpenAI's CLI coding agent"
}

// IsPreview returns true because this is a new integration.
func (c *CodexAgent) IsPreview() bool { return true }

// DetectPresence checks if Codex is configured in the repository.
func (c *CodexAgent) DetectPresence(ctx context.Context) (bool, error) {
	return c.AreHooksInstalled(ctx)
}

// GetSessionID extracts the session ID from hook input.
func (c *CodexAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// resolveCodexHome returns the Codex home directory (CODEX_HOME or ~/.codex).
func resolveCodexHome() (string, error) {
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return codexHome, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".codex"), nil
}

// GetSessionDir returns the directory where Codex stores session transcripts.
// Codex stores transcripts under CODEX_HOME/sessions/YYYY/MM/DD/.
func (c *CodexAgent) GetSessionDir(_ string) (string, error) {
	if override := os.Getenv("ENTIRE_TEST_CODEX_SESSION_DIR"); override != "" {
		return override, nil
	}
	codexHome, err := resolveCodexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(codexHome, "sessions"), nil
}

// ResolveSessionFile returns the path to a Codex session transcript file.
// Codex provides the transcript path directly in hook payloads as an absolute path.
// When only a session ID is available, callers recover it from the
// sessions/YYYY/MM/DD/rollout-...-<session-id>.jsonl layout.
func (c *CodexAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	if filepath.IsAbs(agentSessionID) {
		return agentSessionID
	}
	if path := findRolloutBySessionID(sessionDir, agentSessionID); path != "" {
		return path
	}
	if sessionDir != "" {
		return filepath.Join(sessionDir, agentSessionID+".jsonl")
	}
	return agentSessionID
}

// ResolveRestoredSessionFile returns the canonical Codex rollout path for a
// restored session so `codex resume <id>` can rediscover it.
func (c *CodexAgent) ResolveRestoredSessionFile(sessionDir, agentSessionID string, transcript []byte) (string, error) {
	if err := validation.ValidateAgentSessionID(agentSessionID); err != nil {
		return "", fmt.Errorf("validate agent session ID: %w", err)
	}
	startTime, err := parseSessionStartTime(transcript)
	if err != nil {
		return "", fmt.Errorf("parse session start time: %w", err)
	}
	return restoredRolloutPath(sessionDir, agentSessionID, startTime), nil
}

// ProtectedDirs returns directories that Codex uses for config/state.
func (c *CodexAgent) ProtectedDirs() []string { return []string{".codex"} }

// ReadSession reads a session from Codex's storage (JSONL rollout file).
func (c *CodexAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	startTime, err := parseSessionStartTime(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse session start time: %w", err)
	}

	// Extract modified files from the rollout transcript (best-effort, deduplicated).
	var modifiedFiles []string
	seen := make(map[string]struct{})
	for _, lineData := range splitJSONL(data) {
		for _, f := range extractFilesFromLine(lineData) {
			if _, exists := seen[f]; !exists {
				seen[f] = struct{}{}
				modifiedFiles = append(modifiedFiles, f)
			}
		}
	}

	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     c.Name(),
		SessionRef:    input.SessionRef,
		StartTime:     startTime,
		NativeData:    data,
		ModifiedFiles: modifiedFiles,
	}, nil
}

// WriteSession writes a session to Codex's storage (JSONL rollout file).
func (c *CodexAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}

	if session.AgentName != "" && session.AgentName != c.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, c.Name())
	}

	if session.SessionRef == "" {
		return errors.New("session reference (transcript path) is required")
	}

	if len(session.NativeData) == 0 {
		return errors.New("session has no native data to write")
	}

	dataToWrite := SanitizePortableTranscript(session.NativeData)
	if err := os.WriteFile(session.SessionRef, dataToWrite, 0o600); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}

	return nil
}

// FormatResumeCommand returns the command to resume a Codex session.
func (c *CodexAgent) FormatResumeCommand(sessionID string) string {
	return "codex resume " + sessionID
}

// ReadTranscript reads the raw JSONL transcript bytes for a session.
func (c *CodexAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}
	return data, nil
}

// ChunkTranscript splits a JSONL transcript at line boundaries.
func (c *CodexAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk JSONL transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks with newlines.
func (c *CodexAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

func restoredRolloutPath(codexHome, agentSessionID string, startTime time.Time) string {
	timestamp := startTime.UTC()
	datePath := filepath.Join(
		codexHome,
		timestamp.Format("2006"),
		timestamp.Format("01"),
		timestamp.Format("02"),
	)
	filename := fmt.Sprintf("rollout-%s-%s.jsonl", timestamp.Format("2006-01-02T15-04-05"), agentSessionID)
	return filepath.Join(datePath, filename)
}

// LaunchCmd builds an exec.Cmd for `codex "<initialPrompt>"`. Stdio is wired
// to the caller's TTY so the agent runs foreground and the user interacts
// normally. The call site is expected to Run() and wait. Hooks inherit the
// parent environment.
func (c *CodexAgent) LaunchCmd(ctx context.Context, initialPrompt string) (*exec.Cmd, error) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return nil, fmt.Errorf("codex binary not on PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, initialPrompt)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd, nil
}

func findRolloutBySessionID(codexHome, agentSessionID string) string {
	if codexHome == "" || validation.ValidateAgentSessionID(agentSessionID) != nil {
		return ""
	}

	patterns := []string{
		filepath.Join(codexHome, "rollout-*-"+agentSessionID+".jsonl"),
		filepath.Join(codexHome, "*", "*", "*", "rollout-*-"+agentSessionID+".jsonl"),
		filepath.Join(filepath.Dir(codexHome), "archived_sessions", "*", "*", "*", "rollout-*-"+agentSessionID+".jsonl"),
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			continue
		}
		// Multiple restored rollouts for the same session ID can exist. Return the
		// lexicographically latest path so newer dated restores win deterministically.
		sort.Strings(matches)
		return matches[len(matches)-1]
	}

	return ""
}

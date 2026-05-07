// Package claudecode implements the Agent interface for Claude Code.
package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameClaudeCode, NewClaudeCodeAgent)
}

// ClaudeCodeAgent implements the Agent interface for Claude Code.
//
//nolint:revive // ClaudeCodeAgent is clearer than Agent in this context
type ClaudeCodeAgent struct {
	CommandRunner func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// NewClaudeCodeAgent creates a new Claude Code agent instance.
func NewClaudeCodeAgent() agent.Agent {
	return &ClaudeCodeAgent{CommandRunner: exec.CommandContext}
}

// Name returns the agent registry key.
func (c *ClaudeCodeAgent) Name() types.AgentName {
	return agent.AgentNameClaudeCode
}

// Type returns the agent type identifier.
func (c *ClaudeCodeAgent) Type() types.AgentType {
	return agent.AgentTypeClaudeCode
}

// Description returns a human-readable description.
func (c *ClaudeCodeAgent) Description() string {
	return "Claude Code - Anthropic's CLI coding assistant"
}

func (c *ClaudeCodeAgent) IsPreview() bool { return false }

// DetectPresence checks if Claude Code is configured in the repository.
func (c *ClaudeCodeAgent) DetectPresence(ctx context.Context) (bool, error) {
	// Get worktree root to check for .claude directory
	// This is needed because the CLI may be run from a subdirectory
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Not in a git repo, fall back to CWD-relative check
		repoRoot = "."
	}

	// Check for .claude directory
	claudeDir := filepath.Join(repoRoot, ".claude")
	if _, err := os.Stat(claudeDir); err == nil {
		return true, nil
	}
	// Check for .claude/settings.json
	settingsFile := filepath.Join(repoRoot, ".claude", "settings.json")
	if _, err := os.Stat(settingsFile); err == nil {
		return true, nil
	}
	return false, nil
}

// GetSessionID extracts the session ID from hook input.
func (c *ClaudeCodeAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// ResolveSessionFile returns the path to a Claude session file.
// Claude names files directly as <id>.jsonl.
func (c *ClaudeCodeAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

// ProtectedDirs returns directories that Claude uses for config/state.
func (c *ClaudeCodeAgent) ProtectedDirs() []string { return []string{".claude"} }

// GetSessionDir returns the directory where Claude stores session transcripts.
func (c *ClaudeCodeAgent) GetSessionDir(repoPath string) (string, error) {
	// Check for test environment override
	if override := os.Getenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR"); override != "" {
		return override, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	projectDir := SanitizePathForClaude(repoPath)
	return filepath.Join(homeDir, ".claude", "projects", projectDir), nil
}

// GetSessionBaseDir returns the base directory containing per-project session subdirectories.
// Unlike GetSessionDir, this does NOT use ENTIRE_TEST_CLAUDE_PROJECT_DIR because the
// test override points to a specific project dir, not the base containing all projects.
func (c *ClaudeCodeAgent) GetSessionBaseDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".claude", "projects"), nil
}

// ReadSession reads a session from Claude's storage (JSONL transcript file).
// The session data is stored in NativeData as raw JSONL bytes.
// ModifiedFiles is computed by parsing the transcript.
func (c *ClaudeCodeAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	// Read the raw JSONL file
	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	// Parse to extract computed fields
	lines, err := transcript.ParseFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}

	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     c.Name(),
		SessionRef:    input.SessionRef,
		StartTime:     time.Now(),
		NativeData:    data,
		ModifiedFiles: ExtractModifiedFiles(lines),
	}, nil
}

// WriteSession writes a session to Claude's storage (JSONL transcript file).
// Uses the NativeData field which contains raw JSONL bytes.
// The session must have been created by Claude Code (AgentName check).
func (c *ClaudeCodeAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}

	// Verify this session belongs to Claude Code
	if session.AgentName != "" && session.AgentName != c.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, c.Name())
	}

	if session.SessionRef == "" {
		return errors.New("session reference (transcript path) is required")
	}

	if len(session.NativeData) == 0 {
		return errors.New("session has no native data to write")
	}

	// Write the raw JSONL data
	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}

	return nil
}

// FormatResumeCommand returns the command to resume a Claude Code session.
func (c *ClaudeCodeAgent) FormatResumeCommand(sessionID string) string {
	return "claude -r " + sessionID
}

// Session helper methods - work on AgentSession with Claude's native JSONL data

// TruncateAtUUID returns a new session truncated at the given UUID (inclusive).
// This is used for rewind operations to restore transcript state.
// Requires NativeData to be populated.
func (c *ClaudeCodeAgent) TruncateAtUUID(session *agent.AgentSession, uuid string) (*agent.AgentSession, error) {
	if session == nil {
		return nil, errors.New("session is nil")
	}

	if len(session.NativeData) == 0 {
		return nil, errors.New("session has no native data")
	}

	if uuid == "" {
		// No truncation needed, return copy
		return &agent.AgentSession{
			SessionID:     session.SessionID,
			AgentName:     session.AgentName,
			RepoPath:      session.RepoPath,
			SessionRef:    session.SessionRef,
			StartTime:     session.StartTime,
			NativeData:    session.NativeData,
			ModifiedFiles: session.ModifiedFiles,
		}, nil
	}

	// Parse, truncate, re-serialize
	lines, err := transcript.ParseFromBytes(session.NativeData)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transcript: %w", err)
	}

	truncated := TruncateAtUUID(lines, uuid)

	newData, err := SerializeTranscript(truncated)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize truncated transcript: %w", err)
	}

	return &agent.AgentSession{
		SessionID:     session.SessionID,
		AgentName:     session.AgentName,
		RepoPath:      session.RepoPath,
		SessionRef:    session.SessionRef,
		StartTime:     session.StartTime,
		NativeData:    newData,
		ModifiedFiles: ExtractModifiedFiles(truncated),
	}, nil
}

// FindCheckpointUUID finds the UUID of the message containing the tool_result
// for the given tool use ID. Used for task checkpoint rewind.
// Returns the UUID and true if found, empty string and false otherwise.
func (c *ClaudeCodeAgent) FindCheckpointUUID(session *agent.AgentSession, toolUseID string) (string, bool) {
	if session == nil || len(session.NativeData) == 0 {
		return "", false
	}

	lines, err := transcript.ParseFromBytes(session.NativeData)
	if err != nil {
		return "", false
	}

	return FindCheckpointUUID(lines, toolUseID)
}

// ReadSessionFromPath is a convenience method that reads a session directly from a file path.
// This is useful when you have the path but not a HookInput.
func (c *ClaudeCodeAgent) ReadSessionFromPath(transcriptPath, sessionID string) (*agent.AgentSession, error) {
	return c.ReadSession(&agent.HookInput{
		SessionID:  sessionID,
		SessionRef: transcriptPath,
	})
}

// SanitizePathForClaude converts a path to Claude's project directory format.
// Claude replaces any non-alphanumeric character with a dash.
var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9]`)

func SanitizePathForClaude(path string) string {
	return nonAlphanumericRegex.ReplaceAllString(path, "-")
}

// TranscriptAnalyzer interface implementation

// GetTranscriptPosition returns the current line count of a Claude Code transcript.
// Claude Code uses JSONL format, so position is the number of lines.
// This is a lightweight operation that only counts lines without parsing JSON.
// Uses bufio.Reader to handle arbitrarily long lines (no size limit).
// Returns 0 if the file doesn't exist or is empty.
func (c *ClaudeCodeAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}

	file, err := os.Open(path) //nolint:gosec // Path comes from Claude Code transcript location
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to open transcript file: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineCount := 0

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				if len(line) > 0 {
					lineCount++ // Count final line without trailing newline
				}
				break
			}
			return 0, fmt.Errorf("failed to read transcript: %w", err)
		}
		lineCount++
	}

	return lineCount, nil
}

// ExtractModifiedFilesFromOffset extracts files modified since a given line number.
// For Claude Code (JSONL format), offset is the starting line number.
// Uses bufio.Reader to handle arbitrarily long lines (no size limit).
// Returns:
//   - files: list of file paths modified by Claude (from Write/Edit tools)
//   - currentPosition: total number of lines in the file
//   - error: any error encountered during reading
func (c *ClaudeCodeAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error) {
	if path == "" {
		return nil, 0, nil
	}

	file, openErr := os.Open(path) //nolint:gosec // Path comes from Claude Code transcript location
	if openErr != nil {
		return nil, 0, fmt.Errorf("failed to open transcript file: %w", openErr)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var lines []TranscriptLine
	lineNum := 0

	for {
		lineData, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, 0, fmt.Errorf("failed to read transcript: %w", readErr)
		}

		if len(lineData) > 0 {
			lineNum++
			if lineNum > startOffset {
				var line TranscriptLine
				if parseErr := json.Unmarshal(lineData, &line); parseErr == nil {
					lines = append(lines, line)
				}
				// Skip malformed lines silently
			}
		}

		if readErr == io.EOF {
			break
		}
	}

	return ExtractModifiedFiles(lines), lineNum, nil
}

// ChunkTranscript splits a JSONL transcript at line boundaries.
// Claude Code uses JSONL format (one JSON object per line), so chunking
// is done at newline boundaries to preserve message integrity.
func (c *ClaudeCodeAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk JSONL transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks with newlines.
//

func (c *ClaudeCodeAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

// LaunchCmd builds an exec.Cmd for `claude "<initialPrompt>"`. Stdio is wired
// to the caller's TTY so the agent runs foreground and the user interacts
// normally. The call site is expected to Run() and wait. Hooks inherit the
// parent environment.
func (c *ClaudeCodeAgent) LaunchCmd(ctx context.Context, initialPrompt string) (*exec.Cmd, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("claude binary not on PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, initialPrompt)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd, nil
}

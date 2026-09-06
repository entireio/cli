// Package cursor implements the Agent interface for Cursor.
package cursor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// Compile-time interface assertion.
var _ agent.TranscriptPreparer = (*CursorAgent)(nil)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameCursor, NewCursorAgent)
}

// CursorAgent implements the Agent interface for Cursor.
//
//nolint:revive // CursorAgent is clearer than Agent in this context
type CursorAgent struct {
	CommandRunner agent.TextCommandRunner
}

// NewCursorAgent creates a new Cursor agent instance.
func NewCursorAgent() agent.Agent {
	return &CursorAgent{}
}

// Name returns the agent registry key.
func (c *CursorAgent) Name() types.AgentName {
	return agent.AgentNameCursor
}

// Type returns the agent type identifier.
func (c *CursorAgent) Type() types.AgentType {
	return agent.AgentTypeCursor
}

// Description returns a human-readable description.
func (c *CursorAgent) Description() string {
	return "Cursor - AI-powered code editor"
}

func (c *CursorAgent) IsPreview() bool { return true }

// DetectPresence checks whether Cursor is used for this repository.
//
// Two signals, because a repo-local check alone is structurally biased against
// Cursor. Every other agent keeps its per-project config in the repo (.claude,
// .codex, .gemini), so "does <repo>/.<agent> exist" is a fair test for them.
// Cursor does not: project rules are optional, and its actual per-project state
// lives in ~/.cursor/projects/<sanitized-repo-path>. A repo driven daily from
// Cursor therefore reported "not present", and since `entire enable` enables
// only detected agents when non-interactive and pre-selects only detected ones
// on first run (see selectAgents in setup.go), Cursor silently ended up with no
// hooks installed while enable still reported success.
//
// The home-dir signal deliberately accepts a false positive — a repo opened in
// Cursor once and never used with its agent — because that installs hooks that
// simply never fire, whereas a false negative captures nothing and says nothing.
func (c *CursorAgent) DetectPresence(ctx context.Context) (bool, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		worktreeRoot = "."
	}

	cursorDir := filepath.Join(worktreeRoot, ".cursor")
	if _, err := os.Stat(cursorDir); err == nil {
		return true, nil
	}

	// Cursor's own per-project state directory, keyed by the repo path.
	projectDir, err := c.projectStateDir(worktreeRoot)
	if err != nil {
		// No resolvable home dir is not an error here, just no second signal.
		return false, nil //nolint:nilerr // detection is best-effort
	}
	if info, err := os.Stat(projectDir); err == nil && info.IsDir() {
		return true, nil
	}
	return false, nil
}

// GetSessionID extracts the session ID from hook input.
func (c *CursorAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// ResolveSessionFile returns the path to a Cursor session file.
// Cursor IDE uses a nested layout: <dir>/<id>/<id>.jsonl
// Cursor CLI uses a flat layout: <dir>/<id>.jsonl
// We prefer nested if the file OR directory exists (the directory may be created
// before the file is flushed), otherwise fall back to flat.
func (c *CursorAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	nestedDir := filepath.Join(sessionDir, agentSessionID)
	nested := filepath.Join(nestedDir, agentSessionID+".jsonl")
	if _, err := os.Stat(nested); err == nil {
		return nested
	}
	// IDE creates the directory before the transcript file — predict nested path.
	if info, err := os.Stat(nestedDir); err == nil && info.IsDir() {
		return nested
	}
	return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

// ProtectedDirs returns directories that Cursor uses for config/state.
func (c *CursorAgent) ProtectedDirs() []string { return []string{".cursor"} }

// GetSessionDir returns the directory where Cursor stores session transcripts.
func (c *CursorAgent) GetSessionDir(repoPath string) (string, error) {
	if override := os.Getenv("ENTIRE_TEST_CURSOR_PROJECT_DIR"); override != "" {
		return override, nil
	}

	projectDir, err := c.projectStateDir(repoPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(projectDir, "agent-transcripts"), nil
}

// projectStateDir returns Cursor's per-project state directory for repoPath:
// ~/.cursor/projects/<sanitized-repo-path>. Both GetSessionDir (which appends
// agent-transcripts) and DetectPresence resolve it through here so the two can
// never disagree about where Cursor keeps a repo's state.
func (c *CursorAgent) projectStateDir(repoPath string) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".cursor", "projects", sanitizePathForCursor(repoPath)), nil
}

// GetSessionBaseDir returns the base directory containing per-project session subdirectories.
// Unlike GetSessionDir, this does NOT use test overrides because the override
// points to a specific project dir, not the base containing all projects.
func (c *CursorAgent) GetSessionBaseDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".cursor", "projects"), nil
}

// ReadSession reads a session from Cursor's storage (JSONL transcript file).
// ModifiedFiles is populated from the transcript's tool_use blocks; see
// ExtractModifiedFiles. Git status remains the broader signal, since Cursor can
// also change files through Shell commands that record no path.
func (c *CursorAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

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

// PrepareTranscript waits for Cursor's transcript file to be flushed to disk.
// Cursor writes transcripts asynchronously; during mid-turn commits the file
// may not yet contain data. This polls until the file exists and is non-empty,
// or until the timeout expires.
func (c *CursorAgent) PrepareTranscript(ctx context.Context, sessionRef string) error {
	const (
		maxWait      = 5 * time.Second
		pollInterval = 50 * time.Millisecond
	)

	logCtx := logging.WithComponent(ctx, "agent.cursor")

	start := time.Now()
	deadline := start.Add(maxWait)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	effectiveTimeout := deadline.Sub(start)

	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("context ended while waiting for transcript: %w", err)
		}

		info, err := os.Stat(sessionRef)
		if err == nil {
			if info.Size() > 0 {
				logging.Debug(logCtx, "transcript file ready",
					slog.Int64("size", info.Size()),
				)
				return nil
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("failed to stat transcript %q: %w", sessionRef, err)
		}

		wait := pollInterval
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("context ended while waiting for transcript: %w", ctx.Err())
		case <-timer.C:
		}
	}

	logging.Warn(logCtx, "transcript file not ready within timeout, proceeding",
		slog.Duration("timeout", effectiveTimeout),
		slog.String("path", sessionRef),
	)
	return nil
}

// WriteSession writes a session to Cursor's storage (JSONL transcript file).
func (c *CursorAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
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

	if err := agent.WriteSessionFile(c, session, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("write transcript: %w", err)
	}

	return nil
}

// FormatResumeCommand returns an instruction to resume a Cursor session.
// Cursor is a GUI IDE, so there's no CLI command to resume a session directly.
func (c *CursorAgent) FormatResumeCommand(_ string) string {
	return "Open this project in Cursor."
}

// sanitizePathForCursor converts a path to Cursor's project directory format.
var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9]`)

func sanitizePathForCursor(path string) string {
	path = strings.TrimLeft(path, "/")
	return nonAlphanumericRegex.ReplaceAllString(path, "-")
}

// ChunkTranscript splits a JSONL transcript at line boundaries.
func (c *CursorAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk JSONL transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks with newlines.
func (c *CursorAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

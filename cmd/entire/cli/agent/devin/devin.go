// Package devin implements the Agent interface for Devin CLI ("Devin for
// Terminal", Cognition). Devin uses Claude Code-format lifecycle hooks
// installed in .devin/hooks.v1.json and stores its canonical transcript as an
// ATIF JSON document keyed by session ID. See AGENT.md for the verified
// behavior this integration is built on.
package devin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameDevin, NewDevinAgent)
}

// ProjectDirEnv is set by Devin CLI in the environment of every hook process
// it spawns (the Devin analog of CLAUDE_PROJECT_DIR). Real Claude Code never
// sets it, which makes it a reliable signal that a claude-code hook was
// cross-fired by Devin's .claude/settings.json compatibility loading.
const ProjectDirEnv = "DEVIN_PROJECT_DIR"

// DevinAgent implements the Agent interface for Devin CLI.
//
//nolint:revive // DevinAgent is clearer than Agent in this context
type DevinAgent struct{}

// NewDevinAgent creates a new Devin agent instance.
func NewDevinAgent() agent.Agent {
	return &DevinAgent{}
}

// Name returns the agent registry key.
func (d *DevinAgent) Name() types.AgentName {
	return agent.AgentNameDevin
}

// Type returns the agent type identifier.
func (d *DevinAgent) Type() types.AgentType {
	return agent.AgentTypeDevin
}

// Description returns a human-readable description.
func (d *DevinAgent) Description() string {
	return "Devin CLI - Cognition's terminal coding agent"
}

func (d *DevinAgent) IsPreview() bool { return true }

// DetectPresence checks if Devin is configured in the repository.
func (d *DevinAgent) DetectPresence(ctx context.Context) (bool, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".devin")); err == nil {
		return true, nil
	}
	return false, nil
}

// ProtectedDirs returns directories that Devin uses for config/state.
func (d *DevinAgent) ProtectedDirs() []string { return []string{".devin"} }

// GetSessionID extracts the session ID from hook input.
func (d *DevinAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// GetSessionDir returns the directory where Devin stores session transcripts.
// Devin's transcript directory is flat (one <session_id>.json per session,
// not per-project), so repoPath is unused.
func (d *DevinAgent) GetSessionDir(_ string) (string, error) {
	if override := os.Getenv("ENTIRE_TEST_DEVIN_TRANSCRIPT_DIR"); override != "" {
		return override, nil
	}
	dataDir, err := devinDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, "cli", "transcripts"), nil
}

// devinDataDir returns Devin's per-user data directory
// (~/.local/share/devin on Unix, %APPDATA%\devin on Windows).
func devinDataDir() (string, error) {
	if runtime.GOOS == "windows" {
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", errors.New("APPDATA environment variable not set")
		}
		return filepath.Join(appData, "devin"), nil
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "devin"), nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".local", "share", "devin"), nil
}

// ResolveSessionFile returns the path to a Devin transcript file.
// Devin names transcripts directly as <session_id>.json.
func (d *DevinAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".json")
}

// ReadSession reads a session from Devin's storage (ATIF transcript file).
func (d *DevinAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	modifiedFiles, err := ExtractModifiedFiles(data)
	if err != nil {
		// Non-fatal: the session is still usable without the file list.
		modifiedFiles = nil
	}

	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     d.Name(),
		SessionRef:    input.SessionRef,
		NativeData:    data,
		ModifiedFiles: modifiedFiles,
	}, nil
}

// WriteSession writes a session back to Devin's transcript location.
//
// Limitation (see AGENT.md): Devin resumes conversations from its SQLite
// store, not this file, so restoring the transcript preserves it for
// explain/analysis but does not rewrite Devin's own conversation memory.
func (d *DevinAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}
	if session.AgentName != "" && session.AgentName != d.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, d.Name())
	}
	if session.SessionRef == "" {
		return errors.New("session reference (transcript path) is required")
	}
	if len(session.NativeData) == 0 {
		return errors.New("session has no native data to write")
	}

	if err := os.MkdirAll(filepath.Dir(session.SessionRef), 0o750); err != nil {
		return fmt.Errorf("failed to create transcript directory: %w", err)
	}
	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}
	return nil
}

// FormatResumeCommand returns the command to resume a Devin session.
func (d *DevinAgent) FormatResumeCommand(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return "devin -c"
	}
	return "devin -r " + sessionID
}

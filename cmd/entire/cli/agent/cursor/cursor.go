// Package cursor implements the Agent interface for Cursor IDE.
// Cursor uses .cursor/hooks.json for hooks and does not use .claude/settings.json.
package cursor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/sessionid"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameCursor, NewCursorAgent)
}

// CursorAgent implements the Agent interface for Cursor IDE.
//
//nolint:revive // CursorAgent is clearer than Agent in this context
type CursorAgent struct{}

// NewCursorAgent creates a new Cursor agent instance.
func NewCursorAgent() agent.Agent {
	return &CursorAgent{}
}

// Name returns the agent registry key.
func (c *CursorAgent) Name() agent.AgentName {
	return agent.AgentNameCursor
}

// Type returns the agent type identifier.
func (c *CursorAgent) Type() agent.AgentType {
	return agent.AgentTypeCursor
}

// Description returns a human-readable description.
func (c *CursorAgent) Description() string {
	return "Cursor - AI-powered code editor"
}

// DetectPresence checks if Cursor is configured in the repository.
// Only checks for .cursor/ directory or .cursor/hooks.json; does not touch .claude/*.
func (c *CursorAgent) DetectPresence() (bool, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}
	cursorDir := filepath.Join(repoRoot, ".cursor")
	if _, err := os.Stat(cursorDir); err == nil {
		return true, nil
	}
	hooksPath := filepath.Join(repoRoot, ".cursor", "hooks.json")
	if _, err := os.Stat(hooksPath); err == nil {
		return true, nil
	}
	return false, nil
}

// GetHookConfigPath returns the path to Cursor's hook config file.
func (c *CursorAgent) GetHookConfigPath() string {
	return ".cursor/hooks.json"
}

// SupportsHooks returns true as Cursor supports lifecycle hooks.
func (c *CursorAgent) SupportsHooks() bool {
	return true
}

// ParseHookInput parses Cursor hook input from stdin.
func (c *CursorAgent) ParseHookInput(hookType agent.HookType, reader io.Reader) (*agent.HookInput, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("empty input")
	}

	input := &agent.HookInput{
		HookType:  hookType,
		Timestamp: time.Now(),
		RawData:   make(map[string]interface{}),
	}

	sessionID := func(sid, cid string) string {
		if sid != "" {
			return sid
		}
		return cid
	}

	switch hookType {
	case agent.HookUserPromptSubmit:
		var raw userPromptSubmitRaw
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("failed to parse beforeSubmitPrompt: %w", err)
		}
		input.SessionID = sessionID(raw.SessionID, raw.ConversationID)
		input.SessionRef = raw.TranscriptPath
		input.UserPrompt = raw.Prompt

	case agent.HookSessionStart, agent.HookSessionEnd, agent.HookStop:
		var raw sessionInfoRaw
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("failed to parse session info: %w", err)
		}
		input.SessionID = sessionID(raw.SessionID, raw.ConversationID)
		input.SessionRef = raw.TranscriptPath

	case agent.HookPreToolUse:
		var raw taskHookInputRaw
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("failed to parse preToolUse: %w", err)
		}
		input.SessionID = sessionID(raw.SessionID, raw.ConversationID)
		input.SessionRef = raw.TranscriptPath
		input.ToolUseID = raw.ToolUseID
		input.ToolInput = raw.ToolInput

	case agent.HookPostToolUse:
		var raw postToolHookInputRaw
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("failed to parse postToolUse: %w", err)
		}
		input.SessionID = sessionID(raw.SessionID, raw.ConversationID)
		input.SessionRef = raw.TranscriptPath
		input.ToolUseID = raw.ToolUseID
		input.ToolInput = raw.ToolInput
		if raw.ToolResponse.AgentID != "" {
			input.RawData["agent_id"] = raw.ToolResponse.AgentID
		}
		if raw.ToolName != "" {
			input.RawData["tool_name"] = raw.ToolName
		}
	}

	return input, nil
}

// GetSessionID extracts the session ID from hook input.
func (c *CursorAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// TransformSessionID converts a Cursor session ID to an Entire session ID.
func (c *CursorAgent) TransformSessionID(agentSessionID string) string {
	return agentSessionID
}

// ExtractAgentSessionID extracts the Cursor session ID from an Entire session ID.
func (c *CursorAgent) ExtractAgentSessionID(entireSessionID string) string {
	return sessionid.ModelSessionID(entireSessionID)
}

// ProtectedDirs returns directories that Cursor uses; does not include .claude.
func (c *CursorAgent) ProtectedDirs() []string {
	return []string{".cursor"}
}

// GetSessionDir returns where Cursor stores session data for this repo.
// Uses ENTIRE_TEST_CURSOR_PROJECT_DIR in tests. Otherwise uses a placeholder path
// until Cursor transcript storage is confirmed (e.g. macOS: ~/Library/Application Support/Cursor/...).
func (c *CursorAgent) GetSessionDir(repoPath string) (string, error) {
	if override := os.Getenv("ENTIRE_TEST_CURSOR_PROJECT_DIR"); override != "" {
		return override, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	// Placeholder: Cursor storage path TBD from discovery. Use a sanitized repo path.
	projectDir := SanitizePathForCursor(repoPath)
	return filepath.Join(homeDir, ".cursor", "projects", projectDir), nil
}

// ResolveSessionFile returns the path to the session transcript file.
// Cursor format TBD; default to <sessionDir>/<agentSessionID>.jsonl for JSONL-style transcripts.
func (c *CursorAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

// ReadSession reads session data from Cursor storage.
// Stub: returns error until Cursor transcript path/format is confirmed.
func (c *CursorAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}
	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}
	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     c.Name(),
		SessionRef:    input.SessionRef,
		StartTime:     time.Now(),
		NativeData:    data,
		ModifiedFiles: nil, // Could parse transcript when format is known
	}, nil
}

// WriteSession writes session data for resumption.
// Stub: not implemented until Cursor supports resume.
func (c *CursorAgent) WriteSession(session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}
	// Cursor resume not yet supported
	return errors.New("Cursor WriteSession not implemented")
}

// FormatResumeCommand returns the command to resume a Cursor session.
func (c *CursorAgent) FormatResumeCommand(sessionID string) string {
	return "cursor --resume " + sessionID
}

// SanitizePathForCursor converts a path to a safe directory name for Cursor project storage.
var cursorNonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9]`)

func SanitizePathForCursor(path string) string {
	return cursorNonAlphanumericRegex.ReplaceAllString(path, "-")
}

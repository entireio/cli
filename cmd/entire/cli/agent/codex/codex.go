// Package codex implements the Agent interface for OpenAI's Codex CLI.
package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/sessionid"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameCodex, NewCodexAgent)
}

// CodexAgent implements the Agent interface for Codex CLI.
//
//nolint:revive // CodexAgent is clearer than Agent in this context
type CodexAgent struct{}

// NewCodexAgent creates a new Codex CLI agent instance.
func NewCodexAgent() agent.Agent {
	return &CodexAgent{}
}

// Name returns the agent registry key.
func (c *CodexAgent) Name() agent.AgentName {
	return agent.AgentNameCodex
}

// Type returns the agent type identifier.
func (c *CodexAgent) Type() agent.AgentType {
	return agent.AgentTypeCodex
}

// Description returns a human-readable description.
func (c *CodexAgent) Description() string {
	return "Codex CLI - OpenAI's AI coding assistant"
}

// DetectPresence checks if Codex CLI is configured in the repository.
func (c *CodexAgent) DetectPresence() (bool, error) {
	// Get repo root to check for .codex directory
	// This is needed because the CLI may be run from a subdirectory
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		// Not in a git repo, fall back to CWD-relative check
		repoRoot = "."
	}

	// Check for .codex directory (if it doesn't exist, config.toml inside it won't either)
	codexDir := filepath.Join(repoRoot, ".codex")
	if _, err := os.Stat(codexDir); err == nil {
		return true, nil
	}
	return false, nil
}

// GetHookConfigPath returns the path to Codex's hook config file.
func (c *CodexAgent) GetHookConfigPath() string {
	return ".codex/config.toml"
}

// SupportsHooks returns true as Codex CLI supports the notify hook.
func (c *CodexAgent) SupportsHooks() bool {
	return true
}

// ParseHookInput parses Codex CLI hook input from stdin.
// Codex sends a JSON payload to the notify command.
func (c *CodexAgent) ParseHookInput(hookType agent.HookType, reader io.Reader) (*agent.HookInput, error) {
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

	var payload notifyPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("failed to parse notify payload: %w", err)
	}

	input.SessionID = payload.ThreadID
	input.RawData["type"] = payload.Type
	input.RawData["turn_id"] = payload.TurnID
	input.RawData["cwd"] = payload.Cwd

	// Extract user prompt from input-messages
	if len(payload.InputMessages) > 0 {
		input.UserPrompt = payload.InputMessages[len(payload.InputMessages)-1]
	}

	// Resolve transcript path from session dir
	sessionDir, dirErr := c.getCodexHome()
	if dirErr == nil {
		input.SessionRef = filepath.Join(sessionDir, "sessions")
	}

	return input, nil
}

// GetSessionID extracts the session ID from hook input.
func (c *CodexAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// TransformSessionID converts a Codex session ID to an Entire session ID.
// This is an identity function - the agent session ID IS the Entire session ID.
func (c *CodexAgent) TransformSessionID(agentSessionID string) string {
	return agentSessionID
}

// ExtractAgentSessionID extracts the Codex session ID from an Entire session ID.
// Since Entire session ID = agent session ID (identity), this returns the input unchanged.
// For backwards compatibility with legacy date-prefixed IDs, it strips the prefix if present.
func (c *CodexAgent) ExtractAgentSessionID(entireSessionID string) string {
	return sessionid.ModelSessionID(entireSessionID)
}

// ProtectedDirs returns directories that Codex uses for config/state.
func (c *CodexAgent) ProtectedDirs() []string { return []string{".codex"} }

// GetSessionDir returns the directory where Codex stores session transcripts.
// Codex stores sessions in ~/.codex/sessions/ (respects CODEX_HOME).
func (c *CodexAgent) GetSessionDir(_ string) (string, error) {
	// Check for test environment override
	if override := os.Getenv("ENTIRE_TEST_CODEX_PROJECT_DIR"); override != "" {
		return override, nil
	}

	codexHome, err := c.getCodexHome()
	if err != nil {
		return "", err
	}

	return filepath.Join(codexHome, "sessions"), nil
}

// getCodexHome returns the Codex home directory.
// Respects CODEX_HOME env var, defaults to ~/.codex.
func (c *CodexAgent) getCodexHome() (string, error) {
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return codexHome, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	return filepath.Join(homeDir, ".codex"), nil
}

// ResolveSessionFile returns the path to a Codex session file.
// Codex stores sessions as rollout-*.jsonl in date-based directories (YYYY/MM/DD/).
// Searches recursively for an existing file matching the session ID, falls back to a default path.
func (c *CodexAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	// filepath.Glob does NOT support ** recursive matching.
	// Walk the directory tree to find rollout files containing the session ID.
	var found string
	//nolint:errcheck // WalkDir errors are non-fatal; fallback path handles missing dirs
	_ = filepath.WalkDir(sessionDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // Skip inaccessible directories
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".jsonl") && strings.Contains(name, agentSessionID) {
			found = path
			// Don't stop early — keep walking to find the latest (deepest) match
		}
		return nil
	})

	if found != "" {
		return found
	}

	// Fallback: construct a default path
	return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

// ReadSession reads a session from Codex's storage (JSONL rollout file).
// The session data is stored in NativeData as raw JSONL bytes.
func (c *CodexAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (file path) is required")
	}

	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read session file: %w", err)
	}

	return &agent.AgentSession{
		SessionID:  input.SessionID,
		AgentName:  c.Name(),
		SessionRef: input.SessionRef,
		NativeData: data,
	}, nil
}

// WriteSession writes a session to Codex's storage (JSONL rollout file).
func (c *CodexAgent) WriteSession(session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}

	if session.AgentName != "" && session.AgentName != c.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, c.Name())
	}

	if session.SessionRef == "" {
		return errors.New("session reference (file path) is required")
	}

	if len(session.NativeData) == 0 {
		return errors.New("session has no native data to write")
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(session.SessionRef), 0o750); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("failed to write session file: %w", err)
	}

	return nil
}

// FormatResumeCommand returns the command to resume a Codex CLI session.
func (c *CodexAgent) FormatResumeCommand(sessionID string) string {
	return "codex resume " + sessionID
}

// TranscriptAnalyzer interface implementation

// GetTranscriptPosition returns the current line count of a Codex transcript.
// Codex uses JSONL format, so position is the number of lines.
// Returns 0 if the file doesn't exist or is empty.
func (c *CodexAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}

	file, err := os.Open(path) //nolint:gosec // Path comes from Codex transcript location
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
		lineData, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				// Count a final partial line (no trailing newline)
				if len(lineData) > 0 {
					lineCount++
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
// For Codex (JSONL format), offset is the starting line number.
// Returns:
//   - files: list of file paths modified by Codex (from file_change events)
//   - currentPosition: total number of lines in the file
//   - error: any error encountered during reading
func (c *CodexAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error) {
	if path == "" {
		return nil, 0, nil
	}

	file, openErr := os.Open(path) //nolint:gosec // Path comes from Codex transcript location
	if openErr != nil {
		return nil, 0, fmt.Errorf("failed to open transcript file: %w", openErr)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	fileSet := make(map[string]bool)
	lineNum := 0

	for {
		lineData, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, 0, fmt.Errorf("failed to read transcript: %w", readErr)
		}

		if len(lineData) > 0 {
			lineNum++
			if lineNum > startOffset {
				if fp := extractFilePathFromLine(lineData); fp != "" && !fileSet[fp] {
					fileSet[fp] = true
					files = append(files, fp)
				}
			}
		}

		if readErr == io.EOF {
			break
		}
	}

	return files, lineNum, nil
}

// TranscriptChunker interface implementation

// ChunkTranscript splits a JSONL transcript at line boundaries.
// Codex uses JSONL format, so chunking is done at newline boundaries.
func (c *CodexAgent) ChunkTranscript(content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk JSONL transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks with newlines.
//
//nolint:unparam // error return is required by interface, kept for consistency
func (c *CodexAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

// ExtractModifiedFiles extracts file paths from Codex transcript data.
// Parses JSONL events and collects file paths from file_change and function_call items.
func ExtractModifiedFiles(data []byte) []string {
	fileSet := make(map[string]bool)
	var files []string

	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		if fp := extractFilePathFromLine(scanner.Bytes()); fp != "" && !fileSet[fp] {
			fileSet[fp] = true
			files = append(files, fp)
		}
	}

	return files
}

// extractFilePathFromLine extracts a file path from a single JSONL line.
// Checks for file_change items and function_call items that modify files.
func extractFilePathFromLine(lineData []byte) string {
	var event rolloutEvent
	if json.Unmarshal(lineData, &event) != nil {
		return ""
	}
	if event.Item == nil {
		return ""
	}

	var item rolloutItem
	if json.Unmarshal(event.Item, &item) != nil {
		return ""
	}

	// Check for file_change items
	if item.Type == ItemTypeFileChange {
		return extractFilePathFromItem(event.Item)
	}

	// Check for function_call items that modify files (e.g., write_file, apply_diff)
	if item.Type == ItemTypeFunctionCall {
		return extractFilePathFromFunctionCall(event.Item)
	}

	return ""
}

// extractFilePathFromItem extracts a file path from an item's various path fields.
func extractFilePathFromItem(data json.RawMessage) string {
	var fileItem struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		Filename string `json:"filename"`
	}
	if json.Unmarshal(data, &fileItem) != nil {
		return ""
	}

	if fileItem.FilePath != "" {
		return fileItem.FilePath
	}
	if fileItem.Path != "" {
		return fileItem.Path
	}
	return fileItem.Filename
}

// extractFilePathFromFunctionCall extracts a file path from a function call's input.
func extractFilePathFromFunctionCall(data json.RawMessage) string {
	var fc struct {
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(data, &fc) != nil {
		return ""
	}

	// Only extract from file-modifying functions
	switch fc.Name {
	case "write_file", "apply_diff", "edit_file", "create_file":
		return extractFilePathFromItem(fc.Input)
	default:
		return ""
	}
}

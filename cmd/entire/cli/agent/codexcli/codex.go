package codexcli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameCodex, NewCodexCLIAgent)
}

// CodexCLIAgent implements the Agent interface for OpenAI Codex CLI.
//
//nolint:revive // CodexCLIAgent is clearer than Agent in this context
type CodexCLIAgent struct{}

// NewCodexCLIAgent creates a new Codex CLI agent instance.
func NewCodexCLIAgent() agent.Agent {
	return &CodexCLIAgent{}
}

// Name returns the agent registry key.
func (c *CodexCLIAgent) Name() agent.AgentName {
	return agent.AgentNameCodex
}

// Type returns the agent type identifier.
func (c *CodexCLIAgent) Type() agent.AgentType {
	return agent.AgentTypeCodex
}

// Description returns a human-readable description.
func (c *CodexCLIAgent) Description() string {
	return "Codex CLI - OpenAI's CLI coding agent"
}

// DetectPresence checks if Codex CLI is configured in the repository.
func (c *CodexCLIAgent) DetectPresence() (bool, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}

	// Check for AGENTS.md (Codex project config file)
	agentsFile := filepath.Join(repoRoot, "AGENTS.md")
	if _, err := os.Stat(agentsFile); err == nil {
		return true, nil
	}

	// Check for ~/.codex directory (Codex is installed)
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return false, fmt.Errorf("failed to get home directory: %w", err)
	}
	codexDir := filepath.Join(homeDir, ".codex")
	if _, err := os.Stat(codexDir); err == nil {
		return true, nil
	}

	return false, nil
}

// GetHookConfigPath returns the path to Codex's config file.
func (c *CodexCLIAgent) GetHookConfigPath() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(homeDir, ".codex", "config.toml")
}

// SupportsHooks returns true as Codex CLI supports the notify hook.
func (c *CodexCLIAgent) SupportsHooks() bool {
	return true
}

// ParseHookInput parses Codex hook input from stdin.
// Codex's notify sends a JSON payload with turn completion data.
func (c *CodexCLIAgent) ParseHookInput(_ agent.HookType, reader io.Reader) (*agent.HookInput, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}

	if len(data) == 0 {
		return nil, errors.New("empty input")
	}

	var payload notifyPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("failed to parse notify payload: %w", err)
	}

	input := &agent.HookInput{
		HookType:  agent.HookStop, // turn-complete maps to stop semantically
		SessionID: payload.ThreadID,
		Timestamp: time.Now(),
		RawData:   make(map[string]interface{}),
	}

	if len(payload.InputMessages) > 0 {
		input.UserPrompt = payload.InputMessages[len(payload.InputMessages)-1]
	}

	input.RawData["turn_id"] = payload.TurnID
	input.RawData["last_message"] = payload.LastAssistantMessage

	// Resolve the transcript file for this session
	sessionDir, err := c.GetSessionDir("")
	if err == nil {
		transcriptPath := c.findTranscriptBySessionID(sessionDir, payload.ThreadID)
		if transcriptPath != "" {
			input.SessionRef = transcriptPath
		}
	}

	return input, nil
}

// GetSessionID extracts the session ID from hook input.
func (c *CodexCLIAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// ProtectedDirs returns directories that Codex uses for config/state.
// Codex does not create a project-level config directory (unlike .claude or .gemini).
func (c *CodexCLIAgent) ProtectedDirs() []string { return nil }

// GetSessionDir returns the directory where Codex stores session transcripts.
func (c *CodexCLIAgent) GetSessionDir(_ string) (string, error) {
	if override := os.Getenv("ENTIRE_TEST_CODEX_SESSION_DIR"); override != "" {
		return override, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	return filepath.Join(homeDir, ".codex", "sessions"), nil
}

// ResolveSessionFile returns the path to a Codex session file.
// Codex uses date-based directory hierarchy: sessions/<year>/<month>/<day>/rollout-<date>-<uuid>.jsonl
func (c *CodexCLIAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	path := c.findTranscriptBySessionID(sessionDir, agentSessionID)
	if path != "" {
		return path
	}
	// Return a best-guess path using today's date
	now := time.Now()
	return filepath.Join(
		sessionDir,
		strconv.Itoa(now.Year()),
		fmt.Sprintf("%02d", now.Month()),
		fmt.Sprintf("%02d", now.Day()),
		fmt.Sprintf("rollout-%s-%s.jsonl", now.Format("2006-01-02T15-04-05"), agentSessionID),
	)
}

// findTranscriptBySessionID walks the sessions directory to find a transcript containing the session ID.
func (c *CodexCLIAgent) findTranscriptBySessionID(sessionDir, sessionID string) string {
	if sessionDir == "" || sessionID == "" {
		return ""
	}

	var found string
	walkErr := filepath.Walk(sessionDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // skip directories with errors during best-effort search
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		// Check if filename contains the session ID
		if strings.Contains(filepath.Base(path), sessionID) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return ""
	}

	return found
}

// ReadSession reads a session from Codex's storage (JSONL transcript file).
func (c *CodexCLIAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	lines, err := ParseTranscript(data)
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

// WriteSession writes a session to Codex's storage (JSONL transcript file).
func (c *CodexCLIAgent) WriteSession(session *agent.AgentSession) error {
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

	if err := os.WriteFile(session.SessionRef, session.NativeData, 0o600); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}

	return nil
}

// FormatResumeCommand returns the command to resume a Codex session.
func (c *CodexCLIAgent) FormatResumeCommand(sessionID string) string {
	return "codex --resume " + sessionID
}

// TranscriptAnalyzer interface implementation

// GetTranscriptPosition returns the current line count of a Codex transcript.
func (c *CodexCLIAgent) GetTranscriptPosition(path string) (int, error) {
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
		_, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return 0, fmt.Errorf("failed to read transcript: %w", err)
		}
		lineCount++
	}

	return lineCount, nil
}

// ExtractModifiedFilesFromOffset extracts files modified since a given line number.
func (c *CodexCLIAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error) {
	if path == "" {
		return nil, 0, nil
	}

	file, openErr := os.Open(path) //nolint:gosec // Path comes from Codex transcript location
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
			}
		}

		if readErr == io.EOF {
			break
		}
	}

	return ExtractModifiedFiles(lines), lineNum, nil
}

// TranscriptChunker interface implementation

// ChunkTranscript splits a JSONL transcript at line boundaries.
func (c *CodexCLIAgent) ChunkTranscript(content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk JSONL transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks.
//
//nolint:unparam // error return is required by interface, kept for consistency
func (c *CodexCLIAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

// Package agent provides interfaces and types for integrating with coding agents.
// It abstracts agent-specific behavior (hooks, log parsing, session storage) so that
// the same Strategy implementations can work with any coding agent.
package agent

import (
	"io"
)

// Agent defines the interface for interacting with a coding agent.
// Each agent implementation (Claude Code, Cursor, Aider, etc.) converts its
// native format to the normalized types defined in this package.
type Agent interface {
	// Name returns the agent registry key (e.g., "claude-code", "gemini")
	Name() AgentName

	// Type returns the agent type identifier (e.g., "Claude Code", "Gemini CLI")
	// This is stored in metadata and trailers.
	Type() AgentType

	// Description returns a human-readable description for UI
	Description() string

	// DetectPresence checks if this agent is configured in the repository
	DetectPresence() (bool, error)

	// GetHookConfigPath returns path to hook config file (empty if none)
	GetHookConfigPath() string

	// SupportsHooks returns true if agent supports lifecycle hooks
	SupportsHooks() bool

	// ParseHookInput parses hook callback input from stdin
	ParseHookInput(hookType HookType, reader io.Reader) (*HookInput, error)

	// GetSessionID extracts session ID from hook input
	GetSessionID(input *HookInput) string

	// TransformSessionID converts agent session ID to Entire session ID
	TransformSessionID(agentSessionID string) string

	// ExtractAgentSessionID extracts agent session ID from Entire ID
	ExtractAgentSessionID(entireSessionID string) string

	// ProtectedDirs returns repo-root-relative directories that should never be
	// modified or deleted during rewind or other destructive operations.
	// Examples: [".claude"] for Claude, [".gemini"] for Gemini.
	ProtectedDirs() []string

	// GetSessionDir returns where agent stores session data for this repo.
	// Examples:
	//   Claude: ~/.claude/projects/<sanitized-repo-path>/
	//   Aider: current working directory (returns repoPath)
	//   Cursor: ~/Library/Application Support/Cursor/User/globalStorage/
	GetSessionDir(repoPath string) (string, error)

	// SessionFileExtension returns the file extension for session transcript files.
	// Includes the leading dot.
	// Examples: ".jsonl" (Claude), ".json" (Gemini)
	SessionFileExtension() string

	// ReadSession reads session data from agent's storage.
	// Handles different formats: JSONL (Claude), SQLite (Cursor), Markdown (Aider)
	ReadSession(input *HookInput) (*AgentSession, error)

	// WriteSession writes session data for resumption.
	// Agent handles format conversion (JSONL, SQLite, etc.)
	WriteSession(session *AgentSession) error

	// FormatResumeCommand returns command to resume a session
	FormatResumeCommand(sessionID string) string
}

// HookSupport is implemented by agents with lifecycle hooks.
// This optional interface allows agents like Claude Code and Cursor to
// install and manage hooks that notify Entire of agent events.
type HookSupport interface {
	Agent

	// InstallHooks installs agent-specific hooks.
	// If localDev is true, hooks point to local development build.
	// If force is true, removes existing Entire hooks before installing.
	// Returns the number of hooks installed.
	InstallHooks(localDev bool, force bool) (int, error)

	// UninstallHooks removes installed hooks
	UninstallHooks() error

	// AreHooksInstalled checks if hooks are currently installed
	AreHooksInstalled() bool

	// GetSupportedHooks returns the hook types this agent supports
	GetSupportedHooks() []HookType
}

// HookHandler is implemented by agents that define their own hook vocabulary.
// Each agent defines its own hook names (verbs) which become subcommands
// under `entire hooks <agent>`. The actual handling is done by handlers
// registered in the CLI package to avoid circular dependencies.
//
// This allows different agents to have completely different hook vocabularies
// (e.g., Claude Code has "stop", Cursor might have "completion").
type HookHandler interface {
	Agent

	// GetHookNames returns the hook verbs this agent supports.
	// These are the subcommand names that will appear under `entire hooks <agent>`.
	// e.g., ["stop", "user-prompt-submit", "pre-task", "post-task", "post-todo"]
	GetHookNames() []string
}

// FileWatcher is implemented by agents that use file-based detection.
// Agents like Aider that don't support hooks can use file watching
// to detect session activity.
type FileWatcher interface {
	Agent

	// GetWatchPaths returns paths to watch for session changes
	GetWatchPaths() ([]string, error)

	// OnFileChange handles a detected file change and returns session info
	OnFileChange(path string) (*SessionChange, error)
}

// TranscriptAnalyzer is implemented by agents that support transcript analysis.
// This allows agent-agnostic detection of work done between checkpoints.
type TranscriptAnalyzer interface {
	Agent

	// GetTranscriptPosition returns the current position (length) of a transcript.
	// For JSONL formats (Claude Code), this is the line count.
	// For JSON formats (Gemini CLI), this is the message count.
	// Returns 0 if the file doesn't exist or is empty.
	// Use this to efficiently check if the transcript has grown since last checkpoint.
	GetTranscriptPosition(path string) (int, error)

	// ExtractModifiedFilesFromOffset extracts files modified since a given offset.
	// For JSONL formats (Claude Code), offset is the starting line number.
	// For JSON formats (Gemini CLI), offset is the starting message index.
	// Returns:
	//   - files: list of file paths modified by the agent (from Write/Edit tools)
	//   - currentPosition: the current position (line count or message count)
	//   - error: any error encountered during reading
	ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error)
}

// TranscriptChunker is implemented by agents that support transcript chunking.
// This allows agents to split large transcripts into chunks for storage (GitHub has
// a 100MB blob limit) and reassemble them when reading.
type TranscriptChunker interface {
	Agent

	// ChunkTranscript splits a transcript into chunks if it exceeds maxSize.
	// Returns a slice of chunks. If the transcript fits in one chunk, returns single-element slice.
	// The chunking is format-aware: JSONL splits at line boundaries, JSON splits message arrays.
	ChunkTranscript(content []byte, maxSize int) ([][]byte, error)

	// ReassembleTranscript combines chunks back into a single transcript.
	// Handles format-specific reassembly (JSONL concatenation, JSON message merging).
	ReassembleTranscript(chunks [][]byte) ([]byte, error)
}

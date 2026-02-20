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
//
// The interface is organized into four groups:
//
//   - Identity (5 methods): Name, Type, Description, DetectPresence, ProtectedDirs
//   - Event Mapping (2 methods): HookNames, ParseHookEvent
//   - Transcript Storage (3 methods): ReadTranscript, ChunkTranscript, ReassembleTranscript
//   - Legacy (8 methods): Will be moved to optional interfaces or removed in a future phase
type Agent interface {
	// --- Identity ---

	// Name returns the agent registry key (e.g., "claude-code", "gemini")
	Name() AgentName

	// Type returns the agent type identifier (e.g., "Claude Code", "Gemini CLI")
	// This is stored in metadata and trailers.
	Type() AgentType

	// Description returns a human-readable description for UI
	Description() string

	// IsPreview returns whether the agent integration is in preview or stable
	IsPreview() bool

	// DetectPresence checks if this agent is configured in the repository
	DetectPresence() (bool, error)

	// ProtectedDirs returns repo-root-relative directories that should never be
	// modified or deleted during rewind or other destructive operations.
	// Examples: [".claude"] for Claude, [".gemini"] for Gemini.
	ProtectedDirs() []string

	// --- Event Mapping ---

	// HookNames returns the hook verbs this agent supports.
	// These become subcommands under `entire hooks <agent>`.
	// e.g., ["stop", "user-prompt-submit", "session-start", "session-end"]
	HookNames() []string

	// ParseHookEvent translates an agent-native hook into a normalized lifecycle Event.
	// Returns nil if the hook has no lifecycle significance (e.g., pass-through hooks).
	// This is the core contribution surface for new agent implementations.
	ParseHookEvent(hookName string, stdin io.Reader) (*Event, error)

	// --- Transcript Storage ---

	// ReadTranscript reads the raw transcript bytes for a session.
	ReadTranscript(sessionRef string) ([]byte, error)

	// ChunkTranscript splits a transcript into chunks if it exceeds maxSize.
	// Returns a slice of chunks. If the transcript fits in one chunk, returns single-element slice.
	// The chunking is format-aware: JSONL splits at line boundaries, JSON splits message arrays.
	ChunkTranscript(content []byte, maxSize int) ([][]byte, error)

	// ReassembleTranscript combines chunks back into a single transcript.
	// Handles format-specific reassembly (JSONL concatenation, JSON message merging).
	ReassembleTranscript(chunks [][]byte) ([]byte, error)

	// --- Legacy methods (will move to optional interfaces in Phase 4) ---

	// GetSessionID extracts session ID from hook input.
	GetSessionID(input *HookInput) string

	// GetSessionDir returns where agent stores session data for this repo.
	GetSessionDir(repoPath string) (string, error)

	// ResolveSessionFile returns the path to the session transcript file.
	ResolveSessionFile(sessionDir, agentSessionID string) string

	// ReadSession reads session data from agent's storage.
	ReadSession(input *HookInput) (*AgentSession, error)

	// WriteSession writes session data for resumption.
	WriteSession(session *AgentSession) error

	// FormatResumeCommand returns command to resume a session.
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

// TranscriptAnalyzer provides format-specific transcript parsing.
// Agents that implement this get richer checkpoints (transcript-derived file lists,
// prompts, summaries). Agents that don't still participate in the checkpoint lifecycle
// via git-status-based file detection and raw transcript storage.
type TranscriptAnalyzer interface {
	Agent

	// GetTranscriptPosition returns the current position (length) of a transcript.
	// For JSONL formats (Claude Code), this is the line count.
	// For JSON formats (Gemini CLI), this is the message count.
	// Returns 0 if the file doesn't exist or is empty.
	GetTranscriptPosition(path string) (int, error)

	// ExtractModifiedFilesFromOffset extracts files modified since a given offset.
	// For JSONL formats (Claude Code), offset is the starting line number.
	// For JSON formats (Gemini CLI), offset is the starting message index.
	// Returns:
	//   - files: list of file paths modified by the agent (from Write/Edit tools)
	//   - currentPosition: the current position (line count or message count)
	//   - error: any error encountered during reading
	ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error)

	// ExtractPrompts extracts user prompts from the transcript starting at the given offset.
	ExtractPrompts(sessionRef string, fromOffset int) ([]string, error)

	// ExtractSummary extracts a summary of the session from the transcript.
	ExtractSummary(sessionRef string) (string, error)
}

// TranscriptPreparer is called before ReadTranscript to handle agent-specific
// flush/sync requirements (e.g., Claude Code's async transcript writing).
// The framework calls PrepareTranscript before ReadTranscript if implemented.
type TranscriptPreparer interface {
	Agent

	// PrepareTranscript ensures the transcript is ready to read.
	// For Claude Code, this waits for the async transcript flush to complete.
	PrepareTranscript(sessionRef string) error
}

// TokenCalculator provides token usage calculation for a session.
// The framework calls this during step save and checkpoint if implemented.
type TokenCalculator interface {
	Agent

	// CalculateTokenUsage computes token usage from the transcript starting at the given offset.
	CalculateTokenUsage(sessionRef string, fromOffset int) (*TokenUsage, error)
}

// SubagentAwareExtractor provides methods for extracting files and tokens including subagents.
// Agents that support spawning subagents (like Claude Code's Task tool) should implement this
// to ensure subagent contributions are included in checkpoints.
type SubagentAwareExtractor interface {
	Agent

	// ExtractAllModifiedFiles extracts files modified by both the main agent and any spawned subagents.
	// The subagentsDir parameter specifies where subagent transcripts are stored.
	// Returns a deduplicated list of all modified file paths.
	ExtractAllModifiedFiles(sessionRef string, fromOffset int, subagentsDir string) ([]string, error)

	// CalculateTotalTokenUsage computes token usage including all spawned subagents.
	// The subagentsDir parameter specifies where subagent transcripts are stored.
	CalculateTotalTokenUsage(sessionRef string, fromOffset int, subagentsDir string) (*TokenUsage, error)
}

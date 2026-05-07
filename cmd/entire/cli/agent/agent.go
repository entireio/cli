// Package agent provides interfaces and types for integrating with coding agents.
// It abstracts agent-specific behavior (hooks, log parsing, session storage) so that
// the same Strategy implementations can work with any coding agent.
package agent

import (
	"context"
	"io"
	"os/exec"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Agent defines the interface for interacting with a coding agent.
// Each agent implementation (Claude Code, Cursor, Aider, etc.) converts its
// native format to the normalized types defined in this package.
//
// The interface is organized into three groups:
//   - Identity (5 methods): Name, Type, Description, DetectPresence, ProtectedDirs
//   - Transcript Storage (3 methods): ReadTranscript, ChunkTranscript, ReassembleTranscript
//   - Legacy (6 methods): Will be moved to optional interfaces or removed in a future phase
type Agent interface {
	// --- Identity ---

	// Name returns the agent registry key (e.g., "claude-code", "gemini")
	Name() types.AgentName

	// Type returns the agent type identifier (e.g., "Claude Code", "Gemini CLI")
	// This is stored in metadata and trailers.
	Type() types.AgentType

	// Description returns a human-readable description for UI
	Description() string

	// IsPreview returns whether the agent integration is in preview or stable
	IsPreview() bool

	// DetectPresence checks if this agent is configured in the repository
	DetectPresence(ctx context.Context) (bool, error)

	// ProtectedDirs returns repo-root-relative directories that should never be
	// modified or deleted during rewind or other destructive operations.
	// Examples: [".claude"] for Claude, [".gemini"] for Gemini.
	ProtectedDirs() []string

	// --- Transcript Storage ---

	// ReadTranscript reads the raw transcript bytes for a session.
	ReadTranscript(sessionRef string) ([]byte, error)

	// ChunkTranscript splits a transcript into chunks if it exceeds maxSize.
	// Returns a slice of chunks. If the transcript fits in one chunk, returns single-element slice.
	// The chunking is format-aware: JSONL splits at line boundaries, JSON splits message arrays.
	ChunkTranscript(ctx context.Context, content []byte, maxSize int) ([][]byte, error)

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
	WriteSession(ctx context.Context, session *AgentSession) error

	// FormatResumeCommand returns command to resume a session.
	FormatResumeCommand(sessionID string) string
}

// HookSupport is implemented by agents with lifecycle hooks.
// This optional interface allows agents like Claude Code and Cursor to
// install and manage hooks that notify Entire of agent events.
//
// The interface is organized into two groups:
//   - Hook Mapping (2 methods): HookNames, ParseHookEvent
//   - Hook Management (3 methods): InstallHooks, UninstallHooks, AreHooksInstalled
type HookSupport interface {
	Agent

	// HookNames returns the hook verbs this agent supports.
	// These become subcommands under `entire hooks <agent>`.
	// e.g., ["stop", "user-prompt-submit", "session-start", "session-end"]
	HookNames() []string

	// ParseHookEvent translates an agent-native hook into a normalized lifecycle Event.
	// Returns nil if the hook has no lifecycle significance (e.g., pass-through hooks).
	// This is the core contribution surface for new agent implementations.
	ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*Event, error)

	// InstallHooks installs agent-specific hooks.
	// If localDev is true, hooks point to local development build.
	// If force is true, removes existing Entire hooks before installing.
	// Returns the number of hooks installed.
	InstallHooks(ctx context.Context, localDev bool, force bool) (int, error)

	// UninstallHooks removes installed hooks
	UninstallHooks(ctx context.Context) error

	// AreHooksInstalled checks if hooks are currently installed
	AreHooksInstalled(ctx context.Context) bool
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

// ProtectedFilesProvider is implemented by agents that need to exclude
// repo-root-relative files owned by the agent integration itself from session
// tracking or destructive operations.
type ProtectedFilesProvider interface {
	Agent

	// ProtectedFiles returns repo-root-relative files that belong to the
	// agent's own config/state and should be excluded from tracking.
	ProtectedFiles() []string
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
}

// PromptExtractor extracts user prompts from a transcript file.
// Used as a fallback when prompt data isn't captured via hooks (e.g., Factory AI
// Droid's exec mode doesn't fire UserPromptSubmit).
type PromptExtractor interface {
	Agent

	// ExtractPrompts returns user prompts from the transcript starting at the given offset.
	ExtractPrompts(sessionRef string, fromOffset int) ([]string, error)
}

// TranscriptPreparer is called before ReadTranscript to handle agent-specific
// flush/sync requirements (e.g., Claude Code's async transcript writing).
// The framework calls PrepareTranscript before ReadTranscript if implemented.
type TranscriptPreparer interface {
	Agent

	// PrepareTranscript ensures the transcript is ready to read.
	// For Claude Code, this waits for the async transcript flush to complete.
	PrepareTranscript(ctx context.Context, sessionRef string) error
}

// TokenCalculator provides token usage calculation for a session.
// The framework calls this during step save and checkpoint if implemented.
type TokenCalculator interface {
	Agent

	// CalculateTokenUsage computes token usage from the transcript starting at the given offset.
	CalculateTokenUsage(transcriptData []byte, fromOffset int) (*TokenUsage, error)
}

// TextGenerator is an optional interface for agents whose CLI supports
// non-interactive text generation (e.g., claude --print).
// Used for AI-powered metadata generation (trail titles, summaries).
type TextGenerator interface {
	Agent

	// GenerateText sends a prompt to the agent's CLI and returns the raw text response.
	// model is a hint (e.g., "haiku", "sonnet"). Implementations may ignore if not applicable.
	GenerateText(ctx context.Context, prompt string, model string) (string, error)
}

// CompactedTranscript contains the result of transcript compaction into Entire
// Transcript Format. Assets are accepted in the protocol shape for forward
// compatibility but may not yet be persisted by all call sites.
type CompactedTranscript struct {
	Transcript []byte
	Assets     []CompactedTranscriptAsset
}

// CompactedTranscriptAsset is binary data extracted during transcript compaction.
type CompactedTranscriptAsset struct {
	Name      string
	MediaType string
	Data      []byte
}

// TranscriptCompactor is implemented by agents that can produce Entire
// Transcript Format directly from their native transcript representation.
type TranscriptCompactor interface {
	Agent

	// CompactTranscript converts the transcript referenced by sessionRef into
	// Entire Transcript Format and returns the compact transcript bytes.
	CompactTranscript(ctx context.Context, sessionRef string) (*CompactedTranscript, error)
}

// HookResponseWriter is implemented by agents that support structured hook responses.
// Agents that implement this can output messages (e.g., banners) to the user via
// the agent's response protocol. For example, Claude Code outputs JSON with a
// systemMessage field to stdout. Agents that don't implement this will silently
// skip hook response output.
type HookResponseWriter interface {
	Agent

	// WriteHookResponse outputs a message to the user via the agent's hook response protocol.
	WriteHookResponse(message string) error
}

// RestoredSessionPathResolver is implemented by agents that need a
// transcript-specific path when Entire reconstructs a session from checkpoint
// metadata. This is used for restored sessions only; live sessions still use
// the agent's native hook/session references.
type RestoredSessionPathResolver interface {
	Agent

	// ResolveRestoredSessionFile returns where Entire should write a restored
	// transcript so the agent can discover it later.
	ResolveRestoredSessionFile(sessionDir, agentSessionID string, transcript []byte) (string, error)
}

// TestOnly is implemented by agents that exist solely for testing (e.g., the Vogon canary agent).
// These agents are excluded from the user-facing agent selection in `entire enable`.
type TestOnly interface {
	Agent
	IsTestOnly() bool
}

// Launcher is implemented by agents that `entire` can subprocess-spawn.
// This is used by `entire review` to start an agent with a pre-composed
// initial prompt; other commands may use it later.
//
// Contract:
//   - LaunchCmd builds an *exec.Cmd with stdin/stdout/stderr wired to the
//     caller's TTY. The agent runs in the foreground and the call blocks.
//   - The returned cmd is ready to Run() or Start(); it must NOT be modified
//     by the caller except to set environment variables or working dir.
//   - initialPrompt is the first user message to send to the agent.
type Launcher interface {
	LaunchCmd(ctx context.Context, initialPrompt string) (*exec.Cmd, error)
}

// DiscoveredSkill describes one review-adjacent skill found on disk by a
// SkillDiscoverer. Name is the agent-native invocation form (e.g. a
// slash-prefixed command); Description is scraped from on-disk metadata
// if available; SourcePath is kept for debug logging and is not shown to
// the user.
type DiscoveredSkill struct {
	Name        string
	Description string
	SourcePath  string
}

// SkillDiscoverer is implemented by agents that can enumerate review-adjacent
// skills installed locally on disk (e.g. plugin skills under
// ~/.claude/plugins/...). This powers the "Installed plugin skills" section
// of the `entire review` picker and the runtime verification that configured
// skills still exist before spawn.
//
// Contract:
//   - Safe to call on fresh installs where no plugin dir exists yet —
//     return (nil, nil), not an error.
//   - Malformed individual skill metadata must be skipped with a Debug log,
//     not propagated as an error.
//   - A (nil, non-nil) error means "discovery could not run at all" (e.g.
//     home dir inaccessible). Callers may treat all errors as "found nothing"
//     and log at Debug — discovery must never block the picker.
type SkillDiscoverer interface {
	Agent
	DiscoverReviewSkills(ctx context.Context) ([]DiscoveredSkill, error)
}

// SessionBaseDirProvider is implemented by agents that store transcripts in a
// home-directory-based structure with per-project subdirectories. This enables
// cross-project transcript search (e.g., when a session was started from a
// different working directory). Agents with ephemeral/temp-based storage or
// flat session layouts should NOT implement this interface.
type SessionBaseDirProvider interface {
	Agent

	// GetSessionBaseDir returns the base directory containing per-project
	// session subdirectories (e.g., ~/.claude/projects, ~/.gemini/tmp).
	GetSessionBaseDir() (string, error)
}

// SubagentAwareExtractor provides methods for extracting files and tokens including subagents.
// Agents that support spawning subagents (like Claude Code's Task tool) should implement this
// to ensure subagent contributions are included in checkpoints.
type SubagentAwareExtractor interface {
	Agent

	// ExtractAllModifiedFiles extracts files modified by both the main agent and any spawned subagents.
	// The subagentsDir parameter specifies where subagent transcripts are stored.
	// Returns a deduplicated list of all modified file paths.
	ExtractAllModifiedFiles(transcriptData []byte, fromOffset int, subagentsDir string) ([]string, error)

	// CalculateTotalTokenUsage computes token usage including all spawned subagents.
	// The subagentsDir parameter specifies where subagent transcripts are stored.
	CalculateTotalTokenUsage(transcriptData []byte, fromOffset int, subagentsDir string) (*TokenUsage, error)
}

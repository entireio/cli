# Agent Implementation Guide

Before implementing, read the [Agent Integration Checklist](agent-integration-checklist.md) for design principles (full transcript storage, native format preservation) and validation criteria.

## Architecture Overview

The Entire CLI uses **inversion of control** for agent integration: agents are passive data providers that translate their native hook payloads into normalized lifecycle events, and the framework handles all orchestration (state transitions, file detection, checkpoint saving, metadata generation). The flow is:

```
Agent hook invocation → ParseHookEvent() → Event → DispatchLifecycleEvent() → framework actions
```

An agent never calls strategy methods or manages session state directly. It only answers questions: "What event just happened?" and "What does the transcript say?"

## Quick Reference

### Core Interface (`Agent`)

Every agent must implement all 19 methods on the `Agent` interface:

| Group | Method | Purpose |
|-------|--------|---------|
| **Identity** | `Name()` | Registry key (e.g., `"claude-code"`) |
| | `Type()` | Display name for metadata (e.g., `"Claude Code"`) |
| | `Description()` | Human-readable description for UI |
| | `DetectPresence()` | Check if agent is configured in the repo |
| | `ProtectedDirs()` | Directories to preserve during rewind |
| **Event Mapping** | `HookNames()` | Hook verbs that become CLI subcommands |
| | `ParseHookEvent()` | **Core contribution surface** - translate native hooks to Events |
| **Transcript** | `ReadTranscript()` | Read raw transcript bytes |
| | `ChunkTranscript()` | Split large transcripts at format-aware boundaries |
| | `ReassembleTranscript()` | Recombine chunks into a single transcript |
| **Session Management** | `GetHookConfigPath()` | Path to hook config file |
| | `SupportsHooks()` | Whether agent supports lifecycle hooks |
| | `ParseHookInput()` | Parse hook callback input from stdin |
| | `GetSessionID()` | Extract session ID from hook input |
| | `GetSessionDir()` | Where agent stores session data |
| | `ResolveSessionFile()` | Path to session transcript file |
| | `ReadSession()` | Read session data from agent's storage |
| | `WriteSession()` | Write session data for resumption |
| | `FormatResumeCommand()` | Command to resume a session |

### Optional Interfaces

| Interface | Methods | When to implement |
|-----------|---------|-------------------|
| `HookSupport` | `InstallHooks`, `UninstallHooks`, `AreHooksInstalled`, `GetSupportedHooks` | Agent uses a config file for hook registration (e.g., `settings.json`) |
| `HookHandler` | `GetHookNames` | **Required for CLI hook registration** — `entire hooks <agent> <verb>` subcommands are only created for agents implementing this interface. Typically delegates to `HookNames()`. |
| `TranscriptAnalyzer` | `GetTranscriptPosition`, `ExtractModifiedFilesFromOffset` | You want richer checkpoints with transcript-derived file lists |
| `TranscriptPreparer` | `PrepareTranscript` | Agent writes transcripts asynchronously and needs a flush/sync step |
| `TokenCalculator` | `CalculateTokenUsage` | Agent's transcript contains token usage data |
| `SubagentAwareExtractor` | `ExtractAllModifiedFiles`, `CalculateTotalTokenUsage` | Agent spawns subagents (like Claude Code's Task tool) |
| `HookResponseWriter` | `WriteHookResponse` | Agent can display messages from hook responses (e.g., session start banner). Claude Code uses JSON `systemMessage` on stdout; Factory AI Droid uses plain text on stdout. |
| `FileWatcher` | `GetWatchPaths`, `OnFileChange` | Agent doesn't support hooks; uses file-based detection instead |

## Step-by-Step Implementation Guide

### Step 1: Create Package

Create a new directory under `cmd/entire/cli/agent/`:

```
cmd/entire/cli/agent/youragent/
├── youragent.go          # Core Agent implementation + init()
├── lifecycle.go          # ParseHookEvent + compile-time assertions
├── types.go              # Hook input structs, transcript types, tool constants
├── hooks.go              # HookSupport implementation (if applicable)
├── transcript.go         # TranscriptAnalyzer implementation (if applicable)
├── lifecycle_test.go     # Tests for ParseHookEvent
├── hooks_test.go         # Tests for hook installation
└── transcript_test.go    # Tests for transcript analysis
```

### Step 2: Define Types (`types.go`)

Define structs matching your agent's native hook JSON payloads:

```go
package youragent

// Settings file structure (for HookSupport)
type YourAgentSettings struct {
    Hooks YourAgentHooks `json:"hooks"`
}

type YourAgentHooks struct {
    SessionStart []HookMatcher `json:"SessionStart,omitempty"`
    SessionEnd   []HookMatcher `json:"SessionEnd,omitempty"`
    // ... other hook types your agent supports
}

type HookMatcher struct {
    Matcher string      `json:"matcher,omitempty"`
    Hooks   []HookEntry `json:"hooks"`
}

type HookEntry struct {
    Type    string `json:"type"`
    Command string `json:"command"`
}

// Hook input structs - match your agent's JSON payloads

type sessionInfoRaw struct {
    SessionID      string `json:"session_id"`
    TranscriptPath string `json:"transcript_path"`
}

type promptInputRaw struct {
    SessionID      string `json:"session_id"`
    TranscriptPath string `json:"transcript_path"`
    Prompt         string `json:"prompt"`
}

// Tool constants - tools in your agent that modify files
const (
    ToolWrite = "write_file"
    ToolEdit  = "edit_file"
)

var FileModificationTools = []string{ToolWrite, ToolEdit}
```

### Step 3: Implement Core Agent Interface (`youragent.go`)

```go
package youragent

import (
    "errors"
    "fmt"
    "io"
    "os"
    "path/filepath"

    "github.com/entireio/cli/cmd/entire/cli/agent"
    "github.com/entireio/cli/cmd/entire/cli/paths"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
    agent.Register("your-agent", NewYourAgent)
}

type YourAgent struct{}

func NewYourAgent() agent.Agent {
    return &YourAgent{}
}

// --- Identity ---

func (a *YourAgent) Name() agent.AgentName    { return "your-agent" }
func (a *YourAgent) Type() agent.AgentType     { return "Your Agent" }
func (a *YourAgent) Description() string       { return "Your Agent - description here" }
func (a *YourAgent) ProtectedDirs() []string   { return []string{".youragent"} }

func (a *YourAgent) DetectPresence() (bool, error) {
    repoRoot, err := paths.RepoRoot()
    if err != nil {
        repoRoot = "."
    }
    _, err = os.Stat(filepath.Join(repoRoot, ".youragent"))
    if err == nil {
        return true, nil
    }
    return false, nil
}

// --- Transcript Storage ---

func (a *YourAgent) ReadTranscript(sessionRef string) ([]byte, error) {
    data, err := os.ReadFile(sessionRef)
    if err != nil {
        return nil, fmt.Errorf("failed to read transcript: %w", err)
    }
    return data, nil
}

func (a *YourAgent) ChunkTranscript(content []byte, maxSize int) ([][]byte, error) {
    // Use JSONL chunking for line-based formats
    return agent.ChunkJSONL(content, maxSize)
    // Or implement format-specific chunking (see geminicli for JSON example)
}

func (a *YourAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
    return agent.ReassembleJSONL(chunks), nil
}

// --- Session Management ---

func (a *YourAgent) GetHookConfigPath() string                     { return ".youragent/settings.json" }
func (a *YourAgent) SupportsHooks() bool                           { return true }
func (a *YourAgent) GetSessionID(input *agent.HookInput) string    { return input.SessionID }
func (a *YourAgent) FormatResumeCommand(sessionID string) string   { return "youragent --resume " + sessionID }

// ParseHookInput is part of the Agent interface and is called by integration tests.
// Provide a real implementation that populates at least SessionID and SessionRef.
func (a *YourAgent) ParseHookInput(hookType agent.HookType, r io.Reader) (*agent.HookInput, error) {
    raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](r)
    if err != nil {
        return nil, err
    }
    return &agent.HookInput{
        SessionID:  raw.SessionID,
        SessionRef: raw.TranscriptPath,
    }, nil
}

func (a *YourAgent) GetSessionDir(_ string) (string, error) {
    return "", errors.New("not implemented")
}

func (a *YourAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
    return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

func (a *YourAgent) ReadSession(_ *agent.HookInput) (*agent.AgentSession, error) {
    return nil, errors.New("not implemented")
}

func (a *YourAgent) WriteSession(_ *agent.AgentSession) error {
    return errors.New("not implemented")
}
```

### Step 4: Implement `ParseHookEvent` (`lifecycle.go`)

This is the **main contribution surface** for new agents. Map each of your agent's native hook names to the normalized `EventType`:

```go
package youragent

import (
    "io"
    "time"

    "github.com/entireio/cli/cmd/entire/cli/agent"
)

// Hook name constants - these become CLI subcommands
const (
    HookNameSessionStart = "session-start"
    HookNameSessionEnd   = "session-end"
    HookNamePromptSubmit = "prompt-submit"
    HookNameResponse     = "response"
)

func (a *YourAgent) HookNames() []string {
    return []string{
        HookNameSessionStart,
        HookNameSessionEnd,
        HookNamePromptSubmit,
        HookNameResponse,
    }
}

func (a *YourAgent) ParseHookEvent(hookName string, stdin io.Reader) (*agent.Event, error) {
    switch hookName {
    case HookNameSessionStart:
        raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
        if err != nil {
            return nil, err
        }
        return &agent.Event{
            Type:       agent.SessionStart,
            SessionID:  raw.SessionID,
            SessionRef: raw.TranscriptPath,
            Timestamp:  time.Now(),
        }, nil

    case HookNamePromptSubmit:
        raw, err := agent.ReadAndParseHookInput[promptInputRaw](stdin)
        if err != nil {
            return nil, err
        }
        return &agent.Event{
            Type:       agent.TurnStart,
            SessionID:  raw.SessionID,
            SessionRef: raw.TranscriptPath,
            Prompt:     raw.Prompt,
            Timestamp:  time.Now(),
        }, nil

    case HookNameResponse:
        raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
        if err != nil {
            return nil, err
        }
        return &agent.Event{
            Type:       agent.TurnEnd,
            SessionID:  raw.SessionID,
            SessionRef: raw.TranscriptPath,
            Timestamp:  time.Now(),
        }, nil

    case HookNameSessionEnd:
        raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
        if err != nil {
            return nil, err
        }
        return &agent.Event{
            Type:       agent.SessionEnd,
            SessionID:  raw.SessionID,
            SessionRef: raw.TranscriptPath,
            Timestamp:  time.Now(),
        }, nil

    default:
        // Unknown hooks have no lifecycle significance
        return nil, nil //nolint:nilnil
    }
}
```

Key decisions in `ParseHookEvent`:

- **Return `nil, nil`** for hooks with no lifecycle significance (pass-through hooks). This is not an error - it tells the framework to do nothing.
- **Every event should include the fields listed in the [Event Field Requirements](#event-field-requirements) table.** `SessionID` should always be populated (the framework falls back to `"unknown"` for `TurnEnd` if missing, but this degrades checkpoint quality). `SessionRef` is required for `TurnStart` and `TurnEnd` but optional for `Compaction` and `SessionEnd`.
- **`TurnStart` should include `Prompt`** if available - it's used for commit message generation.
- **Use `agent.ReadAndParseHookInput[T]`** - the generic helper reads stdin and unmarshals JSON in one step.
- **Set `Timestamp` to `time.Now()`** - the framework uses this for ordering.

### Step 5: Choose and Implement Optional Interfaces

See the [Optional Interface Decision Tree](#optional-interface-decision-tree) section below.

### Step 6: Register via `init()`

Registration happens in `init()` in your main agent file. The import side-effect pattern ensures your agent is available when the CLI starts:

```go
//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
    agent.Register("your-agent", NewYourAgent)
}
```

Then add a blank import in the CLI's command setup to ensure the package is loaded:

```go
// In cmd/entire/cli/commands/ or wherever agents are imported
import (
    _ "github.com/entireio/cli/cmd/entire/cli/agent/youragent"
)
```

### Step 7: Add Compile-Time Interface Assertions

In `lifecycle.go` (or whichever file implements the optional interfaces), add compile-time checks:

```go
// Compile-time interface assertions
var (
    _ agent.TranscriptAnalyzer = (*YourAgent)(nil)
    _ agent.TokenCalculator    = (*YourAgent)(nil)
    // Add one line per optional interface you implement
)
```

Also add assertions for `HookSupport` and `HookHandler` in `hooks.go` if applicable:

```go
var (
    _ agent.HookSupport = (*YourAgent)(nil)
    _ agent.HookHandler = (*YourAgent)(nil)
)
```

### Step 8: Implement Hook Installation (if `HookSupport`)

If your agent uses a JSON config file for hooks (like Claude Code's `.claude/settings.json`, Gemini's `.gemini/settings.json`, Cursor's `.cursor/hooks.json`, Factory AI Droid's `.factory/settings.json`, or Copilot CLI's `.github/hooks/entire.json`), implement `HookSupport`:

```go
func (a *YourAgent) InstallHooks(localDev bool, force bool) (int, error) {
    // 1. Find repo root
    repoRoot, err := paths.RepoRoot()
    if err != nil {
        return 0, err
    }

    // 2. Read existing settings (preserve unknown fields)
    settingsPath := filepath.Join(repoRoot, ".youragent", "settings.json")
    // ... read and parse ...

    // 3. Build hook commands
    // localDev mode uses $(git rev-parse --show-toplevel) to resolve the repo
    // root at runtime via shell command substitution. This works regardless of
    // where the repo is checked out on disk.
    // Note: Only Claude Code provides a PROJECT_DIR env var (CLAUDE_PROJECT_DIR).
    // Other agents should use the git rev-parse approach instead.
    var cmdPrefix string
    if localDev {
        cmdPrefix = `go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go hooks your-agent `
    } else {
        cmdPrefix = "entire hooks your-agent "
    }

    // 4. Add hooks if they don't exist (idempotent)
    // 5. Write settings back (preserving unknown fields)

    return count, nil
}

func (a *YourAgent) UninstallHooks() error         { /* reverse of install */ }
func (a *YourAgent) AreHooksInstalled() bool        { /* check settings file */ }
func (a *YourAgent) GetSupportedHooks() []agent.HookType { /* list supported types */ }
```

Also implement `HookHandler` — this is required for the CLI to register `entire hooks <agent> <verb>` subcommands:

```go
func (a *YourAgent) GetHookNames() []string {
    return a.HookNames() // delegate to the core interface method
}
```

### Step 9: Write Tests

Test `ParseHookEvent` for every hook name your agent supports. See [Testing Patterns](#testing-patterns) below.

## Event Mapping Reference

The framework dispatcher (`DispatchLifecycleEvent` in `lifecycle.go`) handles each event type as follows:

| Event Type | Framework Actions | Claude Code Hook | Gemini CLI Hook | Cursor Hook | OpenCode Hook | Factory AI Droid Hook | Copilot CLI Hook |
|------------|-------------------|------------------|-----------------|-----------------|---------------|----------------------|-----------------|
| `SessionStart` | Shows banner, checks concurrent sessions, fires state machine transition | `session-start` | `session-start` | `session-start` | `session-start` | `session-start` | `session-start` |
| `TurnStart` | Captures pre-prompt state (git status, transcript position), ensures strategy setup, initializes session | `user-prompt-submit` | `before-agent` | `before-submit-prompt` | `turn-start` | `user-prompt-submit` | `user-prompt-submitted` |
| `TurnEnd` | Validates transcript, extracts metadata (prompts, summary, files), detects file changes via git status, saves step + checkpoint, transitions phase to IDLE | `stop` | `after-agent` | `stop` | `turn-end` | `stop` | `agent-stop` |
| `Compaction` | Fires compaction transition (stays ACTIVE), resets transcript offset | *(not used)* | `pre-compress` | `pre-compact` | `compaction` | `pre-compact` | *(not used)* |
| `SessionEnd` | Marks session as ENDED in state machine | `session-end` | `session-end` | `session-end` | `session-end` | `session-end` | `session-end` |
| `SubagentStart` | Captures pre-task state (git status snapshot) | `pre-task` (PreToolUse[Task]) | *(not used)* | `subagent-start` | *(not used)* | `pre-tool-use` (config-level `matcher: Task`) | *(not used)* |
| `SubagentEnd` | Extracts subagent modified files, detects changes, saves task checkpoint | `post-task` (PostToolUse[Task]) | *(not used)* | `subagent-stop` | *(not used)* | `post-tool-use` (config-level `matcher: Task`) | `subagent-stop` |

### Event Field Requirements

| Event Type | Required Fields | Optional Fields |
|------------|----------------|-----------------|
| `SessionStart` | `SessionID` | `SessionRef`, `ResponseMessage`, `Metadata` |
| `TurnStart` | `SessionID`, `SessionRef` | `Prompt`, `PreviousSessionID`, `Metadata` |
| `TurnEnd` | `SessionRef` | `SessionID` (falls back to `"unknown"`), `Metadata` |
| `Compaction` | `SessionID` | `SessionRef`, `Metadata` |
| `SessionEnd` | `SessionID` | `SessionRef`, `Metadata` |
| `SubagentStart` | `SessionID`, `SessionRef`, `ToolUseID` | `ToolInput`, `Metadata` |
| `SubagentEnd` | `SessionID`, `SessionRef`, `ToolUseID` | `SubagentID`, `ToolInput`, `Metadata` |

`Metadata` (`map[string]string`) holds agent-specific state that the framework stores and makes available on subsequent events. Use it for agent-internal tracking (e.g., cursor positions, background agent flags) that doesn't map to a dedicated Event field.

## Optional Interface Decision Tree

### `TranscriptAnalyzer`

**What it enables:** Transcript-derived file lists (more accurate than git-status-only), extracted user prompts in checkpoint metadata, session summaries.

**Without it:** The framework still creates checkpoints using git-status-based file detection and stores the raw transcript. Prompts and summary fields will be empty.

**Implement when:** Your agent writes a parseable transcript (JSONL, JSON, or any structured format) and you can extract which files were modified and what the user asked.

**Methods:**
- `GetTranscriptPosition(path) (int, error)` - Return current position. For JSONL: line count. For JSON with messages array: message count.
- `ExtractModifiedFilesFromOffset(path, startOffset) (files, currentPosition, error)` - Parse transcript from offset and return files touched by write/edit tools.

### `TranscriptPreparer`

**What it enables:** A pre-read synchronization step before the framework reads the transcript.

**Without it:** The framework reads the transcript immediately, which may be incomplete if the agent writes asynchronously.

**Implement when:** Your agent writes transcripts asynchronously (e.g., Claude Code uses an async writer and needs to wait for a flush sentinel before reading).

**Method:**
- `PrepareTranscript(sessionRef) error` - Wait until the transcript is fully written. Called before `ReadTranscript`.

### `TokenCalculator`

**What it enables:** Token usage metrics in checkpoint metadata (input, output, cache tokens, API call count).

**Without it:** Token usage fields are empty in checkpoint metadata.

**Implement when:** Your agent's transcript contains token/usage data per message.

**Method:**
- `CalculateTokenUsage(sessionRef, fromOffset) (*TokenUsage, error)` - Sum token usage from offset to end of transcript.

### `SubagentAwareExtractor`

**What it enables:** Includes files modified by spawned subagents in the checkpoint file list and aggregates subagent token usage.

**Without it:** Only the main agent's transcript is analyzed. Subagent modifications are still captured by git status but not attributed to the subagent.

**Implement when:** Your agent spawns subagents (task workers) that have their own transcripts.

**Methods:**
- `ExtractAllModifiedFiles(sessionRef, fromOffset, subagentsDir) ([]string, error)` - Deduplicated file list from main + subagent transcripts.
- `CalculateTotalTokenUsage(sessionRef, fromOffset, subagentsDir) (*TokenUsage, error)` - Aggregated usage including subagents.

### `HookSupport`

**What it enables:** `entire enable` automatically installs hooks into the agent's config file.

**Without it:** Users must manually configure hooks to call `entire hooks <agent> <verb>`.

**Implement when:** Your agent supports a config file with hook definitions (e.g., `.claude/settings.json`, `.gemini/settings.json`).

### `HookResponseWriter`

**What it enables:** Displaying messages to the user during hook execution (e.g., the "Entire CLI will link this conversation..." banner on session start).

**Without it:** No message is shown to the user on session start. The framework silently skips the output.

**Implement when:** Your agent displays hook stdout to the user. The output format is agent-specific:
- Claude Code: JSON `{"systemMessage":"..."}` to stdout (parsed by Claude Code's UI)
- Factory AI Droid: Plain text to stdout (displayed directly in terminal)

**Methods:**
- `WriteHookResponse(message string) error` - Output a message via the agent's hook response protocol.

### `FileWatcher`

**What it enables:** Detecting session activity by watching file changes instead of hooks.

**Without it:** The agent must support hooks for the framework to receive events.

**Implement when:** Your agent doesn't support lifecycle hooks but writes session data to predictable file paths.

## Transcript Format Guide

### JSONL Format (Claude Code pattern)

One JSON object per line. Each line is a transcript entry (user message, assistant message, tool use, etc.):

```
{"type":"user","message":{"role":"user","content":"Fix the bug"},"timestamp":"..."}
{"type":"assistant","message":{"role":"assistant","content":[...]},"timestamp":"..."}
```

**Chunking:** Use `agent.ChunkJSONL(content, maxSize)` - splits at newline boundaries.
**Reassembly:** Use `agent.ReassembleJSONL(chunks)` - concatenates with newlines.
**Position:** Line count (`bufio.Reader` + count `\n`).
**Offset:** Start parsing at line N (skip first N lines).

### JSON Format (Gemini CLI pattern)

Single JSON object with a `messages` array:

```json
{"messages": [{"type": "user", "content": "..."}, {"type": "gemini", "content": "..."}]}
```

**Chunking:** Parse the JSON, split the messages array across chunks, marshal each chunk as a complete JSON object with a subset of messages.
**Reassembly:** Parse each chunk, concatenate all message arrays, marshal back.
**Position:** Message count (`len(transcript.Messages)`).
**Offset:** Start iterating messages at index N.

### JSON Format (OpenCode pattern)

Single JSON object with `info` and `messages` array. Messages contain `parts` (text, tool calls with state). Similar to Gemini's pattern but with a different schema:

```json
{"info": {"id": "...", "title": "..."}, "messages": [{"info": {"role": "user"}, "parts": [{"type": "text", "text": "..."}]}]}
```

Key difference: OpenCode stores transcripts in a database, not files. The transcript is fetched via `opencode export <sessionID>` at turn-end and cached to `.entire/tmp/<sessionID>.json`.

**Chunking:** Parse JSON, distribute messages across chunks (each chunk is complete JSON with `info` + subset of `messages`).
**Reassembly:** Merge message arrays from all chunks, preserve `info` from first chunk.
**Position:** Message count (`len(session.Messages)`).
**Offset:** Start iterating messages at index N.

### JSONL Format (Factory AI Droid pattern)

Similar to Claude Code's JSONL format (one JSON object per line), but with a different envelope structure. Factory AI Droid wraps messages as:

```
{"type":"message","id":"...","message":{"role":"assistant","content":[...]}}
{"type":"message","id":"...","message":{"role":"user","content":"Fix the bug"}}
```

Unlike Claude Code's `{"type":"assistant",...}`, Droid uses `type: "message"` with the role inside the `message` object. The `content` field can be either a plain string or an array of content blocks (text, tool_result, etc.). Non-message entries (e.g., `session_start`) are skipped during parsing.

**Chunking:** Same as Claude Code - `agent.ChunkJSONL(content, maxSize)` splits at newline boundaries.
**Reassembly:** Same as Claude Code - `agent.ReassembleJSONL(chunks)` concatenates with newlines.
**Position:** Line count (same as Claude Code).
**Offset:** Start parsing at line N (skip first N lines).

### Using Chunking Helpers

The `agent` package provides format-agnostic entry points:

```go
// These dispatch to the agent's ChunkTranscript/ReassembleTranscript methods
agent.ChunkTranscript(content, agentType)     // agentType → agent lookup → format-aware chunking
agent.ReassembleTranscript(chunks, agentType)  // agentType → agent lookup → format-aware reassembly

// Direct JSONL helpers (usable without an agent)
agent.ChunkJSONL(content, maxSize)
agent.ReassembleJSONL(chunks)

// Chunk file naming
agent.ChunkFileName("full.jsonl", 0)  // "full.jsonl"
agent.ChunkFileName("full.jsonl", 1)  // "full.jsonl.001"
agent.ParseChunkIndex("full.jsonl.002", "full.jsonl")  // 2
agent.SortChunkFiles(files, "full.jsonl")  // sorted by chunk index
```

## Hook Installation Patterns

### JSON Config File Pattern

Claude Code, Gemini CLI, Cursor, Factory AI Droid, and Copilot CLI use a JSON settings file in their config directory. The installation pattern is:

1. **Read existing settings** as `map[string]json.RawMessage` to preserve unknown fields
2. **Parse only the hook types you modify** into typed slices
3. **Remove existing Entire hooks** (for `force` mode or mode-switching)
4. **Add new hooks** idempotently (check if command already exists)
5. **Marshal modified types back** to the raw map
6. **Write the file** with pretty-printing

Key principles:
- **Preserve unknown fields** - don't destroy user's custom hooks or settings
- **Idempotent installs** - running `entire enable` twice doesn't duplicate hooks
- **Support `localDev` mode** - use `go run "$(git rev-parse --show-toplevel)"/...` for development (only Claude Code provides a `PROJECT_DIR` env var; other agents use `git rev-parse` to resolve the repo root at runtime)
- **Identify Entire hooks** by command prefix (e.g., `"entire "` or `go run "$(git rev-parse --show-toplevel)"/...`)

### Example: Claude Code Hook Config

```json
{
  "hooks": {
    "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "entire hooks claude-code session-start"}]}],
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "entire hooks claude-code stop"}]}],
    "PreToolUse": [{"matcher": "Task", "hooks": [{"type": "command", "command": "entire hooks claude-code pre-task"}]}]
  }
}
```

### Example: Gemini CLI Hook Config

```json
{
  "hooksConfig": {"enabled": true},
  "hooks": {
    "SessionStart": [{"hooks": [{"name": "entire-session-start", "type": "command", "command": "entire hooks gemini session-start"}]}],
    "AfterAgent": [{"hooks": [{"name": "entire-after-agent", "type": "command", "command": "entire hooks gemini after-agent"}]}]
  }
}
```

Note: Gemini CLI requires `hooksConfig.enabled: true` and each hook entry requires a `name` field.

### Example: Cursor Hook Config

```json
{
  "version": 1,
  "hooks": {
    "sessionStart": [{"command": "entire hooks cursor session-start"}],
    "beforeSubmitPrompt": [{"command": "entire hooks cursor before-submit-prompt"}],
    "stop": [{"command": "entire hooks cursor stop"}],
    "sessionEnd": [{"command": "entire hooks cursor session-end"}],
    "preCompact": [{"command": "entire hooks cursor pre-compact"}],
    "subagentStart": [{"command": "entire hooks cursor subagent-start"}],
    "subagentStop": [{"command": "entire hooks cursor subagent-stop"}]
  }
}
```

Note: Cursor uses camelCase hook names in `.cursor/hooks.json` and provides a `conversation_id` field as the session identifier.

### Example: Factory AI Droid Hook Config

```json
{
  "hooks": {
    "SessionStart": [
      {"matcher": "", "hooks": [{"type": "command", "command": "entire hooks factoryai-droid session-start"}]},
      {"matcher": "", "hooks": [{"type": "command", "command": "entire hooks factoryai-droid user-prompt-submit"}]}
    ],
    "SessionEnd": [
      {"matcher": "", "hooks": [{"type": "command", "command": "entire hooks factoryai-droid session-end"}]}
    ],
    "UserPromptSubmit": [
      {"matcher": "", "hooks": [{"type": "command", "command": "entire hooks factoryai-droid user-prompt-submit"}]}
    ],
    "Stop": [
      {"matcher": "", "hooks": [{"type": "command", "command": "entire hooks factoryai-droid stop"}]}
    ],
    "PreToolUse": [
      {"matcher": "Task", "hooks": [{"type": "command", "command": "entire hooks factoryai-droid pre-tool-use"}]}
    ],
    "PostToolUse": [
      {"matcher": "Task", "hooks": [{"type": "command", "command": "entire hooks factoryai-droid post-tool-use"}]}
    ],
    "PreCompact": [
      {"matcher": "", "hooks": [{"type": "command", "command": "entire hooks factoryai-droid pre-compact"}]}
    ]
  }
}
```

Note: Factory AI Droid uses PascalCase hook names in `.factory/settings.json` with a nested `matcher`/`hooks` structure. The `matcher` field scopes which tool triggers the hook (empty string = all tools). `SessionStart` includes a second `user-prompt-submit` entry to ensure `TurnStart` fires in droid exec mode where `UserPromptSubmit` doesn't fire. `PreToolUse`/`PostToolUse` use `"matcher": "Task"` to scope to subagent tracking.

### Plugin File Pattern (OpenCode)

OpenCode uses a TypeScript plugin file (`.opencode/plugins/entire.ts`) instead of a JSON config. The plugin is auto-generated by `entire enable --agent opencode` and uses OpenCode's Bun-based plugin API.

Key differences from JSON config agents:
- Plugin file is written/removed entirely (not partial JSON edits)
- Uses `Bun.spawnSync()` for hooks near process exit (`turn-end`, `session-end`)
- Async `callHook()` for non-critical hooks (`session-start`, `turn-start`, `compaction`)
- Calls `opencode export <sessionID>` on turn-end to fetch transcript (not file-based)
- Idempotency via marker string check: `"Auto-generated by \`entire enable --agent opencode\`"`

## Testing Patterns

### Testing `ParseHookEvent`

Test every hook name, including pass-through hooks that return nil:

```go
func TestParseHookEvent_TurnStart(t *testing.T) {
    t.Parallel()

    ag := &YourAgent{}
    input := `{"session_id": "sess-123", "transcript_path": "/tmp/t.jsonl", "prompt": "Fix the bug"}`

    event, err := ag.ParseHookEvent(HookNamePromptSubmit, strings.NewReader(input))

    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if event == nil {
        t.Fatal("expected event, got nil")
    }
    if event.Type != agent.TurnStart {
        t.Errorf("expected TurnStart, got %v", event.Type)
    }
    if event.Prompt != "Fix the bug" {
        t.Errorf("expected prompt 'Fix the bug', got %q", event.Prompt)
    }
}
```

### Testing Nil Returns

Hooks with no lifecycle action must return `nil, nil`:

```go
func TestParseHookEvent_PassThrough_ReturnsNil(t *testing.T) {
    t.Parallel()

    ag := &YourAgent{}
    event, err := ag.ParseHookEvent("some-pass-through-hook", strings.NewReader(`{}`))

    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if event != nil {
        t.Errorf("expected nil event, got %+v", event)
    }
}
```

### Testing Error Cases

```go
func TestParseHookEvent_EmptyInput(t *testing.T) {
    t.Parallel()

    ag := &YourAgent{}
    _, err := ag.ParseHookEvent(HookNameSessionStart, strings.NewReader(""))

    if err == nil {
        t.Fatal("expected error for empty input")
    }
    if !strings.Contains(err.Error(), "empty hook input") {
        t.Errorf("expected 'empty hook input' error, got: %v", err)
    }
}

func TestParseHookEvent_MalformedJSON(t *testing.T) {
    t.Parallel()

    ag := &YourAgent{}
    _, err := ag.ParseHookEvent(HookNameSessionStart, strings.NewReader("not json"))

    if err == nil {
        t.Fatal("expected error for malformed JSON")
    }
}
```

### Testing Hook Installation

```go
func TestInstallHooks_CreatesSettingsFile(t *testing.T) {
    t.Parallel()

    dir := t.TempDir()
    // ... set up test repo, create .youragent/ directory ...

    ag := &YourAgent{}
    count, err := ag.InstallHooks(false, false)

    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if count == 0 {
        t.Error("expected hooks to be installed")
    }

    // Verify settings file was created and contains hooks
    data, err := os.ReadFile(filepath.Join(dir, ".youragent", "settings.json"))
    // ... assert hooks are present ...
}

func TestInstallHooks_Idempotent(t *testing.T) {
    t.Parallel()

    // Install twice, second call should return count=0
    ag := &YourAgent{}
    ag.InstallHooks(false, false)
    count, _ := ag.InstallHooks(false, false)
    if count != 0 {
        t.Errorf("expected 0 new hooks on second install, got %d", count)
    }
}
```

### Test file references

- Claude Code lifecycle tests: `cmd/entire/cli/agent/claudecode/lifecycle_test.go`
- Claude Code hooks tests: `cmd/entire/cli/agent/claudecode/hooks_test.go`
- Claude Code transcript tests: `cmd/entire/cli/agent/claudecode/transcript_test.go`
- Gemini CLI lifecycle tests: `cmd/entire/cli/agent/geminicli/lifecycle_test.go`
- Gemini CLI hooks tests: `cmd/entire/cli/agent/geminicli/hooks_test.go`
- Gemini CLI transcript tests: `cmd/entire/cli/agent/geminicli/transcript_test.go`
- Cursor IDE & CLI lifecycle tests: `cmd/entire/cli/agent/cursor/lifecycle_test.go`
- Cursor IDE & CLI hooks tests: `cmd/entire/cli/agent/cursor/hooks_test.go`
- Cursor IDE & CLI session tests: `cmd/entire/cli/agent/cursor/cursor_test.go`
- OpenCode lifecycle tests: `cmd/entire/cli/agent/opencode/lifecycle_test.go`
- OpenCode hooks tests: `cmd/entire/cli/agent/opencode/hooks_test.go`
- OpenCode transcript tests: `cmd/entire/cli/agent/opencode/transcript_test.go`
- Factory AI Droid lifecycle tests: `cmd/entire/cli/agent/factoryaidroid/lifecycle_test.go`
- Factory AI Droid hooks tests: `cmd/entire/cli/agent/factoryaidroid/hooks_test.go`
- Factory AI Droid transcript tests: `cmd/entire/cli/agent/factoryaidroid/transcript_test.go`

## Common Pitfalls

### go-git v5 Bugs

**Do NOT use go-git v5 for `checkout` or `reset --hard` operations.** go-git v5 has a bug where `worktree.Reset()` with `HardReset` and `worktree.Checkout()` incorrectly delete untracked directories even when listed in `.gitignore`. This would destroy `.entire/` and agent config directories. Use the git CLI instead. See `CLAUDE.md` for details and `hard_reset_test.go` for regression tests.

### Repo Root vs Current Working Directory

Git commands return paths relative to the **repository root**, not the current working directory. When your code runs from a subdirectory, `os.Getwd()` gives wrong results for path construction. Always use `paths.RepoRoot()`:

```go
// WRONG
cwd, _ := os.Getwd()
absPath := filepath.Join(cwd, file) // Breaks from subdirectory

// CORRECT
repoRoot, _ := paths.RepoRoot()
absPath := filepath.Join(repoRoot, file)
```

### Transcript Flush Timing

Some agents write transcripts asynchronously. If `ReadTranscript` is called before the write completes, the transcript will be incomplete. Implement `TranscriptPreparer` if your agent has this behavior. Claude Code solves this by writing a sentinel entry and polling for it (see `waitForTranscriptFlush` in `claudecode/lifecycle.go`).

### Nil Event Return Pattern

`ParseHookEvent` returning `(nil, nil)` is **not an error** - it means the hook has no lifecycle significance. The framework (in `hook_registry.go`) checks:

```go
event, parseErr := ag.ParseHookEvent(hookName, stdin)
if event != nil {
    hookErr = DispatchLifecycleEvent(ag, event)
}
// nil event → no-op (hook is silently acknowledged)
```

Use `//nolint:nilnil` to suppress the linter warning on intentional nil returns.

### Agent Name vs Agent Type

- `AgentName` is the **registry key** used in code (`"claude-code"`, `"gemini"`, `"opencode"`, `"cursor"`, `"factoryai-droid"`, `"copilot-cli"`). It appears in CLI commands: `entire hooks cursor stop`.
- `AgentType` is the **display name** stored in metadata and commit trailers (`"Claude Code"`, `"Gemini CLI"`, `"OpenCode"`, `"Cursor"`, `"Factory AI Droid"`, `"Copilot CLI"`). It's what users see.

Register constants for both in `cmd/entire/cli/agent/registry.go` when adding a new agent.

### Hook Names as CLI Subcommands

The strings returned by `HookNames()` become literal CLI subcommands under `entire hooks <agent>`. For example, if `HookNames()` returns `["session-start", "stop"]`, the CLI creates:
- `entire hooks your-agent session-start`
- `entire hooks your-agent stop`

These commands read JSON from stdin and dispatch to `ParseHookEvent`. The agent's hook config should invoke these commands.

## Complete Code Template

A minimal but functional agent skeleton. Copy this directory structure and fill in agent-specific details:

<details>
<summary>youragent/types.go</summary>

```go
package youragent

// sessionInfoRaw matches your agent's session hook JSON payload.
type sessionInfoRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

// promptInputRaw matches your agent's prompt-submit hook JSON payload.
type promptInputRaw struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Prompt         string `json:"prompt"`
}
```

</details>

<details>
<summary>youragent/youragent.go</summary>

```go
package youragent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register("your-agent", NewYourAgent)
}

type YourAgent struct{}

func NewYourAgent() agent.Agent { return &YourAgent{} }

func (a *YourAgent) Name() agent.AgentName  { return "your-agent" }
func (a *YourAgent) Type() agent.AgentType   { return "Your Agent" }
func (a *YourAgent) Description() string     { return "Your Agent - brief description" }
func (a *YourAgent) ProtectedDirs() []string { return []string{".youragent"} }

func (a *YourAgent) DetectPresence() (bool, error) {
	repoRoot, err := paths.RepoRoot()
	if err != nil {
		repoRoot = "."
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".youragent")); err == nil {
		return true, nil
	}
	return false, nil
}

func (a *YourAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path from agent hook
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}
	return data, nil
}

func (a *YourAgent) ChunkTranscript(content []byte, maxSize int) ([][]byte, error) {
	return agent.ChunkJSONL(content, maxSize)
}

func (a *YourAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

// --- Session Management ---
func (a *YourAgent) GetHookConfigPath() string                                          { return "" }
func (a *YourAgent) SupportsHooks() bool                                                { return true }
func (a *YourAgent) ParseHookInput(_ agent.HookType, r io.Reader) (*agent.HookInput, error) {
	raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](r)
	if err != nil {
		return nil, err
	}
	return &agent.HookInput{SessionID: raw.SessionID, SessionRef: raw.TranscriptPath}, nil
}
func (a *YourAgent) GetSessionID(input *agent.HookInput) string                         { return input.SessionID }
func (a *YourAgent) GetSessionDir(_ string) (string, error)                             { return "", errors.New("not implemented") }
func (a *YourAgent) ResolveSessionFile(dir, id string) string                           { return filepath.Join(dir, id+".jsonl") }
func (a *YourAgent) ReadSession(_ *agent.HookInput) (*agent.AgentSession, error)        { return nil, errors.New("not implemented") }
func (a *YourAgent) WriteSession(_ *agent.AgentSession) error                           { return errors.New("not implemented") }
func (a *YourAgent) FormatResumeCommand(id string) string                               { return "youragent --resume " + id }
```

</details>

<details>
<summary>youragent/lifecycle.go</summary>

```go
package youragent

import (
	"io"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

const (
	HookNameSessionStart = "session-start"
	HookNameSessionEnd   = "session-end"
	HookNamePromptSubmit = "prompt-submit"
	HookNameResponse     = "response"
)

func (a *YourAgent) HookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNamePromptSubmit,
		HookNameResponse,
	}
}

func (a *YourAgent) ParseHookEvent(hookName string, stdin io.Reader) (*agent.Event, error) {
	switch hookName {
	case HookNameSessionStart:
		raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:       agent.SessionStart,
			SessionID:  raw.SessionID,
			SessionRef: raw.TranscriptPath,
			Timestamp:  time.Now(),
		}, nil

	case HookNamePromptSubmit:
		raw, err := agent.ReadAndParseHookInput[promptInputRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:       agent.TurnStart,
			SessionID:  raw.SessionID,
			SessionRef: raw.TranscriptPath,
			Prompt:     raw.Prompt,
			Timestamp:  time.Now(),
		}, nil

	case HookNameResponse:
		raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:       agent.TurnEnd,
			SessionID:  raw.SessionID,
			SessionRef: raw.TranscriptPath,
			Timestamp:  time.Now(),
		}, nil

	case HookNameSessionEnd:
		raw, err := agent.ReadAndParseHookInput[sessionInfoRaw](stdin)
		if err != nil {
			return nil, err
		}
		return &agent.Event{
			Type:       agent.SessionEnd,
			SessionID:  raw.SessionID,
			SessionRef: raw.TranscriptPath,
			Timestamp:  time.Now(),
		}, nil

	default:
		return nil, nil //nolint:nilnil // nil event = no lifecycle action
	}
}
```

</details>

<details>
<summary>youragent/lifecycle_test.go</summary>

```go
package youragent

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestParseHookEvent_SessionStart(t *testing.T) {
	t.Parallel()

	ag := &YourAgent{}
	input := `{"session_id": "test-session", "transcript_path": "/tmp/transcript.jsonl"}`

	event, err := ag.ParseHookEvent(HookNameSessionStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.SessionStart {
		t.Errorf("expected SessionStart, got %v", event.Type)
	}
	if event.SessionID != "test-session" {
		t.Errorf("expected session_id 'test-session', got %q", event.SessionID)
	}
}

func TestParseHookEvent_TurnStart(t *testing.T) {
	t.Parallel()

	ag := &YourAgent{}
	input := `{"session_id": "sess-1", "transcript_path": "/tmp/t.jsonl", "prompt": "Hello"}`

	event, err := ag.ParseHookEvent(HookNamePromptSubmit, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agent.TurnStart {
		t.Errorf("expected TurnStart, got %v", event.Type)
	}
	if event.Prompt != "Hello" {
		t.Errorf("expected prompt 'Hello', got %q", event.Prompt)
	}
}

func TestParseHookEvent_TurnEnd(t *testing.T) {
	t.Parallel()

	ag := &YourAgent{}
	input := `{"session_id": "sess-2", "transcript_path": "/tmp/t.jsonl"}`

	event, err := ag.ParseHookEvent(HookNameResponse, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agent.TurnEnd {
		t.Errorf("expected TurnEnd, got %v", event.Type)
	}
}

func TestParseHookEvent_SessionEnd(t *testing.T) {
	t.Parallel()

	ag := &YourAgent{}
	input := `{"session_id": "sess-3", "transcript_path": "/tmp/t.jsonl"}`

	event, err := ag.ParseHookEvent(HookNameSessionEnd, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Type != agent.SessionEnd {
		t.Errorf("expected SessionEnd, got %v", event.Type)
	}
}

func TestParseHookEvent_UnknownHook(t *testing.T) {
	t.Parallel()

	ag := &YourAgent{}
	event, err := ag.ParseHookEvent("unknown", strings.NewReader(`{}`))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event, got %+v", event)
	}
}

func TestParseHookEvent_EmptyInput(t *testing.T) {
	t.Parallel()

	ag := &YourAgent{}
	_, err := ag.ParseHookEvent(HookNameSessionStart, strings.NewReader(""))

	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestParseHookEvent_MalformedJSON(t *testing.T) {
	t.Parallel()

	ag := &YourAgent{}
	_, err := ag.ParseHookEvent(HookNameSessionStart, strings.NewReader("not json"))

	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}
```

</details>

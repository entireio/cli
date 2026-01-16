# Sessions and Checkpoints

## Overview

Entire CLI creates checkpoints for AI coding sessions. The system is agent-agnostic - it works with Claude Code, Cursor, Copilot, or any tool that triggers Entire hooks.

## Domain Model

### Session

A **Session** is a unit of work. Sessions can be nested - when a subagent runs, it creates a sub-session.

```go
type Session struct {
    ID             string
    FirstPrompt    string       // Raw first user prompt (immutable)
    Description    string       // Display text (derived or editable)
    StartTime      time.Time
    AgentType      string       // "claude-code", "cursor", etc.
    AgentSessionID string       // The agent's session identifier

    Checkpoints    []Checkpoint
    SubSessions    []Session    // Nested sessions (subagent work)

    // Empty for top-level sessions
    ParentID       string       // Parent session ID
    ToolUseID      string       // Tool invocation that spawned this
}

func (s *Session) IsSubSession() bool {
    return s.ParentID != ""
}
```

### Checkpoint

A **Checkpoint** captures a point-in-time within a session.

```go
type Checkpoint struct {
    ID        string
    SessionID string
    Timestamp time.Time
    Type      CheckpointType
    Message   string
}

type CheckpointType int

const (
    Temporary CheckpointType = iota // Full state snapshot, shadow branch
    Committed                        // Metadata + commit ref, entire/sessions
)
```

### Checkpoint Types

| Type | Contents | Use Case |
|------|----------|----------|
| Temporary | Full state (code + metadata) | Intra-session rewind, pre-commit |
| Committed | Metadata + commit reference | Permanent record, post-commit rewind |

### Session Nesting

```
Session (top-level, ParentID="")
├── Checkpoints: [c1, c2, c3]
└── SubSessions:
    └── Session (ParentID=<parent>, ToolUseID="toolu_abc")
        ├── Checkpoints: [c4, c5]
        └── SubSessions: [...] (can nest further)
```

Each session - top-level or nested - has its own FirstPrompt, Description, and Checkpoints.

## Interface

### Session Operations

```go
type Sessions interface {
    Create(ctx context.Context, opts CreateSessionOptions) (*Session, error)
    Get(ctx context.Context, sessionID string) (*Session, error)
    List(ctx context.Context) ([]Session, error) // Top-level sessions only
}
```

### Checkpoint Storage (Low-Level)

Primitives for reading/writing checkpoints. Used by strategies.

```go
type CheckpointStore interface {
    // Temporary checkpoint operations (shadow branches - full state)
    WriteTemporary(ctx context.Context, sessionID string, snapshot TemporaryCheckpoint) error
    ReadTemporary(ctx context.Context, sessionID string) (*TemporaryCheckpoint, error)
    ListTemporary(ctx context.Context) ([]CheckpointInfo, error)

    // Committed checkpoint operations (entire/sessions branch - metadata only)
    WriteCommitted(ctx context.Context, checkpoint CommittedCheckpoint) error
    ReadCommitted(ctx context.Context, checkpointID string) (*CommittedCheckpoint, error)
    ListCommitted(ctx context.Context) ([]CheckpointInfo, error)
}

type TemporaryCheckpoint struct {
    SessionID  string
    CodeTree   plumbing.Hash // Full worktree snapshot
    Transcript []byte
    Prompts    []string
    Context    []byte
}

type CommittedCheckpoint struct {
    ID         string
    SessionID  string
    CommitRef  plumbing.Hash // Reference to user's/auto-commit's code commit
    Transcript []byte
    Prompts    []string
    Context    []byte
    CreatedAt  time.Time
    TokenUsage *TokenUsage   // Token usage for this checkpoint
}

// TokenUsage represents aggregated token usage for a checkpoint
type TokenUsage struct {
    InputTokens         int         // Fresh input tokens (not from cache)
    CacheCreationTokens int         // Tokens written to cache
    CacheReadTokens     int         // Tokens read from cache
    OutputTokens        int         // Output tokens generated
    APICallCount        int         // Number of API calls made
    SubagentTokens      *TokenUsage // Nested usage from spawned subagents
}
```

### Strategy-Level Operations

Strategies compose low-level primitives into higher-level workflows.

**Manual-commit** has condensation logic:

```go
// Condense reads accumulated temporary state and writes a committed checkpoint.
// Handles incremental extraction (since last condense) and derived data generation.
func (s *ManualCommitStrategy) Condense(ctx context.Context, sessionID string, commitRef plumbing.Hash) (*Checkpoint, error)
```

**Auto-commit** writes committed checkpoints directly:

```go
// SaveChanges writes directly to committed storage (no temporary phase).
func (s *AutoCommitStrategy) SaveChanges(ctx context.Context, ...) error
```

## Storage

| Type | Location | Contents |
|------|----------|----------|
| Session State | `.git/entire-sessions/<id>.json` | Active session tracking |
| Temporary | `entire/<commit-hash>` branch | Full state (code + metadata) |
| Committed | `entire/sessions` branch (sharded) | Metadata + commit reference |

### Session State

Location: `.git/entire-sessions/<session-id>.json`

Stored in git common dir (shared across worktrees). Tracks active session info.

### Temporary Checkpoints

Branch: `entire/<base-commit-hash[:7]>`

Contains full worktree snapshot plus metadata overlay. **Multiple concurrent sessions** can share the same shadow branch - their checkpoints interleave:

```
<worktree files...>
.entire/metadata/<session-id-1>/
├── full.jsonl           # Session 1 transcript
├── prompt.txt           # User prompts
├── context.md           # Generated context
└── tasks/<tool-use-id>/ # Task checkpoints
.entire/metadata/<session-id-2>/
├── full.jsonl           # Session 2 transcript (concurrent)
├── ...
```

Tied to a base commit. Condensed to committed on user commit.

**Shadow branch lifecycle:**
- Created on first checkpoint for a base commit
- Migrated automatically if base commit changes (stash → pull → apply scenario)
- Deleted after condensation to `entire/sessions`
- Reset if orphaned (no session state file exists)

### Committed Checkpoints

Branch: `entire/sessions`

Metadata only, sharded by checkpoint ID. Supports **multiple sessions per checkpoint**:

```
<id[:2]>/<id[2:]>/
├── metadata.json        # Checkpoint info (see below)
├── full.jsonl           # Current/latest session transcript
├── prompt.txt
├── context.md
├── tasks/<tool-use-id>/ # Task checkpoints
└── 1/                   # Archived session (if multiple)
    ├── metadata.json
    ├── full.jsonl
    └── ...
```

**Multi-session metadata.json:**
```json
{
  "checkpoint_id": "abc123def456",
  "session_id": "2026-01-13-uuid",
  "session_ids": ["...", "..."],  // All sessions in this checkpoint
  "session_count": 2,
  "strategy": "manual-commit",
  "files_touched": ["file1.txt"],  // Merged from all sessions
  "token_usage": {                 // Token usage for this checkpoint
    "input_tokens": 1500,
    "cache_creation_tokens": 200,
    "cache_read_tokens": 800,
    "output_tokens": 500,
    "api_call_count": 3,
    "subagent_tokens": {           // Optional: usage from spawned agents
      "input_tokens": 1000,
      "output_tokens": 300,
      "api_call_count": 2
    }
  }
}
```

When condensing multiple concurrent sessions:
- Latest session files at root level
- Previous sessions archived to numbered subfolders (`1/`, `2/`, etc.)
- `session_ids` and `files_touched` are merged

### Package Structure

```
session/
├── session.go           # Session type
├── state.go             # Active session state

checkpoint/
├── checkpoint.go        # Checkpoint type
├── store.go             # CheckpointStore interface
├── temporary.go         # Shadow branch storage
├── committed.go         # Metadata branch storage
```

Strategies use `CheckpointStore` primitives - storage details are encapsulated.

## Strategy Role

Strategies determine checkpoint timing and type:

| Strategy | On Save | On SubSession Complete | On User Commit |
|----------|---------|------------------------|----------------|
| Manual-commit | Temporary | Temporary | Condense → Committed |
| Auto-commit | Committed | Committed | — |

## Rewind

Rewind is limited to top-level sessions for simplicity. Subsession rewind out of scope for now.

Each `RewindPoint` includes `SessionID` and `SessionPrompt` to help identify which checkpoint belongs to which session when multiple sessions are interleaved.

## Concurrent Sessions

Multiple AI sessions can run concurrently on the same base commit:

1. **Warning on start** - When a second session starts while another has uncommitted checkpoints, a warning is shown
2. **Both proceed** - User can continue; checkpoints interleave on the same shadow branch
3. **Identification** - Each checkpoint is tagged with its session ID; rewind UI shows session prompt
4. **Condensation** - On commit, all sessions are condensed together with archived subfolders

### Conflict Handling

| Scenario | Behavior |
|----------|----------|
| Concurrent sessions (same worktree) | Warning shown, both proceed |
| Orphaned shadow branch (no state file) | Branch reset, new session proceeds |
| Cross-worktree conflict (state file exists) | `SessionIDConflictError` returned |

### Shadow Branch Migration

If user does stash → pull → apply (HEAD changes without commit):
- Detection: base commit changed AND old shadow branch still exists
- Action: branch renamed from `entire/<old>` to `entire/<new>`
- Result: session continues with checkpoints preserved

---

## Appendix: Legacy Names

| Current | Legacy |
|---------|--------|
| Manual-commit | Shadow |
| Auto-commit | Dual |

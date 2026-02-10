# Sessions and Checkpoints

## Overview

Entire CLI creates checkpoints for AI coding sessions. The system is agent-agnostic - it works with Claude Code, Cursor, Copilot, or any tool that triggers Entire hooks.

## Domain Model

### Session

A **Session** is a unit of work. Defined in `strategy/session.go`:

```go
type Session struct {
    ID          string       // e.g., "2025-12-01-8f76b0e8-b8f1-4a87-9186-848bdd83d62e"
    Description string       // Human-readable summary (first prompt or derived)
    Strategy    string       // Strategy that created this session
    StartTime   time.Time
    Checkpoints []Checkpoint
}
```

### Checkpoint

A **Checkpoint** captures a point-in-time within a session. Defined in `strategy/session.go`:

```go
type Checkpoint struct {
    CheckpointID     id.CheckpointID // Stable 12-hex-char identifier
    Message          string          // Commit message or checkpoint description
    Timestamp        time.Time
    IsTaskCheckpoint bool            // Task checkpoint (subagent) vs session checkpoint
    ToolUseID        string          // Tool use ID for task checkpoints (empty for session)
}
```

### Checkpoint Types

The low-level `checkpoint.Type` (from `checkpoint/checkpoint.go`) indicates storage location:

```go
type Type int

const (
    Temporary Type = iota // Full state snapshot, shadow branch
    Committed             // Metadata + commit ref, entire/checkpoints/v1
)
```

| Type | Contents | Use Case |
|------|----------|----------|
| Temporary | Full state (code + metadata) | Intra-session rewind, pre-commit |
| Committed | Metadata + commit reference | Permanent record, post-commit rewind |

## Interface

### Session Operations

Sessions are accessed via standalone functions in `strategy/session.go`:

```go
// ListSessions returns all sessions from entire/checkpoints/v1,
// plus additional sessions from strategies implementing SessionSource.
func ListSessions() ([]Session, error)

// GetSession finds a session by ID (supports prefix matching).
func GetSession(sessionID string) (*Session, error)
```

### Checkpoint Storage (Low-Level)

The `checkpoint.Store` interface (from `checkpoint/checkpoint.go`) provides primitives for reading/writing checkpoints. Used by strategies.

```go
type Store interface {
    // Temporary checkpoint operations (shadow branches - full state)
    WriteTemporary(ctx context.Context, opts WriteTemporaryOptions) (WriteTemporaryResult, error)
    ReadTemporary(ctx context.Context, baseCommit, worktreeID string) (*ReadTemporaryResult, error)
    ListTemporary(ctx context.Context) ([]TemporaryInfo, error)

    // Committed checkpoint operations (entire/checkpoints/v1 branch - metadata only)
    WriteCommitted(ctx context.Context, opts WriteCommittedOptions) error
    ReadCommitted(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error)
    ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error)
    ReadSessionContentByID(ctx context.Context, checkpointID id.CheckpointID, sessionID string) (*SessionContent, error)
    ListCommitted(ctx context.Context) ([]CommittedInfo, error)
}
```

Key option types (abbreviated):

```go
type WriteTemporaryOptions struct {
    SessionID      string
    BaseCommit     string
    WorktreeID     string   // Internal git worktree identifier (empty for main)
    ModifiedFiles  []string
    NewFiles       []string
    DeletedFiles   []string
    MetadataDir    string   // Relative path to metadata directory
    MetadataDirAbs string   // Absolute path
    CommitMessage  string
    // ...
}

type WriteCommittedOptions struct {
    CheckpointID id.CheckpointID
    SessionID    string
    Strategy     string
    Branch       string
    Transcript   []byte
    Prompts      []string
    Context      []byte
    FilesTouched []string
    TokenUsage   *agent.TokenUsage
    // ...
}
```

Token usage is defined in `agent/types.go`:

```go
type TokenUsage struct {
    InputTokens         int         `json:"input_tokens"`
    CacheCreationTokens int         `json:"cache_creation_tokens"`
    CacheReadTokens     int         `json:"cache_read_tokens"`
    OutputTokens        int         `json:"output_tokens"`
    APICallCount        int         `json:"api_call_count"`
    SubagentTokens      *TokenUsage `json:"subagent_tokens,omitempty"`
}
```

### Strategy-Level Operations

Strategies compose low-level primitives into higher-level workflows.

**Manual-commit** has condensation logic:

```go
// CondenseSession reads accumulated temporary state and writes a committed checkpoint.
func (s *ManualCommitStrategy) CondenseSession(
    repo *git.Repository,
    checkpointID id.CheckpointID,
    state *SessionState,
) (*CondenseResult, error)
```

**Auto-commit** writes committed checkpoints directly:

```go
// SaveChanges creates a commit on the active branch and writes metadata.
func (s *AutoCommitStrategy) SaveChanges(ctx SaveContext) error
```

## Storage

| Type | Location | Contents |
|------|----------|----------|
| Session State | `.git/entire-sessions/<id>.json` | Active session tracking |
| Temporary | `entire/<commit[:7]>-<worktreeHash[:6]>` branch | Full state (code + metadata) |
| Committed | `entire/checkpoints/v1` branch (sharded) | Metadata + commit reference |

### Session State

Location: `.git/entire-sessions/<session-id>.json`

Stored in git common dir (shared across worktrees). Tracks active session info.

### Temporary Checkpoints

Branch: `entire/<commit[:7]>-<worktreeHash[:6]>`

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
- Deleted after condensation to `entire/checkpoints/v1`
- Reset if orphaned (no session state file exists)

### Committed Checkpoints

Branch: `entire/checkpoints/v1`

Metadata only, sharded by checkpoint ID. Supports **multiple sessions per checkpoint**:

```
<id[:2]>/<id[2:]>/
├── metadata.json        # CheckpointSummary (aggregated stats)
├── 0/                   # First session (0-based indexing)
│   ├── metadata.json    # Session-specific CommittedMetadata
│   ├── full.jsonl
│   ├── prompt.txt
│   ├── context.md
│   └── content_hash.txt
├── 1/                   # Second session
│   ├── metadata.json
│   ├── full.jsonl
│   └── ...
└── 2/                   # Third session...
```

**Root-level metadata.json (`CheckpointSummary`):**
```json
{
  "checkpoint_id": "abc123def456",
  "strategy": "manual-commit",
  "branch": "main",
  "checkpoints_count": 3,
  "files_touched": ["file1.txt", "file2.txt"],
  "sessions": [
    {
      "metadata": "ab/c123def456/0/metadata.json",
      "transcript": "ab/c123def456/0/full.jsonl",
      "context": "ab/c123def456/0/context.md",
      "content_hash": "ab/c123def456/0/content_hash.txt",
      "prompt": "ab/c123def456/0/prompt.txt"
    }
  ],
  "token_usage": {
    "input_tokens": 1500,
    "cache_creation_tokens": 200,
    "cache_read_tokens": 800,
    "output_tokens": 500,
    "api_call_count": 3
  }
}
```

When condensing multiple concurrent sessions:
- All sessions are stored in numbered subdirectories using 0-based indexing (`0/`, `1/`, `2/`, ...)
- Each `session_id` is assigned a stable index; subsequent writes for the same session reuse the same numbered folder
- New `session_id` values are appended at the next index, so higher-numbered folders correspond to more recently introduced sessions, not necessarily the chronologically latest activity
- `sessions` array in `CheckpointSummary` maps each session to its file paths
- `files_touched` is merged from all sessions

### Checkpoint ID Linking

The checkpoint ID is the **stable identifier** that links user commits to metadata across branches.

**Format:** 12-hex-character random ID (e.g., `a3b2c4d5e6f7`)

**Generation:**
- Manual-commit: Generated during condensation (post-commit hook)
- Auto-commit: Generated when creating the commit

**Usage:**

1. **User commit trailer** (both strategies):
   - `Entire-Checkpoint: a3b2c4d5e6f7` added to user's commit message
   - Auto-commit: Added programmatically
   - Manual-commit: Added by `prepare-commit-msg` hook (user can remove)

2. **Directory sharding** on `entire/checkpoints/v1`:
   - Path: `<id[:2]>/<id[2:]>/` (e.g., `a3/b2c4d5e6f7/`)
   - First 2 chars = shard (256 possible shards)
   - Remaining 10 chars = directory name

3. **Commit subject** on `entire/checkpoints/v1`:
   - Format: `Checkpoint: a3b2c4d5e6f7`
   - Makes `git log entire/checkpoints/v1` readable

**Bidirectional Lookup:**

```
User commit → Metadata:
  1. Extract "Entire-Checkpoint: a3b2c4d5e6f7" from commit message
  2. Look up metadata:
     - Approach A: Read entire/checkpoints/v1 tree at a3/b2c4d5e6f7/
     - Approach B: Search git log entire/checkpoints/v1 for "Checkpoint: a3b2c4d5e6f7"

Metadata → User commits:
  Given checkpoint ID a3b2c4d5e6f7
  → Search branch history for commits with "Entire-Checkpoint: a3b2c4d5e6f7"
```

**Example Flow:**

```
                    User creates commit
                           ↓
           prepare-commit-msg hook adds trailer
                           ↓
┌──────────────────────────────────────────────────┐
│ Commit on main branch:                           │
│   "Implement login feature                       │
│                                                   │
│   Entire-Checkpoint: a3b2c4d5e6f7"               │
└──────────────────────────────────────────────────┘
                           ↓
                  post-commit hook runs
                           ↓
          Condense shadow → entire/checkpoints/v1
                           ↓
┌──────────────────────────────────────────────────┐
│ Commit on entire/checkpoints/v1:                 │
│   Subject: "Checkpoint: a3b2c4d5e6f7"            │
│                                                   │
│   Tree: a3/b2c4d5e6f7/                           │
│     ├── metadata.json                            │
│     │   (checkpoint_id: "a3b2c4d5e6f7")          │
│     ├── 0/                                       │
│     │   ├── full.jsonl                           │
│     │   ├── prompt.txt                           │
│     │   └── context.md                           │
│     └── ...                                      │
│                                                   │
│   Trailers:                                      │
│     Entire-Session: 2026-01-20-uuid              │
│     Entire-Strategy: manual-commit               │
└──────────────────────────────────────────────────┘
```

The checkpoint ID creates a **bidirectional link**: user commits can find their metadata, and metadata can find the commits that reference it.

### Package Structure

```
strategy/
├── session.go           # Session and Checkpoint types, ListSessions(), GetSession()

session/
├── state.go             # Active session state (StateStore, .git/entire-sessions/)
├── phase.go             # Session phase state machine (ACTIVE, IDLE, ENDED, etc.)

checkpoint/
├── checkpoint.go        # checkpoint.Type, checkpoint.Store interface, CheckpointSummary, etc.
├── store.go             # GitStore implementation
├── temporary.go         # Shadow branch storage
├── committed.go         # Metadata branch storage
├── id/                  # CheckpointID type and generation
│   └── id.go
```

Strategies use `checkpoint.Store` primitives - storage details are encapsulated.

## Strategy Role

Strategies determine checkpoint timing and type:

| Strategy | On Save | On Task Complete | On User Commit |
|----------|---------|------------------|----------------|
| Manual-commit | Temporary | Temporary | Condense → Committed |
| Auto-commit | Committed | Committed | — |

## Rewind

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
- Action: branch renamed from `entire/<old-commit[:7]>-<worktreeHash[:6]>` to `entire/<new-commit[:7]>-<worktreeHash[:6]>`
- Result: session continues with checkpoints preserved

---

## Appendix: Legacy Names

| Current | Legacy |
|---------|--------|
| Manual-commit | Shadow |
| Auto-commit | Dual |

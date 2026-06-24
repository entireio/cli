# Sessions and Checkpoints

## Overview

Entire CLI creates checkpoints for AI coding sessions. The system is agent-agnostic - it works with Claude Code, Codex, Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI, or any tool that triggers Entire hooks.

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
    Ephemeral Type = iota // Full state snapshot, shadow branch
    Persistent            // Metadata + commit ref, entire/checkpoints/v1
)
```

| Type | Contents | Use Case |
|------|----------|----------|
| Ephemeral | Full state (code + metadata) | Intra-session rewind, pre-commit |
| Persistent | Metadata + commit reference | Permanent record, post-commit rewind |

## Interface

### Session Access

`strategy/session.go` keeps the `Session` and `Checkpoint` data types used by
status/explain formatting. Active session state is read from `.git/entire-sessions/`
through `session.StateStore`; committed checkpoint/session content is read
through the checkpoint facade (`checkpoint.Open(ctx, repo, opts)`, which resolves
the ref topology and wires the blob fetcher) and command-specific strategy
methods such as `GetSessionInfo`.

### Checkpoint Storage (Low-Level)

`checkpoint.Open` returns a `*Stores` facade exposing two independent stores,
split by lifecycle:

- `stores.Persistent` — the permanent record on `entire/checkpoints/v1`
  (a `PersistentStore`). This is the pluggable surface.
- `stores.Ephemeral()` — the git-only shadow-branch store for intra-session
  state (an `EphemeralStore`).

Both present a symmetric generic surface — `Read` (differentiated by return
type), `Write` (a sealed request union), and `List`:

```go
type PersistentStore interface {
    Read(ctx, checkpointID id.CheckpointID) (*CheckpointSummary, error)
    List(ctx) ([]CheckpointInfo, error)
    ReadSessionContent(ctx, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error)
    Write(ctx, req WriteRequest) error    // WriteSession / BackfillTranscript / BackfillSummary / BackfillAttribution
    // ...session reads
}

type EphemeralStore interface {
    Read(ctx, baseCommit, worktreeID string) (*ReadEphemeralResult, error)
    List(ctx) ([]EphemeralInfo, error)
    Write(ctx, req EphemeralWriteRequest) (WriteEphemeralResult, error) // WriteCheckpoint / WriteTask
    // ...shadow-branch queries
}
```

Writes go through the request unions rather than per-operation methods, so a
mirror/fan-out store just forwards the request value:

```go
// Persistent: condensation, stop-time backfill, async summary, attribution
stores.Persistent.Write(ctx, checkpoint.WriteSession{CheckpointID: id, /* ... */})
stores.Persistent.Write(ctx, checkpoint.BackfillSummary{CheckpointID: id, Summary: s})

// Ephemeral: shadow-branch capture / task checkpoints
res, _ := stores.Ephemeral().Write(ctx, checkpoint.WriteCheckpoint{BaseCommit: base, /* ... */})
```

`WriteSession`/`BackfillTranscript` are defined types over the option structs
(`WriteOptions`/`UpdateOptions`); `WriteCheckpoint`/`WriteTask` over
`WriteEphemeralOptions`/`WriteEphemeralTaskOptions`.

Token usage and skill events live in the leaf `agent/types` package (so the
contract doesn't pull in the full `agent` package):

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

## Storage

| Type | Location | Contents |
|------|----------|----------|
| Session State | `.git/entire-sessions/<id>.json` | Active session tracking |
| Ephemeral | `entire/<commit[:7]>-<worktreeHash[:6]>` branch | Full state (code + metadata) |
| Persistent | `entire/checkpoints/v1` branch (sharded) | Metadata + commit reference |

### Session State

Location: `.git/entire-sessions/<session-id>.json`

Stored in git common dir (shared across worktrees). Tracks active session info.

The state records `Branch` — the branch HEAD pointed at on the session's last turn
(captured each turn start, so it follows branches created/renamed after the
session began). `entire resume` (bare, no arg) uses it to list stopped sessions
and map each back to its branch; for sessions recorded before the field existed
it falls back to deriving the branch from the session's last checkpoint ID found
in branch-only commit trailers.

### Temporary Checkpoints

Branch: `entire/<commit[:7]>-<worktreeHash[:6]>`

Contains full worktree snapshot plus metadata overlay. **Multiple concurrent sessions** can share the same shadow branch - their checkpoints interleave:

```
<worktree files...>
.entire/metadata/<session-id-1>/
├── full.jsonl           # Session 1 transcript
├── prompt.txt           # Checkpoint-scoped user prompts
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
│   ├── metadata.json    # Session-specific Metadata
│   ├── full.jsonl       # Raw agent transcript (CLI rewind/resume/explain)
│   ├── transcript.jsonl # Compact transcript, scoped to this checkpoint
│   ├── prompt.txt       # Checkpoint-scoped user prompts
│   └── content_hash.txt # sha256 of full.jsonl (dedup short-circuit)
├── 1/                   # Second session
│   ├── metadata.json
│   ├── full.jsonl
│   └── ...
└── 2/                   # Third session...
```

**Compact transcript (`transcript.jsonl`):** generated best-effort from
`full.jsonl` via `transcript/compact` on every committed write and on
transcript replacement during finalization. Unlike `full.jsonl` (the
cumulative session transcript, scoped at read time via
`checkpoint_transcript_start`), `transcript.jsonl` is pre-sliced to the
checkpoint's own portion (`compact.Compact` is called with
`StartLine = checkpoint_transcript_start`), so it needs no offset to consume.
It is written into the checkpoint tree and pushed alongside `full.jsonl`. The
root `metadata.json` `sessions[].transcript` pointer keeps targeting
`full.jsonl`; when a compact transcript was generated the session entry also
carries a `compact_transcript` path pointing at `transcript.jsonl` (omitted
otherwise) so external readers can find it next to `full.jsonl`.
CLI read paths (rewind/resume/explain) read `full.jsonl` by filename. Compact
generation is best-effort: failures are logged but never fail the checkpoint
write, and during finalization a failed regeneration keeps the previous
`transcript.jsonl`.

**Root-level metadata.json (`CheckpointSummary`):**
```json
{
  "cli_version": "0.0.0-dev",
  "checkpoint_version": "branch-v1",
  "checkpoint_id": "abc123def456",
  "strategy": "manual-commit",
  "branch": "main",
  "checkpoints_count": 3,
  "files_touched": ["file1.txt", "file2.txt"],
  "sessions": [
    {
      "metadata": "/ab/c123def456/0/metadata.json",
      "transcript": "/ab/c123def456/0/full.jsonl",
      "compact_transcript": "/ab/c123def456/0/transcript.jsonl",
      "content_hash": "/ab/c123def456/0/content_hash.txt",
      "prompt": "/ab/c123def456/0/prompt.txt"
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

`checkpoints_count` in the root summary is the aggregate displayed "steps" count: the sum of per-session prompt-window counts. Despite the historical name, it is not a count of checkpoint records.

**Session-level metadata.json (`Metadata`, abbreviated):**
```json
{
  "checkpoint_id": "abc123def456",
  "session_id": "2025-12-01-8f76b0e8-b8f1-4a87-9186-848bdd83d62e",
  "strategy": "manual-commit",
  "created_at": "2025-12-01T12:34:56Z",
  "branch": "main",
  "checkpoints_count": 3,
  "save_step_count": 3,
  "files_touched": ["file1.txt", "file2.txt"]
}
```

In session metadata, `checkpoints_count` is the displayed prompt-window count for that session. `save_step_count` records SaveStep-created shadow-branch commits and is the conservative "real checkpoint work happened" signal; it is omitted when zero (for example, commit-only/fallback sessions). `save_step_count` is not aggregated into the root `CheckpointSummary`.

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
- Generated during condensation (post-commit hook)

**Usage:**

1. **User commit trailer**:
   - `Entire-Checkpoint: a3b2c4d5e6f7` added to user's commit message
   - Added by `prepare-commit-msg` hook (user can remove)

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
  2. Read entire/checkpoints/v1 tree at a3/b2c4d5e6f7/

Metadata → User commits:
  Given checkpoint ID a3b2c4d5e6f7
  → Search branch history for commits with "Entire-Checkpoint: a3b2c4d5e6f7"
```

Note: Commit subjects on `entire/checkpoints/v1` (e.g., `Checkpoint: a3b2c4d5e6f7`)
are for human readability in `git log` only. The CLI always reads from the tree at HEAD.

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
│     │   ├── transcript.jsonl                     │
│     │   └── prompt.txt                           │
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
├── session.go           # Session and Checkpoint types

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

| Event | Checkpoint Type |
|-------|----------------|
| On Save | Temporary |
| On Task Complete | Temporary |
| On User Commit | Condense → Committed |

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

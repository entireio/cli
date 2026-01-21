# Comparison: CLI (Entire) vs AgentBlame - Hooks and Metadata Linking

## Overview

| Aspect | CLI (Entire) | AgentBlame |
|--------|--------------|------------|
| **Purpose** | Full session tracking with rewind/replay | Line-level AI attribution (like git blame) |
| **Data stored** | Full transcripts, prompts, context | Only which lines were AI-written |
| **Storage** | Git branches + filesystem | Git notes + SQLite |
| **Linking** | Checkpoint ID trailer | Git notes on commits |

---

## 1. Hook Approaches

### CLI (Entire) - Two-Layer Hook System

**Layer 1: Git Hooks** (installed in `.git/hooks/`)
- `prepare-commit-msg` - Add checkpoint trailer before user edits
- `commit-msg` - Validate/strip empty messages
- `post-commit` - Condense session to `entire/sessions` branch
- `pre-push` - Push metadata branch alongside user push

**Layer 2: Agent Hooks** (in agent config like `.claude/settings.json`)
- `user-prompt-submit` - Initialize session state
- `pre-task` / `post-task` - Subagent checkpoints
- `post-todo` - TodoWrite checkpoints
- `stop` / `session-start` - Lifecycle events

**Key Characteristics:**
- Shell scripts delegate to `entire hooks git <hook-name>`
- Strategy-specific implementations (manual-commit vs auto-commit)
- Interactive TTY support for asking user questions during commit
- Hooks are idempotent and don't block git operations on failure

### AgentBlame - Editor Hooks + Single Git Hook

**Editor Hooks:**
- **Cursor**: `.cursor/hooks.json` → `afterFileEdit` event
- **Claude Code**: `.claude/settings.json` → `PostToolUse` (Edit/Write/MultiEdit)
- Command: `bunx @mesadev/agentblame capture --provider <provider>`

**Git Hook:**
- Only `post-commit` → runs `bun run process HEAD`

**Key Characteristics:**
- Capture happens at edit time, matching at commit time
- No prepare-commit-msg or pre-push hooks
- Uses `bun` runtime for TypeScript execution
- Silent failures (hook errors don't block git operations)

### Hook Comparison Table

| Hook Type | CLI (Entire) | AgentBlame |
|-----------|--------------|------------|
| prepare-commit-msg | ✅ Adds trailer | ❌ Not used |
| commit-msg | ✅ Validates | ❌ Not used |
| post-commit | ✅ Condenses session | ✅ Matches & attaches notes |
| pre-push | ✅ Pushes metadata branch | ❌ Notes pushed in post-commit |
| Editor PostToolUse | ✅ Session tracking | ✅ Capture edits |
| Editor afterFileEdit | ❌ N/A | ✅ Cursor-specific capture |

---

## 2. Metadata Linking Approaches

### CLI (Entire) - Checkpoint Trailer System

**Mechanism:** Git commit trailers with unique IDs

```
# User's commit message
Implement login feature

Entire-Checkpoint: a3b2c4d5e6f7
```

**How it works:**
1. `prepare-commit-msg` hook generates 12-hex-char checkpoint ID
2. Trailer added to commit message (user can remove it)
3. `post-commit` hook reads trailer, copies session data to `entire/sessions` branch
4. Metadata stored at sharded path: `a3/b2c4d5e6f7/` (256 shards)

**Bidirectional Linking:**
```
User commit          →  entire/sessions branch
Entire-Checkpoint:      Commit subject: "Checkpoint: a3b2c4d5e6f7"
a3b2c4d5e6f7           Tree: a3/b2c4d5e6f7/
                            ├── metadata.json
                            ├── full.jsonl (transcript)
                            ├── prompt.txt
                            └── context.md
```

**Storage Locations:**
- Shadow branch: `entire/<base-commit-hash>` (temporary checkpoints)
- Metadata branch: `entire/sessions` (permanent, pushed)
- Session state: `.git/entire-sessions/<session-id>.json` (local)

### AgentBlame - Git Notes System

**Mechanism:** Git notes attached to commits

```bash
git notes --ref=refs/notes/agentblame show <commit-sha>
```

**How it works:**
1. Editor hooks capture edits → store in SQLite with hashes
2. `post-commit` hook diffs the commit
3. Matching algorithm finds which lines came from AI edits
4. Attribution attached as JSON git note on the commit
5. Notes pushed to remote automatically

**Git Note Format (v2):**
```json
{
  "version": 2,
  "timestamp": "2024-01-15T10:30:00Z",
  "attributions": [
    {
      "path": "src/file.ts",
      "startLine": 10,
      "endLine": 15,
      "category": "ai_generated",
      "provider": "cursor",
      "model": "claude-3.5-sonnet",
      "confidence": 1.0,
      "matchType": "exact_hash",
      "contentHash": "sha256:..."
    }
  ]
}
```

**Lookup:**
- `git notes show <sha>` to get attribution
- Query by commit SHA (O(1) lookup)
- No separate branch needed

---

## 3. Storage Architecture Comparison

| Aspect | CLI (Entire) | AgentBlame |
|--------|--------------|------------|
| **Permanent storage** | `entire/sessions` orphan branch | Git notes (`refs/notes/agentblame`) |
| **Temporary storage** | Shadow branches `entire/<hash>` | SQLite `.agentblame/agentblame.db` |
| **Local state** | `.git/entire-sessions/*.json` | SQLite (same db) |
| **Sharding** | Directory sharding (`a3/b2c4d5e6f7/`) | None needed (notes are per-commit) |
| **Push mechanism** | Pre-push hook pushes branch | Post-commit pushes notes |

### Data Volume Stored

**CLI (Entire):**
- Full session transcripts (can be megabytes)
- User prompts, context files
- Task/subagent checkpoints
- Files touched list

**AgentBlame:**
- Line ranges with attribution (kilobytes)
- Provider, model, confidence scores
- Content hashes (for verification)

---

## 4. Key Design Differences

### Session-Centric vs Commit-Centric

**CLI (Entire):** Session-centric
- Groups work into sessions with unique IDs
- Tracks full conversation history
- Supports rewind to any checkpoint
- Maintains session state across commits

**AgentBlame:** Commit-centric
- Attribution attached per commit
- No concept of sessions (just edits → commits)
- No rewind capability
- Forward-only (attributes what was committed)

### Linking Granularity

**CLI (Entire):**
- Links entire session context to commits
- One checkpoint ID → one metadata bundle
- User can opt-out by removing trailer

**AgentBlame:**
- Links individual lines to AI provider/model
- Multiple attributions per commit (one per line range)
- Automatic (no user interaction needed)

### Matching Strategy

**CLI (Entire):**
- No matching needed - hooks capture full session
- Session → checkpoint → commit (direct association)

**AgentBlame:**
- 5-tier matching with confidence scores:
  1. `exact_hash` (1.0 confidence)
  2. `normalized_hash` (0.95 - whitespace tolerant)
  3. `line_in_ai_content` (0.9 - substring match)
  4. `ai_content_in_line` (0.85 - partial match)
  5. `move_detected` (0.85 - refactored code)

---

## 5. Interesting AgentBlame Patterns Worth Noting

1. **Move Detection**: Detects when AI code is moved/refactored between commits
2. **Squash/Rebase Merge Handling**: GitHub Actions workflow transfers attribution through squash merges
3. **Confidence Scores**: Allows downstream consumers to filter by reliability
4. **SQLite as Staging Area**: Clean separation between capture (pending) and storage (committed)
5. **Bun Runtime**: Enables distributing TypeScript directly without build step

---

## 6. Potential Ideas for CLI from AgentBlame

1. **Line-level attribution**: Could complement session tracking with "which AI wrote which line"
2. **Confidence-based matching**: For detecting AI changes that were slightly modified
3. **Move detection**: Track when AI code moves between files
4. **SQLite for staging**: Instead of shadow branches, use SQLite for pending checkpoints (simpler?)
5. **Git notes as alternative**: Notes are simpler than orphan branches for metadata

---

## Summary

The two tools solve different problems:

- **CLI (Entire)**: "What was the full context of this coding session?" - stores everything for replay/rewind
- **AgentBlame**: "Which specific lines were AI-generated?" - lightweight attribution for accountability

CLI's approach is more comprehensive but heavier. AgentBlame's approach is lighter but loses context. They could actually be complementary - CLI for full session replay, AgentBlame for quick line-level blame.

---

## Architecture Diagrams

### CLI (Entire) Flow

```
User starts session (Claude Code UI)
  │
  ▼
SessionStart hook → Ensures git hooks installed
  │
  ▼
User submits prompt
  │
  ▼
UserPromptSubmit hook → InitializeSession() creates .git/entire-sessions/<id>.json
  │
  ▼
Claude makes changes (creates/edits files)
  │
  ▼
User runs git commit
  │
  ▼
prepare-commit-msg hook
  ├─ Finds active sessions
  ├─ Checks for new content or reusable checkpoint ID
  └─ Adds Entire-Checkpoint trailer (or asks user)
  │
  ▼
User edits message (trailer visible in comments)
  │
  ▼
commit-msg hook
  └─ Strips trailer if user removed all content
  │
  ▼
Commit created with trailer
  │
  ▼
post-commit hook
  ├─ Finds checkpoint ID in commit message
  ├─ Condenses session from shadow branch to entire/sessions
  ├─ Updates session state (BaseCommit, CheckpointCount, LastCheckpointID)
  └─ Deletes shadow branch (data preserved)
  │
  ▼
pre-push hook (when user pushes)
  └─ Pushes entire/sessions branch alongside user push
```

### AgentBlame Flow

```
┌─────────────────────┐
│ Cursor/Claude Code  │  (Edited by AI)
└──────────┬──────────┘
           │
           ├─────────────────────────────────────┐
           │                                     │
      afterFileEdit                         PostToolUse
      (Cursor)                              (Claude Code)
           │                                     │
           └─────────────────────────────────────┘
                          │
            ┌─────────────▼──────────────┐
            │   capture.ts (Hook Handler)│
            │  - Extract added lines     │
            │  - Hash (exact + normalized)│
            │  - Store in SQLite         │
            └─────────────┬──────────────┘
                          │
            ┌─────────────▼──────────────┐
            │  .agentblame/agentblame.db │
            │  Pending AI edits          │
            └─────────────┬──────────────┘
                          │
                     User commits
                          │
            ┌─────────────▼──────────────┐
            │ Git Post-Commit Hook       │
            │  - Process HEAD commit     │
            │  - Extract diff hunks      │
            │  - Run matching (5 strats) │
            │  - Merge ranges            │
            └─────────────┬──────────────┘
                          │
            ┌─────────────▼──────────────────────┐
            │ git notes --ref refs/notes/agentblame│
            │ (JSON attribution metadata)        │
            │ Pushed to origin automatically     │
            └─────────────┬──────────────────────┘
                          │
        ┌─────────────────┼─────────────────┐
        │                 │                 │
    CLI blame          Chrome Extension   GitHub PR
    (view locally)     (GitHub UI)        (Workflow)
                                          (Transfer notes
                                           on squash/rebase)
```

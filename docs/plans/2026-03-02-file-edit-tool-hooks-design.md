# File Edit Tool Hooks — Design

## Problem

`FilesTouched` is only updated at turn boundaries (SaveStep/Stop hook). When an agent
commits mid-turn, PostCommit's overlap check sees no intersection between committed
files and the session's known files. The checkpoint trailer is orphaned: no matching
metadata on `entire/checkpoints/v1`.

Root cause: file modifications happen at tool-call granularity, but we only learn about
them at turn-level granularity.

## Solution

Add a `post-file-edit` hook that fires on every file-modifying tool call (Write, Edit).
The hook updates `FilesTouched` in real time and appends detailed edit records to an
append-only log for future attribution.

## Scope

- Claude Code only (Gemini CLI, OpenCode, Cursor follow in separate PRs)
- Write + Edit tool matchers (NotebookEdit deferred)
- Extended payload: file_path, action, tool_name, lines_added, lines_removed, timestamp
- No content storage in edit log (already in transcript)

## Architecture

### Hook Flow

```
Claude Code postToolUse[Write|Edit]
  → entire hooks claude-code post-file-edit
    → parse tool_input (file_path, content/old_string/new_string)
    → normalize file_path to repo-relative
    → compute lines_added/lines_removed
    → append FileEdit to .git/entire-sessions/<session-id>-edits.jsonl
    → merge file_path into FilesTouched in session state
```

### FileEdit Record

```json
{
  "file_path": "cmd/main.go",
  "action": "edit",
  "tool_name": "Edit",
  "lines_added": 15,
  "lines_removed": 3,
  "timestamp": "2026-03-02T10:15:30Z"
}
```

Actions: `"write"` (Write tool) or `"edit"` (Edit tool).

Line computation:
- **Write**: lines_added = line count of `content`, lines_removed = 0
- **Edit**: lines_added = newlines in `new_string`, lines_removed = newlines in `old_string`

### Edit Log Lifecycle

| Event | Action on edit log |
|---|---|
| File edit hook | Append FileEdit line |
| Stop hook (SaveStep) | Leave alone (still accumulating) |
| Post-commit (condense) | Read → include in checkpoint metadata → delete |
| Session end | Clean up if anything remains |

The log survives across turns within a session. It gets consumed at condensation time
(post-commit) and stored as `file_edits.jsonl` in checkpoint metadata on
`entire/checkpoints/v1`.

### Attribution Path (Future)

With edit logs in checkpoint metadata, post-commit can compare aggregate tool edits
against commit diffs to compute precise per-file attribution:
1. Edit log says agent touched `main.go` (+15/-3 lines via Edit)
2. Commit diff says `main.go` changed (+20/-5 lines total)
3. Attribution: agent ~75% of added lines, ~60% of removed lines

Exact line-level attribution would cross-reference transcript tool_input content against
commit diff hunks. This is a follow-up effort.

## Implementation Components

1. **`agent/types.go`** — `FileEdit` struct, `FileEditAction` type
2. **`agent/claudecode/hooks.go`** — `HookNamePostFileEdit` constant, Write/Edit matchers in `InstallHooks()`
3. **`session/state.go`** — `AppendFileEdit()`, `ReadFileEdits()`, `ClearFileEdits()` for JSONL operations
4. **`cli/hook_registry.go`** — Register `post-file-edit` handler for Claude Code
5. **`cli/hooks_claudecode_handlers.go`** — `handleClaudeCodePostFileEdit()` handler
6. **`strategy/manual_commit_condensation.go`** — Include file_edits.jsonl in checkpoint metadata
7. **Tests** — Unit tests for parsing, JSONL ops, integration test for mid-turn commit scenario

## Impact

- Fixes orphaned checkpoint bug (PR #575 fail-open can be reverted once all agents have this)
- `FilesTouched` stays current throughout the turn
- Foundation for line-level attribution

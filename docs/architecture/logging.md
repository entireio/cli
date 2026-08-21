# Logging Architecture

## Overview

The CLI uses Go's `log/slog` package for structured JSON logging. All logs are written to a single file `.entire/logs/entire.log` and help debug hook execution and CLI behavior. Lines logged once a session has been resolved carry a `session_id` attribute, which is how you filter the file down to one session.

## Lifecycle

One `*logging.Logger` per process, carried in the context — the package holds no global state, so nothing has to trust that something upstream initialized it.

| Step | Where |
|------|-------|
| Build it | The root `PersistentPreRun` (`config.go`'s `ensureLogger`), gated on `settings.IsSetUpAny`. `enable` builds one itself only when that gate declined, which is the fresh-repo case it exists for. |
| Pass it | `logging.WithLogger(ctx, l)`, read back with `logging.LoggerFromContext(ctx)`. |
| Close it | The root `PersistentPostRun`, plus `main.go` for the two paths cobra returns on before reaching it (a `RunE` error, and required-flag validation). |

Two consequences worth knowing:

- **A context with no logger falls back to `slog.Default()`**, i.e. the user's terminal. `logging.Debug/Info/Warn/Error` resolve the logger from the context first, so a call site using `context.Background()` writes to stderr no matter what the configured level is — `slog.Default()` is fixed at INFO and ignores `ENTIRE_LOG_LEVEL`.
- **The log file is created on the first line actually written**, not when the logger is built, so a command that logs nothing leaves no empty `entire.log` behind. The flip side is that a `.entire/logs` that cannot be created reports nothing on its own: the failing write drops the line and returns no error. `entire doctor` calls `Logger.EnsureOpen` to name that case.

Packages that cannot take a context — `redact` holds an injected `*slog.Logger` and calls it without one — get theirs from `logging.SessionLoggerFromContext(ctx)`, which stamps the session but deliberately not the component, since those packages tag their own lines and slog does not dedupe attributes.

## Log Levels

| Level | Purpose |
|-------|---------|
| DEBUG | Wrapper breadcrumbs (hook invoked/completed), detailed diagnostics |
| INFO | Handler logs with full context - the primary level for tracing |
| WARN | Unexpected conditions that don't block execution |
| ERROR | Failures that prevent operation completion |

## Configuration

```bash
# Environment variable (takes precedence)
export ENTIRE_LOG_LEVEL=debug

# Or in .entire/settings.json
{"log_level": "debug"}
```

## Tracing Model

Logs use a hierarchical tracing model inspired by OpenTelemetry concepts:

### Identifiers

| Field | Scope | Description |
|-------|-------|-------------|
| `session_id` | Root trace | Entire session ID. Added to lines logged through a context stamped with `logging.WithSessionID` — the hook paths do this once they have resolved a session, so it is not on every line in the file. A call site that passes `session_id` itself wins, and the context's copy is dropped rather than emitted twice. |
| `tool_use_id` | Span | Unique ID for a subagent task lifecycle |
| `agent_id` | Span metadata | The subagent's ID (returned by Claude Code) |

### Hierarchy

```
session_id: 2025-12-31-abc123           ← root trace (all logs)
├── user-prompt-submit                   ← agent-level (no tool_use_id)
├── pre-task (tool_use_id: X)           ← span X starts
│   ├── post-todo (tool_use_id: X)      ← within span X
│   └── post-todo (tool_use_id: X)
├── post-task (tool_use_id: X)          ← span X ends
├── pre-task (tool_use_id: Y)           ← span Y starts
├── post-task (tool_use_id: Y)          ← span Y ends
└── stop                                 ← agent-level (no tool_use_id)
```

### Filtering Examples

```bash
# All logs for a session
jq 'select(.session_id == "2025-12-31-abc123")' .entire/logs/entire.log

# All logs for a specific subagent task
jq 'select(.tool_use_id == "X")' .entire/logs/entire.log

# All subagent activity
jq 'select(.hook_type == "subagent")' .entire/logs/entire.log

# Tail logs in real time
tail -f .entire/logs/entire.log | jq .
```

## Current Gaps

### Prompt-Level Correlation

Agent-level hooks (`user-prompt-submit`, `stop`) lack a correlation ID to tie them to tool activity that happens between them. Within a user prompt:

```
user-prompt-submit              ← no tool_use_id
  ├── Edit (tool_use_id: A)     ← not currently logged
  ├── Edit (tool_use_id: B)     ← not currently logged
  └── Bash (tool_use_id: C)     ← not currently logged
stop                            ← no tool_use_id
```

Future consideration: Generate a `prompt_id` at `user-prompt-submit` and persist for subsequent hooks.

### Tool Use Logging

Currently only Task and TodoWrite tool uses are hooked. Other tool uses (Edit, Write, Bash) are not logged. This is intentional for checkpoint purposes but limits observability.

## Privacy: No User Data in Logs

**Critical rule: Logs must not contain user data.**

### What NOT to log

- User prompts or prompt content
- Task descriptions (the `prompt` field passed to Task tool)
- File contents
- Command outputs
- Any data that could contain PII or sensitive information

### What IS safe to log

- Hook names and types
- Tool names (e.g., "Edit", "Task", "Bash")
- Subagent types (e.g., "general-purpose", "Explore")
- IDs (session_id, tool_use_id, agent_id, checkpoint_id)
- Paths (transcript_path, but not file contents)
- Timing (duration_ms)
- Success/failure status
- Strategy name
- File counts (modified_files, new_files, deleted_files)
- Branch names (shadow_branch)

### Example

```go
// GOOD - logs metadata only
logging.Info(ctx, "post-task",
    slog.String("hook", "post-task"),
    slog.String("tool_use_id", input.ToolUseID),
    slog.String("subagent_type", subagentType),  // e.g., "general-purpose"
)

// BAD - logs user content
logging.Info(ctx, "post-task",
    slog.String("task_description", taskDescription),  // Contains user prompt!
    slog.String("prompt", input.Prompt),               // User data!
)
```

## Components

Logs are tagged with a `component` field indicating the logging source:

| Component | Description |
|-----------|-------------|
| `hooks` | Hook execution (agent hooks, git hooks) |
| `checkpoint` | Checkpoint operations (saves, condensation, branch cleanup) |
| `lifecycle` | Session phase transitions |
| `session` | Session state reads and writes |
| `redaction` | Rule loading, regex compile failures, pack sample mismatches, and the load-time `redaction configured` summary — the lines to grep when a custom rule is not matching |
| `attribution` | Prompt-to-change attribution |
| `migration` | Checkpoint-format migration |

The list is not closed — `grep -rn 'WithComponent(' cmd/ internal/` is the source of truth. Smaller ones in use today: `state`, `resume`, `manual-commit`, `attach`, `cleanup`, `summarize`, `condense-by-id`, `filter-uncommitted`, `trail-refresh`.

## Implementation Details

### Files

| File | Purpose |
|------|---------|
| `cmd/entire/cli/logging/logger.go` | `Logger` (one log file), `New`, `Close`, `EnsureOpen`, and the `Debug/Info/Warn/Error` entry points |
| `cmd/entire/cli/logging/context.go` | Context carriers: `WithLogger`/`LoggerFromContext`, `SessionLoggerFromContext` for packages that hold a bare `*slog.Logger`, and the attribute decorators `WithSessionID`, `WithComponent`, `WithAgent` |
| `cmd/entire/cli/config.go` | `ensureLogger`/`newLogger` — the only place a logger is installed, and where the level is resolved |
| `cmd/entire/cli/root.go` | Builds the logger in `PersistentPreRun`, flushes it in `PersistentPostRun` |
| `cmd/entire/cli/hooks_git_cmd.go` | Git hook logging (uses gitHookContext helper) |
| `cmd/entire/cli/hooks_claudecode_handlers.go` | Claude Code hook logging |
| `cmd/entire/cli/hook_registry.go` | Hook wrapper logging |
| `cmd/entire/cli/strategy/manual_commit_git.go` | Manual-commit checkpoint logging |
| `cmd/entire/cli/strategy/manual_commit_hooks.go` | Condensation and branch cleanup logging |

### Log Entry Structure

**Hook log example:**
```json
{
  "time": "2025-12-31T12:27:52.853381+11:00",
  "level": "INFO",
  "msg": "post-task",
  "session_id": "2025-12-31-abc123",
  "component": "hooks",
  "hook": "post-task",
  "hook_type": "subagent",
  "tool_use_id": "toolu_abc123",
  "agent_id": "agent_xyz",
  "subagent_type": "general-purpose"
}
```

**Checkpoint log example:**
```json
{
  "time": "2025-12-31T12:28:00.123456+11:00",
  "level": "INFO",
  "msg": "checkpoint saved",
  "session_id": "2025-12-31-abc123",
  "component": "checkpoint",
  "strategy": "manual-commit",
  "checkpoint_type": "session",
  "checkpoint_count": 3,
  "modified_files": 2,
  "new_files": 1,
  "deleted_files": 0,
  "shadow_branch": "entire/a1b2c3d",
  "branch_created": false
}
```

**Task checkpoint log example:**
```json
{
  "time": "2025-12-31T12:28:30.789012+11:00",
  "level": "INFO",
  "msg": "task checkpoint saved",
  "session_id": "2025-12-31-abc123",
  "component": "checkpoint",
  "strategy": "manual-commit",
  "checkpoint_type": "task",
  "tool_use_id": "toolu_xyz789",
  "subagent_type": "general-purpose",
  "modified_files": 5,
  "new_files": 2,
  "deleted_files": 0,
  "shadow_branch": "entire/a1b2c3d",
  "branch_created": false
}
```

### Buffered I/O

Logs use an 8KB buffer (`bufio.Writer`) to batch writes and reduce syscall overhead. The buffer is flushed by `Logger.Close`, which the root `PersistentPostRun` calls (and `main.go` for the paths cobra returns on before reaching it). Writes after `Close` are dropped rather than reopening the file.

Because the flush happens at command exit, a long-running command holds up to 8KB of lines invisible for its lifetime — `tail -f` shows nothing until it ends, and a process killed outright loses the buffer.

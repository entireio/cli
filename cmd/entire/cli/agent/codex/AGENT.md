# Codex — Integration One-Pager

## Verdict: COMPATIBLE

Codex (OpenAI's CLI coding agent) supports lifecycle hooks via `hooks.json` config files with JSON stdin/stdout transport. The hook mechanism closely mirrors Claude Code's architecture (matcher-based hook groups, JSON on stdin, structured JSON output on stdout). Entire consumes SessionStart, UserPromptSubmit, Stop, PostToolUse (apply_patch file mutations), and the native subagent hooks SubagentStart / SubagentStop (see [Subagents](#subagents) below). Hooks reached general availability in Codex 0.139.x; this integration is verified against `codex-cli 0.139.0`.

## Static Checks

| Check | Result | Notes |
|-------|--------|-------|
| Binary present | PASS | `codex` found on PATH |
| Help available | PASS | `codex --help` shows full subcommand list |
| Version info | PASS | `codex-cli 0.139.0` |
| Hook keywords | PASS | Hook system via `hooks.json` config files |
| Session keywords | PASS | `resume`, `fork` subcommands; session stored as threads in SQLite + JSONL rollout files |
| Config directory | PASS | `~/.codex/` (overridable via `CODEX_HOME`) |
| Documentation | PASS | JSON schemas at `codex-rs/hooks/schema/generated/` |

## Binary

- Name: `codex`
- Version: `codex-cli 0.139.0`
- Install: `npm install -g @openai/codex` or build from source

## Hook Mechanism

- Config file: `.codex/hooks.json` (project-level, in repo root) or `~/.codex/hooks.json` (user-level)
- Config format: JSON
- Config layer stack: System (`~/.codex/`) → Project (`.codex/`) — project takes precedence
- Hook registration: JSON file with `hooks` object containing event arrays of matcher groups

**hooks.json structure:**
```json
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": null,
        "hooks": [
          {
            "type": "command",
            "command": "entire hooks codex session-start",
            "timeout": 30
          }
        ]
      }
    ],
    "UserPromptSubmit": [...],
    "Stop": [...],
    "PreToolUse": [...]
  }
}
```

**Hook handler fields:**
- `type`: `"command"` (shell execution)
- `command`: Shell command string
- `timeout` / `timeoutSec`: Timeout in seconds (default: 600)
- `async`: Boolean — if true, hook runs asynchronously (default: false)
- `statusMessage`: Optional display message during hook execution

**Matcher field:**
- `null` — matches all events
- `"*"` — matches all
- Regex pattern — matches tool names for PreToolUse (e.g., `"^Bash$"`)

### Hook Names and Event Mapping

| Native Hook Name | When It Fires | Entire EventType | Notes |
|-----------------|---------------|-----------------|-------|
| `SessionStart` | Session begins (startup, resume, or clear) | `SessionStart` | Includes `source` field |
| `UserPromptSubmit` | User submits a prompt | `TurnStart` | Includes `prompt` text |
| `Stop` | Agent finishes a turn | `TurnEnd` | Includes `last_assistant_message` |
| `PreToolUse` | Before tool execution | *(pass-through)* | Shell/Bash only for now; no lifecycle action needed |
| `PostToolUse` | After a tool runs | `ToolUse` (apply_patch only) | File mutations parsed from the apply_patch envelope; other tools pass through |
| `SubagentStart` | A subagent (`spawn_agent`) starts | `SubagentStart` | Matcher filters by `agent_type`; payload carries `agent_id` + `agent_type` |
| `SubagentStop` | A subagent finishes | `SubagentEnd` | Adds `agent_transcript_path` (the child rollout) for per-subagent file/token attribution |

### Hook Input (stdin JSON)

**All events share common fields:**
- `session_id` (string) — UUID thread ID
- `transcript_path` (string|null) — Path to JSONL rollout file, or null in ephemeral mode
- `cwd` (string) — Current working directory
- `hook_event_name` (string) — Event name constant
- `model` (string) — LLM model name
- `permission_mode` (string) — One of: `default`, `acceptEdits`, `plan`, `dontAsk`, `bypassPermissions`

**SessionStart-specific:**
```json
{
  "session_id": "550e8400-e29b-41d4-a716-446655440000",
  "transcript_path": "/Users/user/.codex/rollouts/01/01/rollout-20260324-550e8400.jsonl",
  "cwd": "/path/to/repo",
  "hook_event_name": "SessionStart",
  "model": "gpt-4.1",
  "permission_mode": "default",
  "source": "startup"
}
```
- `source` (string) — `"startup"`, `"resume"`, or `"clear"`

**UserPromptSubmit-specific:**
```json
{
  "session_id": "...",
  "turn_id": "turn-uuid",
  "transcript_path": "...",
  "cwd": "...",
  "hook_event_name": "UserPromptSubmit",
  "model": "gpt-4.1",
  "permission_mode": "default",
  "prompt": "Create a hello.txt file"
}
```
- `prompt` (string) — User's prompt text
- `turn_id` (string) — Turn-scoped identifier

**Stop-specific:**
```json
{
  "session_id": "...",
  "turn_id": "turn-uuid",
  "transcript_path": "...",
  "cwd": "...",
  "hook_event_name": "Stop",
  "model": "gpt-4.1",
  "permission_mode": "default",
  "stop_hook_active": true,
  "last_assistant_message": "I've created hello.txt."
}
```
- `stop_hook_active` (bool) — Whether Stop hook processing is active
- `last_assistant_message` (string|null) — Agent's final message
- `turn_id` (string) — Turn-scoped identifier

**PreToolUse-specific:**
```json
{
  "session_id": "...",
  "turn_id": "turn-uuid",
  "transcript_path": "...",
  "cwd": "...",
  "hook_event_name": "PreToolUse",
  "model": "gpt-4.1",
  "permission_mode": "default",
  "tool_name": "Bash",
  "tool_input": {"command": "ls -la"},
  "tool_use_id": "tool-call-uuid"
}
```
- Currently only fires for `Bash` tool (shell execution)

### Hook Output (stdout JSON)

All hooks accept optional JSON output on stdout. Empty output is valid.

**Universal fields (all events):**
```json
{
  "continue": true,
  "stopReason": null,
  "suppressOutput": false,
  "systemMessage": "Optional message to display"
}
```

The `systemMessage` field can be used to display messages to the user via the agent (similar to Claude Code's `systemMessage`).

## Transcript

- Location: JSONL "rollout" files in `~/.codex/` (sharded directory structure)
- Path pattern: `~/.codex/rollouts/<shard>/<shard>/rollout-<timestamp>-<thread-id>.jsonl`
- The `transcript_path` field in hook payloads provides the exact path
- Format: JSONL (line-delimited JSON)
- Session ID extraction: `session_id` field from hook payload (UUID format)
- Transcript may be null in `--ephemeral` mode

**Note:** Codex's primary storage is SQLite (`~/.codex/state`), but the JSONL rollout file is the file-based transcript we can read. The `transcript_path` in hook payloads points to this file.

## Config Preservation

- Use read-modify-write on entire `hooks.json` file
- Preserve unknown keys in the `hooks` object (future event types)
- The `hooks.json` is separate from `config.toml` — safe to create/modify independently

## CLI Flags

- Non-interactive prompt: `codex exec "<prompt>"` or `codex exec --dangerously-bypass-approvals-and-sandbox "<prompt>"`
- Interactive mode: `codex` or `codex "<prompt>"` (starts TUI)
- Resume session: `codex resume <session-id>` or `codex resume --last`
- Model override: `-m <model>` or `--model <model>`
- Full-auto mode: `codex exec --full-auto "<prompt>"` (workspace-write sandbox + auto-approve)
- JSONL output: `codex exec --json "<prompt>"` (events to stdout)
- Relevant env vars: `CODEX_HOME` (config dir override), `OPENAI_API_KEY` (API auth)

## Gaps & Limitations

- **Hooks require feature flag:** The `codex_hooks` feature is `default_enabled: false` (stage: UnderDevelopment). It must be enabled via `--enable codex_hooks` CLI flag, or `features.codex_hooks = true` in `config.toml`, or `-c features.codex_hooks=true`. Without this, hooks.json is ignored entirely.
- **No SessionEnd hook:** Codex does not fire a hook when a session is completely terminated. The `Stop` hook fires at end-of-turn, not end-of-session. This is similar to some other agents — the framework handles this gracefully.
- **PreToolUse is shell-only:** Currently only fires for `Bash` tool (direct shell execution). MCP tools, stdin streaming, and other tool types are not yet hooked. (PostToolUse IS consumed — see the event-mapping table — for `apply_patch` file mutations.)
- **Transcript may be null:** In `--ephemeral` mode, `transcript_path` is null. The integration should handle this gracefully.
- **Subagents:** Supported via native `SubagentStart`/`SubagentStop` hooks (GA 0.139.x) — see [Subagents](#subagents).
- **Hook response protocol differs from Claude Code:** Codex uses `systemMessage` (same field name) but also supports `hookSpecificOutput` with `additionalContext` for injecting context into the model. For Entire's purposes, `systemMessage` is sufficient.

## Subagents

Codex spawns parallel subagents (custom agents defined as TOML in `~/.codex/agents/` or `.codex/agents/`; `agents.max_depth` defaults to 1, `agents.max_threads` to 6). Entire tracks them with full parity to Claude Code:

- **Lifecycle hooks** — `SubagentStart` and `SubagentStop` (matcher on `agent_type`) map to Entire's `SubagentStart` / `SubagentEnd` events, which drive per-task checkpoints via the agent-agnostic `SaveTaskStep` path. Codex has no per-task `tool_use_id`, so the subagent's `agent_id` (a path-safe UUID) doubles as the task key; `agent_type` becomes the subagent type.
- **Child transcripts** — each subagent runs as its own thread with its own rollout at `CODEX_HOME/sessions/YYYY/MM/DD/rollout-<ts>-<agent_id>.jsonl`. `SubagentStop` provides `agent_transcript_path` directly; Codex implements `SubagentTranscriptResolver` to return it (falling back to globbing the sessions tree by `agent_id`). This differs from Claude Code's sibling-file (`agent-<id>.jsonl`) layout.
- **File + token attribution** — Codex implements `SubagentAwareExtractor`. At turn end it enumerates subagent thread-ids from `spawn_agent` **function-call outputs** in the current checkpoint range (each output is `{"agent_id":"…","nickname":"…"}`), reads each child rollout, and merges their `apply_patch` files and `token_count` usage into `TokenUsage.SubagentTokens`. Keying off spawn outputs (rather than `wait_agent`/`close_agent`/`resume_agent` references) attributes each child exactly once — in the turn it was spawned — so a child resumed or re-waited in a later turn is never double-counted.

**Known limitations / deliberate trade-offs:**

- **Parallel file attribution:** per-subagent *modified* files come primarily from each child's own rollout, and `handleLifecycleSubagentEnd` additionally merges the git-status delta (new/deleted, plus modified tracked files the rollout parse missed) as a safety net. Because subagents run in parallel, that git-status merge can over-attribute concurrently-changed files across siblings.
- **Resumed-subagent token under-count (deliberate):** discovery keys off `spawn_agent` outputs in the current checkpoint range, so a child spawned in a *prior* range and `resume_agent`-ed in this one is not re-attributed — any new work it does during the resumed range is under-counted. This is chosen over the alternative (cross-turn double-counting, the worse, previously-shipped bug); the child's work is still captured as its own task checkpoint, and the case is rare. `extractSpawnedAgentIDs` emits a debug log (`logResumedOutOfRangeChildren`) when it happens, so the under-count is observable. Exact resume accounting would require per-child cross-turn offset state (deferred follow-up).
- **Task-rewind transcript truncation (deferred):** rewinding to a Codex subagent task checkpoint restores files via the shadow tree but does **not** truncate the transcript. The truncation anchor (`CheckpointUUID`) and restore path (`TruncateTranscriptAtUUID`) are Claude-format-specific (keyed by `tool_result` UUIDs); Codex rollouts use `function_call_output` with no such anchor, so `CheckpointUUID` is empty for Codex and rewind falls through to a graceful (non-truncated) restore. Codex-aware line-offset truncation is a deferred follow-up.

## Captured Payloads

- JSON schemas at `codex-rs/hooks/schema/generated/` in the Codex repository
- Hook config structure at `codex-rs/hooks/src/engine/config.rs` in the Codex repository

## Review integration (`entire review`)

Codex review runs via `codex exec --skip-git-repo-check --json [-m <model>] [-c model_reasoning_effort=<level>] -` (prompt on stdin). **`codex exec` fires no lifecycle hooks**, which shapes the whole integration (see CLAUDE.md → `entire review` → "Codex specifics"):

- **Skills are passed verbatim, not paraphrased.** Codex injects its installed-skill catalog into every exec session and loads the matching `SKILL.md`; configured skills use codex's `$name` / `$plugin:name` form (`DiscoverReviewSkills` in `discovery.go`). Native `codex exec review` is not used — it rejects a prompt under a scope flag and can't carry Entire's scope/per-run/checkpoint context.
- **Live tokens come from the rollout file, not stdout.** `codex exec --json` carries `usage` only on the terminal `turn.completed`, and a review is a single turn. `review_tokens.go` resolves the rollout transcript by `thread_id` (from the `thread.started` envelope), tails it (the same `~/.codex/.../rollout-*-<thread-id>.jsonl` documented under Transcript above), and emits cumulative `Tokens` per `token_count` event — the source codex's interactive UI reads.
- **No tagged review session.** Because no hook fires, codex's session is never tagged `KindAgentReview`. The fix manifest therefore sources codex from its **live run output** (`run.Buffer`), and `entire review fix` skill verification is advisory for codex (loose description match), not a hard block.

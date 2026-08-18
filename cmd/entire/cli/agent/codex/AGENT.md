# Codex — Integration One-Pager

## Verdict: COMPATIBLE

Codex (OpenAI's CLI coding agent) supports lifecycle hooks via `hooks.json` config files with JSON stdin/stdout transport. The hook mechanism closely mirrors Claude Code's architecture (matcher-based hook groups, JSON on stdin, structured JSON output on stdout). Eleven hook events are available: PreToolUse, PermissionRequest, PostToolUse, PreCompact, PostCompact, SessionStart, SessionEnd, UserPromptSubmit, SubagentStart, SubagentStop, and Stop.

**Verified against** codex `main` @ `1c042dd4d8` (2026-08-10); installed reference `codex-cli 0.147.0`. Re-check `codex-rs/hooks/schema/generated/` and `codex-rs/config/src/hook_config.rs` when revisiting — this surface moves.

## Static Checks

| Check | Result | Notes |
|-------|--------|-------|
| Binary present | PASS | `codex` found on PATH |
| Help available | PASS | `codex --help` shows full subcommand list |
| Version info | PASS | `codex-cli 0.147.0` (assessed at 0.116.0) |
| Hook keywords | PASS | Hook system via `hooks.json` config files |
| Session keywords | PASS | `resume`, `fork` subcommands; session stored as threads in SQLite + JSONL rollout files |
| Config directory | PASS | `~/.codex/` (overridable via `CODEX_HOME`) |
| Documentation | PASS | JSON schemas at `codex-rs/hooks/schema/generated/` |

## Binary

- Name: `codex`
- Version: `codex-cli 0.147.0` (SessionEnd requires 0.146+)
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
    "SessionEnd": [
      {
        "matcher": null,
        "hooks": [
          {
            "type": "command",
            "command": "entire hooks codex session-end",
            "timeout": 3
          }
        ]
      }
    ],
    "UserPromptSubmit": [...],
    "Stop": [...],
    "PostToolUse": [...]
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
| `SessionEnd` | Thread teardown completes | `SessionEnd` | **Hard 3s cap** — see below |
| `UserPromptSubmit` | User submits a prompt | `TurnStart` | Includes `prompt` text |
| `Stop` | Agent finishes a turn | `TurnEnd` | Includes `last_assistant_message` |
| `PostToolUse` | After tool execution | `ToolUse` | Consumed for `apply_patch` only |
| `PreToolUse` | Before tool execution | *(pass-through)* | No lifecycle action needed |

Not consumed by Entire: `PermissionRequest`, `PreCompact`, `PostCompact`,
`SubagentStart`, `SubagentStop`.

### SessionEnd's timeout ceiling

`SessionEnd` runs inside Codex's shutdown sequence, so it is budgeted far more
tightly than every other hook: it defaults to **1s** and is clamped to
**3s** (`SESSION_END_MAX_TIMEOUT_SEC`, `codex-rs/hooks/src/events/session_end.rs`),
keeping teardown inside app-server's five-second bound. Asking for more prints a
"clamping SessionEnd hook timeout" warning at every startup, and on expiry Codex
terminates the hook's whole process tree (openai/codex#37527).

Entire therefore installs `SessionEnd` at exactly 3s while every other hook keeps
30s, and `CodexAgent.SessionEndBudget` declares a slightly shorter self-imposed
budget (`agent.SessionEndBudgeter`). The gap to the cap is 1s, because Codex's
clock starts when it spawns the `sh -c` wrapper while Entire's starts at package
init — sh startup, the `command -v` PATH walk and loading a ~66MB binary all
happen in between.

That budget bounds only the eager condense; marking the session ENDED is left
unbounded so the cheap step is never the one given up on. Unbounded is not
guaranteed, though: it runs under the session flock, so a concurrent condense can
push it past the cap and get the process tree killed — the exited-owner sweep
reclaims those. A curtailed condense is otherwise safe (`FullyCondensed` stays
false and PostCommit retries), except between the v1 checkpoint write and the
state save, where a kill leaves a committed checkpoint whose bookkeeping never
advanced and PostCommit writes a second one over the same transcript range.

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

**SessionEnd-specific:**
```json
{
  "session_id": "...",
  "transcript_path": "...",
  "cwd": "/path/to/repo",
  "hook_event_name": "SessionEnd",
  "reason": "other"
}
```
- `reason` (string) — currently the constant `"other"`; it cannot distinguish quit from `/clear`
- **Does not share the common fields**: no `model`, no `permission_mode`, no `turn_id`
- **No output schema exists** — stdout is ignored; only the exit code is read
- Codex flushes the rollout before firing, so `transcript_path` is complete when the hook reads it
- Root sessions only; subagent threads use `SubagentStop`

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

- **SessionEnd is tightly budgeted:** 1s default, 3s hard cap, process tree killed on expiry. See "SessionEnd's timeout ceiling" above. This is the only Entire hook that cannot assume it will finish.
- **SessionEnd needs Codex 0.146+:** Added 2026-07-17 (openai/codex#33895), first tagged in `rust-v0.146.0-alpha.3`. On older Codex the hook is simply never called, and such sessions are instead reclaimed by the exited-owner sweep in `entire status` / `entire doctor` (`session.State.OwnerExited`).
- **SessionEnd must be trusted before it fires:** Codex silently skips hooks with no `trusted_hash` entry in the user's `config.toml`. Existing users have trusted the four older events but not `session_end`, so the hook does nothing until they approve it via `/hooks` inside Codex. `HookTrustGaps` and `MissingEntireHooks` both cover `session_end`, so `entire doctor` and the SessionStart banner say so — without that it would fail silently. The e2e suite pre-trusts hooks by generating the same hashes itself (`e2e/agents/codex_trust.go`), so **an event added to `managedHooks` must also be added to `codexHookEventLabels` there** or it is installed but inert for every e2e run; `TestCodexHookTrustState_CoversEveryInstalledEvent` fails when they drift.
- **A pre-SessionEnd install still counts as installed:** `AreHooksInstalled` gates on the core events only, so adding an event does not retroactively drop Codex out of `entire status` and the agent pickers for everyone who enabled it earlier. The stale install is reported as drift by `MissingEntireHooks` instead, with `entire enable` as the fix.
- **`reason` carries no information:** always `"other"`, so a session ended by `/clear` is indistinguishable from one ended by quitting.
- **Transcript may be null:** In `--ephemeral` mode, `transcript_path` is null. The integration handles this gracefully.
- **No hooks fire under `-s read-only`:** verified against 0.147.0 — a `codex exec -s read-only` run produces no hook invocations at all, so no session is tracked. `-s workspace-write` fires the full set.
- **Hook response protocol differs from Claude Code:** Codex uses `systemMessage` (same field name) but also supports `hookSpecificOutput` with `additionalContext` for injecting context into the model. For Entire's purposes, `systemMessage` is sufficient.

### Resolved since the original assessment

- ~~Hooks require feature flag~~ — `CodexHooks` became `Stage::Stable, default_enabled: true` on 2026-04-23 (openai/codex#19012) and the config key was aliased from `codex_hooks` to `hooks` on 2026-05-01 (openai/codex#20522). No flag is needed.
- ~~No SessionEnd hook~~ — added in 0.146; Entire consumes it.
- ~~PreToolUse is shell-only~~ — now dispatched generically from the tool registry (`codex-rs/core/src/tools/registry.rs`), covering shell, `apply_patch`, MCP tools and unified_exec.
- ~~No subagent hooks~~ — `SubagentStart` / `SubagentStop` exist, carrying `agent_id`, `agent_type` and `agent_transcript_path`. Entire does not consume them yet; they are the natural PreTask/PostTask equivalents if subagent checkpoints are wanted for Codex.

## Captured Payloads

- JSON schemas at `codex-rs/hooks/schema/generated/` in the Codex repository
- Hook config structure at `codex-rs/hooks/src/engine/config.rs` in the Codex repository

## Review integration (`entire review`)

Codex review runs via `codex exec --skip-git-repo-check --json [-m <model>] [-c model_reasoning_effort=<level>] -` (prompt on stdin).

> **Stale premise, and a live blocker — needs follow-up.** This integration was
> built on "`codex exec` fires no lifecycle hooks". That is no longer true:
> against codex-cli 0.147.0 a `codex exec` run fires the full set, and with
> `ENTIRE_REVIEW_*` in the environment the session **is** tagged
> `kind: agent_review` with skills and prompt captured.
>
> But an end-to-end `entire review` in a fresh repo still produced **zero** hooks
> and no session ("review skills ran but findings were not persisted"). The gate
> is **hook trust**, not the sandbox. Holding review's exact argv fixed and
> varying one flag at a time:
>
> | added to review's argv | hooks | sessions |
> |---|---|---|
> | *(nothing — what review runs today)* | 0 | 0 |
> | `--dangerously-bypass-hook-trust` | 5 | 1 |
> | `-s workspace-write` | 0 | 0 |
>
> Codex silently skips hooks with no `trusted_hash` in the user's `config.toml`,
> keyed per `<hooks.json path>:<event>:<group>:<handler>`, and `codex exec` is
> non-interactive so it can never prompt. Codex review therefore only yields a
> tagged session in a repo where the user already approved Entire's hooks from an
> interactive Codex session — and after this change `session_end` is untrusted
> even there, until re-approved (`entire doctor` now reports it).
>
> Separately, `-s read-only` suppresses hooks even when trust is bypassed, so the
> sandbox is a second independent gate.
>
> The `run.Buffer` fallback below is therefore still required. Reworking review to
> prefer a tagged session when one exists is left as separate work, and needs a
> decision about trust bootstrapping first.

- **Skills are passed verbatim, not paraphrased.** Codex injects its installed-skill catalog into every exec session and loads the matching `SKILL.md`; configured skills use codex's `$name` / `$plugin:name` form (`DiscoverReviewSkills` in `discovery.go`). Native `codex exec review` is not used — it rejects a prompt under a scope flag and can't carry Entire's scope/per-run/checkpoint context.
- **Live tokens come from the rollout file, not stdout.** `codex exec --json` carries `usage` only on the terminal `turn.completed`, and a review is a single turn. `review_tokens.go` resolves the rollout transcript by `thread_id` (from the `thread.started` envelope), tails it (the same `~/.codex/.../rollout-*-<thread-id>.jsonl` documented under Transcript above), and emits cumulative `Tokens` per `token_count` event — the source codex's interactive UI reads.
- **The fix manifest sources codex from live run output.** Because a tagged session cannot be *relied* on (see the sandbox caveat above), the manifest reads codex's **live run output** (`run.Buffer`), and `entire review fix` skill verification is advisory for codex (loose description match), not a hard block. Under a write-capable sandbox a `KindAgentReview` session now also exists alongside it.

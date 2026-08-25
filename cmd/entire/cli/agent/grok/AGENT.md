# Grok Build — Integration One-Pager

## Verdict: COMPATIBLE — with two hard prerequisites

Grok Build fits Entire's model well: real lifecycle hooks in Claude-compatible
nested JSON, and an append-only native JSONL transcript on disk. Two things must
be solved before any code is written, and neither is optional:

1. **Foreign-config collision.** Grok natively loads `.claude/settings.json` and
   `.cursor/hooks.json` (global *and* project scope) in addition to its own
   config. See [Foreign Config Collision](#foreign-config-collision).
2. **Project trust gate.** Hooks under `<project>/.grok/hooks/` are *silently
   skipped* until the project is trusted, so `entire enable` can appear to
   succeed while nothing ever fires. See [Project Trust](#project-trust).

> **Provenance.** Grok **1.0.5** (macos-aarch64) is installed and its bundled,
> version-matched user-guide at `~/.grok/docs/user-guide/` was read directly —
> so everything marked *(doc)* is confirmed against the shipping binary, not a
> web summary. Items marked *(verified)* were observed by running `grok inspect`
> or by reading Entire's own source. Items marked *(unverified)* still need a
> **live** session: `grok` is not authenticated (`grok login` is interactive and
> `XAI_API_KEY` is unset), so no hook has actually fired and no `updates.jsonl`
> exists yet. Run `scripts/test-grok-agent-integration.sh --manual-live` after
> logging in and fold the results back into this file.

> **Live run completed 2026-08-25.** Grok 1.0.5, authenticated as
> peyton@entire.io, one headless turn in a scratch repo (`grok-4.6`, 2 model
> calls, $0.042). Ten hook invocations captured, plus a real `updates.jsonl`.
> Everything below marked *(observed)* comes from that run. Payload examples are
> verbatim.

## Static Checks

| Check | Result | Notes |
|-------|--------|-------|
| Binary present | **PASS** *(verified)* | `~/.local/bin/grok` → `~/.grok/bin/grok`; also symlinks `agent` |
| Help available | **PASS** *(verified)* | `grok --help` |
| Version info | **PASS** *(verified)* | `grok 1.0.5 (5115b46bc909)` |
| Hook keywords | **PASS** *(verified)* | 15 lifecycle events; `grok inspect` reports a live Hooks section |
| Session keywords | **PASS** *(verified)* | `-r/--resume`, `-c/--continue`, `-s/--session-id` |
| Config directory | **PASS** *(verified)* | `~/.grok/` with `config.toml`, `docs/`, `completions/`, `downloads/` |
| Documentation | **PASS** *(verified)* | bundled at `~/.grok/docs/user-guide/` (26 files), version-matched |
| Authenticated | **PASS** *(observed)* | `grok login`, device OAuth, peyton@entire.io |
| Hooks fire | **PASS** *(observed)* | 10 invocations captured across 6 event types |
| Native transcript | **PASS** *(observed)* | `updates.jsonl`, 16 JSON-RPC lines, 7 update kinds |

## Binary

- Name: `grok`
- Version: *unverified*
- Install (macOS/Linux): `curl -fsSL https://x.ai/cli/install.sh | bash`
- Install (Windows): `irm https://x.ai/cli/install.ps1 | iex`
- Auth for CI/headless: `XAI_API_KEY`

Not to be confused with **`superagent-ai/grok-cli`**, an unaffiliated community
agent for the Grok API. This one-pager targets xAI's official **Grok Build**
(`xai-org/grok-build`). They have different config layouts and hook models.

## Hook Mechanism

- Config files: `~/.grok/hooks/*.json` (global, always trusted),
  `<project>/.grok/hooks/*.json` (project, **requires trust**), plus
  `~/.grok/config.toml` / `managed_config.toml` / `requirements.toml` in TOML.
  Multiple files; **all layers merge, none replaces another.** Identical hooks
  are deduplicated keeping the highest-authority copy.
- Format: JSON (or TOML) — the Claude Code nested shape, verbatim:

```json
{
  "hooks": {
    "Stop": [
      { "matcher": "",
        "hooks": [{ "type": "command", "command": "entire hooks grok stop", "timeout": 10 }] }
    ]
  }
}
```

- `type` is `"command"` or `"http"`. `matcher` is a **regex** (not a literal) and
  tests a different field per event — tool name on tool events, subagent type on
  subagent events, end reason on `SessionEnd`, compaction trigger on compaction
  events, error type on `StopFailure`, cancellation reason on `StopCancelled`.
- `timeout` defaults to **5s**, except `Stop`/`SubagentStop` which default to
  **600s**. Entire's hooks must stay well inside 5s on the non-gate events.
- Env-var expansion (`${VAR}`/`$VAR`) is supported in `command` and `url`.

### Event Mapping

Grok event names are **PascalCase in config** but arrive **snake_case in the
stdin payload's `hookEventName`** and in `$GROK_HOOK_EVENT`. Parse the snake_case
form.

| Native Hook | Fires | Blocking | Entire EventType |
|---|---|---|---|
| `SessionStart` | session begins | no | `SessionStart` |
| `UserPromptSubmit` | user submits a prompt | no | `TurnStart` |
| `Stop` | agent completes turn normally | **yes** | `TurnEnd` |
| `StopCancelled` | turn interrupted/rejected/capped | no | `TurnEnd` |
| `StopFailure` | turn ended on API error | no | `TurnEnd` |
| `SessionEnd` | session closes | no | `SessionEnd` |
| `PreCompact` | compaction begins | no | `Compaction` |
| `PostCompact` | compaction finishes | no | — (offset already reset by `PreCompact`) |
| `SubagentStart` | child agent starts | no | `SubagentStart` |
| `SubagentStop` | child agent's turn ends | **yes** | `SubagentEnd`, `Final: true` |
| `PostToolUse` (matcher `spawn_subagent\|Task`) | subagent launch returns | no | `SubagentEnd`, `Final: false` (launch stub) |
| `PostToolUse` (file tools) | tool completed | no | `ToolUse` |
| `PreToolUse` | tool about to run | **yes** | — (not needed) |
| `PostToolUseFailure`, `PermissionDenied`, `Notification` | — | no | — (not needed) |

**`StopCancelled`/`StopFailure` must map to `TurnEnd`.** `Stop` does *not* fire
when a turn is interrupted or errors out. Wiring only `Stop` loses a checkpoint
for every Ctrl+C'd, permission-rejected, rate-limited, or `max_turns`-capped
turn. This is the sharpest divergence from Claude Code, whose `Stop` covers all
of these.

**Tool-name aliases.** Matchers accept Claude names and map automatically:
`Bash`→`run_terminal_command`, `Read`→`read_file`,
`Edit`/`Write`/`MultiEdit`→`search_replace`, `Task`→`spawn_subagent`,
`Glob`/`ListDir`→`list_dir`, `Grep`→`grep`, `WebSearch`→`web_search`. A matcher
keeps the original name and matches both spellings.

### Hook Input (stdin JSON)

Common to all events: `hookEventName`, `sessionId`, `cwd`, `workspaceRoot`,
`timestamp`, `permissionMode`, and `promptId` (absent on session-scoped events).

```json
{
  "hookEventName": "pre_tool_use",
  "sessionId": "abc-123",
  "cwd": "/path/to/project",
  "workspaceRoot": "/path/to/project",
  "timestamp": "2026-04-14T12:00:00Z",
  "permissionMode": "default|auto|plan|bypassPermissions",
  "promptId": "turn-identifier",
  "toolName": "run_terminal_command",
  "toolInput": { "command": "npm test" },
  "toolUseId": "unique-id",
  "toolInputTruncated": false,
  "toolResult": "output from tool"
}
```

Per-event additions:

- **Tool events**: `toolName`, `toolInput`, `toolUseId`, `toolInputTruncated`; `toolResult` on `PostToolUse` only.
- **`Stop`**: `reason`, `lastAssistantMessage` (clipped to 32,768 chars), `stopHookActive`, `backgroundTasks[]`, `sessionCrons[]`.
- **`StopCancelled`**: `reason` (`user_interrupt`, `permission_rejected`, `permission_cancelled`, `max_turns`, `no_progress`, `unknown`), `cancelledBy` (`user`/`runtime`/`unknown`), `cancelTrigger` (`ctrl_c`, `esc`, `mouse`, `dashboard_stop`), `reasonDetails`, `lastAssistantMessage`, `subagentType`.
- **`StopFailure`**: `error` (`rate_limit`, `authentication_failed`, `invalid_request`, `server_error`, `max_output_tokens`, `unknown`), `errorDetails`, `lastAssistantMessage`.
- **Subagent events**: `subagentType`; `phase` on `SubagentStop`.
- **Compaction events**: `trigger` (`manual`/`auto`).
- **`Notification`**: `notificationType` (`idle_prompt`, `permission_prompt`, `task_complete`), `message`.

**`transcriptPath` IS provided** *(observed — corrects an earlier reading of the
docs, which never mention the field).* It carries the absolute path to
`updates.jsonl` and appears on `user_prompt_submit`, `stop`, and `session_end`.

**It is absent on `session_start`**, which instead carries `source` (`"new"`,
the matcher field for that event). So `ResolveSessionFile` can simply read
`transcriptPath` on every event that matters, and only `SessionStart` needs the
derived path — a large simplification over reproducing the encoding everywhere.

Verbatim `user_prompt_submit` *(observed)*:

```json
{
  "hookEventName": "user_prompt_submit",
  "sessionId": "01a03a9f-0f3b-78d2-bad3-cf2ad4e0ff2e",
  "cwd": "/private/tmp/.../grok-live",
  "workspaceRoot": "/private/tmp/.../grok-live/",
  "timestamp": "2026-08-25T20:31:38.990545+00:00",
  "transcriptPath": "/Users/peytonmontei/.grok/sessions/%2Fprivate%2Ftmp%2F.../01a03a9f-.../updates.jsonl",
  "promptId": "556780c0-0459-4d6b-98e2-2a13a718524b",
  "permissionMode": "bypassPermissions",
  "prompt": "<user_query>\nCreate a file named hello.txt...\n</user_query>"
}
```

Note `workspaceRoot` has a **trailing slash** and `cwd` does not. The prompt text
arrives wrapped in `<user_query>` tags.

### The two `Stop` events *(observed)*

A single turn produced **two** `stop` invocations plus a `session_end`, in this
order:

| t | event | `reason` | `promptId` | extra fields |
|---|---|---|---|---|
| 47.328 | `stop` | `end_turn` | present | `lastAssistantMessage`, `backgroundTasks`, `sessionCrons`, `stopHookActive` |
| 47.414 | `session_end` | `shutdown` | absent | — |
| 47.462 | `stop` | `shutdown` | **absent** | none of the above |

**`SessionEnd` fires between them.** Map `TurnEnd` only on the first — discriminate
on `reason != "shutdown"`, or equivalently on `promptId` being present. Without
that filter every session mints a duplicate, contentless `TurnEnd` checkpoint.

### Environment Variables

Always set: `GROK_HOOK_EVENT` (snake_case), `GROK_HOOK_NAME`, `GROK_SESSION_ID`,
`GROK_WORKSPACE_ROOT`, and `CLAUDE_PROJECT_DIR` (a Claude Code alias for the
workspace root). Plugin hooks also get `GROK_PLUGIN_ROOT`, `GROK_PLUGIN_DATA`.

### Hook Output & Exit Codes

| Code | Meaning |
|---|---|
| `0` | success / allow |
| `2` | explicit deny (`PreToolUse`) or block (`Stop`/`SubagentStop`); first stderr line becomes the reason |
| other | **fail-open** — recorded in scrollback, nothing blocked |

Fail-open is the default for timeouts, crashes, and malformed output. Entire's
hooks are observational, so they should always exit 0 and never emit a
`decision`. Note `Stop` supports `{"decision":"block"}` and
`hookSpecificOutput.additionalContext` — the latter is the `ContextInjector`
surface, though Grok's injection point is `UserPromptSubmit`, matching Claude
Code's `additionalContext` shape.

## Foreign Config Collision

Grok loads these in addition to its own config *(doc)*:

| Scope | Path | Trusted? |
|---|---|---|
| Global | `~/.claude/settings.json`, `settings.local.json` | always |
| Global | `~/.cursor/hooks.json` | always |
| Project | `<project>/.claude/settings.json`, `settings.local.json` | requires trust |
| Project | `<project>/.cursor/hooks.json` | requires trust |

**This is not hypothetical, and it is not confined to this repo — it is already
live machine-wide, before a single line of Grok integration code exists.**
Confirmed on 2026-08-25 by running `grok inspect` in this worktree:

```
  Project trusted: no
  Hooks (11)
  └ command                                user [claude]   x5
  └ command matcher=Agent                  user [claude]   x2
  └ command matcher=TaskCreate|TaskUpdate  user [claude]
  └ file                                   plugin: codex / explanatory-output-style / superpowers
```

All eight `user [claude]` entries are **Entire's own hooks**, read out of
**user-global `~/.claude/settings.json`** — `SessionStart`, `UserPromptSubmit`,
`Stop`, `SubagentStop`, `SessionEnd`, `PreToolUse[Agent]`,
`PostToolUse[Agent]`, `PostToolUse[TaskCreate|TaskUpdate]`, every one of them
`sh -c '… exec entire hooks claude-code <verb>'`.

Two things make this worse than a repo-scoped conflict:

- **The user scope has no trust gate.** `Project trusted: no` here, so the
  *project* `.claude/settings.json` and `.cursor/hooks.json` hooks are correctly
  withheld — but the eight user-global ones loaded anyway. Grok will therefore
  fire Entire's Claude Code hooks in **every directory the user ever runs `grok`
  in**, trusted or not. Trusting this project would add the project's 8 Claude +
  7 Cursor hooks on top.
- **The payloads are incompatible, so the result is malformed, not merely
  mislabelled.** Verified by reading `cmd/entire/cli/agent/claudecode/types.go:39`
  and `claude.go:81`: Entire's Claude parser binds `json:"session_id"` and
  `json:"transcript_path"`. Grok sends camelCase `sessionId` and **no transcript
  path field at all**. So `GetSessionID()` returns `""` and `TranscriptPath` is
  empty for every Grok-driven invocation.

Consequences to design against:

- **Misattribution** — a Grok session recorded as Claude Code.
- **Empty session identity** — `GetSessionID()` → `""`, so session state cannot key correctly.
- **No transcript** — nothing to checkpoint; the transcript-derived paths have no input.
- **Double-firing** — once the project is trusted, claude-code *and* cursor hooks both fire per Grok event.

Grok's hooks fail open, so `grok` itself will not break. The damage lands on
Entire's side, in whatever repo the user happens to be in.

### Proven in Grok's own transcript *(observed)*

The live run was done in a **clean scratch repo with Entire not enabled**, yet
Grok's `updates.jsonl` records Entire's global hooks executing. `hook_execution`
entries name their source (`global/settings:` = `~/.claude/settings.json`;
`project/entire-probe:` = the probe's own hooks):

```
session_start        global/settings:session_start[0].hooks[0]              success
user_prompt_submit   global/settings:user_prompt_submit[0].hooks[0]         success
stop                 global/settings:stop[0].hooks[0]                       success
session_end          global/settings:session_end[0].hooks[0]                success
```

Four of the eight fired; the other four are matcher-gated on `Agent` /
`TaskCreate|TaskUpdate` and no subagent ran. Every one reported `success`,
because Entire's wrapper exits 0 — the failure is silent by construction.

### The off-switch *(doc, `05-configuration.md:371`)*

Vendor compatibility scanning is configurable per vendor and per surface, and
defaults to `true` for every cell:

```toml
[compat.claude]
hooks = false     # stop scanning ~/.claude/settings.json and <cwd>/.claude/settings.json
```

Resolution is **env var > `config.toml` > default (on)**, and there is an
equivalent `[compat.cursor] hooks`. This turns the mitigation from a design
problem into a config write.

The tradeoff is real, though: the cell is all-or-nothing, so disabling it also
drops any *legitimate* Claude hooks the user wants Grok to honor. It also lives
in user-global `~/.grok/config.toml`, so Entire should **not** write it silently.

Recommended shape, cheapest first:
1. `entire doctor` detects Grok installed + Entire hooks in `~/.claude/settings.json`, and prints the `[compat.claude] hooks = false` fix.
2. `entire enable` for Grok warns and offers to write it, with consent.
3. The Grok hook handler defensively no-ops when a Grok-shaped payload arrives on a `claude-code`/`cursor` verb — belt and braces, since the user may want the compat cell left on.

> Worth fixing **independently of shipping the Grok agent** — it is a present-day
> cross-agent bug on any machine with both Entire's global Claude hooks and Grok
> installed, and it needs no Grok integration code to trigger.

Also note Grok maps Cursor's camelCase event names onto its own PascalCase set
(`beforeSubmitPrompt`→`UserPromptSubmit`, `sessionStart`→`SessionStart`,
`afterAgentResponse`→`PostToolUse`, …), so the Cursor hooks will fire on plausible
events rather than failing loudly.

**Open decision for the implementer — needs a human call.** Options, roughly in
order of preference:
1. Have the Grok hook handler detect a Grok payload arriving on a `claude-code`
   or `cursor` verb and no-op with a warning (defensive, no config surgery).
2. Have `entire enable` warn when both Grok and Claude Code/Cursor are enabled
   in one repo.
3. Do nothing and document it. Not recommended — the failure is silent.

## Project Trust

The first time a project with hooks is opened, the user must trust it or
**project hooks are silently skipped**. Trust is granted with the `--trust` flag
or the `/hooks-trust` slash command, and recorded in
`~/.grok/trusted_folders.toml`.

`entire enable` writes hooks into the working tree, so it lands squarely in the
untrusted case: install reports success, and no hook ever fires. The
enable/status path should check `~/.grok/trusted_folders.toml` for the repo root
and tell the user to run `/hooks-trust`. Installing into the always-trusted
global `~/.grok/hooks/` instead would sidestep this, but at the cost of firing
Entire hooks in every repo the user opens — not acceptable.

## Transcript

- Location: `~/.grok/sessions/<url-encoded-cwd>/<session-id>/updates.jsonl`
- Format: **JSONL**, but each line is a **JSON-RPC envelope**, not a bare event
  *(observed)*. The payload is nested two levels down:

```json
{"timestamp":1787689898,"method":"_x.ai/session/update",
 "params":{"sessionId":"01a03a9f-...","update":{"sessionUpdate":"turn_completed", ...}}}
```

  Read the kind at `params.update.sessionUpdate` — **not** at the top level.

- `sessionUpdate` kinds seen in one 2-call turn *(observed)*:

| kind | n | fields |
|---|---|---|
| `hook_execution` | 7 | `event_name`, `prompt_id`, `tool_name`, `runs[]` |
| `agent_thought_chunk` | 2 | `content` |
| `agent_message_chunk` | 2 | `content` |
| `tool_call_update` | 2 | `toolCallId`, `status`, `title`, `kind`, `locations`, `rawInput`, `rawOutput`, `content`, `_meta` |
| `user_message_chunk` | 1 | `content`, `_meta` |
| `tool_call` | 1 | `toolCallId`, `title`, `rawInput`, `_meta` |
| `turn_completed` | 1 | `prompt_id`, `stop_reason`, `usage` |

  `tool_call_update.locations` is the natural input for
  `ExtractModifiedFilesFromOffset`; `turn_completed.usage` for
  `CalculateTokenUsage`. Chunked message kinds mean a single logical message
  spans several lines — the same streaming-boundary problem `compact` already
  handles for other agents.
- Session ID: UUIDv7 generated by Grok, or client-supplied via `-s`. Available
  in every hook payload as `sessionId` and in `$GROK_SESSION_ID`.
- Actual session dir after one turn *(observed)* — richer than the docs list:
  `updates.jsonl`, `chat_history.jsonl`, `events.jsonl`, `summary.json`,
  `signals.json`, `rewind_points.jsonl`, `prompt_context.json`,
  `resources_state.json`, `system_prompt.txt`, `title_refresh_idx`, plus
  `.lock` sidecars for the append-only files. `plan.json`, `feedback.jsonl`,
  `compaction_checkpoints/`, and `subagents/` are created on demand and were
  absent here. **`events.jsonl`, `prompt_context.json`, `resources_state.json`,
  `system_prompt.txt` and the `.lock` files are undocumented.**
- `ProtectedDirs()` should return `.grok`.
- **`grok export <SESSION_ID> [OUTPUT]` exists** *(observed — corrects the earlier
  "no export command" reading)*, but it emits **Markdown**, which is lossy and a
  derived view. Per the native-format-preservation principle, keep reading
  `updates.jsonl` directly: the JSONL file is the canonical artifact, and the
  Markdown export is for humans.
- Token usage: lines with `sessionUpdate == "turn_completed"` carry a usage
  breakdown (this is how `ccusage` reads Grok). `signals.json` also holds
  counters. Good candidate for `TokenCalculator`.

**Group-directory naming, now documented** *(doc, `17-sessions.md:24`)*: Grok
**URL-encodes the working directory** to name the group. **When the encoded name
exceeds 255 bytes it instead uses a slug plus a hash, and records the original
path in a `.cwd` file inside the group directory.**

`GetSessionDir` therefore cannot be a pure function of cwd. It needs both
branches: encode-and-join for the common case, and a scan of
`~/.grok/sessions/*/.cwd` for the long-path case. Measured for reference —
this worktree encodes to 131 bytes (`/` left safe) or 151 bytes (`/` escaped),
so it takes the common branch, but a deeper worktree on a long `$HOME` will not.

**Encoding resolved** *(observed)*: `/` **is** percent-escaped. The group name is
the full absolute path quoted with nothing safe — Python's
`urllib.parse.quote(path, safe="")`:

```
~/.grok/sessions/%2Fprivate%2Ftmp%2Fclaude-501%2F...%2Fgrok-live/<session-id>/
```

Non-ASCII is still unverified (this repo's path holds a `’` → presumably
`%E2%80%99`), as is the >255-byte slug+hash branch. Both are secondary now that
`transcriptPath` is supplied directly on the events that need it.

Store `updates.jsonl` in `NativeData` unmodified — do not merge it with
`chat_history.jsonl` or reshape it. Existing JSONL chunking helpers apply.

## Config Preservation

`.grok/hooks/*.json` supports multiple independent files, all merged. That is a
meaningfully better position than Claude Code's shared `settings.json`: Entire
can own `.grok/hooks/entire.json` exclusively and never read-modify-write a file
a user also edits. Uninstall becomes a file delete.

Still route removal through `agent.DropStaleManagedHooks` so hooks written by
older CLI versions under a different filename or command shape are replaced
rather than left firing alongside the current one.

Hook commands must name the `entire` binary resolved through PATH — never a path
inside the working tree.

## CLI Flags

| Purpose | Flag |
|---|---|
| Single prompt, exit | `-p, --single <PROMPT>` |
| Prompt from file / JSON | `--prompt-file <PATH>`, `--prompt-json <JSON>` |
| New session with fixed UUID | `-s, --session-id <UUID>` |
| Resume by ID or title | `-r, --resume <ID_OR_TITLE>` |
| Continue most recent in cwd | `-c, --continue` |
| Output format | `--output-format plain\|json\|streaming-json\|streaming-messages-json` |
| Auto-approve tools | `--permission-mode bypassPermissions` — **`--yolo` does not exist in 1.0.5** *(observed)* |
| Trust project hooks | `--trust` *(observed: accepted; undocumented in `--help`)* |
| Permission modes | `default`, `acceptEdits`, `auto`, `dontAsk`, `bypassPermissions`, `plan` |
| Structured output | `--json-schema <SCHEMA>` (implies `--output-format json`) |
| Working directory | `--cwd <PATH>` |
| Model | `-m, --model <MODEL>` |
| Cap iterations | `--max-turns <N>` |
| Suppress update check | `--no-auto-update` |

Exit codes: `0` success, `1` error, `130` SIGINT, `143` SIGTERM.

E2E runner shape:

```bash
grok --trust -p "Create a file named hello.txt containing exactly the word hello." \
     --permission-mode bypassPermissions --output-format json
```

**Verified working** *(observed)*. Returns `{text, stopReason, sessionId,
requestId, thought, usage{input_tokens, output_tokens, reasoning_tokens,
cache_read_input_tokens, total_tokens}, num_turns, total_cost_usd, modelUsage{}}`
— note `sessionId` *and* per-model cost, which is more than the docs promised and
is ideal for an E2E runner. Measured: 2 model calls, 32,057 total tokens,
$0.042094 on `grok-4.6`.

`--output-format json` returns `{text, stopReason, sessionId, usage, total_cost_usd}`,
so the runner can capture `sessionId` directly rather than scraping the sessions
directory. `-s <UUID>` lets a test pin the session ID up front, which is simpler
still.

`FormatResumeCommand` → `grok --resume <session-id>`.

## Gaps & Limitations

1. ~~`<url-encoded-cwd>` encoding~~ — **resolved**: `/` is escaped (`quote(path, safe="")`). Non-ASCII and the >255-byte slug+hash branch remain unverified, but are now secondary since `transcriptPath` is supplied on the events that matter.
2. **`sessionUpdate` variants are undocumented** — 7 kinds observed in one trivial turn, so the full set is certainly larger. Compaction, subagent, and error-path kinds are unseen. Enumerate empirically before relying on any exhaustive switch.
3. ~~No `transcript_path` in payloads~~ — **resolved**: `transcriptPath` is present on `user_prompt_submit`, `stop`, `session_end`; absent only on `session_start`.
4. **`Stop` fires twice per session** *(observed)* — see the table above; discriminate on `reason`/`promptId`. Additionally, per the docs: After a blocking `Stop`, the gate re-fires per continuation round, capped at 8. `stopHookActive` distinguishes a continuation. An extra observe-only `Stop` also fires at session teardown with `reason: "channel_closed"` or `"shutdown"`. Without matcher/field filtering this yields duplicate `TurnEnd` checkpoints — filter on `stopHookActive` and `reason`.
5. **Foreign-config collision** — see above; needs a product decision.
6. **Project trust gate** — see above; silent failure mode for `entire enable`.
7. **Subagent transcripts** — a `subagents/` directory exists per session but its layout is undocumented. Relevant to `SubagentSessionResolver` and `Event.SubagentTranscriptPath`; the Droid Workers path is the closest precedent.
8. **Coverage of the live run was one trivial turn** — no subagent, no compaction, no interrupt, no failure, no multi-turn session. `SubagentStart`/`SubagentStop`/`StopCancelled`/`StopFailure`/`PreCompact`/`PostCompact` and `Notification` are **still unobserved**, and those are exactly the paths with the trickiest mappings.
9. **`SessionStart` does not fire for a subagent's own session** *(doc, `10-hooks.md:90`)*, and **`SessionEnd` carries `subagentType` for a child session** so a host can distinguish a child teardown from its own. Both matter for subagent bookkeeping.
10. **`SubagentStop` fires once, inside the subagent** *(doc, `10-hooks.md:101`)* — not in the parent. Child sessions live in the normal sessions tree with only `meta.json` under the parent's `subagents/`, which is the `SubagentSessionResolver` shape (cf. Droid Workers), not a blocking tool call.
11. **Grok has its own worktree system** under `~/.grok/worktrees/` with `grok worktree gc`/`rm` and `grok du`. Unexamined; may interact with Entire's own worktree assumptions.

## E2E Runner

`e2e/agents/grok.go` registers Grok with the E2E framework (`ForEachAgent`
picks it up automatically). Modeled on the Codex runner, which has the closest
shape — an isolated agent home plus a folder-trust gate.

| | |
|---|---|
| Name / Binary / EntireAgent | `grok` / `grok` / `grok` |
| PromptPattern | `❯` *(observed in the 1.0.5 TUI)* |
| TimeoutMultiplier | 1.5 |
| Concurrency gate | 2 |
| Headless invocation | `grok --trust --permission-mode bypassPermissions -p <prompt>` |
| Model override | `E2E_GROK_MODEL`, else Grok's default (`grok-4.6`) |
| Auth | `XAI_API_KEY`, else symlink the developer's `~/.grok/auth.json` |

Each run gets an isolated `GROK_HOME` under the user cache dir (not `/tmp`),
seeded by `seedGrokHome` with three things:

1. `[compat.claude] hooks = false` and `[compat.cursor] hooks = false` —
   **load-bearing, not hygiene.** `GROK_HOME` does not isolate `~/.claude`, so
   without this every E2E turn on a developer machine also fires
   `entire hooks claude-code ...` against Grok payloads. See
   [Foreign Config Collision](#foreign-config-collision).
2. `trusted_folders.toml` pre-trusting the test repo, since project hooks are
   silently skipped in an untrusted folder. `--trust` is passed as well; the
   file covers the paths the flag does not.
3. `[features] telemetry = false`.

`StartSession` additionally dismisses the first-run coding-data consent banner
("Help improve Grok", `[Opt out]` / `[Opt in]`), which renders over the input
caret and is *not* covered by the telemetry switch — that banner is the
separate `/privacy` setting.

### Hazard: Grok installs a binary named `agent`

The installer symlinks **both** `grok` and `agent` into `~/.local/bin`
(→ `~/.grok/bin/`). `agent` is also Cursor CLI's binary name, and
`e2e/agents/cursor_cli.go` declares `Binary() == "agent"`.

On a machine with both installed, whichever wins `$PATH` decides what the
`cursor-cli` E2E leg actually runs. Preflight only does `LookPath("agent")`, so
it passes either way and the suite would exercise Grok while reporting Cursor.
Verified on this machine (Cursor not installed, so nothing was displaced):

```
$ command -v agent
/Users/…/.local/bin/agent -> /Users/…/.grok/bin/agent
$ agent --version
grok 1.0.5 (5115b46bc909) [stable]
```

Not fixed here — it belongs to the Cursor runner, not this one. Cheapest guard
is a `Bootstrap()` check in `cursor_cli.go` asserting `agent --version` does not
report Grok.

## Captured Payloads

Ten invocations captured on 2026-08-25 across `session_start`,
`user_prompt_submit`, `pre_tool_use`, `post_tool_use`, `stop` (×2), and
`session_end`. Verbatim examples are inlined above; raw captures live under the
scratch run's `.entire/tmp/probe-grok-*/captures/` and are not checked in.

To extend coverage to the unobserved events, run in a **scratch repo** (never a
real one — Entire's global Claude hooks fire under Grok):

```bash
scripts/test-grok-agent-integration.sh \
  --run-cmd 'grok --trust -p "<prompt exercising subagents / compaction>" --permission-mode bypassPermissions'
```

Artifacts land in `.entire/tmp/probe-grok-*/captures/` (gitignored), alongside an
`updates.jsonl.sample` and the printed encoded-cwd segment.

## Sources

- Hooks: `xai-org/grok-build` → `crates/codegen/xai-grok-pager/docs/user-guide/10-hooks.md`
- Sessions: same tree, `17-sessions.md`
- Headless: same tree, `14-headless-mode.md`
- Installed copies of all of the above: `~/.grok/docs/user-guide/` (v1.0.5, authoritative)
- Overview: https://docs.x.ai/build/overview
- Sessions (hosted): https://docs.x.ai/build/features/sessions

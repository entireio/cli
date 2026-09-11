# OpenCode — Subagent Tracking One-Pager

Scope: native subagent (child session) tracking for an agent that is already
integrated. Session/turn lifecycle, transcript export, and plugin installation
are covered by the existing integration (`entire_plugin.ts`, `lifecycle.go`,
`cli_commands.go`); this page records only what the Task tool adds.

## Verdict: COMPATIBLE

Every signal Entire needs is delivered to the plugin, in a deterministic order,
with a stable join key (the child session ID) present on the launch signal, the
completion signal, and the child's own transcript. No fallback correlation is
required.

## Static Checks

| Check | Result | Notes |
|-------|--------|-------|
| Binary present | PASS | `/opt/homebrew/bin/opencode` |
| Version info | PASS | 1.18.30 (verified against this version) |
| `run --agent` flag | PASS | `opencode run --agent <name>` selects a primary agent; subagents are invoked by the model through the `task` tool |
| `export` subcommand | PASS | `opencode export <sessionID>` works for child sessions and from any cwd |
| Plugin hooks | PASS | `event`, `tool.execute.before`, `tool.execute.after`, `chat.message` all fire for child sessions |
| Session store | PASS | SQLite at `~/.local/share/opencode/opencode.db`; `session.parent_id` column is indexed |
| Documentation | PASS | https://opencode.ai/docs/agents/ (subagents), https://opencode.ai/docs/plugins/ (hooks) |

## Subagent Model

- Subagents are agents with `mode: subagent`. Built-ins: `general` (full tools),
  `explore` (read-only), `scout`. Users add more under `.opencode/agents/` or in
  `opencode.json`.
- The primary agent invokes one through the **`task` tool** with args
  `{description, prompt, subagent_type, task_id?, background?}`. `@general …`
  in a prompt forces the same path.
- Each invocation creates a **child session** (`sessions.create({parentID,
  title: "<description> (@<agent> subagent)", agent})`) and runs a full
  prompt/turn inside it. The child is a real session: it has its own ID,
  messages, parts, tokens, and an `opencode export` payload.
- The `task` tool blocks the parent turn until the child is idle (foreground).
  Background children exist behind `OPENCODE_EXPERIMENTAL_BACKGROUND_SUBAGENTS=true`
  (`background: true` arg, `metadata.background`, result injected later as a
  synthetic user part). Not exercised; see Gaps.
- `task_id` resumes an existing child session instead of creating one, so one
  child session ID can back several `task` tool calls (several `callID`s).
- Nesting is capped by `subagent_depth` (default 1): a child cannot call `task`
  unless the user raises it. Children also get `task` denied in their permission
  list by default.

## Captured Signal Order (one foreground `general` child, OpenCode 1.18.30)

All on the plugin. `P` = parent session, `C` = child session. Times are ms
offsets from the `tool.execute.before`.

| Δms | Signal | Session | Payload facts |
|----:|--------|---------|---------------|
| -1 | `event message.part.updated` | P | task part `status: pending`, `callID`, no metadata yet |
| 0 | **`tool.execute.before`** | P | `input: {tool: "task", sessionID: P, callID}`, `output.args: {prompt, subagent_type, description}` |
| +5 | **`event session.created`** | C | `info.parentID: P`, `info.agent: "general"`, `info.title: "<description> (@general subagent)"`, `info.directory` |
| +7 | `event message.part.updated` | P | task part `status: running`, **`state.metadata: {parentSessionId: P, sessionId: C, model}`**, `state.time.start` |
| +11 | `chat.message` | C | `input: {sessionID: C, agent: "general", model, messageID}`; the child's user message is the task prompt |
| +127 | `event message.updated` | C | `info.role: user`, `info.agent: general` |
| … | child turn | C | `session.status busy`, tool parts (`read`, `write` …), `tool.execute.before/after` for each child tool with `sessionID: C`, `file.edited` |
| +19644 | `event session.status` | C | `status.type: idle`, then `session.idle` |
| +19645 | **`tool.execute.after`** | P | `input: {tool: "task", sessionID: P, callID, args}`, `output: {title, output: "<task id=\"C\" state=\"completed\">…", metadata: {parentSessionId: P, sessionId: C, model, truncated}}` |
| +19647 | `event message.part.updated` | P | task part `status: completed`, same metadata, `state.time: {start, end}` |

Raw captures: `probe-opencode-subagent.*/captures/events.jsonl` (kept under
`$TMPDIR`, not committed). Re-run with
`scripts/test-opencode-subagent-integration.sh --run-cmd`.

### Join keys

| Purpose | Key | Where it appears |
|---------|-----|------------------|
| Entire `ToolUseID` | `callID` of the `task` tool part | `tool.execute.before/after.input.callID`, parent `message.part.updated.part.callID`, parent export `parts[].callID` |
| Entire `SubagentID` / child transcript | child session ID (`ses_…`) | `session.created.info.id` (with `parentID`), task part `state.metadata.sessionId`, `tool.execute.after.output.metadata.sessionId`, `<task id="…">` in output text, child export `info.id` |
| Parent | parent session ID | `session.created.info.parentID`, `tool.execute.*.input.sessionID`, task metadata `parentSessionId` |
| `SubagentType` | `args.subagent_type` / `session.created.info.agent` | both |
| `TaskDescription` | `args.description` / task part `state.title` | both |

`callID` is model-generated (`toolu_…` for Anthropic, opaque 16-char strings for
Gemini) and unique within the parent. The child session ID is the durable
identity that survives `task_id` resumption and is what `opencode export` takes.

## Child Transcript and Tokens

- `opencode export <childID>` returns the same `ExportSession` shape the
  integration already parses: `info` (now with `parentID`, `agent`, `title`,
  `tokens`) and `messages[]` with `parts[]`. Existing `ExtractModifiedFiles`,
  `ExtractAllUserPrompts`, and `CalculateTokenUsage` apply unchanged.
- Child file attribution comes from the child's own `write`/`edit`/`apply_patch`
  parts (`state.metadata.filepath`, `state.input.filePath`), never from the
  parent — the parent transcript holds only the `task` part.
- Per-child tokens are exact: every child assistant message carries `tokens`
  and `info.tokens` is the session total. This is better than Copilot (total
  only, after the last hook) and on par with Codex.
- Child assistant messages carry `info.parentID` too, but that is the **message**
  parent (the user message ID), not the session parent. Do not confuse the two.

## Current Entire Behaviour (before this work)

Confirmed live with Entire enabled in the probe repo:

- The plugin's `session.created` handler calls `resetSessionTracking(child)`
  and fires `session-start` for the child, so the child becomes a **separate
  top-level Entire session**. `entire session list` shows two OpenCode
  sessions; the child's row carries the task prompt as its "user" prompt.
- The child's `write` produced the checkpoint (shadow branch on the child
  session), and the commit condensed **only the child**. The parent — the
  session the user actually drove — logged "no files modified, skipping
  checkpoint" and has no checkpoint linkage. The trail therefore shows the
  subagent's prompt as the session, not the user's.
- The reset also clears `seenUserMessages`, so when the parent's events resume
  after the child, the plugin re-fires `session-start` and `turn-start` for the
  parent mid-turn (phase already ACTIVE, so no state damage observed, but
  duplicate hook processes and a second `opencode export`).
- No task record is written; `checkpoint list --pending` shows no `[Task]` rows.

This is the OpenCode analogue of Pi issue #1870: a child mistaken for a
top-level session.

## Proposed Mapping

| Native signal | Entire EventType | Notes |
|---------------|------------------|-------|
| `tool.execute.before` with `tool == "task"` on P | `SubagentStart` | `ToolUseID = callID`, `SessionID = P`, `SubagentType`/`TaskDescription` from `args`. Child ID is **not** known yet here. |
| parent `message.part.updated`, task part `status: running` with `metadata.sessionId` | (attach) | First moment the child ID is bound to the `callID`; alternative launch signal if the child ID is wanted at start. |
| `session.created` with `info.parentID` | (suppress) | Must **not** fire `session-start`; record child→parent in plugin state instead. |
| child `message.updated` (user) / `session.status` / `session.idle` | (suppress) | Must not fire `turn-start`/`turn-end` for a session whose `parentID` is set. |
| `tool.execute.after` with `tool == "task"` on P | `SubagentEnd` | `ToolUseID = callID`, `SubagentID = output.metadata.sessionId`, `SubagentTranscriptPath` = export path of the child (`.entire/tmp/<childID>.json`, produced via `opencode export` at hook time), `ModifiedFiles` from the child export. Single-signal agent: leave `Final` false unless background tasks are supported. |

The plugin must forward `parentID` (or a `parent_session_id` field) so the Go
side can make the suppression decision from the payload rather than from
ordering. `session.created` arrives 5 ms after `tool.execute.before`, and the
child's first `message.updated` 120 ms later, so plugin-side state keyed on
`callID → childID` is sufficient for foreground tasks; the `session.created`
event itself is the authoritative parent link.

## Gaps & Limitations

- **Background subagents** (`OPENCODE_EXPERIMENTAL_BACKGROUND_SUBAGENTS=true`)
  return from `tool.execute.after` immediately with `metadata.background: true`
  and `state: "running"`; true completion is a later synthetic user part on the
  parent. Not exercised. If supported later it needs the two-signal `Final`
  model (Claude Code shape), keyed on the child's own `session.status idle`.
- **`task_id` resumption** reuses a child session across several `callID`s.
  A task record per `callID` would share one child transcript; a record per
  child session would span turns (Droid Worker shape, `UpsertCompletedTaskRecord`).
- **Model-specific `callID` format**: opaque and not globally unique; the child
  session ID is the safe cross-process key.
- **Nested subagents** are off by default (`subagent_depth: 1`); when enabled,
  `parentID` chains and the same mapping applies recursively.
- **Plugin runs in-process with OpenCode**: hook processes spawned from
  `tool.execute.after` should stay short. `opencode export` of the child at
  that point is a local DB read (tens of ms observed).
- The `general` child with `gemini-2.5-flash` returned an empty
  `<task_result>` text; result text is not needed for tracking.
- Anthropic auth stored in OpenCode on this machine was rejected (401); the
  probe ran on `google/gemini-2.5-flash`. Nothing in the contract is
  provider-specific except `callID` spelling.

## Verification Runs (2026-09-11, OpenCode 1.18.30, gemini-2.5-flash)

| Scenario | Children | Result |
|----------|----------|--------|
| `single` — one `general` child writes `docs/red.md` | 1 | Order above. Entire: child became its own session and took the checkpoint; parent "no files modified". |
| `concurrent` — two `general` children launched in one assistant message (`red.md`, `blue.md`) | 2 | Both `tool.execute.before(task)` fired (seq 106, 116) before either child finished (221, 303). Each `tool.execute.after.metadata.sessionId` named a different child; each child's `write` carried its own `sessionID`, so per-child file attribution is disjoint by construction. Second child's `session.created` came 190 ms after the first's; per task the order `before → session.created → part running (metadata binds callID→sessionId)` held. Entire: three top-level sessions, checkpoint condensed both children, parent again empty. |
| `readonly` — one `explore` child lists files and reads README | 1 | Identical signals, no `write`/`edit`, child export has tokens but no file parts. Entire: child still became a separate session (no prompt shown in `session list` because its first hook was `turn-start`), no checkpoint anywhere. |

Design consequence: bind `callID → childID` from the task part's
`state.metadata` (or `tool.execute.after.output.metadata`), never from
"the next `session.created` after a `tool.execute.before`" — with two
launches in one message the creations can interleave.

## Captured Payloads

- Contract captured 2026-09-11 with OpenCode 1.18.30, one foreground `general`
  child creating one file, Entire enabled alongside the probe plugin.
- Static cross-check: 24 historical child sessions in the local SQLite store
  (1.2.x–1.18.x) all carry `parent_id` and export with `info.parentID`; task
  parts from 1.2.9 carried `metadata.sessionId` but not `parentSessionId`,
  which was added later — treat `parentSessionId` as optional.
- Probe script: `scripts/test-opencode-subagent-integration.sh`
  (`--run-cmd` automated, `--manual-live` interactive, `--no-entire` raw
  signals only, `--scenario single|concurrent|readonly`; `OPENCODE_MODEL`,
  `ENTIRE_BIN`, `PROBE_KEEP=1`).
